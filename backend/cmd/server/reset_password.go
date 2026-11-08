package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/auth"
	"github.com/nianzhibai/91/backend/internal/backup"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
)

func runServerCommand(args []string, in io.Reader, out, errorOut io.Writer) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "frontend":
		return true, runFrontendCommand(args[1:], out, errorOut)
	case "storage-paths":
		return true, runStoragePathsCommand(args[1:], out, errorOut)
	case "reset-password":
		return true, runResetPasswordCommand(args[1:], in, out, errorOut)
	case "hash-password":
		if len(args) != 1 {
			return true, errors.New("hash-password does not accept arguments")
		}
		return true, runHashPasswordCommand(in, out)
	case "help", "--help", "-h":
		_, err := fmt.Fprintln(out, "Usage: server [frontend status|install|activate|rollback|reset [options] | reset-password [--config PATH] [--user-id ID] | storage-paths [--config PATH] | hash-password]")
		return true, err
	default:
		return true, errors.New("unknown command; use server --help")
	}
}

func applicationConfigPath() string {
	if path := os.Getenv("VIDEO_CONFIG"); path != "" {
		return path
	}
	return "./config.yaml"
}

func runResetPasswordCommand(args []string, in io.Reader, out, errorOut io.Writer) error {
	flags := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	flags.SetOutput(errorOut)
	configPath := flags.String("config", applicationConfigPath(), "existing server configuration file")
	userIDArg := flags.String("user-id", "", "existing user ID; omitted to select interactively")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("reset-password does not accept positional arguments or a custom password")
	}
	userIDProvided := false
	flags.Visit(func(f *flag.Flag) { userIDProvided = userIDProvided || f.Name == "user-id" })
	var userID int64
	if userIDProvided {
		var err error
		userID, err = parseResetUserID(*userIDArg)
		if err != nil {
			return err
		}
	}

	// Maintenance reads the file directly: Load would create a missing config,
	// and Catalog.Open would run unrelated startup migrations.
	data, err := os.ReadFile(*configPath)
	if err != nil {
		return fmt.Errorf("read existing configuration: %w", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return fmt.Errorf("parse configuration: %w", err)
	}
	workingDir, err := os.Getwd()
	if err != nil {
		return err
	}
	storage, err := config.ResolveStoragePaths(cfg.Storage, workingDir)
	if err != nil {
		return err
	}
	info, err := os.Stat(storage.DBPath)
	if err != nil {
		if os.IsNotExist(err) {
			legacy := filepath.Join(filepath.Dir(storage.DBPath), "video-site.db")
			if _, legacyErr := os.Stat(legacy); legacyErr == nil {
				return errors.New("legacy database needs migration; start the new server once before resetting passwords")
			}
		}
		return fmt.Errorf("locate existing database: %w", err)
	}
	check := func() error {
		if _, err := os.Lstat(backup.PendingMarkerPath(storage.DataDir)); err == nil {
			return errors.New("backup restore is pending; retry after it has completed")
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("check pending restore: %w", err)
		}
		current, err := os.Stat(storage.DBPath)
		if err != nil {
			return err
		}
		if !os.SameFile(info, current) {
			return errors.New("database was replaced; run reset-password again")
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	ctx := context.Background()
	cat, err := catalog.OpenExisting(ctx, storage.DBPath)
	if err != nil {
		return fmt.Errorf("open existing database: %w", err)
	}
	defer cat.Close()
	if !userIDProvided {
		users, err := cat.ListUsers(ctx)
		if err != nil {
			return fmt.Errorf("list users: %w", err)
		}
		if len(users) == 0 {
			return errors.New("no existing users to reset")
		}
		if _, err := fmt.Fprintln(out, "ID | Username | Role | Account banned"); err != nil {
			return err
		}
		for _, user := range users {
			if _, err := fmt.Fprintf(out, "%d | %q | %s | %t\n", user.ID, user.Username, user.Role, user.Banned); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprint(out, "User ID to reset: "); err != nil {
			return err
		}
		scanner := bufio.NewScanner(in)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("read user ID: %w", err)
			}
			return errors.New("no user ID supplied; password was not changed")
		}
		userID, err = parseResetUserID(scanner.Text())
		if err != nil {
			return err
		}
	}
	user, err := cat.GetUserByID(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("user ID does not exist")
	}
	if err != nil {
		return fmt.Errorf("load user: %w", err)
	}
	password, err := auth.GeneratePassword()
	if err != nil {
		return fmt.Errorf("generate password: %w", err)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash generated password: %w", err)
	}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := cat.UpdateUserPasswordWithCheck(writeCtx, userID, hash, check); err != nil {
		return fmt.Errorf("reset password (database may be busy or restoring): %w", err)
	}
	if _, err := fmt.Fprintf(out, "Password reset for %q (ID: %d).\nNew password: %s\n", user.Username, userID, password); err != nil {
		return errors.New("password reset committed, but output failed; run reset-password again")
	}
	return nil
}

func parseResetUserID(value string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("user ID must be a positive integer")
	}
	return id, nil
}
