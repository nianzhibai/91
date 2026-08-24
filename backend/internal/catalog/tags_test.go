package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/video-site/backend/internal/tagging"
)

func TestListVideosNeedingThumbnailIncludesExistingThumbnailMissingDuration(t *testing.T) {
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
	videos := []*Video{
		{
			ID:           "duration-only",
			DriveID:      "drive",
			FileID:       "file-duration-only",
			Title:        "Duration Only",
			ThumbnailURL: "/p/thumb/duration-only",
			PublishedAt:  now,
			CreatedAt:    now,
			UpdatedAt:    now,
		},
		{
			ID:              "complete",
			DriveID:         "drive",
			FileID:          "file-complete",
			Title:           "Complete",
			DurationSeconds: 12,
			ThumbnailURL:    "/p/thumb/complete",
			PublishedAt:     now.Add(time.Second),
			CreatedAt:       now.Add(time.Second),
			UpdatedAt:       now.Add(time.Second),
		},
		{
			ID:              "missing-thumb",
			DriveID:         "drive",
			FileID:          "file-missing-thumb",
			Title:           "Missing Thumb",
			DurationSeconds: 18,
			PublishedAt:     now.Add(2 * time.Second),
			CreatedAt:       now.Add(2 * time.Second),
			UpdatedAt:       now.Add(2 * time.Second),
		},
		{
			ID:          "failed",
			DriveID:     "drive",
			FileID:      "file-failed",
			Title:       "Failed",
			PublishedAt: now.Add(3 * time.Second),
			CreatedAt:   now.Add(3 * time.Second),
			UpdatedAt:   now.Add(3 * time.Second),
		},
	}
	for _, v := range videos {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed %s: %v", v.ID, err)
		}
	}
	if err := cat.UpdateVideoMeta(ctx, "failed", VideoMetaPatch{ThumbnailStatus: "failed"}); err != nil {
		t.Fatalf("mark failed thumbnail: %v", err)
	}

	items, err := cat.ListVideosNeedingThumbnail(ctx, "drive", 0)
	if err != nil {
		t.Fatalf("list videos needing thumbnail: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %#v, want duration-only and missing-thumb", items)
	}
	if items[0].ID != "duration-only" || items[1].ID != "missing-thumb" {
		t.Fatalf("item ids = %q, %q; want duration-only, missing-thumb", items[0].ID, items[1].ID)
	}

	count, err := cat.CountVideosNeedingThumbnail(ctx, "drive")
	if err != nil {
		t.Fatalf("count videos needing thumbnail: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}

	counts, err := cat.CountThumbnailsByDrive(ctx)
	if err != nil {
		t.Fatalf("count thumbnails by drive: %v", err)
	}
	if got := counts["drive"]; got.Ready != 2 || got.Pending != 1 || got.Failed != 1 || got.DurationPending != 1 {
		t.Fatalf("thumbnail counts = %#v, want ready=2 pending=1 failed=1 durationPending=1", got)
	}

	if err := cat.UpdateVideoMeta(ctx, "duration-only", VideoMetaPatch{ThumbnailStatus: "skipped"}); err != nil {
		t.Fatalf("mark duration-only skipped: %v", err)
	}
	count, err = cat.CountVideosNeedingThumbnail(ctx, "drive")
	if err != nil {
		t.Fatalf("count videos needing thumbnail after skip: %v", err)
	}
	if count != 1 {
		t.Fatalf("count after skip = %d, want 1", count)
	}
	counts, err = cat.CountThumbnailsByDrive(ctx)
	if err != nil {
		t.Fatalf("count thumbnails by drive after skip: %v", err)
	}
	if got := counts["drive"]; got.Ready != 2 || got.Pending != 1 || got.Failed != 1 || got.DurationPending != 0 {
		t.Fatalf("thumbnail counts after skip = %#v, want ready=2 pending=1 failed=1 durationPending=0", got)
	}
}

func TestCreateTagAndClassifyMatchesExistingVideos(t *testing.T) {
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
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "清纯短发合集",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed matching video: %v", err)
	}
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "video-2",
		DriveID:     "drive",
		FileID:      "file-2",
		Title:       "普通标题",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed non-matching video: %v", err)
	}

	classified, err := cat.CreateTagAndClassify(ctx, "清纯", nil, "user")
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if classified != 1 {
		t.Fatalf("classified = %d, want 1", classified)
	}

	got, err := cat.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatalf("get matching video: %v", err)
	}
	if !sameStrings(got.Tags, []string{"清纯"}) {
		t.Fatalf("matching tags = %#v, want 清纯", got.Tags)
	}

	other, err := cat.GetVideo(ctx, "video-2")
	if err != nil {
		t.Fatalf("get non-matching video: %v", err)
	}
	if len(other.Tags) != 0 {
		t.Fatalf("non-matching tags = %#v, want none", other.Tags)
	}
}

func TestUpsertVideoMatchesExistingTagsForNewVideo(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "new-auto-tagged",
		DriveID:     "drive",
		FileID:      "file-auto",
		Title:       "大奶揉胸合集",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("upsert video: %v", err)
	}
	got, err := cat.GetVideo(ctx, "new-auto-tagged")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if !sameStrings(got.Tags, []string{"奶子"}) {
		t.Fatalf("new video tags = %#v, want 奶子", got.Tags)
	}
}

func TestDeleteTagRemovesTagFromVideos(t *testing.T) {
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
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "清纯短发",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if _, err := cat.CreateTagAndClassify(ctx, "清纯", nil, "user"); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if err := cat.SetManualVideoTags(ctx, "video-1", []string{"清纯"}); err != nil {
		t.Fatalf("set manual tag: %v", err)
	}

	tag := mustTagByLabel(t, ctx, cat, "清纯")
	removed, err := cat.DeleteTag(ctx, tag.ID)
	if err != nil {
		t.Fatalf("delete tag: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}

	got, err := cat.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if len(got.Tags) != 0 {
		t.Fatalf("video tags = %#v, want none", got.Tags)
	}
	for _, tag := range mustListTags(t, ctx, cat) {
		if tag.Label == "清纯" {
			t.Fatal("deleted tag still appears in ListTags")
		}
	}
}

func TestCreateTagAndClassifyRecreatesDeletedTag(t *testing.T) {
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
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "清纯短发",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if _, err := cat.CreateTagAndClassify(ctx, "清纯", nil, "user"); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	tag := mustTagByLabel(t, ctx, cat, "清纯")
	if _, err := cat.DeleteTag(ctx, tag.ID); err != nil {
		t.Fatalf("delete tag: %v", err)
	}

	classified, err := cat.CreateTagAndClassify(ctx, "清纯", nil, "user")
	if err != nil {
		t.Fatalf("recreate tag: %v", err)
	}
	if classified != 1 {
		t.Fatalf("classified = %d, want 1", classified)
	}
	got, err := cat.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if !sameStrings(got.Tags, []string{"清纯"}) {
		t.Fatalf("video tags = %#v, want 清纯", got.Tags)
	}
}

func TestEnsureTagForVideoIDPrefixBackfillsSourceTag(t *testing.T) {
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
	for _, seed := range []struct {
		id     string
		manual bool
	}{
		{id: "scriptcrawler-crawler-a-source001"},
		{id: "scriptcrawler-crawler-a-source002", manual: true},
		{id: "scriptcrawler-other-source003"},
	} {
		if err := cat.UpsertVideo(ctx, &Video{
			ID:          seed.id,
			DriveID:     "crawler-a",
			FileID:      seed.id + ".mp4",
			Title:       "legacy title without source text",
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("seed %s: %v", seed.id, err)
		}
		if seed.manual {
			if err := cat.SetManualVideoTags(ctx, seed.id, nil); err != nil {
				t.Fatalf("mark %s manual: %v", seed.id, err)
			}
		}
	}

	added, err := cat.EnsureCrawlerTagForVideoIDPrefix(ctx, "scriptcrawler-crawler-a-", "crawler-tag")
	if err != nil {
		t.Fatalf("ensure prefix tag: %v", err)
	}
	if added != 2 {
		t.Fatalf("added = %d, want 2", added)
	}
	if tag := mustTagByLabel(t, ctx, cat, "crawler-tag"); tag.Source != "generated" {
		t.Fatalf("crawler tag source = %q, want generated", tag.Source)
	}
	got, err := cat.GetVideo(ctx, "scriptcrawler-crawler-a-source001")
	if err != nil {
		t.Fatalf("get tagged video: %v", err)
	}
	if !sameStrings(got.Tags, []string{"crawler-tag"}) {
		t.Fatalf("tagged video tags = %#v, want crawler-tag", got.Tags)
	}
	manual, err := cat.GetVideo(ctx, "scriptcrawler-crawler-a-source002")
	if err != nil {
		t.Fatalf("get manual video: %v", err)
	}
	if !sameStrings(manual.Tags, []string{"crawler-tag"}) {
		t.Fatalf("manual video tags = %#v, want crawler-tag", manual.Tags)
	}
	other, err := cat.GetVideo(ctx, "scriptcrawler-other-source003")
	if err != nil {
		t.Fatalf("get other prefix video: %v", err)
	}
	if len(other.Tags) != 0 {
		t.Fatalf("other prefix video tags = %#v, want unchanged", other.Tags)
	}
}

func TestAutoGenerateTagsSettingPreventsNewGeneratedTags(t *testing.T) {
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

	enabled, err := cat.AutoGenerateTagsEnabled(ctx)
	if err != nil {
		t.Fatalf("read default setting: %v", err)
	}
	if enabled {
		t.Fatal("auto-generate tags should default to disabled")
	}

	if err := cat.SetAutoGenerateTagsEnabled(ctx, false); err != nil {
		t.Fatalf("disable auto-generate tags: %v", err)
	}
	enabled, err = cat.AutoGenerateTagsEnabled(ctx)
	if err != nil {
		t.Fatalf("read disabled setting: %v", err)
	}
	if enabled {
		t.Fatal("auto-generate tags setting stayed enabled")
	}

	if _, err := cat.EnsureTag(ctx, "new-auto", "generated"); !errors.Is(err, ErrAutoTagGenerationDisabled) {
		t.Fatalf("ensure new generated tag err = %v, want ErrAutoTagGenerationDisabled", err)
	}
	if _, err := cat.getTagByLabel(ctx, "new-auto"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("new generated tag exists: %v", err)
	}

	if err := cat.SetAutoGenerateTagsEnabled(ctx, true); err != nil {
		t.Fatalf("enable auto-generate tags: %v", err)
	}
	enabled, err = cat.AutoGenerateTagsEnabled(ctx)
	if err != nil {
		t.Fatalf("read re-enabled setting: %v", err)
	}
	if enabled {
		t.Fatal("auto-generate tags should stay disabled")
	}

	userTag, err := cat.EnsureTag(ctx, "manual-tag", "user")
	if err != nil {
		t.Fatalf("ensure user tag while disabled: %v", err)
	}
	if userTag.Source != "user" {
		t.Fatalf("user tag source = %q, want user", userTag.Source)
	}

	crawlerTag, err := cat.EnsureCrawlerTag(ctx, "Crawler Owner")
	if err != nil {
		t.Fatalf("ensure crawler tag while disabled: %v", err)
	}
	if crawlerTag.Source != "generated" {
		t.Fatalf("crawler tag source = %q, want generated", crawlerTag.Source)
	}
	if _, err := cat.DeleteTag(ctx, crawlerTag.ID); err != nil {
		t.Fatalf("delete crawler tag: %v", err)
	}
	restoredCrawlerTag, err := cat.EnsureCrawlerTag(ctx, "Crawler Owner")
	if err != nil {
		t.Fatalf("restore crawler tag while disabled: %v", err)
	}
	if restoredCrawlerTag.Source != "generated" {
		t.Fatalf("restored crawler tag source = %q, want generated", restoredCrawlerTag.Source)
	}
}

func TestEnsureCrawlerTagForVideoIDPrefixIgnoresAutoGenerateSetting(t *testing.T) {
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
	if err := cat.SetAutoGenerateTagsEnabled(ctx, false); err != nil {
		t.Fatalf("disable auto-generate tags: %v", err)
	}
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "scriptcrawler-demo-source001",
		DriveID:     "demo",
		FileID:      "source001",
		Title:       "crawler video",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed crawler video: %v", err)
	}
	if _, err := cat.EnsureTag(ctx, "manual-only", "user"); err != nil {
		t.Fatalf("seed manual tag: %v", err)
	}
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "scriptcrawler-demo-source002",
		DriveID:     "demo",
		FileID:      "source002",
		Title:       "manual crawler video",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed manual crawler video: %v", err)
	}
	if err := cat.SetManualVideoTags(ctx, "scriptcrawler-demo-source002", []string{"manual-only"}); err != nil {
		t.Fatalf("lock manual crawler video: %v", err)
	}
	added, err := cat.EnsureCrawlerTagForVideoIDPrefix(ctx, "scriptcrawler-demo-", "Demo Crawler")
	if err != nil {
		t.Fatalf("ensure crawler tag prefix: %v", err)
	}
	if added != 2 {
		t.Fatalf("added = %d, want 2", added)
	}
	got, err := cat.GetVideo(ctx, "scriptcrawler-demo-source001")
	if err != nil {
		t.Fatalf("get crawler video: %v", err)
	}
	if !sameStrings(got.Tags, []string{"Demo Crawler"}) {
		t.Fatalf("crawler video tags = %#v, want Demo Crawler", got.Tags)
	}
	manual, err := cat.GetVideo(ctx, "scriptcrawler-demo-source002")
	if err != nil {
		t.Fatalf("get manual crawler video: %v", err)
	}
	if !sameStrings(manual.Tags, []string{"manual-only", "Demo Crawler"}) {
		t.Fatalf("manual crawler video tags = %#v, want manual-only + Demo Crawler", manual.Tags)
	}

	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "other-crawler-video",
		DriveID:     "demo",
		FileID:      "source003",
		Title:       "direct manual crawler video",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed direct manual crawler video: %v", err)
	}
	if err := cat.SetManualVideoTags(ctx, "other-crawler-video", []string{"manual-only"}); err != nil {
		t.Fatalf("lock direct manual crawler video: %v", err)
	}
	changed, err := cat.EnsureCrawlerTagForVideo(ctx, "other-crawler-video", "Direct Crawler")
	if err != nil {
		t.Fatalf("ensure direct crawler tag: %v", err)
	}
	if !changed {
		t.Fatal("direct crawler tag did not report a change")
	}
	direct, err := cat.GetVideo(ctx, "other-crawler-video")
	if err != nil {
		t.Fatalf("get direct manual crawler video: %v", err)
	}
	if !sameStrings(direct.Tags, []string{"manual-only", "Direct Crawler"}) {
		t.Fatalf("direct manual crawler video tags = %#v, want manual-only + Direct Crawler", direct.Tags)
	}
}

