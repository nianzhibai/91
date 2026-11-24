package catalog

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestListVideosKeywordMatchesFileName(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "p115-115-sone-089-4k",
		DriveID:     "drive",
		FileID:      "file-sone-089-4k",
		FileName:    "www.98T.la@sone-089-4k.mp4",
		Title:       "www.98T.la@sone-089",
		Author:      "4k",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	items, total, err := cat.ListVideos(ctx, ListParams{
		Keyword:  "www.98T.la@sone-089-4k.mp4",
		Page:     1,
		PageSize: 10,
	})
	if err != nil {
		t.Fatalf("list videos: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
	if len(items) != 1 || items[0].ID != "p115-115-sone-089-4k" {
		t.Fatalf("items = %#v, want seeded video", items)
	}
}

func TestListVideosAdvancedFilters(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	seed := func(id, driveID string, createdAt, publishedAt time.Time, durationSeconds int) {
		t.Helper()
		if err := cat.UpsertVideo(ctx, &Video{
			ID:              id,
			DriveID:         driveID,
			FileID:          "file-" + id,
			Title:           id,
			DurationSeconds: durationSeconds,
			PublishedAt:     publishedAt,
			CreatedAt:       createdAt,
			UpdatedAt:       createdAt,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	localDate := func(year int, month time.Month, day int) time.Time {
		return time.Date(year, month, day, 12, 0, 0, 0, time.Local)
	}
	seed("scriptcrawler-crawler-a-source-1", "cloud-a", localDate(2026, time.July, 10), localDate(2026, time.June, 1), 5*60)
	seed("scriptcrawler-crawler-b-source-2", "cloud-a", localDate(2026, time.July, 15), localDate(2026, time.June, 15), 15*60)
	seed("manual-video", "cloud-b", localDate(2026, time.July, 20), localDate(2026, time.July, 1), 0)
	if err := cat.MarkCrawlerSourceSeen(ctx, "scriptcrawler", "crawler-a", "source-1", "imported", "scriptcrawler-crawler-a-source-1", "", 0); err != nil {
		t.Fatalf("mark crawler-a source: %v", err)
	}
	if err := cat.MarkCrawlerSourceSeen(ctx, "scriptcrawler", "crawler-b", "source-2", "imported", "scriptcrawler-crawler-b-source-2", "", 0); err != nil {
		t.Fatalf("mark crawler-b source: %v", err)
	}
	if err := cat.MarkCrawlerSourceSeen(ctx, "scriptcrawler", "crawler-a", "duplicate", "duplicate", "manual-video", "", 0); err != nil {
		t.Fatalf("mark duplicate crawler source: %v", err)
	}

	assertIDs := func(name string, params ListParams, want ...string) {
		t.Helper()
		params.Page = 1
		params.PageSize = 20
		items, total, err := cat.ListVideos(ctx, params)
		if err != nil {
			t.Fatalf("%s: list videos: %v", name, err)
		}
		if total != len(want) || len(items) != len(want) {
			t.Fatalf("%s: total/items = %d/%d, want %d", name, total, len(items), len(want))
		}
		got := make(map[string]bool, len(items))
		for _, item := range items {
			got[item.ID] = true
		}
		for _, id := range want {
			if !got[id] {
				t.Fatalf("%s: missing %s in %#v", name, id, got)
			}
		}
	}

	assertIDs("drive", ListParams{DriveID: "cloud-a"},
		"scriptcrawler-crawler-a-source-1", "scriptcrawler-crawler-b-source-2")
	assertIDs("crawler", ListParams{CrawlerID: "crawler-a"},
		"scriptcrawler-crawler-a-source-1")
	assertIDs("created range", ListParams{
		CreatedAtFrom:   time.Date(2026, time.July, 15, 0, 0, 0, 0, time.Local).UnixMilli(),
		CreatedAtBefore: time.Date(2026, time.July, 20, 0, 0, 0, 0, time.Local).UnixMilli(),
	}, "scriptcrawler-crawler-b-source-2")
	assertIDs("duration range", ListParams{
		DurationSecondsMin: 10 * 60,
		DurationSecondsMax: 20 * 60,
	}, "scriptcrawler-crawler-b-source-2")
	assertIDs("duration max excludes unknown", ListParams{
		DurationSecondsMax: 20 * 60,
	}, "scriptcrawler-crawler-a-source-1", "scriptcrawler-crawler-b-source-2")
	assertIDs("combined", ListParams{
		DriveID:            "cloud-a",
		CrawlerID:          "crawler-a",
		CreatedAtFrom:      time.Date(2026, time.July, 10, 0, 0, 0, 0, time.Local).UnixMilli(),
		CreatedAtBefore:    time.Date(2026, time.July, 11, 0, 0, 0, 0, time.Local).UnixMilli(),
		DurationSecondsMin: 4 * 60,
		DurationSecondsMax: 6 * 60,
	}, "scriptcrawler-crawler-a-source-1")
}

func TestListVideosKeywordMatchesDirectoryPath(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	seed := func(id, fileName, dirName string, ancestorDirNames []string) {
		t.Helper()
		if err := cat.UpsertVideo(ctx, &Video{
			ID: id, DriveID: "drive", FileID: id, FileName: fileName, Title: id,
			DirName: dirName, AncestorDirNames: ancestorDirNames,
			PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("trip", "片段.mp4", "2025", []string{"片库", "旅行", "2025"})
	seed("legacy", "clip.mp4", "旧目录", nil)
	seed("loose", "loose.mp4", "", nil)

	assertKeyword := func(keyword string, want ...string) {
		t.Helper()
		items, total, err := cat.ListVideos(ctx, ListParams{Keyword: keyword, Page: 1, PageSize: 10})
		if err != nil {
			t.Fatalf("list %q: %v", keyword, err)
		}
		var got []string
		for _, item := range items {
			got = append(got, item.ID)
		}
		if total != len(want) || !sameStrings(got, want) {
			t.Fatalf("keyword %q matched %v (total %d), want %v", keyword, got, total, want)
		}
	}
	assertKeyword("旅行", "trip")
	assertKeyword("2025", "trip")
	assertKeyword("旅行/2025/片段", "trip")
	assertKeyword("旧目录/clip", "legacy")
	assertKeyword("loose.mp4", "loose")
	// The scan root contains the whole drive, so its name matches nothing.
	assertKeyword("片库")

	// A rescan that moves the file updates the searchable path.
	if err := cat.UpdateVideoMeta(ctx, "trip", VideoMetaPatch{
		DirName: "2026", DirNameSet: true,
		AncestorDirNames: []string{"片库", "出差", "2026"}, AncestorDirNamesSet: true,
	}); err != nil {
		t.Fatalf("move video: %v", err)
	}
	assertKeyword("旅行")
	assertKeyword("出差/2026", "trip")
}

func TestOpenBackfillsVideoDirectoryPaths(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	cat, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.UpsertVideo(ctx, &Video{
		ID: "old-video", DriveID: "drive", FileID: "clip", FileName: "clip.mp4", Title: "clip",
		DirName: "2025", AncestorDirNames: []string{"片库", "旅行", "2025"},
	}); err != nil {
		t.Fatal(err)
	}
	// Rows scanned before the chain was recorded only know their direct parent.
	if err := cat.UpsertDrive(ctx, &Drive{ID: "drive", Kind: "local", Name: "drive", RootID: "root"}); err != nil {
		t.Fatal(err)
	}
	for _, video := range []*Video{
		{ID: "legacy-nested", DriveID: "drive", FileID: "nested", FileName: "a.mp4", Title: "a", ParentID: "sub", DirName: "旧目录"},
		{ID: "legacy-root", DriveID: "drive", FileID: "top", FileName: "b.mp4", Title: "b", ParentID: "root", DirName: "片库"},
	} {
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cat.db.ExecContext(ctx, `DROP TRIGGER sync_video_dir_path_on_insert; DROP TRIGGER sync_video_dir_path_on_update; ALTER TABLE videos DROP COLUMN dir_path`); err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	cat, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	for id, want := range map[string]string{
		"old-video":     "旅行/2025",
		"legacy-nested": "旧目录",
		"legacy-root":   "",
	} {
		var dirPath string
		if err := cat.db.QueryRowContext(ctx, `SELECT dir_path FROM videos WHERE id = ?`, id).Scan(&dirPath); err != nil {
			t.Fatal(err)
		}
		if dirPath != want {
			t.Fatalf("%s dir_path = %q, want %q", id, dirPath, want)
		}
	}
}

func TestListVideosKeywordMultiTermSearch(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	seed := func(id, title, author, fileName, parentID string, dirs ...string) {
		t.Helper()
		video := &Video{
			ID: id, DriveID: "drive", FileID: id, FileName: fileName, Title: title, Author: author,
			ParentID: parentID, AncestorDirNames: dirs,
			PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		}
		if len(dirs) > 0 {
			video.DirName = dirs[len(dirs)-1]
			video.AncestorDirNames = append([]string{"片库"}, dirs...)
		}
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("trip-clip", "海边剪辑", "alice", "片段.mp4", "trip-2025", "旅行", "2025")
	seed("trip-other", "2025日本旅行", "bob", "vlog.mp4", "")
	seed("junior-ep2", "第2集", "", "第2集.mp4", "junior", "大三")
	seed("junior-ep10", "第10集", "", "第10集.mp4", "junior", "大三")
	seed("only-year", "2025总结", "", "summary.mp4", "")

	assertIDs := func(keyword, sort string, want ...string) {
		t.Helper()
		ids, err := cat.ListVideoIDs(ctx, ListParams{Keyword: keyword, Sort: sort}, 0)
		if err != nil {
			t.Fatalf("list %q (%s): %v", keyword, sort, err)
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("keyword %q (%s) = %v, want %v", keyword, sort, ids, want)
		}
	}

	// Directory + file name / title across terms.
	assertIDs("旅行 2025", "hot", "trip-clip", "trip-other")
	assertIDs("大三 第2集", "hot", "junior-ep2")
	assertIDs("alice 片段", "hot", "trip-clip")
	assertIDs("   旅行   片段  ", "hot", "trip-clip")
}

func TestListVideosKeywordMatchesLikeWildcardsLiterally(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	for id, fileName := range map[string]string{
		"underscore": "sone_089.mp4",
		"dash":       "sone-089.mp4",
		"percent":    "100%.mp4",
		"digits":     "1000.mp4",
		"backslash":  `a\b.mp4`,
	} {
		if err := cat.UpsertVideo(ctx, &Video{
			ID: id, DriveID: "drive", FileID: id, FileName: fileName, Title: id,
			PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	for keyword, want := range map[string]string{
		"_":        "underscore",
		"sone_089": "underscore",
		"%":        "percent",
		"100%":     "percent",
		`\`:        "backslash",
	} {
		for _, sort := range []string{"latest", "hot"} {
			items, total, err := cat.ListVideos(ctx, ListParams{Keyword: keyword, Sort: sort, Page: 1, PageSize: 10})
			if err != nil {
				t.Fatalf("list %q: %v", keyword, err)
			}
			if total != 1 || len(items) != 1 || items[0].ID != want {
				var got []string
				for _, item := range items {
					got = append(got, item.ID)
				}
				t.Fatalf("%s search %q matched %v (total %d), want only %s", sort, keyword, got, total, want)
			}
		}
	}
}
