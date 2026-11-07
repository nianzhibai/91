package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/crawljob"
	"github.com/nianzhibai/91/backend/internal/uploadjob"
)

func TestCrawlerIdentityAndTasksRespectBackupSelection(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(map[bool]string{false: "cloud_only", true: "all"}[all], func(t *testing.T) {
			env := newTestBackupEnv(t)
			ctx := context.Background()
			if err := env.cat.UpsertDrive(ctx, &catalog.Drive{ID: "crawler", Kind: "scriptcrawler", Credentials: map[string]string{"script_path": "/unused.py"}}); err != nil {
				t.Fatal(err)
			}
			if err := env.cat.MarkCrawlerSourceSeen(ctx, "scriptcrawler", "crawler", "source", "duplicate", "remote-video", "", 1); err != nil {
				t.Fatal(err)
			}
			if err := env.cat.BindCrawlerDiscovery(ctx, "crawler", "alias", "source"); err != nil {
				t.Fatal(err)
			}
			if err := env.cat.CreateCrawlerTask(ctx, crawljob.Result{TaskID: "task", DriveID: "crawler", State: "completed", AcceptedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if err := env.cat.SaveCrawlerUploadResult(ctx, uploadjob.Result{TaskID: "upload", ParentTaskID: "task", DriveID: "crawler", State: "succeeded"}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "snapshot.db")
			if err := env.cat.BackupTo(ctx, path); err != nil {
				t.Fatal(err)
			}
			selection := BackupSelection{CloudDrives: true}
			want := 0
			if all {
				selection = FullBackupSelection()
				want = 1
			}
			if _, err := filterSnapshotDatabase(ctx, path, selection); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, table := range []string{"crawler_discoveries", "crawler_tasks", "crawler_upload_tasks", "crawler_upload_results"} {
				var count int
				if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE drive_id='crawler'`).Scan(&count); err != nil || count != want {
					t.Fatalf("%s count=%d err=%v want=%d", table, count, err, want)
				}
			}
			if err := validateArchiveDatabaseScope(ctx, path, Manifest{Selection: &selection}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
