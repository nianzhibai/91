package catalog

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"time"
)

// Shorts watch outcome kinds. They are mutually exclusive for one recorded
// watch: a video is either finished, partly watched, or swiped past.
const (
	ShortsEventComplete = "complete"
	ShortsEventPartial  = "partial"
	ShortsEventSkip     = "skip"
)

// MaxShortsEventsPerRequest bounds one batch report. The client flushes far
// smaller batches; the cap only protects the endpoint from a hostile caller.
const MaxShortsEventsPerRequest = 32

var ErrInvalidShortsEvent = errors.New("invalid shorts event")

const (
	// Directory affinity prior: with no observations a directory is assumed to
	// be as interesting as an average one. Smoothing keeps a single lucky
	// completion from pinning a directory to the top forever.
	shortsDirAffinityPrior  = 0.35
	shortsDirAffinityWeight = 5.0

	// Tag weights per outcome. A completion is worth three partial watches: it
	// is the only outcome that proves the whole clip was worth the time.
	shortsTagWeightComplete = 3.0
	shortsTagWeightPartial  = 1.0

	shortsTagDecayDays = 30.0
	shortsConsumedLimit = 2000
)

// ShortsEvent is one settled watch of one video.
type ShortsEvent struct {
	VideoID   string
	Kind      string
	WatchMs   int
	LoopCount int
	Position  int
}

// ShortsWatchProfile is the per-user signal set the recommendation feed needs.
// It is derived from video_user_events and holds no session state.
type ShortsWatchProfile struct {
	// DirAffinity maps a directory id to a smoothed completion rate in [0,1].
	DirAffinity map[string]float64
	// TagAffinity maps a tag label to a decayed interest weight. The value is
	// not normalised; the feed squashes it.
	TagAffinity map[string]float64
	// Consumed holds recently watched videos so the next batch does not repeat
	// them.
	Consumed map[string]struct{}
	// LastCompleted is the most recently finished video, or "" when unknown.
	LastCompleted string
}

// ShortsCandidate is the minimal video projection the feed scores. Tags are the
// matched label set, not the free-form videos.tags column.
type ShortsCandidate struct {
	ID              string
	ParentID        string
	FileName        string
	DurationSeconds int
	PublishedAt     int64
	Views           int
	Likes           int
	Tags            []string
}

// ShortsDirectoryTagShare summarises how concentrated one directory's tags are.
// A high share means the directory groups one kind of content; a low share means
// it is an archive of whatever was downloaded together.
type ShortsDirectoryTagShare struct {
	ParentID string
	Tagged   int
	TopShare float64
}

