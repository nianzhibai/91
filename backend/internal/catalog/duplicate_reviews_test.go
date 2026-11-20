package catalog

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/mediasig"
)

func openReviewTestCatalog(t *testing.T, ids ...string) *Catalog {
	t.Helper()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	for i, id := range ids {
		if err := cat.UpsertVideo(context.Background(), &Video{
			ID: id, DriveID: "drive", FileID: id, Title: id, Size: int64(100 * (i + 1)), DurationSeconds: 300,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return cat
}

func probableReview(left, right string) dedupe.Review {
	return dedupe.Review{LeftID: left, RightID: right, Match: dedupe.Match{
		Stage: dedupe.StageSignature, Reason: dedupe.ReasonSignature, LeftID: left, RightID: right,
		Signature: &mediasig.Comparison{Verdict: mediasig.VerdictProbable, MatchedWindows: 2, Windows: 4},
	}}
}

func TestDuplicateReviewsRefreshKeepsDismissals(t *testing.T) {
	ctx := context.Background()
	cat := openReviewTestCatalog(t, "a", "b", "c")
	if err := cat.ReplaceDuplicateReviews(ctx, []dedupe.Review{probableReview("b", "a"), probableReview("a", "c")}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	list, err := cat.ListDuplicateReviews(ctx, 1, 10)
	if err != nil || list.Total != 2 || len(list.Items) != 2 {
		t.Fatalf("list = %+v err = %v, want two pending reviews", list, err)
	}
	firstSeen := map[string]int64{}
	for _, item := range list.Items {
		firstSeen[item.Left.ID+"-"+item.Right.ID] = item.FirstSeenAt.UnixMilli()
		if item.Evidence.Signature == nil || item.Evidence.Signature.Verdict != mediasig.VerdictProbable {
			t.Fatalf("evidence = %+v", item.Evidence)
		}
	}
	if _, ok := firstSeen["a-b"]; !ok {
		t.Fatalf("pairs = %v, want pairs stored in id order", firstSeen)
	}

	if err := cat.DismissDuplicateReview(ctx, "b", "a"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if err := cat.DismissDuplicateReview(ctx, "a", "b"); !errors.Is(err, ErrDuplicateReviewNotFound) {
		t.Fatalf("second dismiss err = %v, want not found", err)
	}
	// The next plan reports both pairs again: the dismissal holds, the other
	// pair keeps its first-seen time.
	if err := cat.ReplaceDuplicateReviews(ctx, []dedupe.Review{probableReview("a", "b"), probableReview("a", "c")}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	list, err = cat.ListDuplicateReviews(ctx, 1, 10)
	if err != nil || list.Total != 1 || list.Items[0].Left.ID != "a" || list.Items[0].Right.ID != "c" ||
		list.Items[0].FirstSeenAt.UnixMilli() != firstSeen["a-c"] {
		t.Fatalf("list = %+v err = %v, want only a-c with its first-seen time", list, err)
	}
	distinct, err := cat.DismissedDuplicatePairs(ctx)
	if err != nil || !distinct("b", "a") || distinct("a", "c") {
		t.Fatalf("dismissed pairs wrong, err = %v", err)
	}

	// A plan that no longer reports a-c removes it.
	if err := cat.ReplaceDuplicateReviews(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if list, _ := cat.ListDuplicateReviews(ctx, 1, 10); list.Total != 0 {
		t.Fatalf("list = %+v, want no pending reviews", list)
	}
}

func TestMergeDuplicateReviewRetiresTheMergedVideo(t *testing.T) {
	ctx := context.Background()
	cat := openReviewTestCatalog(t, "keep", "merge", "other")
	if err := cat.ReplaceDuplicateReviews(ctx, []dedupe.Review{probableReview("keep", "merge"), probableReview("merge", "other")}); err != nil {
		t.Fatal(err)
	}
	if err := cat.MergeDuplicateReview(ctx, "keep", "other"); !errors.Is(err, ErrDuplicateReviewNotFound) {
		t.Fatalf("merge of an unreviewed pair err = %v, want not found", err)
	}
	if err := cat.MergeDuplicateReview(ctx, "keep", "merge"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if _, err := cat.GetVideo(ctx, "merge"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("merged video lookup = %v, want retired", err)
	}
	merged, err := cat.GetMergedVideo(ctx, "merge")
	if err != nil || merged.CanonicalVideoID != "keep" {
		t.Fatalf("merge record = %+v err = %v", merged, err)
	}
	var origin, selection string
	if err := cat.db.QueryRowContext(ctx, `SELECT origin, selection_reason FROM duplicate_records WHERE video_id = 'merge'`).Scan(&origin, &selection); err != nil ||
		origin != DuplicateOriginReview || selection != "manual_review" {
		t.Fatalf("decision origin = %q selection = %q err = %v", origin, selection, err)
	}
	if list, _ := cat.ListDuplicateReviews(ctx, 1, 10); list.Total != 0 {
		t.Fatalf("reviews = %+v, want none after the merge", list)
	}
}
