package catalog

import (
	"context"
	"math"
	"testing"
	"time"
)

func openShortsTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	return cat
}

func insertShortsTestVideo(t *testing.T, cat *Catalog, id, parentID, fileName string, publishedAt time.Time) {
	t.Helper()
	if err := cat.UpsertVideo(context.Background(), &Video{
		ID:          id,
		DriveID:     "drive",
		FileID:      id,
		FileName:    fileName,
		ParentID:    parentID,
		Title:       id,
		PublishedAt: publishedAt,
		CreatedAt:   publishedAt,
		UpdatedAt:   publishedAt,
	}); err != nil {
		t.Fatal(err)
	}
}

// assignTag 先建标签再挂到视频上：ReplaceAutoVideoTags 只会关联已存在的标签，
// 未知标签会被静默跳过。
func assignTag(t *testing.T, cat *Catalog, videoID, label string) {
	t.Helper()
	ctx := context.Background()
	if _, err := cat.EnsureTag(ctx, label, "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ReplaceAutoVideoTags(ctx, videoID, []TagAssignment{
		{Label: label, Source: "auto", Evidence: "测试"},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRecordShortsEventsDropsUnknownVideos(t *testing.T) {
	cat := openShortsTestCatalog(t)
	ctx := context.Background()
	now := time.Now()
	insertShortsTestVideo(t, cat, "known", "dir", "known.mp4", now)

	// 播放到上报之间视频可能已被隐藏或删除：丢这一条，不能丢整批。
	stored, err := cat.RecordShortsEvents(ctx, 7, []ShortsEvent{
		{VideoID: "known", Kind: ShortsEventComplete, WatchMs: 1200, LoopCount: 1},
		{VideoID: "gone", Kind: ShortsEventSkip},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("stored = %d, want 1", stored)
	}
}

func TestRecordShortsEventsRejectsUnknownKind(t *testing.T) {
	cat := openShortsTestCatalog(t)
	now := time.Now()
	insertShortsTestVideo(t, cat, "known", "dir", "known.mp4", now)

	if _, err := cat.RecordShortsEvents(context.Background(), 7, []ShortsEvent{
		{VideoID: "known", Kind: "watched"},
	}, now); err == nil {
		t.Fatal("unknown kind must be rejected")
	}
}

func TestShortsWatchProfileSmoothsDirectoryAffinity(t *testing.T) {
	cat := openShortsTestCatalog(t)
	ctx := context.Background()
	now := time.Now()
	insertShortsTestVideo(t, cat, "v1", "watched-dir", "v1.mp4", now)
	insertShortsTestVideo(t, cat, "v2", "watched-dir", "v2.mp4", now)
	insertShortsTestVideo(t, cat, "lucky", "lucky-dir", "lucky.mp4", now)

	if _, err := cat.RecordShortsEvents(ctx, 7, []ShortsEvent{
		{VideoID: "v1", Kind: ShortsEventComplete},
		{VideoID: "v2", Kind: ShortsEventPartial},
		{VideoID: "v2", Kind: ShortsEventSkip},
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.RecordShortsEvents(ctx, 7, []ShortsEvent{
		{VideoID: "lucky", Kind: ShortsEventComplete},
	}, now); err != nil {
		t.Fatal(err)
	}

	profile, err := cat.ShortsWatchProfile(ctx, 7, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	want := (1 + shortsDirAffinityWeight*shortsDirAffinityPrior) / (3 + shortsDirAffinityWeight)
	if got := profile.DirAffinity["watched-dir"]; math.Abs(got-want) > 1e-9 {
		t.Fatalf("watched-dir affinity = %v, want %v", got, want)
	}
	// 一次完播不能让一个目录直接冲到满分：平滑是防止「1 次 100%」永久霸榜的
	// 唯一手段。
	lucky := profile.DirAffinity["lucky-dir"]
	if lucky >= 1 {
		t.Fatalf("single completion produced affinity %v, want a smoothed value below 1", lucky)
	}
	if lucky <= profile.DirAffinity["watched-dir"] {
		t.Fatalf("a fully completed directory must still outrank a mostly skipped one: %v vs %v",
			lucky, profile.DirAffinity["watched-dir"])
	}
}

func TestShortsWatchProfileBuildsTagAffinityAndConsumedSet(t *testing.T) {
	cat := openShortsTestCatalog(t)
	ctx := context.Background()
	now := time.Now()
	insertShortsTestVideo(t, cat, "v1", "dir", "v1.mp4", now)
	assignTag(t, cat, "v1", "舞蹈")

	if _, err := cat.RecordShortsEvents(ctx, 7, []ShortsEvent{
		{VideoID: "v1", Kind: ShortsEventComplete, WatchMs: 5000},
	}, now); err != nil {
		t.Fatal(err)
	}

	profile, err := cat.ShortsWatchProfile(ctx, 7, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if profile.TagAffinity["舞蹈"] <= 0 {
		t.Fatalf("tag affinity = %v, want a positive weight", profile.TagAffinity["舞蹈"])
	}
	if _, ok := profile.Consumed["v1"]; !ok {
		t.Fatal("a watched video must be in the consumed set")
	}
	if profile.LastCompleted != "v1" {
		t.Fatalf("last completed = %q, want v1", profile.LastCompleted)
	}
}

func TestSetUserVideoLikeIsIdempotent(t *testing.T) {
	cat := openShortsTestCatalog(t)
	ctx := context.Background()
	now := time.Now()
	insertShortsTestVideo(t, cat, "v1", "dir", "v1.mp4", now)

	first, err := cat.SetUserVideoLike(ctx, 7, "v1", true)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Liked || first.Likes != 1 {
		t.Fatalf("first like = %#v, want liked with 1 like", first)
	}
	// 同一个人重复点赞必须是幂等的：计数器不能被刷高。
	second, err := cat.SetUserVideoLike(ctx, 7, "v1", true)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Liked || second.Likes != 1 {
		t.Fatalf("repeated like = %#v, want the counter to stay at 1", second)
	}

	liked, err := cat.LikedVideoIDsAmong(ctx, 7, []string{"v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := liked["v1"]; !ok {
		t.Fatal("like was not recorded against the account")
	}

	removed, err := cat.SetUserVideoLike(ctx, 7, "v1", false)
	if err != nil {
		t.Fatal(err)
	}
	if removed.Liked || removed.Likes != 0 {
		t.Fatalf("unlike = %#v, want unliked with 0 likes", removed)
	}
	again, err := cat.SetUserVideoLike(ctx, 7, "v1", false)
	if err != nil {
		t.Fatal(err)
	}
	if again.Likes != 0 {
		t.Fatalf("repeated unlike drove the counter to %d", again.Likes)
	}
}

func TestShortsDirectoryTagSharesMeasuresConcentration(t *testing.T) {
	cat := openShortsTestCatalog(t)
	ctx := context.Background()
	now := time.Now()

	contentTags := []string{"舞蹈", "舞蹈", "舞蹈", "舞蹈"}
	archiveTags := []string{"旅行", "美食", "宠物", "音乐"}
	for index, label := range contentTags {
		id := "content-" + string(rune('a'+index))
		insertShortsTestVideo(t, cat, id, "content", id+".mp4", now)
		assignTag(t, cat, id, label)
	}
	for index, label := range archiveTags {
		id := "archive-" + string(rune('a'+index))
		insertShortsTestVideo(t, cat, id, "archive", id+".mp4", now)
		assignTag(t, cat, id, label)
	}

	shares, err := cat.ShortsDirectoryTagShares(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if share := shares["content"].TopShare; math.Abs(share-1) > 1e-9 {
		t.Fatalf("content directory share = %v, want 1", share)
	}
	if share := shares["archive"].TopShare; math.Abs(share-0.25) > 1e-9 {
		t.Fatalf("archive directory share = %v, want 0.25", share)
	}
}

func TestShortsCandidatesCarryTagsAndSkipHiddenVideos(t *testing.T) {
	cat := openShortsTestCatalog(t)
	ctx := context.Background()
	now := time.Now()
	insertShortsTestVideo(t, cat, "v1", "dir", "v1.mp4", now)
	assignTag(t, cat, "v1", "舞蹈")
	hidden := &Video{
		ID: "hidden", DriveID: "drive", FileID: "hidden", FileName: "hidden.mp4",
		Title: "hidden", Hidden: true, PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := cat.UpsertVideo(ctx, hidden); err != nil {
		t.Fatal(err)
	}

	candidates, err := cat.ShortsCandidates(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1 visible video", len(candidates))
	}
	if candidates[0].ID != "v1" || candidates[0].ParentID != "dir" {
		t.Fatalf("unexpected candidate %#v", candidates[0])
	}
	if len(candidates[0].Tags) != 1 || candidates[0].Tags[0] != "舞蹈" {
		t.Fatalf("candidate tags = %v, want [舞蹈]", candidates[0].Tags)
	}
}
