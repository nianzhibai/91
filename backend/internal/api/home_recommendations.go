package api

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

const (
	homeRecommendationSessionTTL  = 24 * time.Hour
	maxHomeRecommendationSessions = 256
	homeRecommendationLookupChunk = 32
	homeLatestSnapshotSize        = 96
	homeLatestSnapshotTTL         = 30 * time.Second
)

// homeRecommendationSession owns independent random and latest snapshots for
// one login session. Cursors advance only after successful requests, and each
// mutex serializes concurrent refreshes of its own section.
type homeRecommendationSession struct {
	requestMu       sync.Mutex
	roundVideoIDs   []string
	roundCursor     int
	latestRequestMu sync.Mutex
	latestVideoIDs  []string
	latestCursor    int
	lastAccess      time.Time
}

// nextHomeRecommendationBatch reads from the current shuffled library snapshot.
// When a request reaches the end of a round, it may continue into a new round to
// keep the home grid full. IDs already returned earlier in that same response are
// moved to the end of the new round, preventing duplicate cards within one grid
// without marking those IDs as consumed in the new round.
func (s *Server) nextHomeRecommendationBatch(
	ctx context.Context,
	session *homeRecommendationSession,
	count int,
) ([]*catalog.Video, []string, int, error) {
	return s.nextHomeSnapshotBatch(
		ctx,
		session.roundVideoIDs,
		session.roundCursor,
		count,
		func() ([]string, error) {
			readyIDs, pendingIDs, err := s.Catalog.ListVisibleVideoIDsByThumbnailReadiness(ctx)
			if err != nil {
				return nil, err
			}
			rand.Shuffle(len(readyIDs), func(i, j int) {
				readyIDs[i], readyIDs[j] = readyIDs[j], readyIDs[i]
			})
			rand.Shuffle(len(pendingIDs), func(i, j int) {
				pendingIDs[i], pendingIDs[j] = pendingIDs[j], pendingIDs[i]
			})
			return append(readyIDs, pendingIDs...), nil
		},
	)
}

func (s *Server) nextHomeLatestBatch(
	ctx context.Context,
	session *homeRecommendationSession,
	count int,
) ([]*catalog.Video, []string, int, error) {
	freshVideoIDs, err := s.cachedHomeLatestVideoIDs(ctx)
	if err != nil {
		return nil, nil, 0, err
	}
	latestVideoIDs, latestCursor := mergeLatestHomeSnapshot(
		session.latestVideoIDs,
		session.latestCursor,
		freshVideoIDs,
	)
	return s.nextHomeSnapshotBatch(
		ctx,
		latestVideoIDs,
		latestCursor,
		count,
		func() ([]string, error) {
			return freshVideoIDs, nil
		},
	)
}

// cachedHomeLatestVideoIDs shares the public latest window across login
// sessions. The short TTL intentionally bounds how long a newly scanned video
// can take to appear while avoiding an incomplete web of write-path
// invalidation hooks. Holding the mutex through the refresh also prevents a
// cold-cache request burst from rebuilding the same snapshot concurrently.
func (s *Server) cachedHomeLatestVideoIDs(ctx context.Context) ([]string, error) {
	now := s.homeRecommendationsNow()
	s.homeLatestSnapshotMu.Lock()
	defer s.homeLatestSnapshotMu.Unlock()

	if now.Before(s.homeLatestSnapshotUntil) {
		return append([]string(nil), s.homeLatestSnapshot...), nil
	}
	ids, err := s.Catalog.ListVisibleVideoIDsLatest(ctx, homeLatestSnapshotSize)
	if err != nil {
		return nil, err
	}
	s.homeLatestSnapshot = append(s.homeLatestSnapshot[:0], ids...)
	s.homeLatestSnapshotUntil = now.Add(homeLatestSnapshotTTL)
	return append([]string(nil), ids...), nil
}

// mergeLatestHomeSnapshot keeps consumed IDs as a round marker, then rebuilds
// the unconsumed tail from the current latest window. New videos therefore take
// their correct ready/latest position without making consumed cards repeat.
func mergeLatestHomeSnapshot(current []string, cursor int, fresh []string) ([]string, int) {
	if len(current) == 0 || cursor <= 0 {
		return fresh, 0
	}
	if cursor > len(current) {
		cursor = len(current)
	}

	consumed := append([]string(nil), current[:cursor]...)
	consumedSet := make(map[string]struct{}, len(consumed))
	for _, id := range consumed {
		consumedSet[id] = struct{}{}
	}
	merged := make([]string, 0, len(consumed)+len(fresh))
	merged = append(merged, consumed...)
	for _, id := range fresh {
		if _, alreadyConsumed := consumedSet[id]; !alreadyConsumed {
			merged = append(merged, id)
		}
	}
	return merged, len(consumed)
}

