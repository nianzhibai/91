package scanner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
)

func TestRunIgnoresRemoteThumbnailFromDriveEntry(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	drv := &scannerFakeDrive{
		entries: []drives.Entry{{
			ID:           "file-1",
			Name:         "clip.mp4",
			Size:         123,
			MimeType:     "video/mp4",
			ModTime:      time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC),
			ThumbnailURL: "https://thumbnail.example/clip.jpg",
		}},
	}
	sc := New(cat, drv, []string{".mp4"}, nil, nil)

	stats, err := sc.Run(ctx, "")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if stats.Added != 1 {
		t.Fatalf("added = %d, want 1", stats.Added)
	}

	got, err := cat.GetVideo(ctx, "fake-drive-file-1")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.ThumbnailURL != "" {
		t.Fatalf("thumbnail = %q, want empty so local thumbnail worker regenerates it", got.ThumbnailURL)
	}
}

func TestRunIgnoresZeroSizeVideoFiles(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	drv := &scannerFakeDrive{
		entries: []drives.Entry{{
			ID:       "empty-file",
			Name:     "empty.mp4",
			Size:     0,
			MimeType: "video/mp4",
			ModTime:  time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC),
		}},
	}
	sc := New(cat, drv, []string{".mp4"}, nil, nil)

	stats, err := sc.Run(ctx, "")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if stats.Added != 0 {
		t.Fatalf("added = %d, want 0", stats.Added)
	}
	if _, err := cat.GetVideo(ctx, "fake-drive-empty-file"); err != sql.ErrNoRows {
		t.Fatalf("get zero-size video error = %v, want sql.ErrNoRows", err)
	}
}

func TestRunScannedCountsOnlyVideoCandidates(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	drv := &scannerFakeDrive{
		entries: []drives.Entry{
			{ID: "file-1", Name: "clip.mp4", Size: 123},
			{ID: "file-2", Name: "notes.txt", Size: 123},
			{ID: "file-3", Name: "empty.mp4", Size: 0},
		},
	}
	sc := New(cat, drv, []string{".mp4"}, nil, nil)

	stats, err := sc.Run(ctx, "")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if stats.Scanned != 1 {
		t.Fatalf("scanned = %d, want one non-empty video candidate", stats.Scanned)
	}
	if stats.Added != 1 {
		t.Fatalf("added = %d, want one added video", stats.Added)
	}
}

func TestRunUsesPathSafeVideoIDForUnsafeFileID(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	drv := &scannerFakeDrive{
		entries: []drives.Entry{{
			ID:   "fid/with space",
			Name: "clip.mp4",
			Size: 123,
		}},
	}
	sc := New(cat, drv, []string{".mp4"}, nil, nil)

	stats, err := sc.Run(ctx, "")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if stats.Added != 1 {
		t.Fatalf("added = %d, want 1", stats.Added)
	}
	if _, ok := stats.SeenFileIDs["fid/with space"]; !ok {
		t.Fatalf("seen file ids = %#v, want original file id", stats.SeenFileIDs)
	}

	wantID := "fake-drive-b64_ZmlkL3dpdGggc3BhY2U"
	got, err := cat.GetVideo(ctx, wantID)
	if err != nil {
		t.Fatalf("get video %s: %v", wantID, err)
	}
	if strings.Contains(got.ID, "/") {
		t.Fatalf("video id = %q, must not contain slash", got.ID)
	}
	if got.FileID != "fid/with space" {
		t.Fatalf("file id = %q, want original", got.FileID)
	}
}

