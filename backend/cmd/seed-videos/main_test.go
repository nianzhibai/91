package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
)

func TestSeedVideosRegistersAndPurgesOnlyItsExactRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	now := time.Now()
	for index, id := range []string{"template", "load_test-real"} {
		if err := cat.UpsertVideo(context.Background(), &catalog.Video{
			ID: id, DriveID: "drive", FileID: "file-" + id,
			FileName: id + ".mp4", Title: id, Size: int64(100 + index*100),
			PublishedAt: now, CreatedAt: now.Add(time.Duration(index) * time.Second), UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed template %s: %v", id, err)
		}
	}
	if err := cat.Close(); err != nil {
		t.Fatalf("close catalog: %v", err)
	}

	seeded, err := seedVideos(dbPath, 3, "load_test", false)
	if err != nil {
		t.Fatalf("seed videos: %v", err)
	}
	if seeded.inserted != 3 || seeded.total != 5 || seeded.listable != 5 {
		t.Fatalf("seed result = %#v", seeded)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	var pendingSeedRows int
	if err := db.QueryRow(`
SELECT COUNT(*)
  FROM videos
 WHERE id IN (SELECT video_id FROM dev_seed_video_rows WHERE namespace = 'load_test')
   AND COALESCE(fingerprint_status, 'pending') = 'pending'`).Scan(&pendingSeedRows); err != nil {
		t.Fatalf("count pending seed fingerprints: %v", err)
	}
	if pendingSeedRows != 0 {
		t.Fatalf("pending seed fingerprints = %d", pendingSeedRows)
	}

	purged, err := seedVideos(dbPath, 0, "load_test", true)
	if err != nil {
		t.Fatalf("purge videos: %v", err)
	}
	if purged.purged != 3 || purged.total != 2 || purged.listable != 2 {
		t.Fatalf("purge result = %#v", purged)
	}

	var realRows, registryRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM videos WHERE id = 'load_test-real'`).Scan(&realRows); err != nil {
		t.Fatalf("count real rows: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM dev_seed_video_rows WHERE namespace = 'load_test'`).Scan(&registryRows); err != nil {
		t.Fatalf("count registry rows: %v", err)
	}
	if realRows != 1 || registryRows != 0 {
		t.Fatalf("real rows = %d, registry rows = %d", realRows, registryRows)
	}
}

func TestSeedVideosRejectsUnsafeNamespacesBeforeOpeningTheDatabase(t *testing.T) {
	_, err := seedVideos(filepath.Join(t.TempDir(), "missing.db"), 1, "load%", false)
	if err == nil || !strings.Contains(err.Error(), "prefix must") {
		t.Fatalf("error = %v", err)
	}
}

func TestSeedVideosRollsBackWhenThereIsNoVisibleTemplate(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := cat.Close(); err != nil {
		t.Fatalf("close catalog: %v", err)
	}

	_, err = seedVideos(dbPath, 2, "load_test", false)
	if err == nil || !strings.Contains(err.Error(), "visible non-seed template") {
		t.Fatalf("error = %v", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM videos`).Scan(&rows); err != nil {
		t.Fatalf("count videos: %v", err)
	}
	if rows != 0 {
		t.Fatalf("video rows after rollback = %d", rows)
	}
}
