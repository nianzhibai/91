package catalog

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/mediasig"
)

func testSignature() *mediasig.Signature {
	return &mediasig.Signature{Duration: 90 * time.Second, Frames: []mediasig.Frame{
		{At: 13500 * time.Millisecond, Window: 0, Hash: 0x0123_4567_89ab_cdef, Informative: true},
		{At: 14 * time.Second, Window: 0, Hash: 0xfedc_ba98_7654_3210},
	}}
}

func openMediaTestCatalog(t *testing.T, videos ...*Video) *Catalog {
	t.Helper()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	for _, video := range videos {
		if err := cat.UpsertVideo(context.Background(), video); err != nil {
			t.Fatal(err)
		}
	}
	return cat
}

func mediaTestVideo(id string, duration int) *Video {
	return &Video{ID: id, DriveID: "drive", FileID: id, Title: id, Size: 100, DurationSeconds: duration}
}

func TestMediaWants(t *testing.T) {
	for _, tc := range []struct {
		name     string
		video    Video
		previews bool
		want     MediaWants
	}{
		{"new video", Video{PreviewStatus: "pending", MediaStatus: "pending"}, true, MediaWants{Teaser: true, Signature: true}},
		{"new video without previews", Video{PreviewStatus: "pending", MediaStatus: "pending"}, false, MediaWants{Signature: true}},
		{"short video", Video{DurationSeconds: 20, PreviewStatus: "pending", MediaStatus: "pending"}, true, MediaWants{Teaser: true}},
		{"short video without previews", Video{DurationSeconds: 20, PreviewStatus: "pending", MediaStatus: "pending"}, false, MediaWants{}},
		{"renewal keeps the teaser fresh", Video{DurationSeconds: 90, PreviewStatus: "ready", PreviewLocal: "a.mp4", MediaStatus: "pending"}, true, MediaWants{Teaser: true, Signature: true}},
		{"done", Video{DurationSeconds: 90, PreviewStatus: "ready", PreviewLocal: "a.mp4", MediaStatus: "ready"}, true, MediaWants{}},
		{"teaser enabled after the job", Video{DurationSeconds: 90, PreviewStatus: "pending", MediaStatus: "ready"}, true, MediaWants{Teaser: true}},
		{"failed", Video{DurationSeconds: 90, PreviewStatus: "failed", MediaStatus: "failed"}, true, MediaWants{}},
		{"hidden", Video{Hidden: true, PreviewStatus: "pending", MediaStatus: "pending"}, true, MediaWants{}},
	} {
		if got := tc.video.MediaWants(tc.previews); got != tc.want {
			t.Errorf("%s: wants = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestMediaJobLifecycle(t *testing.T) {
	ctx := context.Background()
	cat := openMediaTestCatalog(t, mediaTestVideo("signed", 90), mediaTestVideo("failing", 90), mediaTestVideo("short", 10))
	listIDs := func(previews bool) map[string]bool {
		t.Helper()
		videos, err := cat.ListVideosNeedingMedia(ctx, "drive", previews)
		if err != nil {
			t.Fatal(err)
		}
		ids := map[string]bool{}
		for _, video := range videos {
			ids[video.ID] = true
		}
		return ids
	}
	if ids := listIDs(false); len(ids) != 2 || !ids["signed"] || !ids["failing"] {
		t.Fatalf("media work without previews = %v, want the two long videos", ids)
	}

	signed, _ := cat.GetVideo(ctx, "signed")
	if err := cat.CompleteMediaForSource(ctx, signed, testSignature()); err != nil {
		t.Fatal(err)
	}
	failing, _ := cat.GetVideo(ctx, "failing")
	if err := cat.MarkMediaFailedForSource(ctx, failing); err != nil {
		t.Fatal(err)
	}
	short, _ := cat.GetVideo(ctx, "short")
	if err := cat.CompleteMediaForSource(ctx, short, nil); err != nil {
		t.Fatal(err)
	}
	if ids := listIDs(true); len(ids) != 3 {
		t.Fatalf("media work with previews = %v, want every pending teaser", ids)
	}
	if ids := listIDs(false); len(ids) != 0 {
		t.Fatalf("media work = %v, want none after every job settled", ids)
	}

	loaded, err := cat.LoadVideoSignatures(ctx, []string{"signed", "failing", "short", "unknown"})
	if err != nil || len(loaded) != 1 || loaded["signed"].Frames[0] != testSignature().Frames[0] {
		t.Fatalf("loaded = %+v err = %v, want only the stored signature", loaded, err)
	}

	// A write for bytes the row no longer refers to is rejected.
	stale := *signed
	stale.Size = 99
	if err := cat.CompleteMediaForSource(ctx, &stale, testSignature()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stale write err = %v, want sql.ErrNoRows", err)
	}

	counts, err := cat.ResetFailedGeneration(ctx, "drive", GenerationKinds{Previews: true})
	if err != nil || counts.Previews != 1 {
		t.Fatalf("retry counts = %+v err = %v, want the failed job admitted", counts, err)
	}
	if err := cat.RequestMediaRenewal(ctx, []string{"signed"}); err != nil {
		t.Fatal(err)
	}
	if ids := listIDs(false); len(ids) != 2 || !ids["failing"] || !ids["signed"] {
		t.Fatalf("media work after retry and renewal = %v", ids)
	}

	// Replaced content invalidates the signature with the other assets.
	if err := cat.UpdateVideoMeta(ctx, "signed", VideoMetaPatch{ResetContent: true, Size: 200}); err != nil {
		t.Fatal(err)
	}
	if loaded, _ := cat.LoadVideoSignatures(ctx, []string{"signed"}); len(loaded) != 0 {
		t.Fatalf("signature survived a content reset: %+v", loaded)
	}
}

func TestOutdatedSignatureFormatIsIgnored(t *testing.T) {
	ctx := context.Background()
	cat := openMediaTestCatalog(t, mediaTestVideo("old", 90))
	if _, err := cat.db.ExecContext(ctx, `UPDATE videos SET content_signature = x'00ff', media_status = 'ready' WHERE id = 'old'`); err != nil {
		t.Fatal(err)
	}
	if loaded, err := cat.LoadVideoSignatures(ctx, []string{"old"}); err != nil || len(loaded) != 0 {
		t.Fatalf("loaded = %+v err = %v, want outdated signature ignored", loaded, err)
	}
}

func TestStaleMediaRenewsInBatches(t *testing.T) {
	ctx := context.Background()
	cat := openMediaTestCatalog(t, mediaTestVideo("first", 90), mediaTestVideo("second", 90), mediaTestVideo("third", 90))
	if _, err := cat.db.ExecContext(ctx, `UPDATE videos SET media_status = 'stale'`); err != nil {
		t.Fatal(err)
	}
	if videos, _ := cat.ListVideosNeedingMedia(ctx, "drive", false); len(videos) != 0 {
		t.Fatalf("stale videos listed as due: %d", len(videos))
	}
	promoted, err := cat.PromoteStaleMedia(ctx, 2)
	if err != nil || len(promoted) != 2 || promoted[0].ID != "first" || promoted[1].ID != "second" || promoted[0].MediaStatus != "pending" {
		t.Fatalf("promoted = %+v err = %v, want the two oldest", promoted, err)
	}
	if videos, _ := cat.ListVideosNeedingMedia(ctx, "drive", false); len(videos) != 2 {
		t.Fatalf("due videos = %d, want the promoted batch", len(videos))
	}
}

func TestOutdatedSignaturesBecomeStaleOnOpen(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	cat, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.UpsertVideo(ctx, mediaTestVideo("old", 90)); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.db.ExecContext(ctx, `UPDATE videos SET content_signature = x'01ff', media_status = 'ready'`); err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if got, _ := reopened.GetVideo(ctx, "old"); got.MediaStatus != "stale" {
		t.Fatalf("media status = %q, want an outdated signature renewed", got.MediaStatus)
	}
}

func TestPartialSignatureRereadsArePromotedFirst(t *testing.T) {
	ctx := context.Background()
	cat := openMediaTestCatalog(t, mediaTestVideo("old", 90), mediaTestVideo("partial", 90))
	if _, err := cat.db.ExecContext(ctx, `UPDATE videos SET media_status = 'stale', created_at = CASE id WHEN 'old' THEN 1 ELSE 2 END`); err != nil {
		t.Fatal(err)
	}
	partial, _ := cat.GetVideo(ctx, "partial")
	if err := cat.CompletePartialMediaForSource(ctx, partial, testSignature()); err != nil {
		t.Fatal(err)
	}
	promoted, err := cat.PromoteStaleMedia(ctx, 1)
	if err != nil || len(promoted) != 1 || promoted[0].ID != "partial" {
		t.Fatalf("promoted = %+v err = %v, want the partial signature reread first", promoted, err)
	}
}