func TestRunStopsWhenContextCanceledDuringFileLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	drv := &scannerFakeDrive{
		entries: []drives.Entry{
			{ID: "file-1", Name: "one.mp4", Size: 123},
			{ID: "file-2", Name: "two.mp4", Size: 123},
			{ID: "file-3", Name: "three.mp4", Size: 123},
		},
	}
	callbacks := 0
	sc := New(cat, drv, []string{".mp4"}, nil, func(*catalog.Video) {
		callbacks++
		cancel()
	})

	stats, err := sc.Run(ctx, "")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scan error = %v, want context.Canceled", err)
	}
	if stats.Added != 1 || callbacks != 1 {
		t.Fatalf("added=%d callbacks=%d, want exactly one video before cancellation", stats.Added, callbacks)
	}
	if _, err := cat.GetVideo(context.Background(), "fake-drive-file-1"); err != nil {
		t.Fatalf("first video should be persisted before cancellation: %v", err)
	}
	if _, err := cat.GetVideo(context.Background(), "fake-drive-file-2"); err != sql.ErrNoRows {
		t.Fatalf("second video lookup error = %v, want sql.ErrNoRows", err)
	}
	if _, err := cat.GetVideo(context.Background(), "fake-drive-file-3"); err != sql.ErrNoRows {
		t.Fatalf("third video lookup error = %v, want sql.ErrNoRows", err)
	}
}

func TestRunSkipsAdminDeletedVideo(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "fake-drive-file-1",
		DriveID:     "drive",
		FileID:      "file-1",
		FileName:    "clip.mp4",
		ContentHash: "HASH-1",
		Title:       "Deleted Clip",
		Size:        123,
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := cat.DeleteVideoWithTombstone(ctx, "fake-drive-file-1"); err != nil {
		t.Fatalf("delete with tombstone: %v", err)
	}

	drv := &scannerFakeDrive{
		entries: []drives.Entry{{
			ID:       "file-1",
			Name:     "clip.mp4",
			Size:     123,
			Hash:     "hash-1",
			MimeType: "video/mp4",
			ModTime:  now,
		}},
	}
	sc := New(cat, drv, []string{".mp4"}, nil, nil)

	stats, err := sc.Run(ctx, "")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if stats.Added != 0 {
		t.Fatalf("added = %d, want 0", stats.Added)
	}
	if _, err := cat.GetVideo(ctx, "fake-drive-file-1"); err != sql.ErrNoRows {
		t.Fatalf("deleted video was recreated, get error = %v", err)
	}
}

func TestRunDoesNotBackfillRemoteThumbnailForExistingVideo(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:            "fake-drive-file-1",
		DriveID:       "drive",
		FileID:        "file-1",
		Title:         "Clip",
		PreviewStatus: "pending",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	drv := &scannerFakeDrive{
		entries: []drives.Entry{{
			ID:           "file-1",
			Name:         "clip.mp4",
			Size:         123,
			MimeType:     "video/mp4",
			ModTime:      now,
			ThumbnailURL: "https://thumbnail.example/backfilled.jpg",
		}},
	}
	sc := New(cat, drv, []string{".mp4"}, nil, nil)

	stats, err := sc.Run(ctx, "")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if stats.Added != 0 {
		t.Fatalf("added = %d, want 0", stats.Added)
	}

	got, err := cat.GetVideo(ctx, "fake-drive-file-1")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.ThumbnailURL != "" {
		t.Fatalf("thumbnail = %q, want empty so local thumbnail worker regenerates it", got.ThumbnailURL)
	}
}

func TestRunSyncsRenamedExistingVideoMetadata(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:            "fake-drive-file-1",
		DriveID:       "drive",
		FileID:        "file-1",
		FileName:      "old-name - Old Author.mp4",
		Title:         "old-name",
		Author:        "Old Author",
		PreviewStatus: "pending",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	drv := &scannerFakeDrive{
		entries: []drives.Entry{{
			ID:      "file-1",
			Name:    "[4K] renamed clip.mp4",
			Size:    123,
			ModTime: now,
		}},
	}
	sc := New(cat, drv, []string{".mp4"}, nil, nil)

	stats, err := sc.Run(ctx, "")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if stats.Added != 0 {
		t.Fatalf("added = %d, want existing video to be updated in place", stats.Added)
	}

	got, err := cat.GetVideo(ctx, "fake-drive-file-1")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.FileName != "[4K] renamed clip.mp4" {
		t.Fatalf("file_name = %q, want remote name", got.FileName)
	}
	if got.Title != "[4K] renamed clip" {
		t.Fatalf("title = %q, want full remote filename without extension", got.Title)
	}
	if got.Author != "" {
		t.Fatalf("author = %q, want cleared author from remote name without author suffix", got.Author)
	}
}

