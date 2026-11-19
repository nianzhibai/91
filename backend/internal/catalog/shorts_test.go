package catalog

import (
	"context"
	"testing"
	"time"
)

// TestRandomVideosExcluding 验证短视频"不重复随机"的核心数据层行为：
// 1) 已传入的 id 不会被返回
// 2) 同一调用返回的视频之间互不相同
// 3) limit 大于剩余可选时只返回剩余的全部
// 4) 隐藏的视频不会出现在结果中
func TestRandomVideosExcluding(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	// 6 个可见 + 1 个隐藏
	all := []string{"v1", "v2", "v3", "v4", "v5", "v6"}
	for i, id := range all {
		if err := cat.UpsertVideo(ctx, &Video{
			ID:          id,
			DriveID:     "drive",
			FileID:      "f-" + id,
			Title:       id,
			PublishedAt: now.Add(time.Duration(i) * time.Second),
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	if err := cat.UpsertVideo(ctx, &Video{
		ID: "v-hidden", DriveID: "drive", FileID: "f-hidden",
		Title: "hidden", PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed hidden: %v", err)
	}
	if err := cat.MarkInvalidSizeSources(ctx, "drive", []string{"f-hidden"}); err != nil {
		t.Fatalf("hide v-hidden: %v", err)
	}

	total, err := cat.CountVisibleVideos(ctx)
	if err != nil {
		t.Fatalf("count visible: %v", err)
	}
	if total != len(all) {
		t.Fatalf("visible count = %d, want %d", total, len(all))
	}

	// 1) 排除 v1, v2, v3，请求 2 个，应当从 {v4,v5,v6} 里随机取 2 个，互不相同
	got, err := cat.RandomVideosExcluding(ctx, []string{"v1", "v2", "v3"}, 2)
	if err != nil {
		t.Fatalf("random excluding: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2", len(got))
	}
	seen := map[string]struct{}{}
	for _, v := range got {
		if v.ID == "v1" || v.ID == "v2" || v.ID == "v3" {
			t.Fatalf("excluded id %s was returned", v.ID)
		}
		if v.ID == "v-hidden" {
			t.Fatalf("hidden video was returned")
		}
		if _, dup := seen[v.ID]; dup {
			t.Fatalf("duplicate id in result: %s", v.ID)
		}
		seen[v.ID] = struct{}{}
	}

	// 2) limit 大于剩余可选时只返回全部剩余
	got2, err := cat.RandomVideosExcluding(ctx, []string{"v1", "v2", "v3", "v4"}, 10)
	if err != nil {
		t.Fatalf("random excluding (oversize limit): %v", err)
	}
	if len(got2) != 2 {
		t.Fatalf("oversize-limit result = %d items, want 2 (v5, v6)", len(got2))
	}

	// 3) 不传 exclude 时返回 limit 个不重复
	got3, err := cat.RandomVideosExcluding(ctx, nil, 4)
	if err != nil {
		t.Fatalf("random no exclude: %v", err)
	}
	if len(got3) != 4 {
		t.Fatalf("no-exclude result = %d, want 4", len(got3))
	}
	dedupe := map[string]struct{}{}
	for _, v := range got3 {
		if _, dup := dedupe[v.ID]; dup {
			t.Fatalf("no-exclude duplicate id: %s", v.ID)
		}
		dedupe[v.ID] = struct{}{}
	}

	// 4) limit <= 0 直接返回 nil
	got4, err := cat.RandomVideosExcluding(ctx, nil, 0)
	if err != nil {
		t.Fatalf("limit 0: %v", err)
	}
	if got4 != nil {
		t.Fatalf("limit 0 should return nil, got %v", got4)
	}
}

func TestRandomVideosWithReadyThumbnailsExcluding(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	for i := 0; i < 4; i++ {
		id := "ready-" + string(rune('a'+i))
		if err := cat.UpsertVideo(ctx, &Video{
			ID:           id,
			DriveID:      "drive",
			FileID:       "f-" + id,
			Title:        id,
			ThumbnailURL: "/p/thumb/" + id,
			PublishedAt:  now,
			CreatedAt:    now,
			UpdatedAt:    now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	for i := 0; i < 4; i++ {
		id := "pending-" + string(rune('a'+i))
		if err := cat.UpsertVideo(ctx, &Video{
			ID:          id,
			DriveID:     "drive",
			FileID:      "f-" + id,
			Title:       id,
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	got, err := cat.RandomVideosWithReadyThumbnailsExcluding(ctx, []string{"ready-a"}, 10)
	if err != nil {
		t.Fatalf("random ready excluding: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ready random count = %d, want 3", len(got))
	}
	for _, v := range got {
		if v.ID == "ready-a" {
			t.Fatal("excluded ready video was returned")
		}
		if v.ThumbnailURL == "" {
			t.Fatalf("pending video %q was returned", v.ID)
		}
	}
}
