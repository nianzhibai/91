package main

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/auth"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"gopkg.in/yaml.v3"
)

func TestServerRestartClearsLoginProtection(t *testing.T) {
	if os.Getenv("VIDEO_TEST_LOGIN_RESTART") == "1" {
		os.Args = os.Args[:1]
		main()
		return
	}
	if runtime.GOOS == "windows" {
		t.Skip("graceful subprocess shutdown requires an interrupt signal")
	}
	ctx := context.Background()
	root := t.TempDir()
	fileLogging := false
	cfg := config.Config{
		Server: config.Server{Listen: "127.0.0.1:0"},
		Storage: config.Storage{
			DataDir:         filepath.Join(root, "data"),
			DBPath:          filepath.Join(root, "data", "91.db"),
			LocalPreviewDir: filepath.Join(root, "data", "previews"),
		},
		Logging: config.Logging{FileEnabled: &fileLogging},
		Nightly: config.Nightly{Disabled: true},
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Storage.DBPath), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(cfg.Storage.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	hash, err := auth.HashPassword("restart-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.CreateUser(ctx, "owner", hash, "admin"); err != nil {
		t.Fatal(err)
	}
	for cycle := 0; cycle < 2; cycle++ {
		const bannedIP = "203.0.113.40"
		const pendingIP = "203.0.113.41"
		if err := cat.BanLoginIP(ctx, bannedIP, "previous process"); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if banned, err := cat.RecordLoginAttempt(ctx, pendingIP, false, time.Now(), 30*time.Minute, 3); err != nil || banned {
				t.Fatalf("seed failures banned=%v err=%v", banned, err)
			}
		}
		stop := startLoginRestartServer(t, configPath)
		ips, err := cat.ListBannedLoginIPs(ctx)
		if err != nil || len(ips) != 0 {
			t.Fatalf("startup retained bans: %v err=%v", ips, err)
		}
		a := &auth.Authenticator{Catalog: cat}
		for _, ip := range []string{bannedIP, pendingIP} {
			request := httptest.NewRequest("POST", "/admin/api/login", nil)
			request.RemoteAddr = ip + ":12345"
			if role, err := a.UserLogin(httptest.NewRecorder(), request, "owner", "wrong"); err != nil || role != "" {
				t.Fatalf("first attempt after startup role=%q err=%v", role, err)
			}
			if role, err := a.UserLogin(httptest.NewRecorder(), request, "owner", "restart-secret"); err != nil || role != "admin" {
				t.Fatalf("login after startup role=%q err=%v", role, err)
			}
		}
		stop()
	}
}

func startLoginRestartServer(t *testing.T, configPath string) func() {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	command := exec.Command(os.Args[0], "-test.run=^TestServerRestartClearsLoginProtection$")
	command.Dir = filepath.Dir(configPath)
	command.Env = append(os.Environ(), "VIDEO_TEST_LOGIN_RESTART=1", "VIDEO_CONFIG="+configPath)
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = command.Wait()
		close(done)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = command.Process.Signal(os.Interrupt)
			select {
			case <-done:
				if waitErr != nil {
					logs, _ := os.ReadFile(logPath)
					t.Errorf("server exit: %v\n%s", waitErr, logs)
				}
			case <-time.After(15 * time.Second):
				_ = command.Process.Kill()
				<-done
				t.Error("server did not stop after interrupt")
			}
		})
	}
	t.Cleanup(stop)
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		logs, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(logs), "91 backend listening on") {
			return stop
		}
		select {
		case <-done:
			t.Fatalf("server exited before listening: %v\n%s", waitErr, logs)
		case <-deadline.C:
			t.Fatalf("server did not start\n%s", logs)
		case <-ticker.C:
		}
	}
}