func TestEnsureCrawlerTagForVideoIDPrefixDoesNotCreateTagWithoutVideos(t *testing.T) {
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

	added, err := cat.EnsureCrawlerTagForVideoIDPrefix(ctx, "scriptcrawler-empty-", "Empty Crawler")
	if err != nil {
		t.Fatalf("ensure empty crawler tag prefix: %v", err)
	}
	if added != 0 {
		t.Fatalf("added = %d, want 0", added)
	}
	if _, err := cat.getTagByLabel(ctx, "Empty Crawler"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("empty crawler tag was created: %v", err)
	}
}

func TestUserSelectableTagsFollowManagedCatalog(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	if _, err := cat.CreateTagAndClassify(ctx, "自定义上传", nil, "user"); err != nil {
		t.Fatalf("create user tag: %v", err)
	}
	if _, err := cat.EnsureCrawlerTag(ctx, "爬虫来源"); err != nil {
		t.Fatalf("create crawler tag: %v", err)
	}

	tags, err := cat.ListUserSelectableTags(ctx)
	if err != nil {
		t.Fatalf("list user-selectable tags: %v", err)
	}
	labels := make([]string, 0, len(tags))
	for _, tag := range tags {
		labels = append(labels, tag.Label)
	}
	if !stringSliceContains(labels, "奶子") || !stringSliceContains(labels, "自定义上传") {
		t.Fatalf("selectable labels = %#v, want builtin and user tags", labels)
	}
	if stringSliceContains(labels, "AV") || stringSliceContains(labels, "爬虫来源") {
		t.Fatalf("selectable labels = %#v, want no inferred or crawler tags", labels)
	}

	canonical, ok, err := cat.LookupUserSelectableTagLabel(ctx, " 自定义上传 ")
	if err != nil || !ok || canonical != "自定义上传" {
		t.Fatalf("lookup user tag = %q, %v, %v", canonical, ok, err)
	}
	if _, ok, err := cat.LookupUserSelectableTagLabel(ctx, "爬虫来源"); err != nil || ok {
		t.Fatalf("crawler lookup selectable = %v, %v; want false", ok, err)
	}

	managed := mustTagByLabel(t, ctx, cat, "自定义上传")
	if _, err := cat.DeleteTag(ctx, managed.ID); err != nil {
		t.Fatalf("delete user tag: %v", err)
	}
	if _, ok, err := cat.LookupUserSelectableTagLabel(ctx, "自定义上传"); err != nil || ok {
		t.Fatalf("deleted user tag selectable = %v, %v; want false", ok, err)
	}
}

func TestDeleteTagAllowsBuiltinTagsWithoutMaintenanceReseed(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	label := "美臀"
	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}

	tag := mustTagByLabel(t, ctx, cat, label)
	if _, err := cat.DeleteTag(ctx, tag.ID); err != nil {
		t.Fatalf("delete builtin tag: %v", err)
	}
	if _, err := cat.getTagByLabel(ctx, label); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted builtin tag still exists: %v", err)
	}
	if err := cat.Close(); err != nil {
		t.Fatalf("close catalog: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen catalog: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.getTagByLabel(ctx, label); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted builtin tag was recreated on reopen: %v", err)
	}
	if err := reopened.RunPostStartupTagMaintenance(ctx); err != nil {
		t.Fatalf("post-startup maintenance with deleted builtin tag: %v", err)
	}
	if _, err := reopened.getTagByLabel(ctx, label); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted builtin tag was recreated by maintenance: %v", err)
	}
}

