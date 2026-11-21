package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/nianzhibai/91/backend/internal/driveevents"
)

// Thumbnail sources record who owns a video's cover. Covers only serve
// viewers; the media job replaces the covers it generated with the frame it
// selects on every read, and never touches covers it does not own.
const (
	// ThumbnailGenerated covers are selected from the video by the workers.
	ThumbnailGenerated = "generated"
	// ThumbnailProvided covers came with the source, such as a crawled item.
	ThumbnailProvided = "provided"
	// ThumbnailCustom covers were set by an administrator.
	ThumbnailCustom = "custom"
)

// GeneratesThumbnail reports whether the workers produce v's cover: it has
// none, or the one it has was generated.
func (v *Video) GeneratesThumbnail() bool {
	return v.ThumbnailURL == "" || v.ThumbnailSource == ThumbnailGenerated
}

// thumbnailSourceState is the stored source: a video without a cover has no
// owner for it.
func (v *Video) thumbnailSourceState() string {
	if v.ThumbnailURL == "" {
		return ""
	}
	return v.ThumbnailSource
}

// GeneratedThumbnailURL is the URL of a video's generated or provided local
// cover file.
func GeneratedThumbnailURL(videoID string) string {
	return "/p/thumb/" + videoID
}

// SaveGeneratedThumbnailForSource records the generated cover of source. The
// write is rejected when the row no longer refers to the bytes that were read,
// or when the video got a cover it does not generate in the meantime.
func (c *Catalog) SaveGeneratedThumbnailForSource(ctx context.Context, source *Video) (resultErr error) {
	defer c.notifyDriveWrite(&resultErr, "", driveevents.MediaChanged)
	if source == nil {
		return fmt.Errorf("catalog: save generated thumbnail: missing source")
	}
	where, match := videoRowWhere(source.ID, source)
	res, err := c.db.ExecContext(ctx, `UPDATE videos
		   SET thumbnail_url = ?, thumbnail_source = ?, thumbnail_status = 'ready', thumbnail_failures = 0,
		       thumbnail_updated_at = MAX(COALESCE(thumbnail_updated_at, 0) + 1, ?)
		 WHERE `+where+` AND (COALESCE(thumbnail_url, '') = '' OR thumbnail_source = ?)`,
		append([]any{GeneratedThumbnailURL(source.ID), ThumbnailGenerated, time.Now().UnixMilli()}, append(match, ThumbnailGenerated)...)...)
	if err != nil {
		return err
	}
	if count, err := res.RowsAffected(); err == nil && count == 0 {
		return errStaleThumbnailWrite
	}
	source.ThumbnailURL, source.ThumbnailSource = GeneratedThumbnailURL(source.ID), ThumbnailGenerated
	return nil
}

var errStaleThumbnailWrite = fmt.Errorf("catalog: video changed before its generated thumbnail was saved: %w", sql.ErrNoRows)

// backfillThumbnailSources attributes existing covers once, when the column is
// added. Local covers of crawled videos are taken as provided, which keeps a
// cover the crawler may have supplied; other local covers were generated, and
// any other URL was set by an administrator.
func (c *Catalog) backfillThumbnailSources(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, `UPDATE videos SET thumbnail_source = CASE
		  WHEN thumbnail_url != '/p/thumb/' || id THEN ?
		  WHEN EXISTS (SELECT 1 FROM crawler_seen_sources s
		                WHERE s.canonical_video_id = videos.id AND s.status = 'imported') THEN ?
		  ELSE ?
		END
		WHERE COALESCE(thumbnail_url, '') != ''`, ThumbnailCustom, ThumbnailProvided, ThumbnailGenerated)
	return err
}
