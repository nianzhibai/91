package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/video-site/backend/internal/catalog"
)

func TestTelegramLibraryBackupRestoresPortableVideosAndPreservesLiveLibrary(t *testing.T) {
	for _, selection := range []BackupSelection{FullBackupSelection(), {UploadStorage: true}} {
		name := "uploads"
		if selection.AllResources() {
			name = "full"
		}
		t.Run(name, func(t *testing.T) {
			env := newTestBackupEnv(t)
			ctx := context.Background()
			root := filepath.Join(t.TempDir(), "PRIVATE_TG_HOST_PATH", "library")
			seed := func(id, body string) {
				t.Helper()
				fileID := id + ".media"
				directory := "海边旅行-" + id
				writeTestFile(t, filepath.Join(root, directory, fileID), []byte(body))
				if _, err := env.cat.ReserveTelegramLocalFile(ctx, catalog.TelegramLocalFile{FileID: fileID, JobID: id, Directory: directory, GroupKey: id}); err != nil {
					t.Fatal(err)
				}
				if err := env.cat.UpsertVideo(ctx, &catalog.Video{ID: id, DriveID: catalog.TelegramLocalDriveID, FileID: fileID, FileName: id + ".mp4", Title: id, Size: int64(len(body)), Ext: "mp4"}); err != nil {
					t.Fatal(err)
				}
			}
			seed("source", "saved video")
			relocated := filepath.Join(t.TempDir(), "relocated")
			if err := os.Rename(filepath.Dir(root), relocated); err != nil {
				t.Fatal(err)
			}
			root = filepath.Join(relocated, "library")
			env.cfg.Telegram.LocalFilesRoot = relocated
			writeTestFile(t, filepath.Join(filepath.Dir(root), "sessions/bot.session.json"), []byte("PRIVATE_BOT_SESSION"))
			writeTestFile(t, filepath.Join(root, "unregistered.media"), []byte("unregistered file"))
			record := createAndWaitForBackup(t, env.manager, selection)
			archive, _, err := env.manager.resolveBackup(record.ID)
			if err != nil {
				t.Fatal(err)
			}
			zr, err := zip.OpenReader(archive)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, f := range zr.File {
				if f.Name == "payload/uploads/telegram-source.media" {
					found = true
				}
				if strings.Contains(f.Name, "unregistered") || strings.Contains(f.Name, "sessions/bot.session.json") {
					t.Fatal("unrelated TG files exported")
				}
				if f.Name == "payload/database.sqlite" {
					r, err := f.Open()
					if err != nil {
						t.Fatal(err)
					}
					data, err := io.ReadAll(r)
					r.Close()
					if err != nil {
						t.Fatal(err)
					}
					if bytes.Contains(data, []byte("PRIVATE_TG_HOST_PATH")) {
						t.Fatal("private storage location leaked into backup")
					}
				}
			}
			zr.Close()
			if !found {
				t.Fatal("TG video missing from backup")
			}
			if err := env.cat.DeleteVideo(ctx, "source"); err != nil {
				t.Fatal(err)
			}
			if err := env.cat.DeleteTelegramLocalFile(ctx, "source.media"); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(root, "海边旅行-source", "source.media")); err != nil {
				t.Fatal(err)
			}
			seed("target", "live video")
			if _, err := env.manager.PrepareRestore(ctx, record.ID); err != nil {
				t.Fatal(err)
			}
			env.manager.Close()
			if err := env.cat.Close(); err != nil {
				t.Fatal(err)
			}
			applied, err := ApplyPendingRestore(env.root)
			if err != nil {
				t.Fatal(err)
			}
			if applied == nil {
				t.Fatal("restore not applied")
			}
			restored, err := catalog.Open(env.cfg.Storage.DBPath)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if err := CommitAppliedRestore(applied); err != nil {
				t.Fatal(err)
			}
			v, err := restored.GetVideo(ctx, "source")
			if err != nil {
				t.Fatal(err)
			}
			if v.DriveID != "local-upload" {
				t.Fatalf("restore depends on Bot API: %+v", v)
			}
			if data, err := os.ReadFile(filepath.Join(env.root, "uploads", v.FileID)); err != nil || string(data) != "saved video" {
				t.Fatalf("restored video: %q %v", data, err)
			}
			v, err = restored.GetVideo(ctx, "target")
			if err != nil || v.DriveID != catalog.TelegramLocalDriveID {
				t.Fatalf("live TG video replaced: %+v %v", v, err)
			}
			if file, err := restored.TelegramLocalFile(ctx, "target.media"); err != nil || file.Directory != "海边旅行-target" || file.GroupKey != "target" {
				t.Fatal("live TG album location lost", err)
			}
			if data, err := os.ReadFile(filepath.Join(root, "海边旅行-target", "target.media")); err != nil || string(data) != "live video" {
				t.Fatal("live TG volume overwritten")
			}

		})
	}
}
