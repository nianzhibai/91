package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/video-site/backend/internal/auth"
	"github.com/video-site/backend/internal/catalog"
)

// shortsEventsBodyLimit bounds one report. The client flushes small batches;
// the limit only protects the endpoint from a hostile caller.
const shortsEventsBodyLimit = 32 * 1024

type shortsEventsRequest struct {
	Events []shortsEventPayload `json:"events"`
}

type shortsEventPayload struct {
	VideoID   string `json:"videoId"`
	Kind      string `json:"kind"`
	WatchMs   int    `json:"watchMs"`
	LoopCount int    `json:"loopCount"`
	Position  int    `json:"position"`
}

// handleShortsEvents stores a batch of settled watches. It is the only write
// path that feeds the recommendation signals, so it is deliberately tolerant:
// unknown video ids are dropped instead of failing the batch, and a repeated
// report is harmless because each row is an observation, not a state change.
func (s *Server) handleShortsEvents(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		writeErr(w, r, http.StatusUnauthorized, errors.New("authentication required"))
		return
	}

	var body shortsEventsRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, shortsEventsBodyLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeErr(w, r, http.StatusBadRequest, errors.New("request body must contain one JSON object"))
		return
	}

	events := make([]catalog.ShortsEvent, 0, len(body.Events))
	for _, payload := range body.Events {
		events = append(events, catalog.ShortsEvent{
			VideoID:   payload.VideoID,
			Kind:      payload.Kind,
			WatchMs:   payload.WatchMs,
			LoopCount: payload.LoopCount,
			Position:  payload.Position,
		})
	}

	stored, err := s.Catalog.RecordShortsEvents(r.Context(), userID, events, s.shortsNow())
	if err != nil {
		if errors.Is(err, catalog.ErrInvalidShortsEvent) {
			writeErr(w, r, http.StatusBadRequest, err)
			return
		}
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]int{"stored": stored})
}