func TestRunUsesFullFilenameForTitleButKeepsAuthorParser(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()

	drv := &scannerFakeDrive{entries: []drives.Entry{
		{ID: "hnd", Name: "HND-970.mp4", Size: 123},
		{ID: "weather", Name: "天气晴朗-小白.mp4", Size: 124},
	}}
	if _, err := New(cat, drv, []string{".mp4"}, nil, nil).Run(ctx, ""); err != nil {
		t.Fatalf("scan: %v", err)
	}

	hnd, err := cat.GetVideo(ctx, "fake-drive-hnd")
	if err != nil {
		t.Fatalf("get HND video: %v", err)
	}
	if hnd.Title != "HND-970" || hnd.Author != "970" {
		t.Fatalf("HND metadata = title %q author %q", hnd.Title, hnd.Author)
	}
	weather, err := cat.GetVideo(ctx, "fake-drive-weather")
	if err != nil {
		t.Fatalf("get weather video: %v", err)
	}
	if weather.Title != "天气晴朗-小白" || weather.Author != "小白" {
		t.Fatalf("weather metadata = title %q author %q", weather.Title, weather.Author)
	}
}

func TestRunUpdatesMigratedCrawlerByDriveFileID(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID: "scriptcrawler-crawler-memojav-DASS-984", DriveID: "drive", FileID: "remote-file",
		FileName: "Long title-DASS-984.mp4", Title: "Untruncated source title", Author: "Haduki Honami",
		Size: 123, PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed crawler video: %v", err)
	}
	drv := &scannerFakeDrive{entries: []drives.Entry{{ID: "remote-file", Name: "Long title-DASS-984.mp4", Size: 123}}}
	if _, err := New(cat, drv, []string{".mp4"}, nil, nil).Run(ctx, ""); err != nil {
		t.Fatalf("scan: %v", err)
	}
	got, err := cat.GetVideo(ctx, "scriptcrawler-crawler-memojav-DASS-984")
	if err != nil {
		t.Fatalf("get crawler video: %v", err)
	}
	if got.Title != "Long title-DASS-984" {
		t.Fatalf("title = %q", got.Title)
	}
	if got.Author != "Haduki Honami" {
		t.Fatalf("author = %q, want preserved crawler author", got.Author)
	}
	if _, err := cat.GetVideo(ctx, "fake-drive-remote-file"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("generated duplicate exists: %v", err)
	}
}

func TestRunPreservesExistingManualTagsInsteadOfFilenameTags(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:            "fake-drive-file-1",
		DriveID:       "drive",
		FileID:        "file-1",
		Title:         "Old",
		Tags:          []string{"sunny", "kenny"},
		PreviewStatus: "pending",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	drv := &scannerFakeDrive{
		entries: []drives.Entry{{
			ID:      "file-1",
			Name:    "女大后入.mp4",
			Size:    123,
			ModTime: now,
		}},
	}
	sc := New(cat, drv, []string{".mp4"}, nil, nil)

	if _, err := sc.Run(ctx, ""); err != nil {
		t.Fatalf("scan: %v", err)
	}

	got, err := cat.GetVideo(ctx, "fake-drive-file-1")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	want := []string{"sunny", "kenny"}
	if !sameStrings(got.Tags, want) {
		t.Fatalf("tags = %#v, want %#v", got.Tags, want)
	}
}

