package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/frontend"
	"github.com/nianzhibai/91/backend/internal/httpapi"
)

func runFrontendCommand(args []string, out, errorOut io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: server frontend status|install|activate|rollback|reset [options]")
	}
	if args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(out, "Usage: server frontend status|install|activate|rollback|reset [--config PATH] [--repository owner/name --version v1.0.0 | --package FILE --sha256 HASH | --id name@version]")
		return err
	}
	action := args[0]
	flags := flag.NewFlagSet("frontend "+action, flag.ContinueOnError)
	flags.SetOutput(errorOut)
	configPath := flags.String("config", applicationConfigPath(), "server configuration")
	repository := flags.String("repository", "", "frontend GitHub repository")
	version := flags.String("version", "", "explicit frontend release tag")
	archive := flags.String("package", "", "local frontend.tar.gz")
	checksum := flags.String("sha256", "", "expected local archive SHA-256")
	id := flags.String("id", "", "installed frontend name@version")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected frontend command arguments")
	}
	data, err := os.ReadFile(*configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return err
	}
	workingDir, err := os.Getwd()
	if err != nil {
		return err
	}
	storage, err := config.ResolveStoragePaths(cfg.Storage, workingDir)
	if err != nil {
		return err
	}
	builtin, _ := resolveFrontendDir()
	manager, err := frontend.New(storage.DataDir, builtin, httpapi.Version)
	if err != nil {
		return err
	}
	switch action {
	case "status":
	case "install":
		var release frontend.Release
		if *archive != "" {
			if *version != "" {
				return errors.New("use either --package or --version")
			}
			file, openErr := os.Open(*archive)
			if openErr != nil {
				return openErr
			}
			release, err = manager.Install(file, frontend.InstallOptions{SHA256: *checksum})
			file.Close()
		} else if *repository == "" {
			return errors.New("--repository or --package is required")
		} else {
			release, err = frontend.NewReleaseSource().Install(context.Background(), manager, frontend.ReleaseRequest{Repository: *repository, Version: *version})
		}
		if err != nil {
			return err
		}
		if err = manager.Activate(release.ID()); err != nil {
			return err
		}
	case "activate":
		if *id == "" {
			return errors.New("--id is required")
		}
		if err = manager.Activate(*id); err != nil {
			return err
		}
	case "rollback":
		if err = manager.Rollback(); err != nil {
			return err
		}
	case "reset":
		if err = manager.Activate(""); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown frontend operation %q", action)
	}
	status, err := manager.Status()
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(status)
}
