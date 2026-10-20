package catalog

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func seedFavoriteTestVideo(t *testing.T, cat *Catalog, id string) {
	t.Helper()
	now := time.Now()
	if err := cat.UpsertVideo(context.Background(), &Video{
		ID:          id,
		DriveID:     "drive-1",
		FileID:      "file-" + id,
		FileName:    id + ".mp4",
		Title:       "Favorite test video " + id,
		Size:        int64(len(id)) * 1024,
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video %s: %v", id, err)
	}
}

func openFavoriteTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	return cat
}

func assertFavoriteIDs(t *testing.T, cat *Catalog, userID int64, want ...string) {
	t.Helper()
	got, err := cat.FavoriteVideoIDs(context.Background(), userID)
	if err != nil {
		t.Fatalf("list favorites for user %d: %v", userID, err)
	}
	if len(got) != len(want) {
		t.Fatalf("favorites for user %d = %v, want %v", userID, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("favorites for user %d = %v, want %v", userID, got, want)
		}
	}
}

func TestVideoFavoriteIsAccountScopedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	cat := openFavoriteTestCatalog(t)
	seedFavoriteTestVideo(t, cat, "video-1")
	seedFavoriteTestVideo(t, cat, "video-2")

	const userA, userB = int64(1), int64(2)
	assertFavorite := func(userID int64, videoID string, favorite, want bool) {
		t.Helper()
		got, err := cat.SetVideoFavorite(ctx, userID, videoID, favorite)
		if err != nil {
			t.Fatalf("set favorite user=%d video=%s favorite=%t: %v", userID, videoID, favorite, err)
		}
		if got.Favorited != want {
			t.Fatalf("favorite user=%d video=%s = %t, want %t", userID, videoID, got.Favorited, want)
		}
	}

	assertFavorite(userA, "video-1", true, true)
	assertFavorite(userA, "video-1", true, true) // idempotent retry
	// Another account keeps its own state for the same video.
	assertFavorite(userB, "video-1", false, false)
	assertFavorite(userB, "video-2", true, true)

	assertFavoriteIDs(t, cat, userA, "video-1")
	assertFavoriteIDs(t, cat, userB, "video-2")

	assertFavorite(userA, "video-1", false, false)
	assertFavorite(userA, "video-1", false, false) // idempotent retry
	assertFavoriteIDs(t, cat, userA)
	// Removing one account's favorite never touches the other account's rows.
	assertFavoriteIDs(t, cat, userB, "video-2")
}

func TestVideoFavoriteRejectsUnavailableVideos(t *testing.T) {
	ctx := context.Background()
	cat := openFavoriteTestCatalog(t)
	seedFavoriteTestVideo(t, cat, "video-1")

	if _, err := cat.SetVideoFavorite(ctx, 1, "missing", true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("favorite unknown video error = %v, want sql.ErrNoRows", err)
	}
	if _, err := cat.SetVideoFavorite(ctx, 0, "video-1", true); !errors.Is(err, ErrInvalidVideoFavorite) {
		t.Fatalf("favorite without account error = %v, want ErrInvalidVideoFavorite", err)
	}
	if _, err := cat.db.ExecContext(ctx, `UPDATE videos SET hidden = 1 WHERE id = ?`, "video-1"); err != nil {
		t.Fatalf("hide video: %v", err)
	}
	if _, err := cat.SetVideoFavorite(ctx, 1, "video-1", true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("favorite hidden video error = %v, want sql.ErrNoRows", err)
	}
}

func TestFavoriteVideoIDsOrderNewestFirstAndSkipHidden(t *testing.T) {
	ctx := context.Background()
	cat := openFavoriteTestCatalog(t)
	for _, id := range []string{"video-1", "video-2", "video-3"} {
		seedFavoriteTestVideo(t, cat, id)
	}

	const userID = int64(7)
	// 直接写行固定收藏时间：接口层用当前时间，无法区分同一毫秒内的两次收藏。
	for _, favorite := range []struct {
		videoID   string
		createdAt int64
	}{
		{"video-1", 1_000},
		{"video-2", 2_000},
		{"video-3", 3_000},
	} {
		if _, err := cat.db.ExecContext(ctx,
			`INSERT INTO video_favorites (user_id, video_id, created_at) VALUES (?, ?, ?)`,
			userID, favorite.videoID, favorite.createdAt); err != nil {
			t.Fatalf("seed favorite %s: %v", favorite.videoID, err)
		}
	}
	// 隐藏的视频不再进入收藏栏目，但仍保留在表里等待恢复。
	if _, err := cat.db.ExecContext(ctx, `UPDATE videos SET hidden = 1 WHERE id = ?`, "video-2"); err != nil {
		t.Fatalf("hide video: %v", err)
	}

	assertFavoriteIDs(t, cat, userID, "video-3", "video-1")
}

func TestFavoritedVideoIDsAmongSelectsOnlyRequestedVideos(t *testing.T) {
	ctx := context.Background()
	cat := openFavoriteTestCatalog(t)
	for _, id := range []string{"video-1", "video-2", "video-3"} {
		seedFavoriteTestVideo(t, cat, id)
	}

	const userID = int64(3)
	for _, id := range []string{"video-1", "video-3"} {
		if _, err := cat.SetVideoFavorite(ctx, userID, id, true); err != nil {
			t.Fatalf("favorite %s: %v", id, err)
		}
	}

	got, err := cat.FavoritedVideoIDsAmong(ctx, userID, []string{"video-1", "video-2"})
	if err != nil {
		t.Fatalf("lookup favorites: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("favorited subset = %v, want only video-1", got)
	}
	if _, ok := got["video-1"]; !ok {
		t.Fatalf("favorited subset = %v, want video-1", got)
	}

	// 未登录（userID 0）时没有账号收藏可言，调用方拿到空集合而不是错误。
	if empty, err := cat.FavoritedVideoIDsAmong(ctx, 0, []string{"video-1"}); err != nil || len(empty) != 0 {
		t.Fatalf("anonymous favorite lookup = %v, %v; want empty, nil", empty, err)
	}
}
