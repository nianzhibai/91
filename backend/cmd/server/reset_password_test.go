package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/auth"
	"github.com/nianzhibai/91/backend/internal/backup"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

type resetPasswordFixture struct {
	root, configPath, dbPath string
	cat                      *catalog.Catalog
	id, otherID              int64
	originalHash             string
}

func newResetPasswordFixture(t *testing.T) resetPasswordFixture {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "91.db")
	cat, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	hash, err := auth.HashPassword("old-password")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, err := cat.CreateUser(ctx, "test_owner", hash, "admin")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := cat.CreateUser(ctx, "test_viewer", hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	for token, userID := range map[string]int64{"target-session": id, "other-session": otherID} {
		if err := cat.CreateSession(ctx, token, time.Hour, userID); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.BanLoginIP(ctx, "203.0.113.90", "preserved ban"); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.RecordLoginAttempt(ctx, "203.0.113.91", false, time.Now(), 30*time.Minute, 3); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "custom-config.yaml")
	data, err := yaml.Marshal(map[string]any{
		"server":  map[string]any{"listen": "127.0.0.1:0"},
		"storage": map[string]any{"data_dir": root},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return resetPasswordFixture{root, configPath, dbPath, cat, id, otherID, hash}
}

func TestResetPasswordExplainsLegacyDatabaseWithoutMigratingIt(t *testing.T) {
	f := newResetPasswordFixture(t)
	if err := f.cat.Close(); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(f.root, "video-site.db")
	if err := os.Rename(f.dbPath, legacy); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatal(err)
	}
	err = runResetPasswordCommand([]string{"--config", f.configPath, "--user-id", strconv.FormatInt(f.id, 10)}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "start the new server once") {
		t.Fatalf("missing migration guidance: %v", err)
	}
	after, err := os.ReadFile(legacy)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("maintenance changed legacy database: %v", err)
	}
	if _, err := os.Stat(f.dbPath); !os.IsNotExist(err) {
		t.Fatalf("maintenance created a new database: %v", err)
	}
}

func resetCommandSQL(t *testing.T, path, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

func resetPasswordFromOutput(t *testing.T, output string) string {
	t.Helper()
	_, remainder, found := strings.Cut(output, "New password: ")
	if !found {
		t.Fatal("successful reset did not display a password")
	}
	password, _, _ := strings.Cut(remainder, "\n")
	if len(password) != 12 {
		t.Fatalf("generated length=%d", len(password))
	}
	var upper, lower, digit bool
	for _, ch := range password {
		switch {
		case ch >= 'A' && ch <= 'Z':
			upper = true
		case ch >= 'a' && ch <= 'z':
			lower = true
		case ch >= '0' && ch <= '9':
			digit = true
		default:
			t.Fatal("generated password contains a forbidden character")
		}
	}
	if !upper || !lower || !digit || strings.Count(output, password) != 1 {
		t.Fatal("password classes or display count are incorrect")
	}
	return password
}

func TestResetPasswordCommandWorksOnlineAndPreservesProtection(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		t.Run(fmt.Sprint(interactive), func(t *testing.T) {
			f := newResetPasswordFixture(t)
			t.Setenv("VIDEO_CONFIG", f.configPath)
			before, err := os.ReadFile(f.configPath)
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"--user-id", strconv.FormatInt(f.id, 10)}
			if interactive {
				args = nil
			}
			var output, errorOutput bytes.Buffer
			if err := runResetPasswordCommand(args, strings.NewReader(fmt.Sprintf("%d\n", f.id)), &output, &errorOutput); err != nil {
				t.Fatal(err)
			}
			password := resetPasswordFromOutput(t, output.String())
			if errorOutput.Len() != 0 {
				t.Fatal("password reset wrote to the error/log stream")
			}
			user, err := f.cat.GetUserByID(context.Background(), f.id)
			if err != nil {
				t.Fatal(err)
			}
			if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password)); err != nil {
				t.Fatal("displayed password does not match stored hash")
			}
			if user.Username != "test_owner" || user.Role != "admin" || user.Banned {
				t.Fatal("unrelated account properties changed")
			}
			other, err := f.cat.GetUserByID(context.Background(), f.otherID)
			if err != nil || other.Password != f.originalHash {
				t.Fatal("other user's password changed")
			}
			for _, token := range []string{"target-session", "other-session"} {
				valid, _, err := f.cat.ValidateSession(context.Background(), token)
				if err != nil || valid != (token == "other-session") {
					t.Fatalf("token=%s valid=%v err=%v", token, valid, err)
				}
			}
			banned, err := f.cat.IsLoginIPBanned(context.Background(), "203.0.113.90")
			if err != nil || !banned {
				t.Fatal("reset cleared an IP ban")
			}
			for i := 0; i < 2; i++ {
				banned, err := f.cat.RecordLoginAttempt(context.Background(), "203.0.113.91", false, time.Now(), 30*time.Minute, 3)
				if err != nil || banned != (i == 1) {
					t.Fatalf("failure counters changed: banned=%v err=%v", banned, err)
				}
			}
			after, err := os.ReadFile(f.configPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("reset rewrote configuration")
			}
			if _, err := os.Stat(filepath.Join(f.root, "previews")); !os.IsNotExist(err) {
				t.Fatal("reset ran server startup")
			}
			a := &auth.Authenticator{Catalog: f.cat}
			req := httptest.NewRequest("POST", "/api/auth/login", nil)
			if role, err := a.UserLogin(httptest.NewRecorder(), req, user.Username, password); err != nil || role != "admin" {
				t.Fatalf("new-password login role=%q err=%v", role, err)
			}
			if role, err := a.UserLogin(httptest.NewRecorder(), req, user.Username, "old-password"); err != nil || role != "" {
				t.Fatalf("old-password login role=%q err=%v", role, err)
			}
		})
	}
}