func TestRunDoesNotCreateTagFromDirectoryName(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	now := time.Now()
	for _, id := range []string{"existing-1", "existing-2"} {
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID:          id,
			DriveID:     "drive",
			FileID:      id,
			Title:       "Existing",
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("seed existing sunny video: %v", err)
		}
	}

	drv := &scannerTreeFakeDrive{
		entries: map[string][]drives.Entry{
			"root": {{
				ID:    "dir-1",
				Name:  "sunny",
				IsDir: true,
			}},
			"dir-1": {{
				ID:       "file-1",
				ParentID: "dir-1",
				Name:     "clip.mp4",
				Size:     123,
				ModTime:  now,
			}},
		},
	}
	sc := New(cat, drv, []string{".mp4"}, nil, nil)

	if _, err := sc.Run(ctx, ""); err != nil {
		t.Fatalf("scan: %v", err)
	}

	got, err := cat.GetVideo(ctx, "fake-drive-file-1")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if len(got.Tags) != 0 {
		t.Fatalf("tags = %#v, want none", got.Tags)
	}
}

func TestRunMapsAVCodeDirectoryToExistingAVTag(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	now := time.Now()
	for _, id := range []string{"existing-1", "existing-2"} {
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID:          id,
			DriveID:     "drive",
			FileID:      id,
			Title:       "Existing",
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("seed existing AV code video: %v", err)
		}
	}

	drv := &scannerTreeFakeDrive{
		entries: map[string][]drives.Entry{
			"root": {{
				ID:    "dir-1",
				Name:  "SSNI-001",
				IsDir: true,
			}},
			"dir-1": {{
				ID:       "file-1",
				ParentID: "dir-1",
				Name:     "clip.mp4",
				Size:     123,
				ModTime:  now,
			}},
		},
	}
	sc := New(cat, drv, []string{".mp4"}, nil, nil)

	if _, err := sc.Run(ctx, ""); err != nil {
		t.Fatalf("scan: %v", err)
	}

	got, err := cat.GetVideo(ctx, "fake-drive-file-1")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if !sameStrings(got.Tags, []string{"AV", "SSNI"}) {
		t.Fatalf("tags = %#v, want AV + SSNI", got.Tags)
	}
}

