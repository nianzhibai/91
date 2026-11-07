package backup

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

func TestTelegramBackupSanitizesRuntimeStateAndPreservesFileIdentity(t *testing.T) {
	for _, includeUploads := range []bool{false, true} {
		t.Run(map[bool]string{false: "without uploads", true: "with uploads"}[includeUploads], func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "source.db")
			c, err := catalog.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			legacyDB, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = legacyDB.Exec(`INSERT INTO telegram_settings(id,config,bot_token,api_id,api_hash,version) VALUES(1,'{"enabled":true}','123:PRIVATE_TOKEN',1234,'PRIVATE_API_HASH','legacy')`)
			legacyDB.Close()
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.TelegramPaused(ctx, 123); err != nil {
				t.Fatal(err)
			}
			if err = c.AcceptTelegramUpdate(ctx, catalog.TelegramReceipt{BotID: 123, UpdateID: 1, ChatID: 42, MessageID: 1, SenderID: 42}, &catalog.TelegramSource{BotID: 123, SenderID: 42, FileID: "PRIVATE_ID", UniqueID: "stable"}, "tg-job", "video", 100); err != nil {
				t.Fatal(err)
			}
			if err = c.StageTelegramMediaGroupUpdate(ctx, catalog.TelegramMediaGroupUpdate{BotID: 123, ChatID: 42, MessageID: 2, UpdateID: 2, MediaGroupID: "album", Payload: `{"caption":"PRIVATE_ALBUM_CAPTION"}`}); err != nil {
				t.Fatal(err)
			}
			if err = c.UpsertVideo(ctx, &catalog.Video{ID: "local-upload-video", DriveID: "local-upload", FileID: "video.mp4", Title: "video"}); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err = db.Exec(`UPDATE remote_upload_jobs SET state='completed',completed_video_id='local-upload-video'; UPDATE telegram_files SET video_id='local-upload-video'`); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`INSERT INTO telegram_upload_cleanups(video_id,source_drive_id,source_file_id,size_bytes,mod_time_ns,file_identity,created_at,updated_at)
 VALUES('migrated-video','telegram-local','old.media',5,123,'PRIVATE_LOCAL_FILE_IDENTITY',1,1)`); err != nil {
				t.Fatal(err)
			}
			snapshot := filepath.Join(t.TempDir(), "snapshot.db")
			if err = c.BackupTo(ctx, snapshot); err != nil {
				t.Fatal(err)
			}
			selection := BackupSelection{UploadStorage: includeUploads, UserInfo: !includeUploads}
			if _, err = filterSnapshotDatabase(ctx, snapshot, selection); err != nil {
				t.Fatal(err)
			}
			if err = compactSnapshotDatabase(ctx, snapshot); err != nil {
				t.Fatal(err)
			}
			archive, err := sql.Open("sqlite", snapshot)
			if err != nil {
				t.Fatal(err)
			}
			defer archive.Close()
			for _, table := range []string{"telegram_settings", "telegram_connections", "telegram_receipts", "telegram_media_group_updates", "telegram_upload_cleanups"} {
				var count int
				if err = archive.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("%s not empty: %d %v", table, count, err)
				}
			}
			var files int
			if err = archive.QueryRow(`SELECT COUNT(*) FROM telegram_files`).Scan(&files); err != nil {
				t.Fatal(err)
			}
			if includeUploads && files != 1 || !includeUploads && files != 0 {
				t.Fatalf("unexpected file mappings: %d", files)
			}
			if includeUploads {
				var id, payload string
				if err = archive.QueryRow(`SELECT file_id FROM telegram_files`).Scan(&id); err != nil {
					t.Fatal(err)
				}
				if err = archive.QueryRow(`SELECT source_payload FROM remote_upload_jobs`).Scan(&payload); err != nil {
					t.Fatal(err)
				}
				if id != "" || payload != "" {
					t.Fatal("downloadable source retained in archive")
				}
			}
			raw, err := os.ReadFile(snapshot)
			if err != nil || bytes.Contains(raw, []byte("PRIVATE_TOKEN")) || bytes.Contains(raw, []byte("PRIVATE_API_HASH")) || bytes.Contains(raw, []byte("PRIVATE_ALBUM_CAPTION")) || bytes.Contains(raw, []byte("PRIVATE_LOCAL_FILE_IDENTITY")) {
				t.Fatal("credentials survived snapshot sanitization")
			}
			private, err := c.GetTelegramSettings(ctx)
			if err != nil || private.BotToken != "123:PRIVATE_TOKEN" {
				t.Fatal("backup changed live credentials")
			}
			var live int
			if err = db.QueryRow(`SELECT COUNT(*) FROM telegram_receipts`).Scan(&live); err != nil || live != 1 {
				t.Fatal("archive sanitization changed live DB", err)
			}
			if err = db.QueryRow(`SELECT COUNT(*) FROM telegram_media_group_updates`).Scan(&live); err != nil || live != 1 {
				t.Fatal("archive sanitization changed live media group", err)
			}
			if err = db.QueryRow(`SELECT COUNT(*) FROM telegram_upload_cleanups`).Scan(&live); err != nil || live != 1 {
				t.Fatal("archive sanitization changed live cleanup tasks", err)
			}
		})
	}
}

func TestTelegramSelectiveRestoreMergesFileMapAndPausesReceiver(t *testing.T) {
	ctx := context.Background()
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	targetPath := filepath.Join(t.TempDir(), "target.db")
	for _, path := range []string{sourcePath, targetPath} {
		c, err := catalog.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	source, err := sql.Open("sqlite", sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err = source.Exec(`INSERT INTO videos(id,drive_id,file_id,title,published_at,created_at,updated_at) VALUES('tg-video','local-upload','v.mp4','video',1,1,1); INSERT INTO remote_upload_jobs(id,source_kind,state,completed_video_id,created_at,updated_at) VALUES('tg-job','telegram','completed','tg-video',1,1); INSERT INTO telegram_files(bot_id,file_unique_id,file_id,job_id,video_id) VALUES(123,'stable','','tg-job','tg-video')`); err != nil {
		t.Fatal(err)
	}
	if err = mergeSelectiveRestoreDatabase(ctx, targetPath, sourcePath, BackupSelection{UploadStorage: true}); err != nil {
		t.Fatal(err)
	}
	target, err := catalog.Open(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	id, err := target.TelegramVideoID(ctx, 123, "stable")
	if err != nil || id != "tg-video" {
		t.Fatalf("mapping=%s err=%v", id, err)
	}
	paused, err := target.TelegramPaused(ctx, 123)
	if err != nil || !paused {
		t.Fatalf("receiver was not paused: %v %v", paused, err)
	}
}
