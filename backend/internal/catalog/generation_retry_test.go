package catalog

import (
	"context"
	"errors"
	"testing"
	"time"
)

func seedFailedGeneration(t *testing.T, cat *Catalog, id, driveID string) {
	t.Helper()
	now := time.Now()
	if err := cat.UpsertVideo(context.Background(), &Video{
		ID: id, DriveID: driveID, FileID: id, FileName: id + ".mp4", Title: id,
		Size: 123, PreviewStatus: "failed", FingerprintStatus: "failed", FingerprintError: "old failure",
		CreatedAt: now, PublishedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.db.Exec(`UPDATE videos SET thumbnail_status = 'failed', thumbnail_failures = 3 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
}

func TestResetFailedGenerationSelectsEligibleWork(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	for _, id := range []string{"failed", "ready", "pending", "hidden", "disabled", "duplicate", "probe", "unknown-size"} {
		seedFailedGeneration(t, cat, id, "drive")
	}
	seedFailedGeneration(t, cat, "other", "other-drive")
	for _, query := range []string{
		`UPDATE videos SET thumbnail_status = 'ready', preview_status = 'ready', fingerprint_status = 'ready',
		 thumbnail_url = '/p/thumb/ready', thumbnail_updated_at = 123, preview_local = '/ready.mp4',
		 preview_updated_at = 456, sampled_sha256 = 's2:ready-hash', duration_seconds = 30 WHERE id = 'ready'`,
		`UPDATE videos SET thumbnail_status = 'pending', preview_status = 'pending', fingerprint_status = 'pending' WHERE id = 'pending'`,
		`UPDATE videos SET hidden = 1 WHERE id = 'hidden'`,
		`UPDATE videos SET thumbnail_status = 'skipped', preview_status = 'disabled', sampled_sha256 = 's2:existing-hash' WHERE id = 'disabled'`,
		`UPDATE videos SET thumbnail_url = '/p/thumb/probe', thumbnail_updated_at = 789, thumbnail_status = 'ready' WHERE id = 'probe'`,
		`UPDATE videos SET size_bytes = 0 WHERE id = 'unknown-size'`,
		// The materialized representative flag is maintained by deduplication.
		// Fingerprint sampling must still visit noncanonical sources.
		`UPDATE videos SET is_canonical = 0 WHERE id = 'duplicate'`,
	} {
		if _, err := cat.db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	counts, err := cat.ResetFailedGeneration(ctx, "drive", GenerationKinds{Thumbnails: true, Previews: true, Fingerprints: true})
	if err != nil {
		t.Fatal(err)
	}
	if counts != (GenerationRetryCounts{Thumbnails: 2, Previews: 3, Fingerprints: 3}) {
		t.Fatalf("retry counts = %+v", counts)
	}
	for _, tc := range []struct {
		id                              string
		thumbnail, preview, fingerprint string
	}{
		{"failed", "pending", "pending", "pending"},
		{"ready", "ready", "ready", "ready"},
		{"pending", "pending", "pending", "pending"},
		{"hidden", "failed", "failed", "failed"},
		{"other", "failed", "failed", "failed"},
		{"disabled", "skipped", "disabled", "failed"},
		{"duplicate", "failed", "failed", "pending"},
		{"probe", "ready", "pending", "pending"},
		{"unknown-size", "pending", "pending", "failed"},
	} {
		var thumbnail, preview, fingerprint string
		if err := cat.db.QueryRow(`SELECT thumbnail_status, preview_status, fingerprint_status FROM videos WHERE id = ?`, tc.id).
			Scan(&thumbnail, &preview, &fingerprint); err != nil {
			t.Fatal(err)
		}
		if thumbnail != tc.thumbnail || preview != tc.preview || fingerprint != tc.fingerprint {
			t.Fatalf("%s states = %s/%s/%s, want %s/%s/%s", tc.id, thumbnail, preview, fingerprint, tc.thumbnail, tc.preview, tc.fingerprint)
		}
	}
	var failures int
	if err := cat.db.QueryRow(`SELECT thumbnail_failures FROM videos WHERE id = 'failed'`).Scan(&failures); err != nil || failures != 0 {
		t.Fatalf("thumbnail failures = %d, err = %v", failures, err)
	}
	failed, err := cat.GetVideo(ctx, "failed")
	if err != nil || failed.FingerprintError != "" {
		t.Fatalf("fingerprint failure was not cleared: %+v, %v", failed, err)
	}
	ready, err := cat.GetVideo(ctx, "ready")
	if err != nil || ready.ThumbnailUpdatedAt.UnixMilli() != 123 || ready.PreviewUpdatedAt.UnixMilli() != 456 || ready.SampledSHA256 != "s2:ready-hash" {
		t.Fatalf("ready assets changed: %+v, %v", ready, err)
	}
	probe, err := cat.GetVideo(ctx, "probe")
	if err != nil || probe.ThumbnailURL != "/p/thumb/probe" || probe.ThumbnailUpdatedAt.UnixMilli() != 789 {
		t.Fatalf("duration retry discarded existing thumbnail: %+v, %v", probe, err)
	}
}

func TestResetFailedGenerationHonorsSelectedKinds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kinds GenerationKinds
	}{
		{"none", GenerationKinds{}},
		{"thumbnails", GenerationKinds{Thumbnails: true}},
		{"previews", GenerationKinds{Previews: true}},
		{"fingerprints", GenerationKinds{Fingerprints: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat, err := Open(t.TempDir() + "/catalog.db")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cat.Close() })
			seedFailedGeneration(t, cat, "failed", "drive")
			counts, err := cat.ResetFailedGeneration(context.Background(), "drive", tc.kinds)
			if err != nil {
				t.Fatal(err)
			}
			if (counts.Thumbnails == 1) != tc.kinds.Thumbnails || (counts.Previews == 1) != tc.kinds.Previews || (counts.Fingerprints == 1) != tc.kinds.Fingerprints {
				t.Fatalf("counts = %+v for %+v", counts, tc.kinds)
			}
		})
	}
}

func TestResetFailedGenerationRollsBackOnErrorOrCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "database error", true: "cancellation"}[canceled], func(t *testing.T) {
			cat, err := Open(t.TempDir() + "/catalog.db")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cat.Close() })
			seedFailedGeneration(t, cat, "failed", "drive")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if canceled {
				cancel()
			} else if _, err := cat.db.Exec(`CREATE TRIGGER reject_preview_retry BEFORE UPDATE OF preview_status ON videos
			 BEGIN SELECT RAISE(ABORT, 'test write failure'); END`); err != nil {
				t.Fatal(err)
			}
			counts, err := cat.ResetFailedGeneration(ctx, "drive", GenerationKinds{Thumbnails: true, Previews: true, Fingerprints: true})
			if err == nil || counts != (GenerationRetryCounts{}) || (canceled && !errors.Is(err, context.Canceled)) {
				t.Fatalf("retry counts = %+v, err = %v", counts, err)
			}
			var unchanged int
			if err := cat.db.QueryRow(`SELECT COUNT(*) FROM videos WHERE id = 'failed' AND thumbnail_status = 'failed'
			 AND thumbnail_failures = 3 AND preview_status = 'failed' AND fingerprint_status = 'failed'`).Scan(&unchanged); err != nil || unchanged != 1 {
				t.Fatalf("failed reset modified generation state: count = %d, err = %v", unchanged, err)
			}
		})
	}
}