func TestRunSkipsDuplicateFileHashes(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	drv := &scannerFakeDrive{
		entries: []drives.Entry{
			{
				ID:      "file-1",
				Name:    "first.mp4",
				Size:    123,
				Hash:    "hash-same",
				ModTime: now,
			},
			{
				ID:      "file-2",
				Name:    "second.mp4",
				Size:    123,
				Hash:    "hash-same",
				ModTime: now,
			},
		},
	}
	addedIDs := []string{}
	sc := New(cat, drv, []string{".mp4"}, nil, func(v *catalog.Video) {
		addedIDs = append(addedIDs, v.ID)
	})

	stats, err := sc.Run(ctx, "")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if stats.Added != 1 {
		t.Fatalf("added = %d, want 1", stats.Added)
	}
	if len(addedIDs) != 1 || addedIDs[0] != "fake-drive-file-1" {
		t.Fatalf("on new ids = %#v, want first file only", addedIDs)
	}

	items, total, err := cat.ListVideos(ctx, catalog.ListParams{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("list videos: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("visible videos total=%d len=%d, want 1", total, len(items))
	}
	if items[0].FileID != "file-1" {
		t.Fatalf("visible file id = %q, want file-1", items[0].FileID)
	}
}

func TestRunSkipsDuplicateFileNamesWithSameSizeWhenHashesMissing(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	drv := &scannerFakeDrive{
		entries: []drives.Entry{
			{
				ID:      "file-1",
				Name:    "same-name.mp4",
				Size:    123,
				ModTime: now,
			},
			{
				ID:      "file-2",
				Name:    "same-name.mp4",
				Size:    123,
				ModTime: now,
			},
			{
				ID:      "file-3",
				Name:    "same-name.mp4",
				Size:    456,
				ModTime: now,
			},
		},
	}
	addedIDs := []string{}
	sc := New(cat, drv, []string{".mp4"}, nil, func(v *catalog.Video) {
		addedIDs = append(addedIDs, v.ID)
	})

	stats, err := sc.Run(ctx, "")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if stats.Added != 2 {
		t.Fatalf("added = %d, want 2", stats.Added)
	}
	wantAdded := []string{"fake-drive-file-1", "fake-drive-file-3"}
	if !sameStrings(addedIDs, wantAdded) {
		t.Fatalf("on new ids = %#v, want %#v", addedIDs, wantAdded)
	}
	if _, err := cat.GetVideo(ctx, "fake-drive-file-2"); err != sql.ErrNoRows {
		t.Fatalf("duplicate video lookup error = %v, want sql.ErrNoRows", err)
	}
}

func TestRunReportsSeenVideoFileIDsAndVisitedDirectories(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	drv := &scannerTreeFakeDrive{
		entries: map[string][]drives.Entry{
			"root": {
				{ID: "dir-1", Name: "Folder", IsDir: true},
				{ID: "root-file", Name: "root.mp4", Size: 123},
				{ID: "note", Name: "note.txt", Size: 123},
			},
			"dir-1": {
				{ID: "nested-file", ParentID: "dir-1", Name: "nested.mp4", Size: 456},
				{ID: "empty-video", ParentID: "dir-1", Name: "empty.mp4", Size: 0},
			},
		},
	}
	sc := New(cat, drv, []string{".mp4"}, nil, nil)

	stats, err := sc.Run(ctx, "")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if _, ok := stats.SeenFileIDs["root-file"]; !ok {
		t.Fatalf("seen file ids = %#v, want root-file", stats.SeenFileIDs)
	}
	if _, ok := stats.SeenFileIDs["nested-file"]; !ok {
		t.Fatalf("seen file ids = %#v, want live non-empty videos", stats.SeenFileIDs)
	}
	if _, ok := stats.SeenFileIDs["note"]; ok {
		t.Fatalf("seen file ids = %#v, want non-video entries excluded", stats.SeenFileIDs)
	}
	if _, ok := stats.SeenFileIDs["empty-video"]; ok {
		t.Fatalf("seen file ids = %#v, want zero-size entries excluded", stats.SeenFileIDs)
	}
	if _, ok := stats.VisitedDirIDs["root"]; !ok {
		t.Fatalf("visited dir ids = %#v, want root", stats.VisitedDirIDs)
	}
	if _, ok := stats.VisitedDirIDs["dir-1"]; !ok {
		t.Fatalf("visited dir ids = %#v, want nested dir", stats.VisitedDirIDs)
	}
	if stats.Errors != 0 {
		t.Fatalf("errors = %d, want 0", stats.Errors)
	}
}

func TestRunSkipsConfiguredDirIDsAndDoesNotRecurse(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	drv := &scannerTreeFakeDrive{
		kind: "p115",
		id:   "115",
		entries: map[string][]drives.Entry{
			"root": {
				{ID: "skip-dir", Name: "Movies", IsDir: true},
				{ID: "normal-file", Name: "normal.mp4", Size: 123},
			},
			"skip-dir": {
				{ID: "skipped-file", ParentID: "skip-dir", Name: "skipped.mp4", Size: 456},
				{ID: "nested-dir", Name: "Nested", IsDir: true},
			},
			"nested-dir": {
				{ID: "nested-skipped-file", ParentID: "nested-dir", Name: "nested.mp4", Size: 789},
			},
		},
	}
	// 把 skip-dir 加入 SkipDirIDs：scanner 应该完全不进该目录，
	// 也不会递归到其下的 nested-dir。
	sc := New(cat, drv, []string{".mp4"}, []string{"skip-dir"}, nil)

	stats, err := sc.Run(ctx, "")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if stats.Scanned != 1 {
		t.Fatalf("scanned = %d, want only non-skipped file counted", stats.Scanned)
	}
	if stats.Added != 1 {
		t.Fatalf("added = %d, want only non-skipped file added", stats.Added)
	}
	// skip-dir 自身和它下面的目录 / 文件都不应被访问。
	if _, ok := stats.VisitedDirIDs["skip-dir"]; ok {
		t.Fatalf("visited skipped dir, want no recursion into skip-dir")
	}
	if _, ok := stats.VisitedDirIDs["nested-dir"]; ok {
		t.Fatalf("visited nested dir under skipped, want no descent")
	}
	if _, ok := stats.SeenFileIDs["skipped-file"]; ok {
		t.Fatalf("seen skipped file, want skipped")
	}
	if _, err := cat.GetVideo(ctx, "p115-115-skipped-file"); err != sql.ErrNoRows {
		t.Fatalf("skipped direct file get error = %v, want sql.ErrNoRows", err)
	}
	if _, err := cat.GetVideo(ctx, "p115-115-nested-skipped-file"); err != sql.ErrNoRows {
		t.Fatalf("nested skipped file get error = %v, want sql.ErrNoRows", err)
	}
	if _, err := cat.GetVideo(ctx, "p115-115-normal-file"); err != nil {
		t.Fatalf("normal video was not added: %v", err)
	}
}

func TestDiscoverDoesNotWriteCatalogBeforeReconcile(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	drv := &discoveryTestSource{entries: map[string][]drives.Entry{
		"root": {{ID: "file-1", Name: "clip.mp4", Size: 123}},
	}}
	scan := New(cat, drv, []string{".mp4"}, nil, nil)
	snapshot, stats, err := scan.Discover(ctx, "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if !snapshot.FullDriveScan || !snapshot.Complete() {
		t.Fatalf("snapshot = full:%v complete:%v, want both true", snapshot.FullDriveScan, snapshot.Complete())
	}
	if stats.Scanned != 1 || len(snapshot.Files) != 1 {
		t.Fatalf("discovery candidates = stats:%d files:%d, want 1", stats.Scanned, len(snapshot.Files))
	}
	if _, err := cat.GetVideo(ctx, "fake-drive-file-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("discover wrote catalog, lookup error = %v", err)
	}

	result, err := scan.Reconcile(ctx, snapshot)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Stats.Added != 1 || len(result.NewVideos) != 1 {
		t.Fatalf("reconcile result = added:%d new:%d, want 1", result.Stats.Added, len(result.NewVideos))
	}
	if _, err := cat.GetVideo(ctx, "fake-drive-file-1"); err != nil {
		t.Fatalf("reconciled video missing: %v", err)
	}
}

func TestScanNestedRateLimitDoesNotPartiallyReconcile(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	drv := &discoveryTestSource{
		entries: map[string][]drives.Entry{
			"root": {
				{ID: "file-before-error", Name: "clip.mp4", Size: 123},
				{ID: "limited-dir", Name: "Limited", IsDir: true},
			},
		},
		errors: map[string]error{
			"limited-dir": &drives.RateLimitError{Provider: "fake", RetryAfter: time.Second},
		},
	}
	result, err := New(cat, drv, []string{".mp4"}, nil, nil).Scan(ctx, "")
	if _, ok := drives.RateLimitRetryAfter(err); !ok {
		t.Fatalf("scan error = %v, want rate limit", err)
	}
	if result.Stats.Scanned != 1 || result.Stats.Added != 0 {
		t.Fatalf("partial result = scanned:%d added:%d, want 1/0", result.Stats.Scanned, result.Stats.Added)
	}
	if _, err := cat.GetVideo(ctx, "fake-drive-file-before-error"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("fatal discovery partially reconciled catalog, lookup error = %v", err)
	}
}

func TestScanReportsRecoverableDirectoryIssueSeparately(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	drv := &discoveryTestSource{
		entries: map[string][]drives.Entry{
			"root": {
				{ID: "broken-dir", Name: "Broken", IsDir: true},
				{ID: "live-file", Name: "live.mp4", Size: 123},
			},
		},
		errors: map[string]error{"broken-dir": errors.New("temporary list failure")},
	}
	result, err := New(cat, drv, []string{".mp4"}, nil, nil).Scan(ctx, "")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if result.Snapshot.Complete() {
		t.Fatal("snapshot with a failed directory reported complete")
	}
	if len(result.Snapshot.Issues) != 1 || result.Snapshot.Issues[0].Stage != IssueDiscovery {
		t.Fatalf("discovery issues = %#v, want one discovery issue", result.Snapshot.Issues)
	}
	if result.Stats.Added != 1 || result.Stats.Errors != 1 {
		t.Fatalf("result = added:%d errors:%d, want 1/1", result.Stats.Added, result.Stats.Errors)
	}
}

// TestRunDoesNotEnforceLegacyMaxDepth 校验扫描会一直递归直到没有子目录，
// 不再受旧的 max_depth 上限限制。构造 7 层嵌套（旧 default=5 时第 6+ 层会被截断），
// 确保最深层的视频也能被入库。
func TestRunDoesNotEnforceLegacyMaxDepth(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	// scannerTreeFakeDrive.RootID() == "root"。
	// 链接 root → d1 → d2 → ... → d7，叶子放一个视频。
	const depth = 7
	entries := map[string][]drives.Entry{}
	dirs := []string{"root"}
	for i := 1; i <= depth; i++ {
		dirs = append(dirs, fmt.Sprintf("d%d", i))
	}
	for i := 0; i < depth; i++ {
		entries[dirs[i]] = []drives.Entry{
			{ID: dirs[i+1], Name: fmt.Sprintf("L%d", i+1), IsDir: true},
		}
	}
	leaf := dirs[depth]
	entries[leaf] = []drives.Entry{
		{ID: "deep-file", ParentID: leaf, Name: "deep.mp4", Size: 10},
	}
	drv := &scannerTreeFakeDrive{entries: entries}

	sc := New(cat, drv, []string{".mp4"}, nil, nil)
	stats, err := sc.Run(ctx, "")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if stats.Added != 1 {
		t.Fatalf("added = %d, want 1 (deepest-leaf video reached)", stats.Added)
	}
	if _, err := cat.GetVideo(ctx, "fake-drive-deep-file"); err != nil {
		t.Fatalf("deepest video not added (legacy max_depth still enforced?): %v", err)
	}
}

func TestRunSynchronizesExistingVideoDirectoryIdentity(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "fake-drive-file-1",
		DriveID:     "drive",
		FileID:      "file-1",
		FileName:    "episode.mp4",
		ParentID:    "old-folder",
		DirName:     "Old Series",
		Title:       "episode",
		Size:        123,
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	drv := &scannerTreeFakeDrive{entries: map[string][]drives.Entry{
		"root": {{ID: "new-folder", Name: "New Series", IsDir: true}},
		"new-folder": {{
			ID:       "file-1",
			ParentID: "incorrect-provider-parent",
			Name:     "episode.mp4",
			Size:     123,
		}},
	}}
	if _, err := New(cat, drv, []string{".mp4"}, nil, nil).Run(ctx, ""); err != nil {
		t.Fatalf("scan: %v", err)
	}

	got, err := cat.GetVideo(ctx, "fake-drive-file-1")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.ParentID != "new-folder" || got.DirName != "New Series" {
		t.Fatalf("directory = parent %q name %q, want new-folder / New Series", got.ParentID, got.DirName)
	}
}

type scannerFakeDrive struct {
	entries []drives.Entry
}

func (d *scannerFakeDrive) Kind() string { return "fake" }
func (d *scannerFakeDrive) ID() string   { return "drive" }
func (d *scannerFakeDrive) Init(context.Context) error {
	return nil
}
func (d *scannerFakeDrive) List(context.Context, string) ([]drives.Entry, error) {
	return d.entries, nil
}
func (d *scannerFakeDrive) Stat(context.Context, string) (*drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *scannerFakeDrive) StreamURL(context.Context, string) (*drives.StreamLink, error) {
	return &drives.StreamLink{URL: "https://video.example/clip.mp4"}, nil
}
func (d *scannerFakeDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *scannerFakeDrive) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *scannerFakeDrive) RootID() string { return "root" }

type scannerTreeFakeDrive struct {
	kind    string
	id      string
	entries map[string][]drives.Entry
}

func (d *scannerTreeFakeDrive) Kind() string {
	if d.kind != "" {
		return d.kind
	}
	return "fake"
}
func (d *scannerTreeFakeDrive) ID() string {
	if d.id != "" {
		return d.id
	}
	return "drive"
}
func (d *scannerTreeFakeDrive) Init(context.Context) error {
	return nil
}
func (d *scannerTreeFakeDrive) List(_ context.Context, parentID string) ([]drives.Entry, error) {
	return d.entries[parentID], nil
}
func (d *scannerTreeFakeDrive) Stat(context.Context, string) (*drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *scannerTreeFakeDrive) StreamURL(context.Context, string) (*drives.StreamLink, error) {
	return &drives.StreamLink{URL: "https://video.example/clip.mp4"}, nil
}
func (d *scannerTreeFakeDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *scannerTreeFakeDrive) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *scannerTreeFakeDrive) RootID() string { return "root" }

