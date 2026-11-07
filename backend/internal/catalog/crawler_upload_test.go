package catalog

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nianzhibai/91/backend/internal/uploadjob"
)

func TestCrawlerUploadResultsSurviveRestartAndDriveDeletion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cat.Close() }()
	if err := cat.UpsertDrive(ctx, &Drive{ID: "crawler", Kind: "scriptcrawler"}); err != nil {
		t.Fatal(err)
	}
	r := uploadjob.Result{DriveID: "crawler", State: "partial", UploadedCount: 2, FailedCount: 1}
	r.AddIssue(uploadjob.Issue{VideoID: "missing", Reason: "missing_file", Message: "本地文件不存在"})
	if err := cat.SaveCrawlerUploadResult(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	cat, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	results, err := cat.LatestCrawlerUploadResults(ctx)
	if err != nil || results["crawler"].UploadedCount != 2 || len(results["crawler"].Issues) != 1 {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	if err := cat.DeleteDrive(ctx, "crawler"); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveCrawlerUploadResult(ctx, r); err != nil {
		t.Fatal(err)
	}
	results, err = cat.LatestCrawlerUploadResults(ctx)
	if err != nil || len(results) != 0 {
		t.Fatalf("results=%+v err=%v", results, err)
	}
}