func TestResetPasswordCommandDoesNotUnbanAccount(t *testing.T) {
	f := newResetPasswordFixture(t)
	if err := f.cat.SetUserBanned(context.Background(), f.id, true); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runResetPasswordCommand([]string{"--config", f.configPath, "--user-id", strconv.FormatInt(f.id, 10)}, strings.NewReader(""), &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	user, err := f.cat.GetUserByID(context.Background(), f.id)
	if err != nil || !user.Banned {
		t.Fatal("password reset unbanned account")
	}
}

func TestResetPasswordCommandRejectsInvalidInputWithoutChanges(t *testing.T) {
	f := newResetPasswordFixture(t)
	t.Setenv("VIDEO_CONFIG", f.configPath)
	for _, args := range [][]string{{"--user-id", "0"}, {"--user-id", "-1"}, {"--user-id", ""}, {"--user-id", "invalid"}, {"--user-id", "9999"}, {"--password", "custom-secret"}, {"custom-secret"}, nil} {
		var output, errorOutput bytes.Buffer
		if err := runResetPasswordCommand(args, strings.NewReader(""), &output, &errorOutput); err == nil {
			t.Fatalf("accepted arguments %v", args)
		}
		if strings.Contains(output.String(), "New password:") || strings.Contains(errorOutput.String(), "custom-secret") {
			t.Fatal("failed command displayed a password")
		}
	}
	user, err := f.cat.GetUserByID(context.Background(), f.id)
	if err != nil || user.Password != f.originalHash {
		t.Fatal("invalid input changed password")
	}
}

func TestResetPasswordCommandDoesNotCreateConfigOrDatabase(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "missing-config.yaml")
	if err := runResetPasswordCommand([]string{"--config", path}, strings.NewReader(""), io.Discard, io.Discard); err == nil {
		t.Fatal("accepted missing config")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("created missing config")
	}
	dbPath := filepath.Join(root, "91.db")
	data, err := yaml.Marshal(map[string]any{"storage": map[string]string{"data_dir": root}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runResetPasswordCommand([]string{"--config", path}, strings.NewReader(""), io.Discard, io.Discard); err == nil {
		t.Fatal("accepted missing database")
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatal("created missing database")
	}
}

func TestResetPasswordCommandHidesPasswordOnTransactionFailure(t *testing.T) {
	f := newResetPasswordFixture(t)
	resetCommandSQL(t, f.dbPath, `CREATE TRIGGER reject_delete BEFORE DELETE ON admin_sessions BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	var output bytes.Buffer
	err := runResetPasswordCommand([]string{"--config", f.configPath, "--user-id", strconv.FormatInt(f.id, 10)}, strings.NewReader(""), &output, io.Discard)
	if err == nil {
		t.Fatal("reset unexpectedly succeeded")
	}
	if output.Len() != 0 {
		t.Fatal("failed reset displayed output")
	}
	user, err := f.cat.GetUserByID(context.Background(), f.id)
	if err != nil || user.Password != f.originalHash {
		t.Fatal("failed reset changed password")
	}
}

type resetSelectionReader struct {
	beforeRead func()
	reader     io.Reader
}

func (r *resetSelectionReader) Read(p []byte) (int, error) {
	if r.beforeRead != nil {
		r.beforeRead()
		r.beforeRead = nil
	}
	return r.reader.Read(p)
}

func TestResetPasswordCommandRechecksPendingRestoreAfterSelection(t *testing.T) {
	f := newResetPasswordFixture(t)
	// Put the media/backup root away from the already open database. Both
	// password reset and its pending-restore guard must use their own roots.
	dataDir := t.TempDir()
	data, err := yaml.Marshal(map[string]any{"storage": map[string]string{"data_dir": dataDir, "db_dir": filepath.Dir(f.dbPath)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	input := &resetSelectionReader{reader: strings.NewReader(fmt.Sprintf("%d\n", f.id)), beforeRead: func() {
		marker := backup.PendingMarkerPath(dataDir)
		if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}}
	var output bytes.Buffer
	err = runResetPasswordCommand([]string{"--config", f.configPath}, input, &output, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "restore is pending") {
		t.Fatalf("error=%v", err)
	}
	if strings.Contains(output.String(), "New password:") {
		t.Fatal("pending restore displayed a password")
	}
	user, err := f.cat.GetUserByID(context.Background(), f.id)
	if err != nil || user.Password != f.originalHash {
		t.Fatal("pending restore changed password")
	}
}

func TestResetPasswordCommandRejectsReplacedDatabase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SQLite's Windows file handle prevents replacing an open database")
	}
	f := newResetPasswordFixture(t)
	if err := f.cat.Close(); err != nil {
		t.Fatal(err)
	}
	input := &resetSelectionReader{reader: strings.NewReader(fmt.Sprintf("%d\n", f.id)), beforeRead: func() {
		original, err := os.ReadFile(f.dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(f.dbPath, f.dbPath+".previous"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.dbPath, original, 0o600); err != nil {
			t.Fatal(err)
		}
	}}
	var output bytes.Buffer
	if err := runResetPasswordCommand([]string{"--config", f.configPath}, input, &output, io.Discard); err == nil {
		t.Fatal("reset accepted a replaced database")
	}
	if strings.Contains(output.String(), "New password:") {
		t.Fatal("replaced database displayed a password")
	}
	current, err := catalog.OpenExisting(context.Background(), f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	user, err := current.GetUserByID(context.Background(), f.id)
	if err != nil || user.Password != f.originalHash {
		t.Fatal("reset changed the replacement database")
	}
}

func TestResetPasswordCommandFailsWhileRestoreWriterIsHeld(t *testing.T) {
	f := newResetPasswordFixture(t)
	barrier, err := f.cat.BeginWriteBarrier(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Close()
	var output bytes.Buffer
	err = runResetPasswordCommand([]string{"--config", f.configPath, "--user-id", strconv.FormatInt(f.id, 10)}, strings.NewReader(""), &output, io.Discard)
	if err == nil || output.Len() != 0 {
		t.Fatalf("reset during restore: err=%v outputBytes=%d", err, output.Len())
	}
	user, err := f.cat.GetUserByID(context.Background(), f.id)
	if err != nil || user.Password != f.originalHash {
		t.Fatal("restore barrier did not protect the password")
	}
}

type resetOutputFailure struct{}

func (resetOutputFailure) Write([]byte) (int, error) { return 0, errors.New("output closed") }

func TestResetPasswordCommandReportsCommittedOutputFailure(t *testing.T) {
	f := newResetPasswordFixture(t)
	err := runResetPasswordCommand([]string{"--config", f.configPath, "--user-id", strconv.FormatInt(f.id, 10)}, strings.NewReader(""), resetOutputFailure{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "committed") {
		t.Fatalf("error=%v", err)
	}
	user, err := f.cat.GetUserByID(context.Background(), f.id)
	if err != nil || user.Password == f.originalHash {
		t.Fatal("output failure did not report actual committed state")
	}
}

func TestServerCommandDispatch(t *testing.T) {
	for _, args := range [][]string{{"reset-password", "--help"}, {"help"}, {"--help"}} {
		var output bytes.Buffer
		handled, err := runServerCommand(args, strings.NewReader(""), &output, &output)
		if err != nil || !handled || output.Len() == 0 {
			t.Fatalf("help handled=%v err=%v", handled, err)
		}
	}
	if handled, err := runServerCommand([]string{"unknown"}, nil, io.Discard, io.Discard); !handled || err == nil {
		t.Fatal("unknown command would start the server")
	}
	if handled, err := runServerCommand(nil, nil, io.Discard, io.Discard); handled || err != nil {
		t.Fatal("normal server startup was intercepted")
	}
}

func TestResetPasswordSubprocessDoesNotStartServer(t *testing.T) {
	if os.Getenv("VIDEO_TEST_PASSWORD_RESET") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append(os.Args[:1], os.Args[i+1:]...)
				main()
				return
			}
		}
		t.Fatal("missing command arguments")
	}
	f := newResetPasswordFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestResetPasswordSubprocessDoesNotStartServer$", "--", "reset-password", "--user-id", strconv.FormatInt(f.id, 10))
	command.Dir = f.root
	command.Env = append(os.Environ(), "VIDEO_TEST_PASSWORD_RESET=1", "VIDEO_CONFIG="+f.configPath)
	var output, errorOutput bytes.Buffer
	command.Stdout, command.Stderr = &output, &errorOutput
	if err := command.Run(); err != nil {
		t.Fatalf("command failed: %v; stderr=%s", err, errorOutput.String())
	}
	password := resetPasswordFromOutput(t, output.String())
	user, err := f.cat.GetUserByID(context.Background(), f.id)
	if err != nil {
		t.Fatal(err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password)); err != nil {
		t.Fatal("subprocess stored a different password")
	}
	if banned, err := f.cat.IsLoginIPBanned(context.Background(), "203.0.113.90"); err != nil || !banned {
		t.Fatal("subprocess cleared login protection")
	}
	if _, err := os.Stat(filepath.Join(f.root, "previews")); !os.IsNotExist(err) {
		t.Fatal("subprocess ran startup side effects")
	}
}

func TestResetPasswordDocker(t *testing.T) {
	if os.Getenv("VIDEO_TEST_DOCKER_PASSWORD_RESET") != "1" || runtime.GOOS != "linux" {
		t.Skip("set VIDEO_TEST_DOCKER_PASSWORD_RESET=1 on Linux to test with Docker")
	}
	f := newResetPasswordFixture(t)
	buildRoot := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(buildRoot, "server"), ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if result, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test server: %v\n%s", err, result)
	}
	dockerfile := "FROM scratch\nCOPY server /app/server\nWORKDIR /app\nENV VIDEO_CONFIG=/data/docker-config.yaml\nENTRYPOINT [\"/app/server\"]\n"
	if err := os.WriteFile(filepath.Join(buildRoot, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	image := fmt.Sprintf("91-password-reset-test:%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", image).Run() })
	if result, err := exec.CommandContext(ctx, "docker", "build", "-q", "-t", image, buildRoot).CombinedOutput(); err != nil {
		t.Fatalf("build Docker test image: %v\n%s", err, result)
	}
	configData, err := yaml.Marshal(map[string]any{"storage": map[string]string{"data_dir": "/data"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "docker-config.yaml"), configData, 0o600); err != nil {
		t.Fatal(err)
	}
	container := fmt.Sprintf("91-password-reset-test-%d", os.Getpid())
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", container).Run() })
	command := exec.CommandContext(ctx, "docker", "run", "--rm", "--name", container, "--network", "none", "--read-only", "--log-driver", "none", "--mount", "type=bind,source="+f.root+",target=/data", image, "reset-password", "--user-id", strconv.FormatInt(f.id, 10))
	var output, errorOutput bytes.Buffer
	command.Stdout, command.Stderr = &output, &errorOutput
	if err := command.Run(); err != nil {
		t.Fatalf("Docker reset failed: %v\n%s", err, errorOutput.String())
	}
	password := resetPasswordFromOutput(t, output.String())
	user, err := f.cat.GetUserByID(context.Background(), f.id)
	if err != nil {
		t.Fatal(err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password)); err != nil {
		t.Fatal("Docker reset did not update the mounted database")
	}
	if banned, err := f.cat.IsLoginIPBanned(context.Background(), "203.0.113.90"); err != nil || !banned {
		t.Fatal("Docker reset cleared IP protection")
	}
	if valid, _, err := f.cat.ValidateSession(context.Background(), "target-session"); err != nil || valid {
		t.Fatal("Docker reset did not revoke the old session")
	}
}