// RecordShortsEvents stores a batch of settled watches in one transaction.
// Unknown video ids are dropped instead of failing the whole batch: a video can
// be hidden or deleted between playback and the report, and losing that one
// event must not discard the rest.
func (c *Catalog) RecordShortsEvents(
	ctx context.Context,
	userID int64,
	events []ShortsEvent,
	now time.Time,
) (int, error) {
	if userID <= 0 {
		return 0, ErrInvalidShortsEvent
	}
	if len(events) > MaxShortsEventsPerRequest {
		events = events[:MaxShortsEventsPerRequest]
	}
	if len(events) == 0 {
		return 0, nil
	}

	cleaned := make([]ShortsEvent, 0, len(events))
	ids := make([]string, 0, len(events))
	for _, event := range events {
		videoID := strings.TrimSpace(event.VideoID)
		if videoID == "" || !validShortsEventKind(event.Kind) {
			return 0, ErrInvalidShortsEvent
		}
		if event.WatchMs < 0 || event.LoopCount < 0 || event.Position < 0 {
			return 0, ErrInvalidShortsEvent
		}
		cleaned = append(cleaned, ShortsEvent{
			VideoID:   videoID,
			Kind:      event.Kind,
			WatchMs:   event.WatchMs,
			LoopCount: event.LoopCount,
			Position:  event.Position,
		})
		ids = append(ids, videoID)
	}

	known, err := c.knownVideoIDs(ctx, ids)
	if err != nil {
		return 0, err
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	statement, err := tx.PrepareContext(ctx,
		`INSERT INTO video_user_events
		   (user_id, video_id, kind, watch_ms, loop_count, position, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer statement.Close()

	createdAt := now.UnixMilli()
	stored := 0
	for _, event := range cleaned {
		if _, ok := known[event.VideoID]; !ok {
			continue
		}
		if _, err := statement.ExecContext(ctx,
			userID, event.VideoID, event.Kind, event.WatchMs, event.LoopCount, event.Position, createdAt,
		); err != nil {
			return 0, err
		}
		stored++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return stored, nil
}

func validShortsEventKind(kind string) bool {
	switch kind {
	case ShortsEventComplete, ShortsEventPartial, ShortsEventSkip:
		return true
	}
	return false
}

// knownVideoIDs filters a batch report down to videos that still exist, so a
// deleted video does not accumulate orphan event rows.
func (c *Catalog) knownVideoIDs(ctx context.Context, ids []string) (map[string]struct{}, error) {
	cleaned := cleanVideoIDs(ids)
	known := make(map[string]struct{}, len(cleaned))
	if len(cleaned) == 0 {
		return known, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(cleaned)), ",")
	args := make([]any, 0, len(cleaned))
	for _, id := range cleaned {
		args = append(args, id)
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT id FROM videos WHERE id IN (`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		known[id] = struct{}{}
	}
	return known, rows.Err()
}

// ShortsWatchProfile aggregates the events recorded since `since` into the
// signals the feed consumes. Every count is smoothed or decayed here rather than
// in the caller, so the scoring package stays a pure function of its input.
func (c *Catalog) ShortsWatchProfile(
	ctx context.Context,
	userID int64,
	since time.Time,
) (*ShortsWatchProfile, error) {
	profile := &ShortsWatchProfile{
		DirAffinity:  map[string]float64{},
		TagAffinity:  map[string]float64{},
		Consumed:     map[string]struct{}{},
		LastCompleted: "",
	}
	if userID <= 0 {
		return profile, nil
	}
	sinceMillis := since.UnixMilli()

	if err := c.fillShortsDirectoryAffinity(ctx, profile, userID, sinceMillis); err != nil {
		return nil, err
	}
	if err := c.fillShortsTagAffinity(ctx, profile, userID, sinceMillis); err != nil {
		return nil, err
	}
	if err := c.fillShortsConsumed(ctx, profile, userID, sinceMillis); err != nil {
		return nil, err
	}
	if err := c.db.QueryRowContext(ctx,
		`SELECT video_id FROM video_user_events
		  WHERE user_id = ? AND kind = ? AND created_at >= ?
		  ORDER BY created_at DESC, id DESC
		  LIMIT 1`,
		userID, ShortsEventComplete, sinceMillis,
	).Scan(&profile.LastCompleted); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		profile.LastCompleted = ""
	}
	return profile, nil
}

func (c *Catalog) fillShortsDirectoryAffinity(
	ctx context.Context,
	profile *ShortsWatchProfile,
	userID int64,
	sinceMillis int64,
) error {
	rows, err := c.db.QueryContext(ctx,
		`SELECT videos.parent_id,
		        SUM(CASE WHEN video_user_events.kind = ? THEN 1 ELSE 0 END),
		        COUNT(*)
		   FROM video_user_events
		   JOIN videos ON videos.id = video_user_events.video_id
		  WHERE video_user_events.user_id = ?
		    AND video_user_events.created_at >= ?
		    AND COALESCE(videos.parent_id, '') != ''
		  GROUP BY videos.parent_id`,
		ShortsEventComplete, userID, sinceMillis,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var parentID string
		var completes, impressions int
		if err := rows.Scan(&parentID, &completes, &impressions); err != nil {
			return err
		}
		profile.DirAffinity[parentID] = (float64(completes) + shortsDirAffinityWeight*shortsDirAffinityPrior) /
			(float64(impressions) + shortsDirAffinityWeight)
	}
	return rows.Err()
}

func (c *Catalog) fillShortsTagAffinity(
	ctx context.Context,
	profile *ShortsWatchProfile,
	userID int64,
	sinceMillis int64,
) error {
	rows, err := c.db.QueryContext(ctx,
		`SELECT tags.label,
		        SUM(CASE WHEN video_user_events.kind = ? THEN ?
		                 WHEN video_user_events.kind = ? THEN ?
		                 ELSE 0 END),
		        MAX(video_user_events.created_at)
		   FROM video_user_events
		   JOIN video_tags ON video_tags.video_id = video_user_events.video_id
		   JOIN tags ON tags.id = video_tags.tag_id
		  WHERE video_user_events.user_id = ?
		    AND video_user_events.created_at >= ?
		  GROUP BY tags.label`,
		ShortsEventComplete, shortsTagWeightComplete,
		ShortsEventPartial, shortsTagWeightPartial,
		userID, sinceMillis,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var label string
		var weight float64
		var lastSeen int64
		if err := rows.Scan(&label, &weight, &lastSeen); err != nil {
			return err
		}
		if weight <= 0 {
			continue
		}
		// Decay by the most recent interaction with the tag. Per-event decay
		// would need one row per event; the difference only matters for tags
		// touched both recently and long ago, which the weight sum already
		// covers.
		ageDays := time.Since(time.UnixMilli(lastSeen)).Hours() / 24
		if ageDays < 0 {
			ageDays = 0
		}
		profile.TagAffinity[normalizeTagLabel(label)] += weight * expDecay(ageDays, shortsTagDecayDays)
	}
	return rows.Err()
}

