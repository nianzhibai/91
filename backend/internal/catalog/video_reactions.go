package catalog

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type VideoReaction string

const (
	VideoReactionNone    VideoReaction = "none"
	VideoReactionLike    VideoReaction = "like"
	VideoReactionDislike VideoReaction = "dislike"
)

var (
	ErrInvalidVideoReaction        = errors.New("invalid video reaction")
	ErrInvalidVideoReactionVisitID = errors.New("invalid video reaction visit id")
)

type VideoReactionResult struct {
	Reaction VideoReaction `json:"reaction"`
	Likes    int           `json:"likes"`
	Dislikes int           `json:"dislikes"`
}

// SetVisitReaction sets the single three-state ballot owned by one detail-page
// visit. A visit starts at none; callers generate a fresh visit ID whenever the
// detail page is opened again or refreshed. The visit ID deliberately carries
// no user, account, browser, or device identity.
//
// The first statement is a write even for an existing visit, so SQLite
// serializes competing updates before we read the old reaction. Together with
// the composite primary key, repeated requests for the same desired state are
// idempotent and cannot increment the aggregate counters twice.
func (c *Catalog) SetVisitReaction(
	ctx context.Context,
	videoID string,
	visitID string,
	reaction VideoReaction,
) (VideoReactionResult, error) {
	videoID = strings.TrimSpace(videoID)
	visitID = strings.TrimSpace(visitID)
	if videoID == "" || !validVideoReactionVisitID(visitID) {
		return VideoReactionResult{}, ErrInvalidVideoReactionVisitID
	}
	if !reaction.Valid() {
		return VideoReactionResult{}, ErrInvalidVideoReaction
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return VideoReactionResult{}, err
	}
	defer tx.Rollback()

	now := time.Now().UnixMilli()
	// INSERT ... SELECT also verifies that the target video exists and is
	// visible. ON CONFLICT keeps an existing visit's current state untouched.
	if _, err := tx.ExecContext(ctx, `
INSERT INTO video_reaction_visits (
  video_id, visit_id, reaction, created_at, updated_at
)
SELECT id, ?, 'none', ?, ?
  FROM videos
 WHERE id = ?
   AND COALESCE(hidden, 0) = 0
ON CONFLICT(video_id, visit_id) DO NOTHING`,
		visitID, now, now, videoID); err != nil {
		return VideoReactionResult{}, err
	}

	var previous VideoReaction
	if err := tx.QueryRowContext(ctx, `
SELECT reaction
  FROM video_reaction_visits
 WHERE video_id = ?
   AND visit_id = ?
   AND EXISTS (
       SELECT 1
         FROM videos
        WHERE videos.id = video_reaction_visits.video_id
          AND COALESCE(videos.hidden, 0) = 0
   )`, videoID, visitID).Scan(&previous); err != nil {
		return VideoReactionResult{}, err
	}

	if previous != reaction {
		likesDelta := reactionContribution(reaction, VideoReactionLike) -
			reactionContribution(previous, VideoReactionLike)
		dislikesDelta := reactionContribution(reaction, VideoReactionDislike) -
			reactionContribution(previous, VideoReactionDislike)

		res, err := tx.ExecContext(ctx, `
UPDATE videos
   SET likes = MAX(likes + ?, 0),
       dislikes = MAX(dislikes + ?, 0),
       last_liked_at = CASE
         WHEN ? > 0 THEN ?
         WHEN ? < 0 AND likes + ? <= 0 THEN 0
         ELSE last_liked_at
       END,
       updated_at = ?
 WHERE id = ?
   AND COALESCE(hidden, 0) = 0`,
			likesDelta,
			dislikesDelta,
			likesDelta,
			now,
			likesDelta,
			likesDelta,
			now,
			videoID,
		)
		if err != nil {
			return VideoReactionResult{}, err
		}
		if rows, err := res.RowsAffected(); err == nil && rows == 0 {
			return VideoReactionResult{}, sql.ErrNoRows
		}

		if _, err := tx.ExecContext(ctx, `
UPDATE video_reaction_visits
   SET reaction = ?, updated_at = ?
 WHERE video_id = ?
   AND visit_id = ?`,
			reaction, now, videoID, visitID); err != nil {
			return VideoReactionResult{}, err
		}
	}

	result := VideoReactionResult{Reaction: reaction}
	if err := tx.QueryRowContext(ctx,
		`SELECT likes, dislikes FROM videos WHERE id = ? AND COALESCE(hidden, 0) = 0`,
		videoID,
	).Scan(&result.Likes, &result.Dislikes); err != nil {
		return VideoReactionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return VideoReactionResult{}, err
	}
	return result, nil
}

// VisitReactionsByVideoIDs returns non-none reactions recorded for visitID
// across the given video IDs. Invalid or empty visit IDs return an empty map
// without error so optional callers (like shorts feeds) can pass client values
// safely.
func (c *Catalog) VisitReactionsByVideoIDs(
	ctx context.Context,
	visitID string,
	videoIDs []string,
) (map[string]VideoReaction, error) {
	visitID = strings.TrimSpace(visitID)
	cleaned := cleanVideoIDs(videoIDs)
	if !validVideoReactionVisitID(visitID) || len(cleaned) == 0 {
		return nil, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(cleaned)), ",")
	args := make([]any, 0, len(cleaned)+1)
	args = append(args, visitID)
	for _, id := range cleaned {
		args = append(args, id)
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT video_id, reaction
  FROM video_reaction_visits
 WHERE visit_id = ?
   AND video_id IN (`+placeholders+`)
   AND reaction != 'none'`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]VideoReaction)
	for rows.Next() {
		var videoID string
		var reaction VideoReaction
		if err := rows.Scan(&videoID, &reaction); err != nil {
			return nil, err
		}
		if reaction.Valid() && reaction != VideoReactionNone {
			out[videoID] = reaction
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r VideoReaction) Valid() bool {
	switch r {
	case VideoReactionNone, VideoReactionLike, VideoReactionDislike:
		return true
	default:
		return false
	}
}

func reactionContribution(current, target VideoReaction) int {
	if current == target {
		return 1
	}
	return 0
}

func validVideoReactionVisitID(value string) bool {
	if len(value) < 16 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9':
		case char == '-', char == '_':
		default:
			return false
		}
	}
	return true
}
