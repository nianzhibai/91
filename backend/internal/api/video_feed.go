package api

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

const (
	defaultVideoFeedBatchSize = 20
	maxVideoFeedBatchSize     = 240
	videoFeedLookupChunk      = 32
	videoFeedTTL              = 24 * time.Hour
	maxVideoFeedSessions      = 64
)

var (
	errVideoFeedExpired     = errors.New("video feed expired")
	errInvalidVideoFeedKind = errors.New("invalid video feed kind")
)

// videoFeedSnapshotKey identifies deterministic feeds whose complete ordered
// ID set is identical for every authenticated user. Batch size is deliberately
// absent because it does not change the logical result set.
type videoFeedSnapshotKey struct {
	keyword string
	tag     string
	sort    string
}

type videoFeedSnapshotSeed struct {
	videoIDs  []string
	sharedKey *videoFeedSnapshotKey
}

type videoFeedResponse struct {
	Items      []VideoCardDTO `json:"items"`
	Total      int            `json:"total"`
	FeedToken  string         `json:"feedToken"`
	NextCursor int            `json:"nextCursor"`
	Exhausted  bool           `json:"exhausted"`
}

// handleVideoFeed creates an immutable ordered ID snapshot when the first
// response has a continuation, then serves idempotent token/cursor reads from
// it. A result set completed by the first response is never retained. Live
// inserts, deletes, thumbnail readiness and reaction changes therefore cannot
// shift a later batch boundary.
func (s *Server) handleVideoFeed(w http.ResponseWriter, r *http.Request) {
	count, err := feedQueryInt(r, "count", defaultVideoFeedBatchSize)
	if err != nil || count < 1 || count > maxVideoFeedBatchSize {
		writeErr(w, r, http.StatusBadRequest, errors.New("invalid video feed count"))
		return
	}
	cursor, err := feedQueryInt(r, "cursor", 0)
	if err != nil || cursor < 0 {
		writeErr(w, r, http.StatusBadRequest, errors.New("invalid video feed cursor"))
		return
	}

	feedToken := strings.TrimSpace(r.URL.Query().Get("feedToken"))
	if len(feedToken) > 128 {
		writeErr(w, r, http.StatusBadRequest, errors.New("invalid video feed token"))
		return
	}

	newFeed := feedToken == ""
	var videoIDs []string
	var snapshotSeed videoFeedSnapshotSeed
	if newFeed {
		if cursor != 0 {
			writeErr(w, r, http.StatusBadRequest, errors.New("video feed cursor requires a token"))
			return
		}
		snapshotSeed, err = s.newVideoFeedSnapshot(r)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, errInvalidVideoFeedKind) {
				status = http.StatusBadRequest
			}
			writeErr(w, r, status, err)
			return
		}
		videoIDs = snapshotSeed.videoIDs
	} else {
		videoIDs, err = s.loadVideoFeed(feedToken)
		if err != nil {
			writeErr(w, r, http.StatusGone, err)
			return
		}
	}

	if cursor > len(videoIDs) {
		writeErr(w, r, http.StatusBadRequest, errors.New("video feed cursor is outside the snapshot"))
		return
	}

	videos, nextCursor, err := s.loadVisibleVideoFeedBatch(
		r.Context(),
		videoIDs,
		cursor,
		count,
	)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	exhausted := nextCursor >= len(videoIDs)
	if newFeed && !exhausted {
		candidateToken, err := newFeedToken()
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, err)
			return
		}
		feedToken, videoIDs = s.storeVideoFeed(
			candidateToken,
			videoIDs,
			snapshotSeed.sharedKey,
		)
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, videoFeedResponse{
		Items:      mapVideoSummaries(videos, s.previewsEnabled()),
		Total:      len(videoIDs),
		FeedToken:  feedToken,
		NextCursor: nextCursor,
		Exhausted:  exhausted,
	})
}

