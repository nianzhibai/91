package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/nianzhibai/91/backend/internal/backup"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"gopkg.in/yaml.v3"
)

func TestStartupFinishesLegacyRestoreBeforeRenamingDatabase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("server subprocess uses an interrupt signal")
	}
	for _, invalidRestore := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback"}[invalidRestore], func(t *testing.T) {
			root := t.TempDir()
			dataRoot := filepath.Join(root, "data")
			dbRoot := filepath.Join(root, "separate database")
			stageRoot := filepath.Join(dataRoot, ".restore-staging", "test-restore")
			for _, dir := range []string{dbRoot, stageRoot, filepath.Join(dataRoot, "backups")} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			legacy := filepath.Join(dbRoot, "video-site.db")
			staged := filepath.Join(dbRoot, ".video-site.db.restore-stage-test")
			seed := func(path, username string) {
				t.Helper()
				cat, err := catalog.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := cat.CreateUser(context.Background(), username, "preserved-hash", "admin"); err != nil {
					t.Fatal(err)
				}
				if err := cat.Close(); err != nil {
					t.Fatal(err)
				}
			}
			seed(legacy, "previous-owner")
			if invalidRestore {
				if err := os.WriteFile(staged, []byte("invalid SQLite database"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				seed(staged, "restored-owner")
			}
			marker := map[string]any{
				"markerVersion": 1, "backupId": "old-backup", "dataRoot": dataRoot, "stageRoot": stageRoot, "state": "pending",
				"operations": []map[string]any{{
					"name": "database", "kind": "file", "state": "pending", "target": legacy, "staged": staged,
					"rollback": filepath.Join(dbRoot, ".video-site.db.restore-rollback-test"),
				}},
			}
			body, err := json.Marshal(marker)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(backup.PendingMarkerPath(dataRoot), body, 0o600); err != nil {
				t.Fatal(err)
			}
			fileLogging := false
			cfg := config.Config{
				Server:  config.Server{Listen: "127.0.0.1:0"},
				Storage: config.Storage{DataDir: dataRoot, DBDir: dbRoot},
				Logging: config.Logging{FileEnabled: &fileLogging}, Nightly: config.Nightly{Disabled: true},
			}
			body, err = yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(root, "config.yaml")
			if err := os.WriteFile(configPath, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if invalidRestore {
				command := exec.Command(os.Args[0], "-test.run=^TestServerRestartClearsLoginProtection$")
				command.Dir = root
				command.Env = append(os.Environ(), "VIDEO_TEST_LOGIN_RESTART=1", "VIDEO_CONFIG="+configPath)
				if output, err := command.CombinedOutput(); err == nil {
					t.Fatalf("invalid restore started: %s", output)
				}
				if _, err := os.Stat(legacy); err != nil {
					t.Fatal("rollback did not restore the old database:", err)
				}
				if _, err := os.Stat(filepath.Join(dbRoot, "91.db")); !os.IsNotExist(err) {
					t.Fatal("invalid restore was migrated")
				}
			}
			for cycle := 0; cycle < 2; cycle++ {
				stop := startLoginRestartServer(t, configPath)
				stop()
			}
			for _, path := range []string{legacy, backup.PendingMarkerPath(dataRoot)} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("old artifact remains at %s: %v", path, err)
				}
			}
			cat, err := catalog.Open(filepath.Join(dbRoot, "91.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer cat.Close()
			username := "restored-owner"
			if invalidRestore {
				username = "previous-owner"
			}
			user, err := cat.GetUserByUsername(context.Background(), username)
			if err != nil || user.Password != "preserved-hash" {
				t.Fatalf("restored account missing: %v", err)
			}
		})
	}
}
