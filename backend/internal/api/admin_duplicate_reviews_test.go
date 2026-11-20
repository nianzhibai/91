package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/mediasig"
)

func TestAdminDuplicateReviewRoutes(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	for _, id := range []string{"a", "b", "c"} {
		if err := cat.UpsertVideo(ctx, &catalog.Video{ID: id, DriveID: "drive", FileID: id, Title: "title " + id, Size: 100, DurationSeconds: 300}); err != nil {
			t.Fatal(err)
		}
	}
	review := func(left, right string) dedupe.Review {
		return dedupe.Review{LeftID: left, RightID: right, Match: dedupe.Match{
			Stage: dedupe.StageSignature, Reason: dedupe.ReasonSignature, LeftID: left, RightID: right,
			Signature: &mediasig.Comparison{Verdict: mediasig.VerdictProbable, Windows: 4, MatchedWindows: 2, Frames: 20, MatchedFrames: 10, MedianDistance: 8},
		}}
	}
	if err := cat.ReplaceDuplicateReviews(ctx, []dedupe.Review{review("a", "b"), review("a", "c")}); err != nil {
		t.Fatal(err)
	}
	a := AdminServer{Catalog: cat, OnMergeDuplicateReview: cat.MergeDuplicateReview}
	r := chi.NewRouter()
	r.Get("/duplicate-reviews", a.handleDuplicateReviews)
	r.Post("/duplicate-reviews/merge", a.handleMergeDuplicateReview)
	r.Post("/duplicate-reviews/dismiss", a.handleDismissDuplicateReview)
	request := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		if w.Code != want {
			t.Fatalf("%s %s: status=%d body=%s", method, path, w.Code, w.Body.String())
		}
		return w
	}
	list := func() duplicateReviewList {
		t.Helper()
		var out duplicateReviewList
		if err := json.Unmarshal(request("GET", "/duplicate-reviews?page=1&size=10", "", 200).Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	got := list()
	if got.Total != 2 || len(got.Items) != 2 {
		t.Fatalf("list = %+v, want two reviews", got)
	}
	item := got.Items[0]
	if item.Left.Title == "" || item.Evidence.Verdict != "probable" || item.Evidence.MatchedWindows != 2 || item.FirstSeenAt.IsZero() {
		t.Fatalf("item = %+v", item)
	}

	request("POST", "/duplicate-reviews/dismiss", `{"leftVideoId":"a"}`, 400)
	request("POST", "/duplicate-reviews/dismiss", `{"leftVideoId":"c","rightVideoId":"a"}`, 200)
	request("POST", "/duplicate-reviews/dismiss", `{"leftVideoId":"a","rightVideoId":"c"}`, 404)
	request("POST", "/duplicate-reviews/merge", `{"keepVideoId":"a","mergeVideoId":"c"}`, 404)
	request("POST", "/duplicate-reviews/merge", `{"keepVideoId":"a"}`, 400)
	request("POST", "/duplicate-reviews/merge", `{"keepVideoId":"a","mergeVideoId":"b"}`, 200)
	if _, err := cat.GetMergedVideo(ctx, "b"); err != nil {
		t.Fatalf("b not merged: %v", err)
	}
	if got := list(); got.Total != 0 {
		t.Fatalf("list = %+v, want no reviews left", got)
	}
}
