package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

// duplicateReviewEvidenceDTO is the comparison that put a pair up for
// review. LeftID and RightID name the compared videos; one may already have
// been merged into a video of the reviewed pair.
type duplicateReviewEvidenceDTO struct {
	LeftID         string `json:"leftId"`
	RightID        string `json:"rightId"`
	Verdict        string `json:"verdict"`
	Windows        int    `json:"windows"`
	MatchedWindows int    `json:"matchedWindows"`
	Frames         int    `json:"frames"`
	MatchedFrames  int    `json:"matchedFrames"`
	MedianDistance int    `json:"medianDistance"`
	Static         bool   `json:"static"`
}

type duplicateReviewDTO struct {
	Left        adminVideoDTO              `json:"left"`
	Right       adminVideoDTO              `json:"right"`
	Evidence    duplicateReviewEvidenceDTO `json:"evidence"`
	FirstSeenAt time.Time                  `json:"firstSeenAt"`
}

type duplicateReviewList struct {
	Items []duplicateReviewDTO `json:"items"`
	Total int                  `json:"total"`
	Page  int                  `json:"page"`
	Size  int                  `json:"size"`
}

type duplicateReviewMergeRequest struct {
	KeepVideoID  string `json:"keepVideoId"`
	MergeVideoID string `json:"mergeVideoId"`
}

type duplicateReviewDismissRequest struct {
	LeftVideoID  string `json:"leftVideoId"`
	RightVideoID string `json:"rightVideoId"`
}

func mapDuplicateReview(review *catalog.DuplicateReview) duplicateReviewDTO {
	evidence := duplicateReviewEvidenceDTO{LeftID: review.Evidence.LeftID, RightID: review.Evidence.RightID}
	if c := review.Evidence.Signature; c != nil {
		evidence.Verdict = c.Verdict.String()
		evidence.Windows, evidence.MatchedWindows = c.Windows, c.MatchedWindows
		evidence.Frames, evidence.MatchedFrames = c.Frames, c.MatchedFrames
		evidence.MedianDistance, evidence.Static = c.MedianDistance, c.Static
	}
	return duplicateReviewDTO{
		Left: mapAdminVideo(review.Left), Right: mapAdminVideo(review.Right),
		Evidence: evidence, FirstSeenAt: review.FirstSeenAt,
	}
}

func (a *AdminServer) handleDuplicateReviews(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("size"))
	list, err := a.Catalog.ListDuplicateReviews(r.Context(), page, size)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	out := duplicateReviewList{Items: make([]duplicateReviewDTO, 0, len(list.Items)), Total: list.Total, Page: list.Page, Size: list.Size}
	for _, review := range list.Items {
		out.Items = append(out.Items, mapDuplicateReview(review))
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

func (a *AdminServer) handleMergeDuplicateReview(w http.ResponseWriter, r *http.Request) {
	var body duplicateReviewMergeRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		strings.TrimSpace(body.KeepVideoID) == "" || strings.TrimSpace(body.MergeVideoID) == "" {
		writeErr(w, r, http.StatusBadRequest, errors.New("keepVideoId and mergeVideoId are required"))
		return
	}
	if a.OnMergeDuplicateReview == nil {
		writeServiceUnavailable(w, r, "merge duplicate review", errors.New("merging is unavailable"))
		return
	}
	switch err := a.OnMergeDuplicateReview(r.Context(), body.KeepVideoID, body.MergeVideoID); {
	case errors.Is(err, catalog.ErrDuplicateReviewNotFound):
		writeErr(w, r, http.StatusNotFound, err)
	case errors.Is(err, catalog.ErrDuplicatePlanStale):
		writeErr(w, r, http.StatusConflict, err)
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusOK, body)
	}
}

func (a *AdminServer) handleDismissDuplicateReview(w http.ResponseWriter, r *http.Request) {
	var body duplicateReviewDismissRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		strings.TrimSpace(body.LeftVideoID) == "" || strings.TrimSpace(body.RightVideoID) == "" {
		writeErr(w, r, http.StatusBadRequest, errors.New("leftVideoId and rightVideoId are required"))
		return
	}
	switch err := a.Catalog.DismissDuplicateReview(r.Context(), body.LeftVideoID, body.RightVideoID); {
	case errors.Is(err, catalog.ErrDuplicateReviewNotFound):
		writeErr(w, r, http.StatusNotFound, err)
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusOK, body)
	}
}
