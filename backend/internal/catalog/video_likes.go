package catalog

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var ErrInvalidVideoLike = errors.New("invalid video like")

// VideoLikeResult reports the account's like state and the public counter after
// a toggle, so the caller can update both the button and the label in one round
// trip.
type VideoLikeResult struct {
	Liked bool `json:"liked"`
	Likes int  `json:"likes"`
}

// SetUserVideoLike idempotently stores whether one account likes one video and
// keeps the redundant videos.likes counter in sync.
//
// Likes are account-scoped. The anonymous per-visit ballots in
// video_reaction_visits deliberately cannot be attributed to a person, and a
// bare counter cannot tell a first click from a repeat one: one account could
// inflate it without bound. The video must still be publicly visible, so a
// hidden, deleted or deduplicated video cannot be liked.
func (c *Catalog) SetUserVideoLike(
	ctx context.Context,
	userID int64,
	videoID string,
	liked bool,
) (VideoLikeResult, error) {
	videoID = strings.TrimSpace(videoID)
	if userID <= 0 || videoID == "" {
		return VideoLikeResult{}, ErrInvalidVideoLike
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return VideoLikeResult{}, err
	}
	defer tx.Rollback()

	var visible int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM videos
		  WHERE videos.id = ?
		    AND COALESCE(videos.hidden, 0) = 0
		    AND `+activeDriveWhereSQL+`
		    AND `+uniqueVideoWhereSQL,
		videoID,
	).Scan(&visible); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return VideoLikeResult{}, sql.ErrNoRows
		}
		return VideoLikeResult{}, err
	}

	now := time.Now().UnixMilli()
	var changed sql.Result
	if liked {
		changed, err = tx.ExecContext(ctx,
			`INSERT INTO video_likes (user_id, video_id, created_at)
			 VALUES (?, ?, ?)
			 ON CONFLICT(user_id, video_id) DO NOTHING`,
			userID, videoID, now)
	} else {
		changed, err = tx.ExecContext(ctx,
			`DELETE FROM video_likes WHERE user_id = ? AND video_id = ?`,
			userID, videoID)
	}
	if err != nil {
		return VideoLikeResult{}, err
	}
	// Only a real state transition touches the counter, so a repeated request
	// is a no-op instead of a second increment.
	rows, err := changed.RowsAffected()
	if err != nil {
		return VideoLikeResult{}, err
	}
	if rows > 0 {
		if liked {
			_, err = tx.ExecContext(ctx,
				`UPDATE videos SET likes = COALESCE(likes, 0) + 1, last_liked_at = ?, updated_at = ?
				  WHERE id = ?`,
				now, now, videoID)
		} else {
			_, err = tx.ExecContext(ctx,
				`UPDATE videos SET likes = MAX(COALESCE(likes, 0) - 1, 0), updated_at = ?
				  WHERE id = ?`,
				now, videoID)
		}
		if err != nil {
			return VideoLikeResult{}, err
		}
	}

	var likes int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(likes, 0) FROM videos WHERE id = ?`, videoID).Scan(&likes); err != nil {
		return VideoLikeResult{}, err
	}
	var stored int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM video_likes WHERE user_id = ? AND video_id = ?`,
		userID, videoID).Scan(&stored); err != nil {
		return VideoLikeResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return VideoLikeResult{}, err
	}
	return VideoLikeResult{Liked: stored > 0, Likes: likes}, nil
}

// LikedVideoIDsAmong returns which of these ids the account likes. It serves the
// per-item button state of every shorts section.
func (c *Catalog) LikedVideoIDsAmong(
	ctx context.Context,
	userID int64,
	ids []string,
) (map[string]struct{}, error) {
	if userID <= 0 {
		return nil, nil
	}
	cleaned := cleanVideoIDs(ids)
	if len(cleaned) == 0 {
		return nil, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(cleaned)), ",")
	args := make([]any, 0, len(cleaned)+1)
	args = append(args, userID)
	for _, id := range cleaned {
		args = append(args, id)
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT video_id FROM video_likes
		  WHERE user_id = ?
		    AND video_id IN (`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	liked := make(map[string]struct{}, len(cleaned))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		liked[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return liked, nil
}