func (s *Server) newVideoFeedSnapshot(r *http.Request) (videoFeedSnapshotSeed, error) {
	switch kind := strings.TrimSpace(r.URL.Query().Get("kind")); kind {
	case "listing", "latest":
		params := publicListParams(r)
		if kind == "latest" {
			params.Keyword = ""
			params.Tag = ""
			params.Sort = catalog.SortLatest
			params.PreferReadyThumbnails = true
		}
		videoIDs, err := s.Catalog.ListVideoIDs(r.Context(), params, 0)
		if err != nil {
			return videoFeedSnapshotSeed{}, err
		}
		sortKey := params.Sort
		if sortKey == "" {
			sortKey = catalog.SortLatest
		}
		sharedKey := videoFeedSnapshotKey{
			keyword: params.Keyword,
			tag:     params.Tag,
			sort:    sortKey,
		}
		return videoFeedSnapshotSeed{videoIDs: videoIDs, sharedKey: &sharedKey}, nil
	case "recommend":
		readyIDs, pendingIDs, err := s.Catalog.ListVisibleVideoIDsByThumbnailReadiness(r.Context())
		if err != nil {
			return videoFeedSnapshotSeed{}, err
		}
		rand.Shuffle(len(readyIDs), func(i, j int) {
			readyIDs[i], readyIDs[j] = readyIDs[j], readyIDs[i]
		})
		rand.Shuffle(len(pendingIDs), func(i, j int) {
			pendingIDs[i], pendingIDs[j] = pendingIDs[j], pendingIDs[i]
		})
		return videoFeedSnapshotSeed{videoIDs: append(readyIDs, pendingIDs...)}, nil
	default:
		return videoFeedSnapshotSeed{}, errInvalidVideoFeedKind
	}
}

func (s *Server) videoFeedTime() time.Time {
	if s.videoFeedNow != nil {
		return s.videoFeedNow()
	}
	return time.Now()
}

func (s *Server) storeVideoFeed(
	token string,
	videoIDs []string,
	sharedKey *videoFeedSnapshotKey,
) (string, []string) {
	return s.videoFeeds.put(
		token,
		videoIDs,
		s.videoFeedTime(),
		videoFeedTTL,
		maxVideoFeedSessions,
		sharedKey,
		slices.Equal[[]string],
	)
}

func (s *Server) loadVideoFeed(token string) ([]string, error) {
	videoIDs, ok := s.videoFeeds.get(token, s.videoFeedTime(), videoFeedTTL)
	if !ok {
		return nil, errVideoFeedExpired
	}
	return videoIDs, nil
}

// loadVisibleVideoFeedBatch skips snapshot entries that stopped being public
// while still advancing the cursor over their positions.
func (s *Server) loadVisibleVideoFeedBatch(
	ctx context.Context,
	videoIDs []string,
	cursor int,
	count int,
) ([]*catalog.VideoSummary, int, error) {
	videos := make([]*catalog.VideoSummary, 0, count)
	lookupSize := videoFeedLookupSize(count)
	for cursor < len(videoIDs) && len(videos) < count {
		end := cursor + lookupSize
		if end > len(videoIDs) {
			end = len(videoIDs)
		}
		visible, err := s.Catalog.VisibleVideoSummariesByIDs(ctx, videoIDs[cursor:end])
		if err != nil {
			return nil, cursor, err
		}
		visibleByID := make(map[string]*catalog.VideoSummary, len(visible))
		for _, video := range visible {
			visibleByID[video.ID] = video
		}
		for cursor < end && len(videos) < count {
			videoID := videoIDs[cursor]
			cursor++
			if video := visibleByID[videoID]; video != nil {
				videos = append(videos, video)
			}
		}
	}
	return videos, cursor, nil
}

func videoFeedLookupSize(count int) int {
	if count > videoFeedLookupChunk {
		return count
	}
	return videoFeedLookupChunk
}
