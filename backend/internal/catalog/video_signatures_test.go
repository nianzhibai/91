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

func signatureTestVideo(id string) *Video {
	return &Video{
		ID: id, DriveID: "drive", FileID: id, Title: id, Size: 100, DurationSeconds: 90,
		PreviewLocal: id + ".mp4", PreviewStatus: "ready",
	}
}

func TestVideoSignatureLifecycle(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	for _, video := range []*Video{signatureTestVideo("signed"), signatureTestVideo("unsigned")} {
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatal(err)
		}
	}
	signed, err := cat.GetVideo(ctx, "signed")
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.SetVideoSignatureForSource(ctx, signed, testSignature()); err != nil {
		t.Fatalf("SetVideoSignatureForSource: %v", err)
	}

	loaded, err := cat.LoadVideoSignatures(ctx, []string{"signed", "unsigned", "unknown"})
	if err != nil {
		t.Fatalf("LoadVideoSignatures: %v", err)
	}
	if len(loaded) != 1 || loaded["signed"] == nil || loaded["signed"].Frames[0] != testSignature().Frames[0] {
		t.Fatalf("loaded = %+v, want only the stored signature", loaded)
	}
	missing, err := cat.ListVideosMissingSignature(ctx, 30)
	if err != nil || len(missing) != 1 || missing[0].ID != "unsigned" {
		t.Fatalf("missing = %+v err = %v, want the unsigned video", missing, err)
	}
	if missing, _ := cat.ListVideosMissingSignature(ctx, 91); len(missing) != 0 {
		t.Fatalf("missing = %+v, want videos below the minimum duration excluded", missing)
	}

	// A write for bytes the row no longer refers to is rejected.
	stale := *signed
	stale.Size = 99
	if err := cat.SetVideoSignatureForSource(ctx, &stale, testSignature()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stale write err = %v, want sql.ErrNoRows", err)
	}

	// Replaced content invalidates the signature with the other assets.
	if err := cat.UpdateVideoMeta(ctx, "signed", VideoMetaPatch{ResetContent: true, Size: 200}); err != nil {
		t.Fatal(err)
	}
	if loaded, _ := cat.LoadVideoSignatures(ctx, []string{"signed"}); len(loaded) != 0 {
		t.Fatalf("signature survived a content reset: %+v", loaded)
	}
}

func TestOutdatedSignatureFormatCountsAsMissing(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	if err := cat.UpsertVideo(ctx, signatureTestVideo("old")); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.db.ExecContext(ctx, `UPDATE videos SET content_signature = x'00ff' WHERE id = 'old'`); err != nil {
		t.Fatal(err)
	}
	if loaded, err := cat.LoadVideoSignatures(ctx, []string{"old"}); err != nil || len(loaded) != 0 {
		t.Fatalf("loaded = %+v err = %v, want outdated signature ignored", loaded, err)
	}
	if missing, err := cat.ListVideosMissingSignature(ctx, 30); err != nil || len(missing) != 1 {
		t.Fatalf("missing = %+v err = %v, want outdated signature recomputed", missing, err)
	}
}