func TestBuiltinTagPackSwitchRemovesAndRestoresCatalogState(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}

	enabled, err := cat.BuiltinTagsEnabled(ctx)
	if err != nil || !enabled {
		t.Fatalf("initial builtin setting = %v, %v; want enabled", enabled, err)
	}
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "builtin-switch-video",
		DriveID:     "drive",
		FileID:      "file",
		FileName:    "SSNI-001.mp4",
		Title:       "翘臀 自定义 SSNI-001",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if _, err := cat.CreateTagAndClassify(ctx, "自定义", nil, "user"); err != nil {
		t.Fatalf("create custom tag: %v", err)
	}
	if err := cat.RunPostStartupTagMaintenance(ctx); err != nil {
		t.Fatalf("initial tag maintenance: %v", err)
	}

	video, err := cat.GetVideo(ctx, "builtin-switch-video")
	if err != nil {
		t.Fatalf("get initially tagged video: %v", err)
	}
	for _, label := range []string{"自定义", "AV", "美臀"} {
		if !stringSliceContains(video.Tags, label) {
			t.Fatalf("initial video tags = %#v, want %q", video.Tags, label)
		}
	}

	changed, err := cat.SetBuiltinTagsEnabled(ctx, false)
	if err != nil || !changed {
		t.Fatalf("disable builtin tags = %v, %v; want changed", changed, err)
	}
	assertBuiltinTagPackState(t, ctx, cat, false, 0)
	video, err = cat.GetVideo(ctx, "builtin-switch-video")
	if err != nil {
		t.Fatalf("get video after disabling: %v", err)
	}
	if !sameStrings(video.Tags, []string{"自定义"}) {
		t.Fatalf("video tags after disabling = %#v, want custom tag only", video.Tags)
	}
	var seriesCount int
	if err := cat.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM tags WHERE source = 'generated' AND origin = ?`, avSeriesOrigin).Scan(&seriesCount); err != nil {
		t.Fatalf("count AV series tags: %v", err)
	}
	if seriesCount != 0 {
		t.Fatalf("AV series tag count after disabling = %d, want 0", seriesCount)
	}
	if _, err := cat.ensureTagWithRules(ctx, "美臀", nil, tagging.Rule{}, "builtin"); !errors.Is(err, ErrBuiltinTagsDisabled) {
		t.Fatalf("creating builtin while disabled returned %v, want ErrBuiltinTagsDisabled", err)
	}
	changed, err = cat.SetBuiltinTagsEnabled(ctx, false)
	if err != nil || changed {
		t.Fatalf("second disable = %v, %v; want unchanged", changed, err)
	}

	if err := cat.Close(); err != nil {
		t.Fatalf("close catalog: %v", err)
	}
	cat, err = Open(path)
	if err != nil {
		t.Fatalf("reopen disabled catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	assertBuiltinTagPackState(t, ctx, cat, false, 0)

	changed, err = cat.SetBuiltinTagsEnabled(ctx, true)
	if err != nil || !changed {
		t.Fatalf("enable builtin tags = %v, %v; want changed", changed, err)
	}
	assertBuiltinTagPackState(t, ctx, cat, true, 8)
	if err := cat.RunPostStartupTagMaintenance(ctx); err != nil {
		t.Fatalf("tag maintenance after enabling: %v", err)
	}
	video, err = cat.GetVideo(ctx, "builtin-switch-video")
	if err != nil {
		t.Fatalf("get video after enabling: %v", err)
	}
	for _, label := range []string{"自定义", "AV", "美臀"} {
		if !stringSliceContains(video.Tags, label) {
			t.Fatalf("restored video tags = %#v, want %q", video.Tags, label)
		}
	}
}

func TestBuiltinTagPackDisabledBeforeInitializationStaysEmpty(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO settings (key, value, updated_at) VALUES (?, 'false', ?)`,
		settingBuiltinTagsEnabled, time.Now().UnixMilli()); err != nil {
		t.Fatalf("disable builtin pack: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	assertBuiltinTagPackState(t, ctx, cat, false, 0)
	marker, err := cat.GetSetting(ctx, settingBuiltinTagPackInit, "")
	if err != nil || !parseSettingBool(marker, false) {
		t.Fatalf("builtin initialization marker = %q, %v; want true", marker, err)
	}
}

func assertBuiltinTagPackState(t *testing.T, ctx context.Context, cat *Catalog, wantEnabled bool, wantCount int) {
	t.Helper()
	enabled, err := cat.BuiltinTagsEnabled(ctx)
	if err != nil {
		t.Fatalf("read builtin setting: %v", err)
	}
	if enabled != wantEnabled {
		t.Fatalf("builtin setting = %v, want %v", enabled, wantEnabled)
	}
	var count int
	if err := cat.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tags WHERE source = 'builtin'`).Scan(&count); err != nil {
		t.Fatalf("count builtin tags: %v", err)
	}
	if count != wantCount {
		t.Fatalf("builtin tag count = %d, want %d", count, wantCount)
	}
}

func TestMigrateResetsLegacyTagPoolToUserTagsPlusBuiltinPack(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	now := time.Now().UnixMilli()
	legacyRule := `{"keywords":["大学生","college student"],"words":["大一"],"excludes":["大学路"]}`
	if _, err := db.ExecContext(ctx, `
INSERT INTO tags (id, label, aliases, match_rules, source, origin, created_at, updated_at)
VALUES
	(1, '我的标签', '[]', '{"keywords":["custom-only"]}', 'user', '', ?, ?),
	(2, '女大', '[]', ?, 'builtin', '', ?, ?),
	(3, '旧自动', '[]', '{"keywords":["old-auto"]}', 'generated', '', ?, ?),
	(4, '旧爬虫', '[]', '{}', 'generated', 'crawler', ?, ?)`,
		now, now, legacyRule, now, now, now, now, now, now); err != nil {
		t.Fatalf("seed legacy tags: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO videos (id, drive_id, file_id, title, tags, tags_manual, published_at, created_at, updated_at)
VALUES ('legacy-video', 'drive', 'file', 'legacy video', '["我的标签","女大","旧自动","旧爬虫"]', 0, ?, ?, ?)`,
		now, now, now); err != nil {
		t.Fatalf("seed legacy video: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO video_tags (video_id, tag_id, source, evidence, created_at)
VALUES
	('legacy-video', 1, 'manual', '', ?),
	('legacy-video', 2, 'auto', '', ?),
	('legacy-video', 3, 'auto', '', ?),
	('legacy-video', 4, 'crawler', '', ?)`,
		now, now, now, now); err != nil {
		t.Fatalf("seed legacy video tags: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	custom := mustTagByLabel(t, ctx, cat, "我的标签")
	if custom.Source != "user" {
		t.Fatalf("custom tag source = %q, want user", custom.Source)
	}
	for _, label := range []string{"旧自动", "旧爬虫"} {
		if _, err := cat.getTagByLabel(ctx, label); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("legacy non-user tag %q still exists: %v", label, err)
		}
	}
	tag := mustTagByLabel(t, ctx, cat, "女大")
	if tag.Source != "builtin" {
		t.Fatalf("女大 source = %q, want builtin", tag.Source)
	}
	want := []string{"女大", "大一", "大二", "大三", "大四", "学妹", "学姐", "研究生"}
	if !sameStrings(tag.MatchRules.Keywords, want) {
		t.Fatalf("keywords = %#v, want %#v", tag.MatchRules.Keywords, want)
	}
	video, err := cat.GetVideo(ctx, "legacy-video")
	if err != nil {
		t.Fatalf("get migrated video: %v", err)
	}
	if !sameStrings(video.Tags, []string{"我的标签"}) {
		t.Fatalf("video tags = %#v, want only user tag", video.Tags)
	}
	marker, err := cat.GetSetting(ctx, settingBuiltinTagPackInit, "")
	if err != nil {
		t.Fatalf("read builtin init marker: %v", err)
	}
	if !parseSettingBool(marker, false) {
		t.Fatalf("builtin init marker = %q, want true", marker)
	}
}

func TestSeedBuiltinTagPackPreservesCustomBuiltinRules(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	customRule := `{"keywords":["custom-only"],"words":["legacy-word"],"excludes":["custom-exclude"]}`
	if _, err := cat.db.ExecContext(ctx,
		`UPDATE tags SET source = 'builtin', match_rules = ? WHERE label = '奶子'`,
		customRule); err != nil {
		t.Fatalf("seed custom rule: %v", err)
	}
	if err := cat.removeRetiredTagRuleFields(ctx); err != nil {
		t.Fatalf("remove retired fields: %v", err)
	}
	if err := cat.seedBuiltinTagPack(ctx); err != nil {
		t.Fatalf("seed builtin pack: %v", err)
	}
	tag := mustTagByLabel(t, ctx, cat, "奶子")
	if !sameStrings(tag.MatchRules.Keywords, []string{"custom-only"}) {
		t.Fatalf("custom keywords overwritten: %#v", tag.MatchRules.Keywords)
	}
	var raw string
	if err := cat.db.QueryRowContext(ctx, `SELECT match_rules FROM tags WHERE label = '奶子'`).Scan(&raw); err != nil {
		t.Fatalf("read raw match_rules: %v", err)
	}
	if strings.Contains(raw, `"words"`) || strings.Contains(raw, `"excludes"`) {
		t.Fatalf("retired fields were not removed: %s", raw)
	}
}

func TestBuiltinKeywordDeletionSurvivesMaintenanceAndReopen(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	tag := mustTagByLabel(t, ctx, cat, "奶子")
	var keywords []string
	for _, keyword := range tag.MatchRules.Keywords {
		if keyword != "大奶" {
			keywords = append(keywords, keyword)
		}
	}
	if stringSliceContains(keywords, "大奶") || !stringSliceContains(tag.MatchRules.Keywords, "大奶") {
		t.Fatalf("test setup failed, keywords = %#v", tag.MatchRules.Keywords)
	}
	if _, err := cat.UpdateTag(ctx, tag.ID, tagging.Rule{Keywords: keywords}); err != nil {
		t.Fatalf("update builtin keywords: %v", err)
	}
	if err := cat.RunPostStartupTagMaintenance(ctx); err != nil {
		t.Fatalf("post-startup maintenance: %v", err)
	}
	afterMaintenance := mustTagByLabel(t, ctx, cat, "奶子")
	if stringSliceContains(afterMaintenance.MatchRules.Keywords, "大奶") {
		t.Fatalf("maintenance restored deleted keyword: %#v", afterMaintenance.MatchRules.Keywords)
	}
	if err := cat.Close(); err != nil {
		t.Fatalf("close catalog: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen catalog: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	afterReopen := mustTagByLabel(t, ctx, reopened, "奶子")
	if stringSliceContains(afterReopen.MatchRules.Keywords, "大奶") {
		t.Fatalf("reopen restored deleted keyword: %#v", afterReopen.MatchRules.Keywords)
	}
}

func TestPostStartupTagMaintenanceClassifiesSystemTagsForExistingVideos(t *testing.T) {
	path := t.TempDir() + "/catalog.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := db.Exec(`
INSERT INTO videos (id, drive_id, file_id, title, tags, tags_manual, published_at, created_at, updated_at)
VALUES
	('video-auto', 'drive', 'file-auto', '巨乳后入合集', '[]', 0, ?, ?, ?),
	('video-manual', 'drive', 'file-manual', '巨乳后入合集', '[]', 1, ?, ?, ?)`,
		now, now, now, now, now, now); err != nil {
		t.Fatalf("seed videos: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	ctx := context.Background()
	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	before, err := cat.GetVideo(ctx, "video-auto")
	if err != nil {
		t.Fatalf("get video before background maintenance: %v", err)
	}
	if len(before.Tags) != 0 {
		t.Fatalf("Open performed full-library tag work: %#v", before.Tags)
	}
	if err := cat.RunPostStartupTagMaintenance(ctx); err != nil {
		t.Fatalf("run post-startup tag maintenance: %v", err)
	}

	got, err := cat.GetVideo(ctx, "video-auto")
	if err != nil {
		t.Fatalf("get auto video: %v", err)
	}
	if !sameStrings(got.Tags, []string{"奶子", "后入"}) {
		t.Fatalf("auto tags = %#v, want 奶子 + 后入", got.Tags)
	}

	manual, err := cat.GetVideo(ctx, "video-manual")
	if err != nil {
		t.Fatalf("get manual video: %v", err)
	}
	if len(manual.Tags) != 0 {
		t.Fatalf("manual tags = %#v, want unchanged", manual.Tags)
	}
}

func TestMigrateDoesNotRewriteAlreadySyncedVideoTags(t *testing.T) {
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
	for _, id := range []string{"video-1", "video-2", "video-3"} {
		if err := cat.UpsertVideo(ctx, &Video{
			ID:          id,
			DriveID:     "drive",
			FileID:      id,
			Title:       "巨乳后入合集",
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	if err := cat.migrate(ctx); err != nil {
		t.Fatalf("first migrate: %v", err)
	}

	before := videoUpdatedAtByID(t, ctx, cat, "video-1", "video-2", "video-3")
	time.Sleep(5 * time.Millisecond)
	if err := cat.migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	after := videoUpdatedAtByID(t, ctx, cat, "video-1", "video-2", "video-3")
	for id, want := range before {
		if got := after[id]; got != want {
			t.Fatalf("%s updated_at changed on no-op migrate: got %d, want %d", id, got, want)
		}
	}
}

func TestMigrateRemovesRetiredVideoQuality(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "video-quality",
		DriveID:     "drive",
		FileID:      "file-quality",
		Title:       "retired quality video",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx, `ALTER TABLE videos ADD COLUMN quality TEXT`); err != nil {
		t.Fatalf("add retired quality column: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx, `UPDATE videos SET quality = 'HD' WHERE id = 'video-quality'`); err != nil {
		t.Fatalf("seed retired quality: %v", err)
	}
	if err := cat.Close(); err != nil {
		t.Fatalf("close catalog: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen catalog: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if hasColumn(t, reopened, "videos", "quality") {
		t.Fatal("retired quality column was not dropped")
	}
	got, err := reopened.GetVideo(ctx, "video-quality")
	if err != nil {
		t.Fatalf("get migrated video: %v", err)
	}
	if got.Title != "retired quality video" {
		t.Fatalf("migrated video lost data: %#v", got)
	}
}

func TestMigrateRemovesRetiredLLMTaggingArtifacts(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "video-llm",
		DriveID:     "drive",
		FileID:      "file-llm",
		Title:       "legacy llm tagged video",
		Tags:        []string{"legacy-ai"},
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	tag := mustTagByLabel(t, ctx, cat, "legacy-ai")
	if _, err := cat.db.ExecContext(ctx, `ALTER TABLE videos ADD COLUMN llm_tagged_at INTEGER DEFAULT 0`); err != nil {
		t.Fatalf("add retired llm column: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx, `
UPDATE video_tags
   SET source = 'llm', evidence = 'retired'
 WHERE video_id = ? AND tag_id = ?`, "video-llm", tag.ID); err != nil {
		t.Fatalf("seed retired llm source: %v", err)
	}
	if err := cat.Close(); err != nil {
		t.Fatalf("close catalog: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen catalog: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if hasColumn(t, reopened, "videos", "llm_tagged_at") {
		t.Fatal("retired llm_tagged_at column was not dropped")
	}
	var retiredRows int
	if err := reopened.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM video_tags WHERE lower(trim(COALESCE(source, ''))) = 'llm'`).Scan(&retiredRows); err != nil {
		t.Fatalf("count retired llm rows: %v", err)
	}
	if retiredRows != 0 {
		t.Fatalf("retired llm video_tags rows = %d, want 0", retiredRows)
	}
	got, err := reopened.GetVideo(ctx, "video-llm")
	if err != nil {
		t.Fatalf("get migrated video: %v", err)
	}
	if len(got.Tags) != 0 {
		t.Fatalf("migrated video tags = %#v, want retired llm tags removed", got.Tags)
	}
}

func TestMigrateDoesNotBackfillLegacyTagsWithoutRelations(t *testing.T) {
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

	now := time.Now().UnixMilli()
	if _, err := cat.db.ExecContext(ctx, `
INSERT INTO videos (id, drive_id, file_id, title, tags, tags_manual, published_at, created_at, updated_at)
VALUES ('legacy-video', 'drive', 'file-legacy', 'legacy title', '["legacy-tag"]', 0, ?, ?, ?)`,
		now, now, now); err != nil {
		t.Fatalf("seed legacy video: %v", err)
	}
	if err := cat.RunPostStartupTagMaintenance(ctx); err != nil {
		t.Fatalf("post-startup tag maintenance: %v", err)
	}

	var count int
	if err := cat.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM video_tags WHERE video_id = 'legacy-video'`).Scan(&count); err != nil {
		t.Fatalf("count video tag: %v", err)
	}
	if count != 0 {
		t.Fatalf("legacy video tag relation count = %d, want 0", count)
	}
	if _, err := cat.getTagByLabel(ctx, "legacy-tag"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("legacy tag was created: %v", err)
	}
	got, err := cat.GetVideo(ctx, "legacy-video")
	if err != nil {
		t.Fatalf("get legacy video: %v", err)
	}
	if len(got.Tags) != 0 {
		t.Fatalf("legacy video tags = %#v, want none", got.Tags)
	}
}

func TestOpenMigratesLegacyVideosWithoutFileName(t *testing.T) {
	path := t.TempDir() + "/catalog.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`
CREATE TABLE videos (
	id               TEXT PRIMARY KEY,
	drive_id         TEXT NOT NULL,
	file_id          TEXT NOT NULL,
	content_hash     TEXT DEFAULT '',
	parent_id        TEXT,
	title            TEXT NOT NULL,
	author           TEXT,
	tags             TEXT,
	duration_seconds INTEGER DEFAULT 0,
	size_bytes       INTEGER DEFAULT 0,
	ext              TEXT,
	quality          TEXT,
	thumbnail_url    TEXT,
	preview_file_id  TEXT,
	preview_local    TEXT,
	preview_status   TEXT DEFAULT 'pending',
	views            INTEGER DEFAULT 0,
	favorites        INTEGER DEFAULT 0,
	comments         INTEGER DEFAULT 0,
	likes            INTEGER DEFAULT 0,
	dislikes         INTEGER DEFAULT 0,
	category         TEXT,
	hidden           INTEGER DEFAULT 0,
	tags_manual      INTEGER DEFAULT 0,
	badges           TEXT,
	description      TEXT,
	published_at     INTEGER NOT NULL,
	created_at       INTEGER NOT NULL,
	updated_at       INTEGER NOT NULL
)`); err != nil {
		t.Fatalf("create legacy videos table: %v", err)
	}
	nowMillis := time.Now().UnixMilli()
	if _, err := db.Exec(`
INSERT INTO videos (
	id, drive_id, file_id, content_hash, parent_id, title, author, tags,
	duration_seconds, size_bytes, ext, quality, thumbnail_url, preview_file_id,
	preview_local, preview_status, views, favorites, comments, likes, dislikes,
	category, hidden, tags_manual, badges, description, published_at, created_at, updated_at
) VALUES (
	'legacy-video', 'drive', 'file-legacy', 'hash-legacy', 'parent-1', 'Legacy Video', 'Legacy Author', '["旧标签"]',
	180, 1024, 'mp4', 'HD', '/thumb.jpg', 'preview-file',
	'/preview.mp4', 'ready', 7, 1, 2, 3, 4,
	'legacy-category', 0, 0, '["精选"]', 'legacy description', ?, ?, ?
)`,
		nowMillis, nowMillis, nowMillis); err != nil {
		t.Fatalf("insert legacy video: %v", err)
	}
	if _, err := db.Exec(`CREATE INDEX idx_legacy_videos_category ON videos(category)`); err != nil {
		t.Fatalf("create legacy category index: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	var fileNameDefault string
	if err := cat.db.QueryRow(`SELECT COALESCE(file_name, '') FROM videos LIMIT 1`).Scan(&fileNameDefault); err != nil && err != sql.ErrNoRows {
		t.Fatalf("query migrated file_name column: %v", err)
	}
	if fileNameDefault != "" {
		t.Fatalf("file_name default = %q, want empty", fileNameDefault)
	}
	if hasColumn(t, cat, "videos", "category") {
		t.Fatal("legacy category column was not dropped")
	}
	if hasColumn(t, cat, "videos", "quality") {
		t.Fatal("retired quality column was not dropped")
	}
	if indexExists(t, cat, "idx_legacy_videos_category") {
		t.Fatal("legacy category index was not dropped")
	}
	for _, index := range []string{"idx_videos_drive", "idx_videos_pub", "idx_videos_views"} {
		if !indexExists(t, cat, index) {
			t.Fatalf("base video index %s was not recreated", index)
		}
	}

	ctx := context.Background()
	got, err := cat.GetVideo(ctx, "legacy-video")
	if err != nil {
		t.Fatalf("get migrated legacy video: %v", err)
	}
	if got.Title != "Legacy Video" || got.Author != "Legacy Author" || got.Views != 7 {
		t.Fatalf("migrated video lost data: %#v", got)
	}
	if got.ThumbnailUpdatedAt.UnixMilli() != nowMillis {
		t.Fatalf("migrated thumbnail revision = %d, want %d", got.ThumbnailUpdatedAt.UnixMilli(), nowMillis)
	}
	if len(got.Tags) != 0 {
		t.Fatalf("migrated video tags = %#v, want none", got.Tags)
	}

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "new-video",
		DriveID:     "drive",
		FileID:      "file-new",
		Title:       "New Video",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("upsert after migration: %v", err)
	}
}

func TestSetManualVideoTagsRejectsUnknownLabels(t *testing.T) {
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
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "普通标题",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	if err := cat.SetManualVideoTags(ctx, "video-1", []string{"不存在"}); err == nil {
		t.Fatal("SetManualVideoTags accepted an unknown tag label")
	}
}

func TestSetAutoVideoTagsDoesNotOverwriteManualVideoTags(t *testing.T) {
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
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "清纯后入",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if _, err := cat.CreateTagAndClassify(ctx, "清纯", nil, "user"); err != nil {
		t.Fatalf("create user tag: %v", err)
	}
	if err := cat.SetManualVideoTags(ctx, "video-1", []string{"清纯"}); err != nil {
		t.Fatalf("set manual tags: %v", err)
	}

	if err := cat.SetAutoVideoTags(ctx, "video-1", []string{"后入"}); err != nil {
		t.Fatalf("set auto tags: %v", err)
	}

	got, err := cat.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if !sameStrings(got.Tags, []string{"清纯"}) {
		t.Fatalf("tags = %#v, want manual 清纯 only", got.Tags)
	}
}

func TestCreateTagAndClassifyMapsAVCodeLabelToAV(t *testing.T) {
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

	if _, err := cat.CreateTagAndClassify(ctx, "SSNI-001", nil, "user"); err != nil {
		t.Fatalf("create code tag: %v", err)
	}

	tags, err := cat.ListTags(ctx)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	for _, tag := range tags {
		if tag.Label == "SSNI-001" {
			t.Fatal("created standalone AV code tag SSNI-001")
		}
		if tag.Label == "AV" && tag.Source != "builtin" {
			t.Fatalf("AV source = %q, want builtin", tag.Source)
		}
	}
}

func TestAVTagUsesCodeRuleNotLegacyAliases(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	aliasesJSON, _ := json.Marshal([]string{"JAV", "番号", "番號", "custom-av-alias"})
	now := time.Now().UnixMilli()
	if _, err := db.ExecContext(ctx, `
INSERT INTO tags (label, aliases, match_rules, source, origin, created_at, updated_at)
VALUES ('AV', ?, '{}', 'generated', '', ?, ?)`, string(aliasesJSON), now, now); err != nil {
		t.Fatalf("seed legacy AV tag shape: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO videos (id, drive_id, file_id, title, file_name, tags, tags_manual, published_at, created_at, updated_at)
VALUES ('video-av-code', 'drive', 'file-av-code', 'SSNI-001', 'SSNI-001.mp4', '[]', 0, ?, ?, ?)`,
		now, now, now); err != nil {
		t.Fatalf("seed AV-code video: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	if err := cat.RunPostStartupTagMaintenance(ctx); err != nil {
		t.Fatalf("post-startup tag maintenance: %v", err)
	}

	tag, err := cat.getTagByLabel(ctx, "AV")
	if err != nil {
		t.Fatalf("get AV tag: %v", err)
	}
	if len(tag.Aliases) != 0 {
		t.Fatalf("AV aliases = %#v, want legacy aliases removed", tag.Aliases)
	}
	if tag.Source != "builtin" {
		t.Fatalf("AV source = %q, want builtin", tag.Source)
	}
	if !tag.MatchRules.MatchAVCode {
		t.Fatalf("AV match rules = %#v, want MatchAVCode", tag.MatchRules)
	}
	for _, text := range []string{"JAV合集", "无码高清番号", "番號整理", "经典 AV 合集", "custom-av-alias"} {
		got, err := cat.MatchTags(ctx, text)
		if err != nil {
			t.Fatalf("match %q: %v", text, err)
		}
		if len(got) != 0 {
			t.Fatalf("MatchTags(%q) = %#v, want no AV alias match", text, got)
		}
	}
	for _, text := range []string{"SSNI-001.mp4", "FC2PPV-4768873.mp4"} {
		got, err := cat.MatchTags(ctx, text)
		if err != nil {
			t.Fatalf("match %q: %v", text, err)
		}
		if !sameStrings(got, []string{"AV"}) {
			t.Fatalf("MatchTags(%q) = %#v, want [AV]", text, got)
		}
	}
}

func TestAVTagPrefixesAreEditable(t *testing.T) {
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

	if got, err := cat.MatchTags(ctx, "SSNI-001.mp4"); err != nil {
		t.Fatalf("match default AV: %v", err)
	} else if !sameStrings(got, []string{"AV"}) {
		t.Fatalf("default MatchTags(SSNI) = %#v, want AV", got)
	}
	av := mustTagByLabel(t, ctx, cat, "AV")
	prefixes := make([]string, 0, len(av.MatchRules.AVCodePrefixes)+1)
	for _, prefix := range av.MatchRules.AVCodePrefixes {
		if prefix == "OBA" {
			continue
		}
		prefixes = append(prefixes, prefix)
	}
	prefixes = append(prefixes, "FHD")
	if _, err := cat.UpdateTag(ctx, av.ID, tagging.Rule{MatchAVCode: true, AVCodePrefixes: prefixes}); err != nil {
		t.Fatalf("update AV prefixes: %v", err)
	}

	got, err := cat.MatchTags(ctx, "FHD-78824.mp4")
	if err != nil {
		t.Fatalf("match custom AV: %v", err)
	}
	if !sameStrings(got, []string{"AV"}) {
		t.Fatalf("custom MatchTags(FHD) = %#v, want AV", got)
	}
	assignments, err := cat.MatchTagAssignments(ctx, "FHD-78824", "FHD-78824.mp4", "", "")
	if err != nil {
		t.Fatalf("match custom AV assignments: %v", err)
	}
	if !sameStrings(assignmentLabels(assignments), []string{"AV", "FHD"}) {
		t.Fatalf("custom AV assignments = %#v, want AV + FHD", assignments)
	}

	got, err = cat.MatchTags(ctx, "OBA-334456.mp4")
	if err != nil {
		t.Fatalf("match removed AV prefix: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("removed-prefix MatchTags(OBA) = %#v, want none", got)
	}
	assignments, err = cat.MatchTagAssignments(ctx, "OBA-334456", "OBA-334456.mp4", "", "")
	if err != nil {
		t.Fatalf("match removed AV assignments: %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("removed-prefix assignments = %#v, want none", assignments)
	}

	got, err = cat.MatchTags(ctx, "SSNI-001.mp4")
	if err != nil {
		t.Fatalf("match retained AV prefix: %v", err)
	}
	if !sameStrings(got, []string{"AV"}) {
		t.Fatalf("retained-prefix MatchTags(SSNI) = %#v, want AV", got)
	}

	listedAV := mustTagByLabel(t, ctx, cat, "AV")
	if !listedAV.MatchRules.MatchAVCode {
		t.Fatalf("listed AV match rule = %#v, want MatchAVCode", listedAV.MatchRules)
	}
	for _, prefix := range []string{"SSNI", "FC2PPV", "FHD"} {
		if !stringSliceContains(listedAV.MatchRules.AVCodePrefixes, prefix) {
			t.Fatalf("listed AV prefixes missing %q: %#v", prefix, listedAV.MatchRules.AVCodePrefixes)
		}
	}
	if stringSliceContains(listedAV.MatchRules.AVCodePrefixes, "OBA") {
		t.Fatalf("listed AV prefixes still include removed OBA: %#v", listedAV.MatchRules.AVCodePrefixes)
	}
	if len(listedAV.Aliases) != 0 {
		t.Fatalf("listed AV aliases = %#v, want prefixes stored in match_rules", listedAV.Aliases)
	}

	if err := cat.RunPostStartupTagMaintenance(ctx); err != nil {
		t.Fatalf("post-startup maintenance after AV prefix edit: %v", err)
	}
	listedAV = mustTagByLabel(t, ctx, cat, "AV")
	if stringSliceContains(listedAV.MatchRules.AVCodePrefixes, "OBA") {
		t.Fatalf("post-startup AV prefixes restored removed OBA: %#v", listedAV.MatchRules.AVCodePrefixes)
	}
	if !stringSliceContains(listedAV.MatchRules.AVCodePrefixes, "FHD") {
		t.Fatalf("post-startup AV prefixes missing custom FHD: %#v", listedAV.MatchRules.AVCodePrefixes)
	}
}

func TestMigrateCollapsesAVCodeTagsIntoAV(t *testing.T) {
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
	for _, seed := range []struct {
		id    string
		label string
	}{
		{id: "video-1", label: "SSNI-001"},
		{id: "video-2", label: "IPX-778-FHD(1)"},
		{id: "video-3", label: "[44x.me]MIMK-786"},
		{id: "video-4", label: "FC2PPV-4162750"},
	} {
		if err := cat.UpsertVideo(ctx, &Video{
			ID:          seed.id,
			DriveID:     "drive",
			FileID:      seed.id,
			Title:       seed.label + " sample",
			Tags:        []string{seed.label},
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("seed polluted video %s: %v", seed.label, err)
		}
	}

	if err := cat.RunPostStartupTagMaintenance(ctx); err != nil {
		t.Fatalf("post-startup tag maintenance: %v", err)
	}

	tags, err := cat.ListTags(ctx)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	var sawAV bool
	polluted := map[string]bool{}
	for _, tag := range tags {
		if tag.Label == "AV" {
			sawAV = true
			if tag.Source != "builtin" {
				t.Fatalf("AV source = %q, want builtin", tag.Source)
			}
		}
		if tag.Label != "AV" && isAVCodePollutedLabel(tag.Label) {
			polluted[tag.Label] = true
		}
	}
	if !sawAV {
		t.Fatal("AV tag was not present")
	}
	if len(polluted) > 0 {
		t.Fatalf("AV code tags were not removed: %#v", polluted)
	}

	for _, id := range []string{"video-1", "video-2", "video-3", "video-4"} {
		got, err := cat.GetVideo(ctx, id)
		if err != nil {
			t.Fatalf("get video %s: %v", id, err)
		}
		if !sameStrings(got.Tags, []string{"AV"}) {
			t.Fatalf("%s tags = %#v, want AV", id, got.Tags)
		}
	}
}

func TestMigrateClearsRemoteThumbnailURLs(t *testing.T) {
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
	if err := cat.UpsertDrive(ctx, &Drive{
		ID:        "onedrive-main",
		Kind:      "onedrive",
		Name:      "OneDrive",
		RootID:    "root",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed onedrive: %v", err)
	}
	if err := cat.UpsertDrive(ctx, &Drive{
		ID:        "p123-main",
		Kind:      "p123",
		Name:      "123Pan",
		RootID:    "root",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed p123: %v", err)
	}
	if err := cat.UpsertDrive(ctx, &Drive{
		ID:        "pikpak-main",
		Kind:      "pikpak",
		Name:      "PikPak",
		RootID:    "root",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed pikpak: %v", err)
	}
	if err := cat.UpsertDrive(ctx, &Drive{
		ID:        "crawler-main",
		Kind:      "scriptcrawler",
		Name:      "Crawler",
		RootID:    "/",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed crawler: %v", err)
	}

	videos := []*Video{
		{
			ID:           "onedrive-video",
			DriveID:      "onedrive-main",
			FileID:       "file-1",
			Title:        "OneDrive",
			ThumbnailURL: "https://westus21-mediap.svc.ms/transform/thumbnail?provider=spo&tempauth=expired",
		},
		{
			ID:           "local-thumb-video",
			DriveID:      "onedrive-main",
			FileID:       "file-2",
			Title:        "Local thumb",
			ThumbnailURL: "/p/thumb/local-thumb-video",
		},
		{
			ID:           "pikpak-video",
			DriveID:      "pikpak-main",
			FileID:       "file-3",
			Title:        "PikPak",
			ThumbnailURL: "https://sg-thumbnail-drive.mypikpak.net/v0/screenshot-thumbnails/demo",
		},
		{
			ID:           "p123-remote-thumb-video",
			DriveID:      "p123-main",
			FileID:       "file-4",
			Title:        "123Pan remote thumb",
			ThumbnailURL: "https://download.123pan.com/thumb/file_70_70?w=70&h=70",
		},
		{
			ID:           "p123-local-thumb-video",
			DriveID:      "p123-main",
			FileID:       "file-5",
			Title:        "123Pan local thumb",
			ThumbnailURL: "/p/thumb/p123-local-thumb-video",
		},
		{
			ID:           "scriptcrawler-crawler-main-local-thumb",
			DriveID:      "crawler-main",
			FileID:       "file-6",
			Title:        "Crawler local thumb",
			ThumbnailURL: "/p/thumb/scriptcrawler-crawler-main-local-thumb",
		},
		{
			ID:           "scriptcrawler-crawler-main-remote-thumb",
			DriveID:      "crawler-main",
			FileID:       "file-7",
			Title:        "Crawler remote thumb",
			ThumbnailURL: "https://example.invalid/crawler-thumb.jpg",
		},
	}
	for _, v := range videos {
		v.PublishedAt = now
		v.CreatedAt = now
		v.UpdatedAt = now
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	if err := cat.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	got, err := cat.GetVideo(ctx, "onedrive-video")
	if err != nil {
		t.Fatalf("get onedrive video: %v", err)
	}
	if got.ThumbnailURL != "" {
		t.Fatalf("onedrive thumbnail = %q, want cleared", got.ThumbnailURL)
	}

	local, err := cat.GetVideo(ctx, "local-thumb-video")
	if err != nil {
		t.Fatalf("get local thumb video: %v", err)
	}
	if local.ThumbnailURL != "/p/thumb/local-thumb-video" {
		t.Fatalf("local thumbnail = %q, want preserved", local.ThumbnailURL)
	}

	pikpak, err := cat.GetVideo(ctx, "pikpak-video")
	if err != nil {
		t.Fatalf("get pikpak video: %v", err)
	}
	if pikpak.ThumbnailURL != "" {
		t.Fatalf("pikpak thumbnail = %q, want cleared", pikpak.ThumbnailURL)
	}

	p123Remote, err := cat.GetVideo(ctx, "p123-remote-thumb-video")
	if err != nil {
		t.Fatalf("get p123 remote thumb video: %v", err)
	}
	if p123Remote.ThumbnailURL != "" {
		t.Fatalf("p123 remote thumbnail = %q, want cleared", p123Remote.ThumbnailURL)
	}
	var p123Status string
	if err := cat.db.QueryRowContext(ctx, `SELECT thumbnail_status FROM videos WHERE id = ?`, "p123-remote-thumb-video").Scan(&p123Status); err != nil {
		t.Fatalf("read p123 thumbnail status: %v", err)
	}
	if p123Status != "pending" {
		t.Fatalf("p123 remote thumbnail_status = %q, want pending", p123Status)
	}

	p123Local, err := cat.GetVideo(ctx, "p123-local-thumb-video")
	if err != nil {
		t.Fatalf("get p123 local thumb video: %v", err)
	}
	if p123Local.ThumbnailURL != "/p/thumb/p123-local-thumb-video" {
		t.Fatalf("p123 local thumbnail = %q, want preserved", p123Local.ThumbnailURL)
	}

	crawlerLocal, err := cat.GetVideo(ctx, "scriptcrawler-crawler-main-local-thumb")
	if err != nil {
		t.Fatalf("get crawler local thumb video: %v", err)
	}
	if crawlerLocal.ThumbnailURL != "/p/thumb/scriptcrawler-crawler-main-local-thumb" {
		t.Fatalf("crawler local thumbnail = %q, want preserved", crawlerLocal.ThumbnailURL)
	}

	crawlerRemote, err := cat.GetVideo(ctx, "scriptcrawler-crawler-main-remote-thumb")
	if err != nil {
		t.Fatalf("get crawler remote thumb video: %v", err)
	}
	if crawlerRemote.ThumbnailURL != "" {
		t.Fatalf("crawler remote thumbnail = %q, want cleared", crawlerRemote.ThumbnailURL)
	}
}

func TestMigrateHidesZeroSizeVideosForKnownDrives(t *testing.T) {
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
	if err := cat.UpsertDrive(ctx, &Drive{
		ID:        "drive-main",
		Kind:      "onedrive",
		Name:      "OneDrive",
		RootID:    "root",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
	for _, v := range []*Video{
		{ID: "empty-video", DriveID: "drive-main", FileID: "file-1", Title: "Empty", Size: 0},
		{ID: "normal-video", DriveID: "drive-main", FileID: "file-2", Title: "Normal", Size: 123},
		{ID: "orphan-empty-video", DriveID: "unknown-drive", FileID: "file-3", Title: "Orphan", Size: 0},
	} {
		v.PublishedAt = now
		v.CreatedAt = now
		v.UpdatedAt = now
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	if err := cat.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	empty, err := cat.GetVideo(ctx, "empty-video")
	if err != nil {
		t.Fatalf("get empty video: %v", err)
	}
	if !empty.Hidden {
		t.Fatal("empty video was not hidden")
	}

	normal, err := cat.GetVideo(ctx, "normal-video")
	if err != nil {
		t.Fatalf("get normal video: %v", err)
	}
	if normal.Hidden {
		t.Fatal("normal video was hidden")
	}

	orphan, err := cat.GetVideo(ctx, "orphan-empty-video")
	if err != nil {
		t.Fatalf("get orphan empty video: %v", err)
	}
	if orphan.Hidden {
		t.Fatal("orphan empty video without a known drive was hidden")
	}
}

func TestListVideosHidesDuplicateContentHashes(t *testing.T) {
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
	for _, v := range []*Video{
		{
			ID:          "video-1",
			DriveID:     "drive",
			FileID:      "file-1",
			Title:       "First",
			ContentHash: "hash-same",
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
		{
			ID:          "video-2",
			DriveID:     "drive",
			FileID:      "file-2",
			Title:       "Second",
			ContentHash: "hash-same",
			PublishedAt: now.Add(time.Second),
			CreatedAt:   now.Add(time.Second),
			UpdatedAt:   now.Add(time.Second),
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	items, total, err := cat.ListVideos(ctx, ListParams{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("list videos: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("visible videos total=%d len=%d, want 1", total, len(items))
	}
	if items[0].ID != "video-1" {
		t.Fatalf("visible id = %q, want video-1", items[0].ID)
	}
}

func TestTagFilterMatchesCanonicalDuplicateVideo(t *testing.T) {
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
	for _, v := range []*Video{
		{
			ID:          "pikpak-canonical",
			DriveID:     "pikpak",
			FileID:      "canonical.mp4",
			Title:       "Canonical",
			Size:        1024,
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
		{
			ID:          "scriptcrawler-crawler-a-dup-1",
			DriveID:     "crawler-a",
			FileID:      "dup-1.mp4",
			Title:       "Crawler duplicate 1",
			Tags:        []string{"crawler-tag"},
			Size:        1024,
			PublishedAt: now.Add(time.Second),
			CreatedAt:   now.Add(time.Second),
			UpdatedAt:   now.Add(time.Second),
		},
		{
			ID:          "scriptcrawler-crawler-a-dup-2",
			DriveID:     "crawler-a",
			FileID:      "dup-2.mp4",
			Title:       "Crawler duplicate 2",
			Tags:        []string{"crawler-tag"},
			Size:        1024,
			PublishedAt: now.Add(2 * time.Second),
			CreatedAt:   now.Add(2 * time.Second),
			UpdatedAt:   now.Add(2 * time.Second),
		},
		{
			ID:          "scriptcrawler-crawler-a-visible",
			DriveID:     "crawler-a",
			FileID:      "visible.mp4",
			Title:       "Crawler visible",
			Tags:        []string{"crawler-tag"},
			Size:        2048,
			PublishedAt: now.Add(3 * time.Second),
			CreatedAt:   now.Add(3 * time.Second),
			UpdatedAt:   now.Add(3 * time.Second),
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed %s: %v", v.ID, err)
		}
	}
	for _, id := range []string{"pikpak-canonical", "scriptcrawler-crawler-a-dup-1", "scriptcrawler-crawler-a-dup-2"} {
		if err := cat.UpdateVideoFingerprint(ctx, id, "same-sampled-sha256", "ready", ""); err != nil {
			t.Fatalf("fingerprint %s: %v", id, err)
		}
	}
	if err := cat.UpdateVideoFingerprint(ctx, "scriptcrawler-crawler-a-visible", "unique-sampled-sha256", "ready", ""); err != nil {
		t.Fatalf("fingerprint visible: %v", err)
	}

	items, total, err := cat.ListVideos(ctx, ListParams{Tag: "crawler-tag", Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("list videos by tag: %v", err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("tagged videos total=%d len=%d, want 2", total, len(items))
	}
	gotIDs := map[string]bool{}
	for _, item := range items {
		gotIDs[item.ID] = true
	}
	for _, want := range []string{"pikpak-canonical", "scriptcrawler-crawler-a-visible"} {
		if !gotIDs[want] {
			t.Fatalf("tagged video ids = %#v, want %s", gotIDs, want)
		}
	}
	if got := mustTagByLabel(t, ctx, cat, "crawler-tag").Count; got != 2 {
		t.Fatalf("crawler-tag count = %d, want 2 visible canonical videos", got)
	}
}

func TestListVideosCanFilterReadyThumbnails(t *testing.T) {
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
	for _, v := range []*Video{
		{
			ID:           "ready-video",
			DriveID:      "drive",
			FileID:       "file-ready",
			Title:        "Ready",
			ThumbnailURL: "/p/thumb/ready-video",
			PublishedAt:  now,
			CreatedAt:    now,
			UpdatedAt:    now,
		},
		{
			ID:          "pending-video",
			DriveID:     "drive",
			FileID:      "file-pending",
			Title:       "Pending",
			PublishedAt: now.Add(time.Second),
			CreatedAt:   now.Add(time.Second),
			UpdatedAt:   now.Add(time.Second),
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	items, total, err := cat.ListVideos(ctx, ListParams{
		Page: 1, PageSize: 10, ThumbnailReadyOnly: true,
	})
	if err != nil {
		t.Fatalf("list videos: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("ready videos total=%d len=%d, want 1", total, len(items))
	}
	if items[0].ID != "ready-video" {
		t.Fatalf("ready video id = %q, want ready-video", items[0].ID)
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringSliceContains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func mustListTags(t *testing.T, ctx context.Context, cat *Catalog) []Tag {
	t.Helper()
	tags, err := cat.ListTags(ctx)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	return tags
}

func mustTagByLabel(t *testing.T, ctx context.Context, cat *Catalog, label string) Tag {
	t.Helper()
	for _, tag := range mustListTags(t, ctx, cat) {
		if tag.Label == label {
			return tag
		}
	}
	t.Fatalf("tag %q not found", label)
	return Tag{}
}

func hasColumn(t *testing.T, cat *Catalog, table, column string) bool {
	t.Helper()
	rows, err := cat.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("query table info for %s: %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan table info for %s: %v", table, err)
		}
		if strings.EqualFold(name, column) {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table info for %s: %v", table, err)
	}
	return false
}

func indexExists(t *testing.T, cat *Catalog, name string) bool {
	t.Helper()
	var count int
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = ?`, name).Scan(&count); err != nil {
		t.Fatalf("query index %s: %v", name, err)
	}
	return count > 0
}

func videoUpdatedAtByID(t *testing.T, ctx context.Context, cat *Catalog, ids ...string) map[string]int64 {
	t.Helper()
	out := make(map[string]int64, len(ids))
	for _, id := range ids {
		var updatedAt int64
		if err := cat.db.QueryRowContext(ctx, `SELECT updated_at FROM videos WHERE id = ?`, id).Scan(&updatedAt); err != nil {
			t.Fatalf("read updated_at for %s: %v", id, err)
		}
		out[id] = updatedAt
	}
	return out
}

// 删除旧版本 collection 标签的最后一个引用视频后，标签应当自动从 tags 表里消失。
// user/builtin 标签不受影响：自定义/内置标签的语义由人维护，孤儿状态保留。
func TestDeleteVideoPrunesLegacyOrphanCollectionTag(t *testing.T) {
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
	for _, id := range []string{"video-a", "video-b"} {
		if err := cat.UpsertVideo(ctx, &Video{
			ID:          id,
			DriveID:     "drive",
			FileID:      id,
			Title:       id,
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	nowMillis := now.UnixMilli()
	if _, err := cat.db.ExecContext(ctx,
		`INSERT INTO tags (label, aliases, source, created_at, updated_at) VALUES (?, '[]', 'collection', ?, ?)`,
		"Better Call Saul S02", nowMillis, nowMillis); err != nil {
		t.Fatalf("insert legacy collection tag: %v", err)
	}
	var collectionTagID int64
	if err := cat.db.QueryRowContext(ctx, `SELECT id FROM tags WHERE label = ?`, "Better Call Saul S02").Scan(&collectionTagID); err != nil {
		t.Fatalf("lookup legacy collection tag: %v", err)
	}
	for _, id := range []string{"video-a", "video-b"} {
		if _, err := cat.db.ExecContext(ctx,
			`INSERT INTO video_tags (video_id, tag_id, source, created_at) VALUES (?, ?, 'auto', ?)`,
			id, collectionTagID, nowMillis); err != nil {
			t.Fatalf("attach legacy collection tag to %s: %v", id, err)
		}
	}

	if _, err := cat.db.ExecContext(ctx,
		`INSERT INTO tags (label, aliases, source, created_at, updated_at) VALUES (?, '[]', 'user', ?, ?)`,
		"用户标签", nowMillis, nowMillis); err != nil {
		t.Fatalf("insert user orphan tag: %v", err)
	}

	collectionExists := func() bool {
		var n int
		if err := cat.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tags WHERE label = ? AND source = 'collection'`,
			"Better Call Saul S02").Scan(&n); err != nil {
			t.Fatalf("count collection tag: %v", err)
		}
		return n > 0
	}
	if !collectionExists() {
		t.Fatal("collection tag missing right after creation")
	}

	// 删第一个视频：还有 video-b 在引用旧 collection 标签，应保留。
	if err := cat.DeleteVideo(ctx, "video-a"); err != nil {
		t.Fatalf("delete video-a: %v", err)
	}
	if !collectionExists() {
		t.Fatal("collection tag was pruned while another video still references it")
	}

	// 删最后一个引用视频，旧 collection 标签应当被同步清掉。
	if err := cat.DeleteVideo(ctx, "video-b"); err != nil {
		t.Fatalf("delete video-b: %v", err)
	}
	if collectionExists() {
		t.Fatal("orphan collection tag was not pruned after deleting the last referencing video")
	}

	// 用户标签即使是孤儿也必须保留。
	var userCount int
	if err := cat.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tags WHERE label = ? AND source = 'user'`,
		"用户标签").Scan(&userCount); err != nil {
		t.Fatalf("count user tag: %v", err)
	}
	if userCount != 1 {
		t.Fatalf("user tag count = %d, want 1 (user-source orphans must be preserved)", userCount)
	}

	// 当前内置标签即使零引用也不能被孤儿清理影响。
	var builtinCount int
	if err := cat.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tags WHERE label = '奶子' AND source = 'builtin'`).Scan(&builtinCount); err != nil {
		t.Fatalf("count builtin tag: %v", err)
	}
	if builtinCount != 1 {
		t.Fatalf("builtin tag count = %d, want 1", builtinCount)
	}
}

func TestMigrateKeepsUserTagsAndRemovesOrdinaryGeneratedSources(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	now := time.Now().UnixMilli()
	sources := []string{"system", "builtin", "user", "series", "crawler", "legacy", "collection", "generated", "unknown"}
	for _, oldSource := range sources {
		label := "source-" + oldSource
		if _, err := cat.db.ExecContext(ctx,
			`INSERT INTO tags (label, aliases, source, created_at, updated_at) VALUES (?, '[]', ?, ?, ?)`,
			label, oldSource, now, now); err != nil {
			t.Fatalf("insert %s tag: %v", oldSource, err)
		}
	}
	if err := cat.Close(); err != nil {
		t.Fatalf("close catalog: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen catalog: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	var userSource string
	if err := reopened.db.QueryRowContext(ctx,
		`SELECT source FROM tags WHERE label = 'source-user'`).Scan(&userSource); err != nil {
		t.Fatalf("read user tag source: %v", err)
	}
	if userSource != "user" {
		t.Fatalf("user tag source = %q, want user", userSource)
	}
	for _, oldSource := range sources {
		if oldSource == "user" {
			continue
		}
		var count int
		if err := reopened.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tags WHERE label = ?`, "source-"+oldSource).Scan(&count); err != nil {
			t.Fatalf("count %s tag: %v", oldSource, err)
		}
		if count != 0 {
			t.Errorf("%s tag was retained, want removed", oldSource)
		}
	}
}

func TestMigrateKeepsOnlyCurrentBuiltinLabels(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "video-butt",
		DriveID:     "drive",
		FileID:      "file-butt",
		Title:       "蜜桃臀",
		Tags:        []string{"臀"},
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed old builtin video: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx, `UPDATE tags SET source = 'builtin' WHERE label = '臀'`); err != nil {
		t.Fatalf("mark old butt tag builtin: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx,
		`INSERT INTO tags (label, aliases, source, created_at, updated_at) VALUES ('丝袜', '[]', 'builtin', ?, ?)`,
		time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed retired builtin: %v", err)
	}
	if err := cat.Close(); err != nil {
		t.Fatalf("close catalog: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen catalog: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	var badBuiltinCount int
	if err := reopened.db.QueryRowContext(ctx, `
SELECT COUNT(*)
  FROM tags
 WHERE source = 'builtin'
   AND label COLLATE NOCASE NOT IN ('AV', '奶子', '女大', '人妻', '后入', '制服', '美臀', '口交')`).Scan(&badBuiltinCount); err != nil {
		t.Fatalf("count retired builtins: %v", err)
	}
	if badBuiltinCount != 0 {
		t.Fatalf("retired builtin count = %d, want 0", badBuiltinCount)
	}
	if _, err := reopened.getTagByLabel(ctx, "臀"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old 臀 tag still exists: %v", err)
	}
	tag := mustTagByLabel(t, ctx, reopened, "美臀")
	if tag.Source != "builtin" {
		t.Fatalf("美臀 source = %q, want builtin", tag.Source)
	}
	if _, err := reopened.getTagByLabel(ctx, "丝袜"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("retired builtin tag still exists: %v", err)
	}
	video, err := reopened.GetVideo(ctx, "video-butt")
	if err != nil {
		t.Fatalf("get migrated video: %v", err)
	}
	if !sameStrings(video.Tags, []string{"美臀"}) {
		t.Fatalf("video tags = %#v, want 美臀", video.Tags)
	}
}

// 监听完成后的后台维护应当清掉历史遗留的孤儿自动生成标签。
func TestPostStartupMaintenancePrunesPreexistingOrphanGeneratedTags(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}

	// 直接往 tags 表里写两条 collection 行：一条没有任何 video_tags 关联（孤儿），另一条人为关联视频（非孤儿）。
	now := time.Now().UnixMilli()
	if _, err := cat.db.ExecContext(ctx,
		`INSERT INTO tags (label, aliases, source, created_at, updated_at) VALUES (?, '[]', 'collection', ?, ?)`,
		"孤儿合集", now, now); err != nil {
		t.Fatalf("insert orphan tag: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx,
		`INSERT INTO tags (label, aliases, source, created_at, updated_at) VALUES (?, '[]', 'collection', ?, ?)`,
		"在用合集", now, now); err != nil {
		t.Fatalf("insert in-use tag: %v", err)
	}
	var inUseTagID int64
	if err := cat.db.QueryRowContext(ctx, `SELECT id FROM tags WHERE label = ?`, "在用合集").Scan(&inUseTagID); err != nil {
		t.Fatalf("lookup in-use tag id: %v", err)
	}
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "video-keeper",
		DriveID:     "drive",
		FileID:      "file-keeper",
		Title:       "keeper",
		PublishedAt: time.Now(),
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("seed keeper video: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx,
		`INSERT INTO video_tags (video_id, tag_id, source, created_at) VALUES (?, ?, 'auto', ?)`,
		"video-keeper", inUseTagID, now); err != nil {
		t.Fatalf("attach in-use tag: %v", err)
	}

	// 同样写一个 user 来源的孤儿，验证 migrate 不会误删 user 孤儿。
	if _, err := cat.db.ExecContext(ctx,
		`INSERT INTO tags (label, aliases, source, created_at, updated_at) VALUES (?, '[]', 'user', ?, ?)`,
		"用户孤儿", now, now); err != nil {
		t.Fatalf("insert user orphan: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx,
		`INSERT INTO tags (label, aliases, source, origin, created_at, updated_at) VALUES (?, '[]', 'generated', 'crawler', ?, ?)`,
		"空爬虫", now, now); err != nil {
		t.Fatalf("insert crawler orphan: %v", err)
	}

	if err := cat.Close(); err != nil {
		t.Fatalf("close before reopen: %v", err)
	}

	// 重新打开不会扫描标签；监听完成后的后台维护才清理孤儿合集。
	cat2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat2.Close() })
	if err := cat2.RunPostStartupTagMaintenance(ctx); err != nil {
		t.Fatalf("post-startup tag maintenance: %v", err)
	}

	count := func(label string) int {
		var n int
		if err := cat2.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tags WHERE label = ?`, label).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", label, err)
		}
		return n
	}
	if count("孤儿合集") != 0 {
		t.Fatal("post-startup maintenance did not prune orphan generated tag")
	}
	if count("在用合集") != 0 {
		t.Fatal("post-startup maintenance did not prune in-use ordinary generated tag")
	}
	if count("用户孤儿") != 1 {
		t.Fatal("post-startup maintenance wrongly pruned user-source orphan tag")
	}
	if count("空爬虫") != 0 {
		t.Fatal("post-startup maintenance did not prune orphan crawler tag")
	}
	video, err := cat2.GetVideo(ctx, "video-keeper")
	if err != nil {
		t.Fatalf("get keeper video: %v", err)
	}
	if len(video.Tags) != 0 {
		t.Fatalf("keeper video tags = %#v, want none", video.Tags)
	}
}

// TestReconcileThumbnailStatusOnce 检查升级时的"url 已写但 status=pending"修复。
// catalog.Open 会自动跑这个 migration（调用链 Open→ensureSchema→reconcileThumbnailStatusOnce）。
// 因此这里通过手动写脏数据 + 直接调 reconcile 来验证；脏数据是"绕过 Open
// 默认运行的 migration"的近似 —— 写完后状态就和遗留库一样。
func TestReconcileThumbnailStatusOnce(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })

	now := time.Now()
	cases := []struct {
		id, url, status string
		wantStatus      string
	}{
		{"v-pending-url", "/p/thumb/v-pending-url", "pending", "ready"},             // 主要修复目标
		{"v-empty-url-pending", "", "pending", "pending"},                           // 没 url 不动
		{"v-failed-with-url", "/p/thumb/v-failed-with-url", "failed", "failed"},     // 显式失败保留
		{"v-empty-url-failed", "", "failed", "failed"},                              // 失败 + 没 url 也保留
		{"v-skipped-with-url", "/p/thumb/v-skipped-with-url", "skipped", "skipped"}, // 已跳过的时长补全保留
		{"v-already-ready", "/p/thumb/v-already-ready", "ready", "ready"},           // 幂等
	}
	for _, c := range cases {
		if err := cat.UpsertVideo(ctx, &Video{
			ID: c.id, DriveID: "d", FileID: "f-" + c.id,
			Title: c.id, ThumbnailURL: c.url,
			PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", c.id, err)
		}
		// UpsertVideo 默认把 thumbnail_status 留给 schema DEFAULT 'pending'。
		// 显式覆盖成测试想要的状态。
		if _, err := cat.db.ExecContext(ctx,
			`UPDATE videos SET thumbnail_status = ? WHERE id = ?`, c.status, c.id); err != nil {
			t.Fatalf("force seed status %s: %v", c.id, err)
		}
	}
	// 抹掉 Open 自动跑过的 marker，让我们直接验证函数本体
	if err := cat.SetSetting(ctx, "videos.thumbnail_status.url_present_to_ready_migrated", ""); err != nil {
		t.Fatalf("clear marker: %v", err)
	}

	if err := cat.reconcileThumbnailStatusOnce(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, c := range cases {
		var got string
		if err := cat.db.QueryRowContext(ctx,
			`SELECT thumbnail_status FROM videos WHERE id = ?`, c.id).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", c.id, err)
		}
		if got != c.wantStatus {
			t.Errorf("%s: status = %q, want %q", c.id, got, c.wantStatus)
		}
	}

	// 二次调用应是 no-op（marker 已写）
	// 为验证：人为再插一条脏数据，第二次 reconcile 不应碰它
	if err := cat.UpsertVideo(ctx, &Video{
		ID: "v-second-call", DriveID: "d", FileID: "f-2nd",
		Title: "second", ThumbnailURL: "/p/thumb/v-second-call",
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed second-call: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx,
		`UPDATE videos SET thumbnail_status='pending' WHERE id='v-second-call'`); err != nil {
		t.Fatalf("force-pending second-call: %v", err)
	}
	if err := cat.reconcileThumbnailStatusOnce(ctx); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	var status string
	if err := cat.db.QueryRowContext(ctx,
		`SELECT thumbnail_status FROM videos WHERE id='v-second-call'`).Scan(&status); err != nil {
		t.Fatalf("read second-call: %v", err)
	}
	if status != "pending" {
		t.Errorf("second-call status = %q, want pending (migration should be no-op after marker)", status)
	}
}

func TestRequeueSkippedPreviews(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })

	now := time.Now()
	cases := []struct {
		id         string
		status     string
		local      string
		fileID     string
		wantStatus string
		wantLocal  string
		wantFileID string
	}{
		{"preview-skipped", "skipped", "/tmp/old-preview.mp4", "old-preview-file", "pending", "", ""},
		{"preview-ready", "ready", "/tmp/ready-preview.mp4", "ready-preview-file", "ready", "/tmp/ready-preview.mp4", "ready-preview-file"},
		{"preview-failed", "failed", "/tmp/failed-preview.mp4", "failed-preview-file", "failed", "/tmp/failed-preview.mp4", "failed-preview-file"},
	}
	for _, c := range cases {
		if err := cat.UpsertVideo(ctx, &Video{
			ID: c.id, DriveID: "d", FileID: "source-" + c.id, Title: c.id,
			PreviewStatus: c.status, PreviewLocal: c.local, PreviewFileID: c.fileID,
			PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", c.id, err)
		}
	}

	if err := cat.requeueSkippedPreviews(ctx); err != nil {
		t.Fatalf("requeue skipped previews: %v", err)
	}
	if err := cat.requeueSkippedPreviews(ctx); err != nil {
		t.Fatalf("second requeue skipped previews: %v", err)
	}

	for _, c := range cases {
		got, err := cat.GetVideo(ctx, c.id)
		if err != nil {
			t.Fatalf("get %s: %v", c.id, err)
		}
		if got.PreviewStatus != c.wantStatus {
			t.Errorf("%s: preview status = %q, want %q", c.id, got.PreviewStatus, c.wantStatus)
		}
		if got.PreviewLocal != c.wantLocal {
			t.Errorf("%s: preview local = %q, want %q", c.id, got.PreviewLocal, c.wantLocal)
		}
		if got.PreviewFileID != c.wantFileID {
			t.Errorf("%s: preview file id = %q, want %q", c.id, got.PreviewFileID, c.wantFileID)
		}
	}

	pending, err := cat.ListVideosByPreviewStatus(ctx, "d", "pending", 0)
	if err != nil {
		t.Fatalf("list pending previews: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "preview-skipped" {
		t.Fatalf("pending previews = %#v, want only preview-skipped", pending)
	}
}

// TestUpsertVideoSyncsThumbnailStatus 验证 scanner 创建/补回视频时
// thumbnail_status 跟随 thumbnail_url 自动设。这是历史 bug 的修复回归测试 ——
// 之前 UpsertVideo 的 SQL 不带 thumbnail_status 列，所有新视频都依赖
// 列 DEFAULT 'pending'，url 非空时和 status 字段长期不一致。
func TestUpsertVideoSyncsThumbnailStatusFromURL(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })

	now := time.Now()
	cases := []struct {
		name       string
		thumb      string
		wantStatus string
	}{
		{"insert with remote URL → ready", "https://drive.example/thumb.jpg", "ready"},
		{"insert with local /p/thumb URL → ready", "/p/thumb/insert-local", "ready"},
		{"insert without URL → pending", "", "pending"},
	}
	for _, c := range cases {
		id := "ins-" + c.wantStatus + "-" + c.thumb
		if err := cat.UpsertVideo(ctx, &Video{
			ID: id, DriveID: "d", FileID: "f-" + id, Title: c.name,
			ThumbnailURL: c.thumb, PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("%s: upsert: %v", c.name, err)
		}
		var got string
		if err := cat.db.QueryRowContext(ctx,
			`SELECT thumbnail_status FROM videos WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("%s: read: %v", c.name, err)
		}
		if got != c.wantStatus {
			t.Errorf("%s: status = %q, want %q", c.name, got, c.wantStatus)
		}
	}
}

func TestUpsertVideoOnConflictSyncsStatusOnURLChange(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })

	now := time.Now()
	id := "conflict-vid"

	// 第一次 upsert：无 url → pending
	if err := cat.UpsertVideo(ctx, &Video{
		ID: id, DriveID: "d", FileID: "f", Title: "v",
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	var s string
	cat.db.QueryRowContext(ctx, `SELECT thumbnail_status FROM videos WHERE id=?`, id).Scan(&s)
	if s != "pending" {
		t.Fatalf("first: status = %q, want pending", s)
	}

	// 第二次 upsert（ON CONFLICT 路径）：带上 url → 自动同步 status='ready'
	if err := cat.UpsertVideo(ctx, &Video{
		ID: id, DriveID: "d", FileID: "f", Title: "v",
		ThumbnailURL: "https://drive.example/thumb.jpg",
		PublishedAt:  now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	cat.db.QueryRowContext(ctx, `SELECT thumbnail_status FROM videos WHERE id=?`, id).Scan(&s)
	if s != "ready" {
		t.Fatalf("after url upsert: status = %q, want ready", s)
	}

	// 第三次 upsert：清空 url（直 SQL 模拟 clearVolatileOneDriveThumbnails 没走 UpsertVideo
	// 这条路径，但场景就是用户手动把 thumbnail 改空。url='' 时 UpsertVideo
	// 不应改变已有 status，因为 UpsertVideo 不是清空场景的合法接口）。
	if err := cat.UpsertVideo(ctx, &Video{
		ID: id, DriveID: "d", FileID: "f", Title: "v",
		ThumbnailURL: "",
		PublishedAt:  now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("third upsert: %v", err)
	}
	cat.db.QueryRowContext(ctx, `SELECT thumbnail_status FROM videos WHERE id=?`, id).Scan(&s)
	if s != "ready" {
		t.Fatalf("after url cleared via upsert: status = %q, want unchanged 'ready'", s)
	}
}

func TestUpdateVideoMetaInfersReadyWhenURLPresent(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })

	now := time.Now()
	id := "meta-test"
	if err := cat.UpsertVideo(ctx, &Video{
		ID: id, DriveID: "d", FileID: "f", Title: "v",
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// status 初始 pending（无 url）
	var s string
	cat.db.QueryRowContext(ctx, `SELECT thumbnail_status FROM videos WHERE id=?`, id).Scan(&s)
	if s != "pending" {
		t.Fatalf("seed status = %q, want pending", s)
	}

	// 仅传 ThumbnailURL，期望 status 自动推到 'ready'
	if err := cat.UpdateVideoMeta(ctx, id, VideoMetaPatch{
		ThumbnailURL: "/p/thumb/" + id,
	}); err != nil {
		t.Fatalf("update meta: %v", err)
	}
	cat.db.QueryRowContext(ctx, `SELECT thumbnail_status FROM videos WHERE id=?`, id).Scan(&s)
	if s != "ready" {
		t.Errorf("after URL-only patch: status = %q, want ready (auto-inferred)", s)
	}

	// 显式传 ThumbnailStatus 时 patch 应该被尊重，而不是被自动推断覆盖
	if err := cat.UpdateVideoMeta(ctx, id, VideoMetaPatch{
		ThumbnailURL:    "/p/thumb/another",
		ThumbnailStatus: "failed",
	}); err != nil {
		t.Fatalf("update meta with status: %v", err)
	}
	cat.db.QueryRowContext(ctx, `SELECT thumbnail_status FROM videos WHERE id=?`, id).Scan(&s)
	if s != "failed" {
		t.Errorf("explicit status overrides inference: got %q, want failed", s)
	}
}

func TestClearVolatileOneDriveThumbnailsResetsStatus(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &Video{
		ID: "od-vid", DriveID: "OneDrive", FileID: "od-1", Title: "od",
		ThumbnailURL: "https://westus21-mediap.svc.ms/transform/thumbnail?provider=spo&tempauth=expired",
		PublishedAt:  now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// 经 UpsertVideo 自动同步，此时 status='ready'
	var s string
	cat.db.QueryRowContext(ctx, `SELECT thumbnail_status FROM videos WHERE id='od-vid'`).Scan(&s)
	if s != "ready" {
		t.Fatalf("pre-clear: status = %q, want ready", s)
	}

	if err := cat.clearVolatileOneDriveThumbnails(ctx); err != nil {
		t.Fatalf("clear: %v", err)
	}

	var url string
	cat.db.QueryRowContext(ctx, `SELECT COALESCE(thumbnail_url,''), thumbnail_status FROM videos WHERE id='od-vid'`).Scan(&url, &s)
	if url != "" {
		t.Errorf("url after clear = %q, want empty", url)
	}
	if s != "pending" {
		t.Errorf("status after clear = %q, want pending (so worker re-enqueues)", s)
	}
}