type discoveryTestSource struct {
	entries map[string][]drives.Entry
	errors  map[string]error
}

func (d *discoveryTestSource) Kind() string { return "fake" }
func (d *discoveryTestSource) ID() string   { return "drive" }
func (d *discoveryTestSource) List(_ context.Context, dirID string) ([]drives.Entry, error) {
	if err := d.errors[dirID]; err != nil {
		return nil, err
	}
	return d.entries[dirID], nil
}
func (d *discoveryTestSource) RootID() string { return "root" }

// captureLog 把 log 包默认输出引到一个 bytes.Buffer，便于断言进度日志被打印；
// 测试结束自动恢复。
func captureLog(t *testing.T) *strings.Builder {
	t.Helper()
	buf := &strings.Builder{}
	originalOutput := log.Writer()
	originalFlags := log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(originalOutput)
		log.SetFlags(originalFlags)
	})
	return buf
}

func TestScannerProgressHeartbeatEmits(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { cat.Close() })

	// 准备 5 个文件，足够触发循环结尾的 progress() 调用至少一次。
	entries := make([]drives.Entry, 5)
	for i := range entries {
		entries[i] = drives.Entry{
			ID:      fmt.Sprintf("file-%d", i),
			Name:    fmt.Sprintf("clip-%d.mp4", i),
			Size:    100,
			ModTime: time.Now(),
		}
	}
	drv := &scannerFakeDrive{entries: entries}

	sc := New(cat, drv, []string{".mp4"}, nil, nil)
	// 极短间隔，确保至少一次 heartbeat 在 walk 内被触发
	sc.ProgressInterval = 1 * time.Microsecond

	buf := captureLog(t)
	if _, err := sc.Run(ctx, ""); err != nil {
		t.Fatalf("scan: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "[scanner] drive=drive progress:") {
		t.Fatalf("expected progress heartbeat in log, got:\n%s", out)
	}
}

func TestScannerProgressHeartbeatDisabled(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { cat.Close() })

	drv := &scannerFakeDrive{entries: []drives.Entry{
		{ID: "f-1", Name: "x.mp4", Size: 1, ModTime: time.Now()},
	}}
	sc := New(cat, drv, []string{".mp4"}, nil, nil)
	sc.ProgressInterval = -1 // 显式关闭

	buf := captureLog(t)
	if _, err := sc.Run(ctx, ""); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if strings.Contains(buf.String(), "progress:") {
		t.Fatalf("progress heartbeat should be silenced when interval < 0, got:\n%s", buf.String())
	}
}