func (c *Catalog) fillShortsConsumed(
	ctx context.Context,
	profile *ShortsWatchProfile,
	userID int64,
	sinceMillis int64,
) error {
	rows, err := c.db.QueryContext(ctx,
		`SELECT video_id FROM video_user_events
		  WHERE user_id = ? AND created_at >= ?
		  ORDER BY created_at DESC, id DESC
		  LIMIT ?`,
		userID, sinceMillis, shortsConsumedLimit,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		profile.Consumed[id] = struct{}{}
	}
	return rows.Err()
}

// ShortsCandidates returns the scored candidate pool: every publicly visible
// video with the fields the feed needs, bounded so a very large library cannot
// pull an unbounded slice into memory on each batch.
func (c *Catalog) ShortsCandidates(ctx context.Context, limit int) ([]ShortsCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT videos.id,
		        COALESCE(videos.parent_id, ''),
		        COALESCE(videos.file_name, ''),
		        COALESCE(videos.duration_seconds, 0),
		        videos.published_at,
		        COALESCE(videos.views, 0),
		        COALESCE(videos.likes, 0),
		        COALESCE(GROUP_CONCAT(tags.label, char(31)), '')
		   FROM videos
		   LEFT JOIN video_tags ON video_tags.video_id = videos.id
		   LEFT JOIN tags ON tags.id = video_tags.tag_id
		  WHERE COALESCE(videos.hidden, 0) = 0
		    AND `+activeDriveWhereSQL+`
		    AND `+uniqueVideoWhereSQL+`
		  GROUP BY videos.id
		  ORDER BY videos.published_at DESC, videos.id ASC
		  LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ShortsCandidate, 0, 64)
	for rows.Next() {
		var candidate ShortsCandidate
		var tagList string
		if err := rows.Scan(
			&candidate.ID,
			&candidate.ParentID,
			&candidate.FileName,
			&candidate.DurationSeconds,
			&candidate.PublishedAt,
			&candidate.Views,
			&candidate.Likes,
			&tagList,
		); err != nil {
			return nil, err
		}
		if tagList != "" {
			for _, label := range strings.Split(tagList, string(rune(31))) {
				if trimmed := strings.TrimSpace(label); trimmed != "" {
					candidate.Tags = append(candidate.Tags, normalizeTagLabel(trimmed))
				}
			}
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}

// ShortsDirectoryTagShares measures tag concentration per directory. The feed
// uses it to tell a content directory ("某个系列") apart from an archive
// directory ("2024-03 下载"), where same-directory recommendations are noise.
func (c *Catalog) ShortsDirectoryTagShares(ctx context.Context) (map[string]ShortsDirectoryTagShare, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT videos.parent_id, tags.label, COUNT(*) AS uses
		   FROM video_tags
		   JOIN tags ON tags.id = video_tags.tag_id
		   JOIN videos ON videos.id = video_tags.video_id
		  WHERE COALESCE(videos.parent_id, '') != ''
		    AND COALESCE(videos.hidden, 0) = 0
		    AND `+activeDriveWhereSQL+`
		    AND `+uniqueVideoWhereSQL+`
		  GROUP BY videos.parent_id, tags.label`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	totals := map[string]int{}
	tops := map[string]int{}
	for rows.Next() {
		var parentID, label string
		var uses int
		if err := rows.Scan(&parentID, &label, &uses); err != nil {
			return nil, err
		}
		totals[parentID] += uses
		if uses > tops[parentID] {
			tops[parentID] = uses
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	shares := make(map[string]ShortsDirectoryTagShare, len(totals))
	for parentID, total := range totals {
		if total <= 0 {
			continue
		}
		shares[parentID] = ShortsDirectoryTagShare{
			ParentID: parentID,
			Tagged:   total,
			TopShare: float64(tops[parentID]) / float64(total),
		}
	}
	return shares, nil
}

func normalizeTagLabel(label string) string {
	return strings.ToLower(strings.TrimSpace(label))
}

// expDecay returns 1 for a fresh observation and falls off with an e-folding
// time of decayDays.
func expDecay(ageDays, decayDays float64) float64 {
	if decayDays <= 0 {
		return 1
	}
	return math.Exp(-ageDays / decayDays)
}