// nextHomeSnapshotBatch consumes one stable ID snapshot, loading only the
// requested cards. If a response crosses a round boundary, entries already in
// that response are postponed in the new round so a grid never duplicates a
// card.
func (s *Server) nextHomeSnapshotBatch(
	ctx context.Context,
	roundVideoIDs []string,
	roundCursor int,
	count int,
	loadRound func() ([]string, error),
) ([]*catalog.Video, []string, int, error) {
	items := make([]*catalog.Video, 0, count)
	selected := make(map[string]struct{}, count)
	// The current snapshot may contain IDs that became hidden or were deleted
	// after it was built. Allow one refresh, but stop once a freshly loaded
	// round is exhausted without yielding any visible videos; otherwise a stale
	// loader could make this loop reload the same unavailable IDs forever.
	freshRoundStartItemCount := -1

	for len(items) < count {
		eligibleEnd := len(roundVideoIDs)
		if roundCursor >= len(roundVideoIDs) {
			freshVideoIDs, err := loadRound()
			if err != nil {
				return nil, nil, 0, err
			}
			roundVideoIDs = freshVideoIDs
			roundCursor = 0
			eligibleEnd = len(roundVideoIDs)
			freshRoundStartItemCount = len(items)
			if len(roundVideoIDs) == 0 {
				break
			}

			if len(selected) > 0 {
				roundVideoIDs, eligibleEnd = postponeSelectedHomeVideoIDs(roundVideoIDs, selected)
				if eligibleEnd == 0 {
					// The library is smaller than the requested grid. Keep the freshly
					// created round intact for the next request instead of duplicating
					// cards in this response.
					break
				}
			}
		}

		loaded, nextCursor, err := s.loadHomeRecommendationRange(
			ctx,
			roundVideoIDs,
			roundCursor,
			eligibleEnd,
			count-len(items),
		)
		if err != nil {
			return nil, nil, 0, err
		}
		roundCursor = nextCursor
		for _, video := range loaded {
			if video == nil {
				continue
			}
			if _, exists := selected[video.ID]; exists {
				continue
			}
			selected[video.ID] = struct{}{}
			items = append(items, video)
		}

		if roundCursor < eligibleEnd {
			// The requested batch is full. loadHomeRecommendationRange only
			// stops before eligibleEnd when it has returned the requested count.
			break
		}
		if eligibleEnd < len(roundVideoIDs) {
			// The tail belongs to the newly-started round but was shown at the
			// end of the previous round in this response. Leave it for next time.
			break
		}
		if freshRoundStartItemCount >= 0 && len(items) == freshRoundStartItemCount {
			break
		}
	}

	return items, roundVideoIDs, roundCursor, nil
}

func (s *Server) loadHomeRecommendationRange(
	ctx context.Context,
	videoIDs []string,
	cursor int,
	end int,
	count int,
) ([]*catalog.Video, int, error) {
	videos := make([]*catalog.Video, 0, count)
	for cursor < end && len(videos) < count {
		chunkEnd := cursor + homeRecommendationLookupChunk
		if chunkEnd > end {
			chunkEnd = end
		}
		visible, err := s.Catalog.VisibleVideosByIDs(ctx, videoIDs[cursor:chunkEnd])
		if err != nil {
			return nil, cursor, err
		}
		visibleByID := make(map[string]*catalog.Video, len(visible))
		for _, video := range visible {
			visibleByID[video.ID] = video
		}

		for cursor < chunkEnd && len(videos) < count {
			videoID := videoIDs[cursor]
			cursor++
			if video := visibleByID[videoID]; video != nil {
				videos = append(videos, video)
			}
		}
	}
	return videos, cursor, nil
}

func postponeSelectedHomeVideoIDs(videoIDs []string, selected map[string]struct{}) ([]string, int) {
	available := make([]string, 0, len(videoIDs))
	postponed := make([]string, 0, len(selected))
	for _, id := range videoIDs {
		if _, exists := selected[id]; exists {
			postponed = append(postponed, id)
			continue
		}
		available = append(available, id)
	}
	eligibleEnd := len(available)
	return append(available, postponed...), eligibleEnd
}

func (s *Server) homeRecommendationsNow() time.Time {
	if s.homeRecommendationNow != nil {
		return s.homeRecommendationNow()
	}
	return time.Now()
}

func (s *Server) homeRecommendationSession(identity string) *homeRecommendationSession {
	now := s.homeRecommendationsNow()
	s.homeRecommendationMu.Lock()
	defer s.homeRecommendationMu.Unlock()

	if s.homeRecommendationSessions == nil {
		s.homeRecommendationSessions = make(map[string]*homeRecommendationSession)
	}
	s.pruneHomeRecommendationSessionsLocked(now)
	if session := s.homeRecommendationSessions[identity]; session != nil {
		session.lastAccess = now
		return session
	}

	for len(s.homeRecommendationSessions) >= maxHomeRecommendationSessions {
		var oldestIdentity string
		var oldestAccess time.Time
		for candidateIdentity, session := range s.homeRecommendationSessions {
			if oldestIdentity == "" || session.lastAccess.Before(oldestAccess) {
				oldestIdentity = candidateIdentity
				oldestAccess = session.lastAccess
			}
		}
		if oldestIdentity == "" {
			break
		}
		delete(s.homeRecommendationSessions, oldestIdentity)
	}

	session := &homeRecommendationSession{lastAccess: now}
	s.homeRecommendationSessions[identity] = session
	return session
}

func (s *Server) touchHomeRecommendationSession(session *homeRecommendationSession) {
	s.homeRecommendationMu.Lock()
	defer s.homeRecommendationMu.Unlock()
	session.lastAccess = s.homeRecommendationsNow()
}

func (s *Server) pruneHomeRecommendationSessionsLocked(now time.Time) {
	for identity, session := range s.homeRecommendationSessions {
		if now.Sub(session.lastAccess) >= homeRecommendationSessionTTL {
			delete(s.homeRecommendationSessions, identity)
		}
	}
}
