package catalog

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/crawljob"
	"github.com/nianzhibai/91/backend/internal/uploadjob"
)

func TestCrawlerIdentityBackfillPreservesLegacyAndDeletedSources(t *testing.T) {
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	ctx := context.Background()
	for _, id := range []string{"kept", "deleted"} {
		v := &Video{ID: "scriptcrawler-demo-" + id, DriveID: "remote", FileID: id, Size: 1}
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.DeleteVideoWithTombstone(ctx, "scriptcrawler-demo-deleted"); err != nil {
		t.Fatal(err)
	}
	if err := cat.BackfillCrawlerSources(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if err := cat.BindCrawlerDiscovery(ctx, "demo", "alias", "kept"); err != nil {
		t.Fatal(err)
	}
	if err := cat.BindCrawlerDiscovery(ctx, "demo", "failed-alias", "never-imported"); err != nil {
		t.Fatal(err)
	}
	known, err := cat.KnownCrawlerCandidates(ctx, "demo", []CrawlerIdentity{{"kept-key", "kept"}, {"deleted-key", "deleted"}, {"alias", ""}, {"failed-alias", ""}})
	if err != nil || !known["kept-key"] || !known["deleted-key"] || !known["alias"] || known["failed-alias"] {
		t.Fatalf("%v %v", known, err)
	}
	other, err := cat.KnownCrawlerCandidates(ctx, "different", []CrawlerIdentity{{"alias", "kept"}})
	if err != nil || len(other) != 0 {
		t.Fatalf("scope leaked: %v %v", other, err)
	}
	if _, err := cat.GetVideo(ctx, "scriptcrawler-demo-kept"); err != nil {
		t.Fatal("legacy video lost:", err)
	}
}
func TestCrawlerImportIdentityAndAliasAreAtomic(t *testing.T) {
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	ctx := context.Background()
	// Force the last write to fail and prove no half-imported video/history remains.
	if _, err := cat.db.Exec(`CREATE TRIGGER reject_alias BEFORE INSERT ON crawler_discoveries BEGIN SELECT RAISE(ABORT,'alias failure'); END`); err != nil {
		t.Fatal(err)
	}
	v := &Video{ID: "opaque", DriveID: "demo", FileID: "opaque.mp4", Size: 1}
	if err := cat.ImportCrawlerVideo(ctx, v, "source", "discovery"); err == nil {
		t.Fatal("expected transaction failure")
	}
	if _, err := cat.GetVideo(ctx, v.ID); err == nil {
		t.Fatal("video committed without identity")
	}
	known, err := cat.KnownCrawlerCandidates(ctx, "demo", []CrawlerIdentity{{"discovery", "source"}})
	if err != nil || len(known) > 0 {
		t.Fatalf("history committed after failure: %v %v", known, err)
	}
}
func TestCrawlerTaskRecoveryIncludesUploadChildren(t *testing.T) {
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	ctx := context.Background()
	if err := cat.UpsertDrive(ctx, &Drive{ID: "demo", Kind: "scriptcrawler"}); err != nil {
		t.Fatal(err)
	}
	task := crawljob.Result{TaskID: "parent", DriveID: "demo", State: "queued", AcceptedAt: time.Now()}
	if err := cat.CreateCrawlerTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	child := uploadjob.Result{TaskID: "child", ParentTaskID: "parent", DriveID: "demo", State: "running"}
	if err := cat.SaveCrawlerUploadResult(ctx, child); err != nil {
		t.Fatal(err)
	}
	if err := cat.InterruptCrawlerTasks(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := cat.GetCrawlerTask(ctx, "demo", "parent")
	if err != nil || stored.State != "interrupted" {
		t.Fatalf("%+v %v", stored, err)
	}
	children, err := cat.CrawlerTaskUploads(ctx, "parent")
	if err != nil || len(children) != 1 || children[0].State != "interrupted" {
		t.Fatalf("%+v %v", children, err)
	}
	latest, err := cat.LatestCrawlerUploadResults(ctx)
	if err != nil || latest["demo"].State != "interrupted" {
		t.Fatalf("%+v %v", latest, err)
	}
}
