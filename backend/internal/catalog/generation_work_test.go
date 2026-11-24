package catalog

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func openGenerationWorkCatalog(t *testing.T) *Catalog {
	t.Helper()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	return cat
}

// seedSettledVideo stores a video with every step settled, so each test sets
// only the state it is about.
func seedSettledVideo(t *testing.T, cat *Catalog, id, driveID string, created time.Time) {
	t.Helper()
	if err := cat.UpsertVideo(context.Background(), &Video{
		ID: id, DriveID: driveID, FileID: id, FileName: id + ".mp4", Title: id, Size: 1000,
		CreatedAt: created, PublishedAt: created, UpdatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.db.Exec(`UPDATE videos SET sampled_sha256 = 's2:` + id + `', fingerprint_status = 'ready',
		media_status = 'ready', preview_status = 'ready', preview_local = '` + id + `.mp4',
		thumbnail_url = '/p/thumb/` + id + `', thumbnail_status = 'ready', duration_seconds = 60
		WHERE id = '` + id + `'`); err != nil {
		t.Fatal(err)
	}
}

// dueForStep lists the drive's due videos that need the step selected by
// need: those whose fingerprint is due, then those whose assets are due, each
// in runner order. An asset step waiting for a fingerprint counts as due.
func dueForStep(t *testing.T, cat *Catalog, driveID string, previews bool, need func(GenerationSteps) bool) []string {
	t.Helper()
	ids := listDue(t, cat, driveID, previews, time.Now())
	var out []string
	ctx := context.Background()
	for _, id := range ids {
		_, steps, err := cat.GenerationWork(ctx, id, previews)
		if err != nil {
			t.Fatal(err)
		}
		if need(steps) {
			out = append(out, id)
		}
	}
	return out
}

// listDue returns the fingerprint lane's due videos followed by the asset
// lane's.
func listDue(t *testing.T, cat *Catalog, driveID string, previews bool, now time.Time) []string {
	t.Helper()
	ctx := context.Background()
	fingerprints, err := cat.ListDueFingerprints(ctx, driveID, now, 100000)
	if err != nil {
		t.Fatal(err)
	}
	assets, err := cat.ListDueAssets(ctx, driveID, previews, now, 100000)
	if err != nil {
		t.Fatal(err)
	}
	return append(fingerprints, assets...)
}

func execAll(t *testing.T, cat *Catalog, queries ...string) {
	t.Helper()
	for _, query := range queries {
		if _, err := cat.db.Exec(query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
}

func TestGenerationWorkMediaStepMatchesMediaWants(t *testing.T) {
	ctx := context.Background()
	cat := openGenerationWorkCatalog(t)
	created := time.Now()
	type state struct {
		media, preview string
		duration       int
	}
	var states []state
	for _, media := range []string{"pending", "", "ready", "failed", "stale"} {
		for _, preview := range []string{"pending", "", "ready", "failed", "disabled"} {
			for _, duration := range []int{0, 3, 60} {
				states = append(states, state{media, preview, duration})
			}
		}
	}
	for i, s := range states {
		id := fmt.Sprintf("video-%02d", i)
		seedSettledVideo(t, cat, id, "drive", created)
		execAll(t, cat, fmt.Sprintf(`UPDATE videos SET media_status = '%s', preview_status = '%s', duration_seconds = %d WHERE id = '%s'`,
			s.media, s.preview, s.duration, id))
	}
	for _, previews := range []bool{true, false} {
		for i, s := range states {
			video, steps, err := cat.GenerationWork(ctx, fmt.Sprintf("video-%02d", i), previews)
			if err != nil {
				t.Fatal(err)
			}
			if want := video.MediaWants(previews).Any(); steps.Media != want {
				t.Fatalf("previews=%v state=%+v media step = %v, MediaWants = %v", previews, s, steps.Media, want)
			}
		}
	}
}

func TestGenerationWorkSelectsStepsOfVisibleVideos(t *testing.T) {
	ctx := context.Background()
	cat := openGenerationWorkCatalog(t)
	created := time.Now()
	for _, id := range []string{"settled", "fingerprint", "unknown-size", "media", "thumbnail", "failed-thumbnail", "duration", "duplicate", "hidden"} {
		seedSettledVideo(t, cat, id, "drive", created)
	}
	execAll(t, cat,
		`UPDATE videos SET sampled_sha256 = '', fingerprint_status = 'pending' WHERE id IN ('fingerprint', 'unknown-size', 'duplicate', 'hidden')`,
		`UPDATE videos SET size_bytes = 0 WHERE id = 'unknown-size'`,
		`UPDATE videos SET media_status = 'pending' WHERE id IN ('media', 'duplicate', 'hidden')`,
		`UPDATE videos SET thumbnail_url = '', thumbnail_status = 'pending' WHERE id IN ('thumbnail', 'duplicate')`,
		`UPDATE videos SET thumbnail_url = '', thumbnail_status = 'failed' WHERE id = 'failed-thumbnail'`,
		`UPDATE videos SET duration_seconds = 0 WHERE id IN ('duration', 'duplicate')`,
		`UPDATE videos SET is_canonical = 0 WHERE id = 'duplicate'`,
		`UPDATE videos SET hidden = 1 WHERE id = 'hidden'`,
	)
	for id, want := range map[string]GenerationSteps{
		"settled":          {},
		"fingerprint":      {Fingerprint: true},
		"unknown-size":     {},
		"media":            {Media: true},
		"thumbnail":        {Thumbnail: true},
		"failed-thumbnail": {},
		"duration":         {Duration: true},
		"duplicate":        {Fingerprint: true},
		"hidden":           {},
	} {
		_, steps, err := cat.GenerationWork(ctx, id, true)
		if err != nil {
			t.Fatal(err)
		}
		if steps != want {
			t.Fatalf("%s steps = %+v, want %+v", id, steps, want)
		}
	}
}

func TestDueListsSplitFingerprintsFromAssets(t *testing.T) {
	ctx := context.Background()
	cat := openGenerationWorkCatalog(t)
	base := time.Now().Add(-time.Hour)
	ids := []string{"old-thumbnail", "old-media", "new-fingerprint", "old-duration", "new-media", "settled", "unsampled-media", "other-drive"}
	for i, id := range ids {
		driveID := "drive"
		if id == "other-drive" {
			driveID = "other"
		}
		seedSettledVideo(t, cat, id, driveID, base.Add(time.Duration(i)*time.Minute))
	}
	execAll(t, cat,
		`UPDATE videos SET thumbnail_url = '', thumbnail_status = 'pending' WHERE id = 'old-thumbnail'`,
		`UPDATE videos SET media_status = 'pending' WHERE id IN ('old-media', 'new-media', 'unsampled-media', 'other-drive')`,
		`UPDATE videos SET sampled_sha256 = '', fingerprint_status = 'pending' WHERE id IN ('new-fingerprint', 'unsampled-media', 'other-drive')`,
		`UPDATE videos SET duration_seconds = -1 WHERE id = 'old-duration'`,
	)
	fingerprints, err := cat.ListDueFingerprints(ctx, "drive", time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"new-fingerprint", "unsampled-media"}; !reflect.DeepEqual(fingerprints, want) {
		t.Fatalf("due fingerprints = %v, want %v", fingerprints, want)
	}
	// A media job waits for the video's fingerprint.
	assets, err := cat.ListDueAssets(ctx, "drive", true, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"old-media", "new-media", "old-thumbnail", "old-duration"}; !reflect.DeepEqual(assets, want) {
		t.Fatalf("due assets = %v, want %v", assets, want)
	}
	counts, err := cat.CountDueGeneration(ctx, "drive", true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if counts != (GenerationCounts{Fingerprints: 2, Media: 3, Thumbnails: 1, Durations: 1, Covers: 2}) {
		t.Fatalf("counts = %+v", counts)
	}
	if assets, err = cat.ListDueAssets(ctx, "drive", true, time.Now(), 2); err != nil || len(assets) != 2 {
		t.Fatalf("limited due = %v, %v", assets, err)
	}
	_, steps, err := cat.GenerationWork(ctx, "unsampled-media", true)
	if err != nil || steps != (GenerationSteps{Fingerprint: true, Media: true}) || steps.AssetsDue() {
		t.Fatalf("unsampled steps = %+v (assets due %v), %v", steps, steps.AssetsDue(), err)
	}
}

func TestDeferGenerationUntilRetryOrSourceChange(t *testing.T) {
	ctx := context.Background()
	cat := openGenerationWorkCatalog(t)
	now := time.Now()
	seedSettledVideo(t, cat, "video", "drive", now)
	execAll(t, cat, `UPDATE videos SET duration_seconds = 0, media_status = 'failed' WHERE id = 'video'`)
	due := func(at time.Time) bool {
		t.Helper()
		ids := listDue(t, cat, "drive", true, at)
		counts, err := cat.CountDueGeneration(ctx, "drive", true, at)
		if err != nil {
			t.Fatal(err)
		}
		if (len(ids) > 0) != counts.Any() {
			t.Fatalf("listed %v, counted %+v", ids, counts)
		}
		return len(ids) > 0
	}
	if !due(now) {
		t.Fatal("missing duration is not due")
	}
	if err := cat.DeferGeneration(ctx, "video", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if due(now) || !due(now.Add(2*time.Hour)) {
		t.Fatal("deferral did not hold until its time")
	}
	if _, steps, err := cat.GenerationWork(ctx, "video", true); err != nil || !steps.Duration {
		t.Fatalf("taken video steps = %+v, %v", steps, err)
	}
	if _, err := cat.ResetFailedGeneration(ctx, "drive", GenerationKinds{Previews: true}); err != nil {
		t.Fatal(err)
	}
	if !due(now) {
		t.Fatal("retry did not lift the deferral")
	}

	if err := cat.DeferGeneration(ctx, "video", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := cat.RequestMediaRenewal(ctx, []string{"video"}); err != nil {
		t.Fatal(err)
	}
	if !due(now) {
		t.Fatal("renewal request did not lift the deferral")
	}

	if err := cat.DeferGeneration(ctx, "video", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	execAll(t, cat, `UPDATE videos SET media_status = 'stale' WHERE id = 'video'`)
	if promoted, err := cat.PromoteStaleMedia(ctx, 10); err != nil || len(promoted) != 1 {
		t.Fatalf("promoted = %d, %v", len(promoted), err)
	}
	if !due(now) {
		t.Fatal("stale media promotion did not lift the deferral")
	}

	for _, change := range []struct {
		name  string
		patch VideoMetaPatch
	}{
		{"suspected replacement", VideoMetaPatch{VerifyContent: true, SourceModTime: "t2"}},
		{"proven replacement", VideoMetaPatch{ResetContent: true, Size: 2000}},
	} {
		if err := cat.DeferGeneration(ctx, "video", now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := cat.UpdateVideoMeta(ctx, "video", change.patch); err != nil {
			t.Fatal(err)
		}
		if !due(now) {
			t.Fatalf("%s did not lift the deferral", change.name)
		}
	}
}

func TestCountDueGenerationCountsEachCoverOnce(t *testing.T) {
	ctx := context.Background()
	cat := openGenerationWorkCatalog(t)
	now := time.Now()
	for _, id := range []string{"cover", "duration", "both"} {
		seedSettledVideo(t, cat, id, "drive", now)
	}
	execAll(t, cat,
		`UPDATE videos SET thumbnail_url = '', thumbnail_status = 'pending' WHERE id IN ('cover', 'both')`,
		`UPDATE videos SET duration_seconds = 0 WHERE id IN ('duration', 'both')`,
	)
	counts, err := cat.CountDueGeneration(ctx, "drive", true, now)
	if err != nil {
		t.Fatal(err)
	}
	if counts != (GenerationCounts{Thumbnails: 2, Durations: 2, Covers: 3}) {
		t.Fatalf("counts = %+v", counts)
	}
}

func TestDueQueriesReadOnlyThroughPartialIndexes(t *testing.T) {
	ctx := context.Background()
	cat := openGenerationWorkCatalog(t)
	// Production catalogs carry no ANALYZE statistics, so neither does this one.
	for i := range 200 {
		seedSettledVideo(t, cat, fmt.Sprintf("video-%03d", i), "drive", time.Now())
	}
	queries := map[string]struct {
		query   dueQuery
		indexes []string
	}{
		"fingerprints":         {dueFingerprintsQuery(), []string{"idx_videos_due_fingerprints"}},
		"assets, previews on":  {dueAssetsQuery(true), []string{"idx_videos_due_generation", "idx_videos_due_previews"}},
		"assets, previews off": {dueAssetsQuery(false), []string{"idx_videos_due_generation"}},
		"counts, previews on":  {dueCountsQuery(true), []string{"idx_videos_due_generation", "idx_videos_due_previews"}},
		"counts, previews off": {dueCountsQuery(false), []string{"idx_videos_due_generation"}},
	}
	for name, tc := range queries {
		args := tc.query.args("drive", time.Now())
		if strings.HasSuffix(strings.TrimSpace(tc.query.sql), "LIMIT ?") {
			args = append(args, 32)
		}
		rows, err := cat.db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+tc.query.sql, args...)
		if err != nil {
			t.Fatalf("%s explain: %v", name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				rows.Close()
				t.Fatalf("%s scan: %v", name, err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		joined := strings.Join(plan, "\n")
		for _, index := range tc.indexes {
			if !strings.Contains(joined, "USING INDEX "+index+" ") {
				t.Errorf("%s plan does not read %s:\n%s", name, index, joined)
			}
		}
		if strings.Contains(joined, "SCAN videos") {
			t.Errorf("%s plan scans the table:\n%s", name, joined)
		}
	}
}

func TestPreviewWorkStaysOutOfTheGenerationIndexWhileOff(t *testing.T) {
	ctx := context.Background()
	cat := openGenerationWorkCatalog(t)
	// Teasers stay pending while previews are off; the generation index must
	// not grow with them, or the due queries would read the whole drive.
	seedSettledVideo(t, cat, "teaser-only", "drive", time.Now())
	execAll(t, cat, `UPDATE videos SET preview_status = 'pending' WHERE id = 'teaser-only'`)
	var inGeneration, inPreviews int
	if err := cat.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM videos INDEXED BY idx_videos_due_generation
		WHERE drive_id = 'drive' AND generation_after <= 0 AND `+generationIndexSQL).Scan(&inGeneration); err != nil {
		t.Fatal(err)
	}
	if err := cat.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM videos INDEXED BY idx_videos_due_previews
		WHERE drive_id = 'drive' AND generation_after <= 0 AND `+previewIndexSQL).Scan(&inPreviews); err != nil {
		t.Fatal(err)
	}
	if inGeneration != 0 || inPreviews != 1 {
		t.Fatalf("generation index rows = %d, preview index rows = %d; want the teaser only in the preview index", inGeneration, inPreviews)
	}
	if ids := listDue(t, cat, "drive", false, time.Now()); len(ids) != 0 {
		t.Fatalf("due with previews off = %v", ids)
	}
	if ids := listDue(t, cat, "drive", true, time.Now()); len(ids) != 1 {
		t.Fatalf("due with previews on = %v, want the teaser", ids)
	}
}

func needsDuration(s GenerationSteps) bool    { return s.Duration }
func needsMedia(s GenerationSteps) bool       { return s.Media }
func needsThumbnail(s GenerationSteps) bool   { return s.Thumbnail }
func needsFingerprint(s GenerationSteps) bool { return s.Fingerprint }

func TestLiftPreviewDeferralsLeavesOtherWorkDeferred(t *testing.T) {
	ctx := context.Background()
	cat := openGenerationWorkCatalog(t)
	now := time.Now()
	seedSettledVideo(t, cat, "teaser", "drive", now)
	seedSettledVideo(t, cat, "duration", "drive", now)
	execAll(t, cat,
		`UPDATE videos SET preview_status = 'pending' WHERE id = 'teaser'`,
		`UPDATE videos SET duration_seconds = 0 WHERE id = 'duration'`)
	for _, id := range []string{"teaser", "duration"} {
		if err := cat.DeferGeneration(ctx, id, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if lifted, err := cat.LiftPreviewDeferrals(ctx); err != nil || lifted != 1 {
		t.Fatalf("lifted = %d, err = %v; want the teaser only", lifted, err)
	}
	if ids := listDue(t, cat, "drive", true, now); len(ids) != 1 || ids[0] != "teaser" {
		t.Fatalf("due = %v, want the teaser while the failed duration stays deferred", ids)
	}
}
