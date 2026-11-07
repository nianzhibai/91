package main

import (
	"context"
	"encoding/json"
	"image/color"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
	"gopkg.in/yaml.v3"
)

func TestManualDataMigrationKeepsMediaAndCrawlersAvailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("server subprocess uses an interrupt signal")
	}
	for _, scenario := range []struct {
		name                                string
		convertBeforeMove, separateDatabase bool
	}{
		{name: "legacy-database-moved-before-upgrade"},
		{name: "portable-database", convertBeforeMove: true},
		{name: "media-and-database-move-independently", separateDatabase: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			installDir := t.TempDir()
			dataDir := filepath.Join(installDir, "data")
			databaseDir := dataDir
			if scenario.separateDatabase {
				databaseDir = t.TempDir()
			}
			previewDir := filepath.Join(dataDir, "previews")
			for _, directory := range []string{previewDir, filepath.Join(dataDir, "uploads"), filepath.Join(dataDir, "crawler-scripts")} {
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			const videoID = "local-upload-migration"
			previewPath := mediaasset.PreviewPath(previewDir, videoID)
			for path, contents := range map[string]string{
				previewPath: "migration-preview",
				filepath.Join(dataDir, "uploads", "video.mp4"):       "migration-video",
				filepath.Join(dataDir, "crawler-scripts", "demo.py"): "CRAWLER_NAME = 'Migrated crawler'\n",
			} {
				if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			writeSolidJPEG(t, mediaasset.ThumbnailPath(previewDir, videoID), color.RGBA{R: 180, G: 80, B: 40, A: 255})
			cat, err := catalog.Open(filepath.Join(databaseDir, "video-site.db"))
			if err != nil {
				t.Fatal(err)
			}
			userID, err := cat.CreateUser(ctx, "migration-admin", "unused-hash", "admin")
			if err != nil {
				t.Fatal(err)
			}
			const token = "migration-session"
			if err := cat.CreateSession(ctx, token, time.Hour, userID); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if err := cat.UpsertVideo(ctx, &catalog.Video{
				ID: videoID, DriveID: "local-upload", FileID: "video.mp4", Title: "Migrated video",
				PreviewLocal: previewPath, PreviewStatus: "ready", PreviewUpdatedAt: now,
				ThumbnailURL: "/p/thumb/" + videoID, DurationSeconds: 30,
				FingerprintStatus: "ready", SampledSHA256: "already-generated", Views: 17, Likes: 5,
				PublishedAt: now, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			if err := cat.UpsertDrive(ctx, &catalog.Drive{
				ID: "crawler", Kind: "scriptcrawler", Name: "Migrated crawler", RootID: "/",
				Credentials: map[string]string{"script_path": filepath.Join(dataDir, "crawler-scripts", "demo.py")},
			}); err != nil {
				t.Fatal(err)
			}
			if err := cat.Close(); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			_ = listener.Close()
			cfg := config.Config{Server: config.Server{Listen: address}, Nightly: config.Nightly{Disabled: true}, Preview: config.Preview{Enabled: true}}
			configPath := filepath.Join(installDir, "config.yaml")
			writeConfig := func(root string, legacy bool) {
				t.Helper()
				cfg.Storage = config.Storage{DataDir: root}
				if scenario.separateDatabase {
					cfg.Storage.DBDir = databaseDir
				}
				document := map[string]any{
					"server": cfg.Server, "storage": cfg.Storage, "logging": cfg.Logging,
					"nightly": cfg.Nightly, "preview": cfg.Preview,
				}
				if legacy {
					document["storage"] = map[string]string{"db_path": filepath.Join(root, "video-site.db"), "local_preview_dir": filepath.Join(root, "previews")}
					document["logging"] = map[string]string{"directory": filepath.Join(root, "logs")}
				}
				encoded, err := yaml.Marshal(document)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(configPath, encoded, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			assertDataLayout := func() {
				t.Helper()
				encoded, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				var document map[string]map[string]any
				if err := yaml.Unmarshal(encoded, &document); err != nil {
					t.Fatal(err)
				}
				if len(document["storage"]) != 2 || document["storage"]["data_dir"] != dataDir || document["storage"]["db_dir"] != cfg.Storage.DBDir || document["logging"]["directory"] != nil {
					t.Fatalf("server changed directory configuration: storage=%v logging=%v", document["storage"], document["logging"])
				}
				for _, name := range []string{"previews", "uploads", "crawler-scripts", "logs", "backups", ".backup-snapshots", ".restore-staging"} {
					if info, err := os.Stat(filepath.Join(dataDir, name)); err != nil || !info.IsDir() {
						t.Fatalf("missing directory under data_dir: %s, err=%v", name, err)
					}
					if scenario.separateDatabase {
						if _, err := os.Stat(filepath.Join(databaseDir, name)); !os.IsNotExist(err) {
							t.Fatalf("non-database directory followed db_dir: %s, err=%v", name, err)
						}
					}
				}
				if scenario.separateDatabase {
					if _, err := os.Stat(filepath.Join(dataDir, "91.db")); !os.IsNotExist(err) {
						t.Fatalf("database created in data_dir despite db_dir override: %v", err)
					}
				}
			}
			if scenario.convertBeforeMove {
				writeConfig(dataDir, true)
				stop := startLoginRestartServer(t, configPath)
				stop()
				assertDataLayout()
				if _, err := os.Stat(filepath.Join(databaseDir, "video-site.db")); !os.IsNotExist(err) {
					t.Fatalf("legacy database name remains after startup: %v", err)
				}
			}
			client := &http.Client{Timeout: 5 * time.Second}
			for cycle := 0; cycle < 2; cycle++ {
				// All file movement is performed by the operator (the test), with
				// the service stopped. There is no migration command or symlink.
				destination := filepath.Join(t.TempDir(), "moved data")
				oldDir := dataDir
				if scenario.separateDatabase && cycle == 1 {
					oldDir = databaseDir
					databaseDir = destination
				} else {
					dataDir = destination
					if !scenario.separateDatabase {
						databaseDir = destination
					}
				}
				if err := os.Rename(oldDir, destination); err != nil {
					t.Fatal(err)
				}
				writeConfig(dataDir, false)
				stop := startLoginRestartServer(t, configPath)
				get := func(path string) []byte {
					t.Helper()
					request, err := http.NewRequest(http.MethodGet, "http://"+address+path, nil)
					if err != nil {
						t.Fatal(err)
					}
					request.AddCookie(&http.Cookie{Name: "vs_admin", Value: token})
					response, err := client.Do(request)
					if err != nil {
						t.Fatal(err)
					}
					defer response.Body.Close()
					body, err := io.ReadAll(response.Body)
					if err != nil || response.StatusCode != http.StatusOK {
						t.Fatalf("GET %s: status=%d body=%s err=%v", path, response.StatusCode, body, err)
					}
					return body
				}
				if body := get("/p/preview/" + videoID); string(body) != "migration-preview" {
					t.Fatalf("preview = %q", body)
				}
				if body := get("/p/upload/" + videoID); string(body) != "migration-video" {
					t.Fatalf("upload = %q", body)
				}
				get("/p/thumb/" + videoID)
				var crawlers []struct{ Name, ScriptPath string }
				if err := json.Unmarshal(get("/admin/api/crawlers"), &crawlers); err != nil {
					t.Fatal(err)
				}
				if len(crawlers) != 1 || crawlers[0].Name != "Migrated crawler" || crawlers[0].ScriptPath != filepath.Join(dataDir, "crawler-scripts", "demo.py") {
					t.Fatalf("crawlers after relocation = %+v", crawlers)
				}
				stop()
				assertDataLayout()
				if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
					t.Fatalf("server recreated the old data directory: %v", err)
				}
				cat, err := catalog.Open(filepath.Join(databaseDir, "91.db"))
				if err != nil {
					t.Fatal(err)
				}
				video, err := cat.GetVideo(ctx, videoID)
				if err != nil || video.PreviewLocal != mediaasset.PreviewFilename(videoID) || video.PreviewStatus != "ready" || video.Views != 17 || video.Likes != 5 {
					t.Fatalf("video after restart = %+v, err=%v", video, err)
				}
				if err := cat.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
