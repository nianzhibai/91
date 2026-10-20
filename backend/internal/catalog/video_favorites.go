package catalog

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var ErrInvalidVideoFavorite = errors.New("invalid video favorite")

// VideoFavoriteResult reports the account's favorite state after a toggle.
type VideoFavoriteResult struct {
	Favorited bool `json:"favorited"`
}

// SetVideoFavorite idempotently stores whether one account favorites one video.
// Favorites belong to the account and outlive the session, unlike the
// per-visit ballots in video_reaction_visits. The video must still be publicly
// visible, so a hidden, deleted, or deduplicated video cannot be favorited.
func (c *Catalog) SetVideoFavorite(
	ctx context.Context,
	userID int64,
	videoID string,
	favorite bool,
) (VideoFavoriteResult, error) {
	videoID = strings.TrimSpace(videoID)
	if userID <= 0 || videoID == "" {
		return VideoFavoriteResult{}, ErrInvalidVideoFavorite
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return VideoFavoriteResult{}, err
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
			return VideoFavoriteResult{}, sql.ErrNoRows
		}
		return VideoFavoriteResult{}, err
	}

	if favorite {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO video_favorites (user_id, video_id, created_at)
			 VALUES (?, ?, ?)
			 ON CONFLICT(user_id, video_id) DO NOTHING`,
			userID, videoID, time.Now().UnixMilli()); err != nil {
			return VideoFavoriteResult{}, err
		}
	} else if _, err := tx.ExecContext(ctx,
		`DELETE FROM video_favorites WHERE user_id = ? AND video_id = ?`,
		userID, videoID); err != nil {
		return VideoFavoriteResult{}, err
	}

	var favorited int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM video_favorites WHERE user_id = ? AND video_id = ?`,
		userID, videoID).Scan(&favorited); err != nil {
		return VideoFavoriteResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return VideoFavoriteResult{}, err
	}
	return VideoFavoriteResult{Favorited: favorited > 0}, nil
}

// FavoriteVideoIDs returns the videos one account favorites that are still
// eligible for the public feed, newest favorite first. The shorts "favorites"
// section snapshots this list once per feed round.
func (c *Catalog) FavoriteVideoIDs(ctx context.Context, userID int64) ([]string, error) {
	if userID <= 0 {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT videos.id
		   FROM video_favorites
		   JOIN videos ON videos.id = video_favorites.video_id
		  WHERE video_favorites.user_id = ?
		    AND COALESCE(videos.hidden, 0) = 0
		    AND `+activeDriveWhereSQL+`
		    AND `+uniqueVideoWhereSQL+`
		  ORDER BY video_favorites.created_at DESC, videos.id ASC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// FavoritedVideoIDsAmong returns which of these ids the account favorites.
// One query serves both the batch filter of the "favorites" section and the
// per-item button state of every other section.
func (c *Catalog) FavoritedVideoIDsAmong(
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
		`SELECT video_id FROM video_favorites
		  WHERE user_id = ?
		    AND video_id IN (`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	favorited := make(map[string]struct{}, len(cleaned))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		favorited[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return favorited, nil
}
