package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/mediasig"
)

// GenerationSteps are the generation steps a video still needs. The catalog
// is the only record of due work: a drive's runner selects videos by these
// steps, and each step settles its own state when it runs.
type GenerationSteps struct {
	Fingerprint bool
	Media       bool
	Thumbnail   bool
	Duration    bool
}

func (s GenerationSteps) Any() bool {
	return s.Fingerprint || s.Media || s.Thumbnail || s.Duration
}

// AssetsDue reports whether the asset steps can run now. They wait for the
// fingerprint, so a video it shows to be a duplicate never reaches them.
func (s GenerationSteps) AssetsDue() bool {
	return !s.Fingerprint && (s.Media || s.Thumbnail || s.Duration)
}

// GenerationCounts counts a drive's videos by due step. A video that needs
// several steps counts toward each of them.
type GenerationCounts struct {
	Fingerprints int
	Media        int
	Thumbnails   int
	Durations    int
	// Covers counts the videos the cover step has work for, a missing cover
	// or duration, each once: one run settles both.
	Covers int
}

func (c GenerationCounts) Any() bool {
	return c.Fingerprints > 0 || c.Media > 0 || c.Thumbnails > 0 || c.Durations > 0
}

// generationStepSQL returns the predicate of each step over a visible row.
// They mirror the checks the steps make before running: a sampled fingerprint
// of the current scheme, Video.MediaWants, a missing cover, and a missing
// duration. Duplicates only need their fingerprint, which keeps them out of
// the expensive steps.
func generationStepSQL(previewsEnabled bool) (fingerprint, media, thumbnail, duration string) {
	fingerprint = `(COALESCE(size_bytes, 0) > 0 AND NOT ` + currentFingerprintSQL("sampled_sha256") + `
		AND COALESCE(fingerprint_status, 'pending') = 'pending')`

	teaser := "0"
	if previewsEnabled {
		teaser = teaserWorkSQL
	}
	signature := fmt.Sprintf(`(%s AND (COALESCE(duration_seconds, 0) = 0 OR duration_seconds >= %d))`,
		mediaPendingSQL, int(mediasig.MinDuration/time.Second))
	media = `(` + uniqueVideoWhereSQL + ` AND (` + teaser + ` OR ` + signature + `))`

	thumbnail = `(` + uniqueVideoWhereSQL + ` AND COALESCE(thumbnail_url, '') = ''
		AND COALESCE(thumbnail_status, 'pending') NOT IN ('failed', 'skipped'))`
	duration = `(` + uniqueVideoWhereSQL + ` AND COALESCE(duration_seconds, 0) <= 0)`
	return fingerprint, media, thumbnail, duration
}

const (
	mediaPendingSQL = `COALESCE(media_status, 'pending') IN ('pending', '')`
	// teaserWorkSQL is the media work previews add: a pending job renews the
	// teaser of any video, and a pending teaser is generated on its own.
	teaserWorkSQL = `(` + mediaPendingSQL + ` OR COALESCE(preview_status, 'pending') IN ('pending', ''))`
)

// Due queries read candidates through partial indexes, so their cost follows
// the drive's pending work instead of its size. Each index predicate is a
// superset of the work it serves, and the queries repeat it verbatim: SQLite
// uses a partial index only when the query's WHERE clause contains the
// index's terms. Previews have their own index because a teaser stays
// pending while previews are off; inside the generation index, those videos
// would make it cover most of the drive.
var (
	// fingerprintIndexSQL covers every due fingerprint.
	fingerprintIndexSQL = func() string {
		fingerprint, _, _, _ := generationStepSQL(false)
		return `COALESCE(hidden, 0) = 0 AND ` + fingerprint
	}()
	// generationIndexSQL covers every step that is due with previews off.
	generationIndexSQL = func() string {
		fingerprint, media, thumbnail, duration := generationStepSQL(false)
		return `COALESCE(hidden, 0) = 0 AND (` + strings.Join([]string{fingerprint, media, thumbnail, duration}, " OR ") + `)`
	}()
	// previewIndexSQL covers the media work only previews make due.
	previewIndexSQL = `COALESCE(hidden, 0) = 0 AND ` + uniqueVideoWhereSQL + ` AND ` + teaserWorkSQL
)

var generationWorkIndexes = []struct{ name, predicate string }{
	{"idx_videos_due_fingerprints", fingerprintIndexSQL},
	{"idx_videos_due_generation", generationIndexSQL},
	{"idx_videos_due_previews", previewIndexSQL},
}

// ensureGenerationWorkIndexes creates the partial indexes of the due queries,
// replacing any whose predicate changed.
func (c *Catalog) ensureGenerationWorkIndexes(ctx context.Context) error {
	for _, idx := range generationWorkIndexes {
		statement := `CREATE INDEX ` + idx.name + ` ON videos(drive_id, generation_after, created_at, id) WHERE ` + idx.predicate
		var existing string
		err := c.db.QueryRowContext(ctx, `SELECT COALESCE(sql, '') FROM sqlite_master WHERE type = 'index' AND name = ?`, idx.name).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("inspect generation work index %s: %w", idx.name, err)
		}
		if existing == statement {
			continue
		}
		if existing != "" {
			if _, err := c.db.ExecContext(ctx, `DROP INDEX IF EXISTS `+idx.name); err != nil {
				return fmt.Errorf("drop outdated generation work index %s: %w", idx.name, err)
			}
		}
		if _, err := c.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create generation work index %s: %w", idx.name, err)
		}
	}
	return nil
}

// dueQuery is a due query over the drive's candidate videos: those the given
// index predicates select and that are not deferred past a time.
type dueQuery struct {
	sql        string
	predicates int
}

// newDueQuery builds `SELECT columns FROM videos WHERE id IN (candidates)`
// followed by tail.
func newDueQuery(columns, tail string, predicates ...string) dueQuery {
	parts := make([]string, len(predicates))
	for i, predicate := range predicates {
		parts[i] = `SELECT id FROM videos WHERE drive_id = ? AND generation_after <= ? AND ` + predicate
	}
	return dueQuery{
		sql:        `SELECT ` + columns + ` FROM videos WHERE id IN (` + strings.Join(parts, ` UNION ALL `) + `) ` + tail,
		predicates: len(predicates),
	}
}

// args are the candidate arguments followed by extra.
func (q dueQuery) args(driveID string, now time.Time, extra ...any) []any {
	args := make([]any, 0, 2*q.predicates+len(extra))
	for range q.predicates {
		args = append(args, driveID, now.UnixMilli())
	}
	return append(args, extra...)
}

// assetIndexes are the index predicates whose rows can have due asset steps.
func assetIndexes(previewsEnabled bool) []string {
	if previewsEnabled {
		return []string{generationIndexSQL, previewIndexSQL}
	}
	return []string{generationIndexSQL}
}

func dueFingerprintsQuery() dueQuery {
	fingerprint, _, _, _ := generationStepSQL(false)
	return newDueQuery(`id`, `AND `+fingerprint+` ORDER BY created_at ASC, id ASC LIMIT ?`, fingerprintIndexSQL)
}

func dueAssetsQuery(previewsEnabled bool) dueQuery {
	fingerprint, media, thumbnail, duration := generationStepSQL(previewsEnabled)
	return newDueQuery(`id`,
		`AND NOT `+fingerprint+` AND (`+strings.Join([]string{media, thumbnail, duration}, " OR ")+`)
		 ORDER BY `+media+` DESC, `+thumbnail+` DESC, created_at ASC, id ASC LIMIT ?`,
		assetIndexes(previewsEnabled)...)
}

func dueCountsQuery(previewsEnabled bool) dueQuery {
	fingerprint, media, thumbnail, duration := generationStepSQL(previewsEnabled)
	return newDueQuery(`COALESCE(SUM(`+fingerprint+`), 0), COALESCE(SUM(`+media+`), 0),
		   COALESCE(SUM(`+thumbnail+`), 0), COALESCE(SUM(`+duration+`), 0),
		   COALESCE(SUM(`+thumbnail+` OR `+duration+`), 0)`, ``,
		assetIndexes(previewsEnabled)...)
}

// ListDueFingerprints returns up to limit videos of the drive whose
// fingerprint is due, oldest first. Videos deferred past now are left out.
func (c *Catalog) ListDueFingerprints(ctx context.Context, driveID string, now time.Time, limit int) ([]string, error) {
	return c.listDue(ctx, dueFingerprintsQuery(), driveID, now, limit)
}

// ListDueAssets returns up to limit videos of the drive whose asset steps are
// due, as GenerationSteps.AssetsDue defines them: media jobs first, then
// covers and durations, oldest videos first. Videos deferred past now are
// left out.
func (c *Catalog) ListDueAssets(ctx context.Context, driveID string, previewsEnabled bool, now time.Time, limit int) ([]string, error) {
	return c.listDue(ctx, dueAssetsQuery(previewsEnabled), driveID, now, limit)
}

func (c *Catalog) listDue(ctx context.Context, query dueQuery, driveID string, now time.Time, limit int) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, query.sql, query.args(driveID, now, max(limit, 1))...)
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
	return ids, rows.Err()
}

// GenerationWork loads the video with the steps it needs now. A deferral does
// not hide steps here: the runner already took the video.
func (c *Catalog) GenerationWork(ctx context.Context, id string, previewsEnabled bool) (*Video, GenerationSteps, error) {
	video, err := c.GetVideo(ctx, id)
	if err != nil {
		return nil, GenerationSteps{}, err
	}
	var steps GenerationSteps
	if video.Hidden {
		return video, steps, nil
	}
	fingerprint, media, thumbnail, duration := generationStepSQL(previewsEnabled)
	err = c.db.QueryRowContext(ctx, `SELECT `+fingerprint+`, `+media+`, `+thumbnail+`, `+duration+`
		 FROM videos WHERE id = ?`, id).Scan(&steps.Fingerprint, &steps.Media, &steps.Thumbnail, &steps.Duration)
	return video, steps, err
}

// CountDueGeneration counts the drive's videos by due step, leaving out
// videos deferred past now. Asset steps waiting for a fingerprint count too:
// they are due once it settles.
func (c *Catalog) CountDueGeneration(ctx context.Context, driveID string, previewsEnabled bool, now time.Time) (GenerationCounts, error) {
	query := dueCountsQuery(previewsEnabled)
	var counts GenerationCounts
	err := c.db.QueryRowContext(ctx, query.sql, query.args(driveID, now)...).
		Scan(&counts.Fingerprints, &counts.Media, &counts.Thumbnails, &counts.Durations, &counts.Covers)
	return counts, err
}

// LiftPreviewDeferrals makes the deferred videos with media work that only
// previews make due take it again at once. Previews call it when they are
// enabled: a video deferred around the switch, its teaser skipped while
// previews were off, must not wait out its deferral.
func (c *Catalog) LiftPreviewDeferrals(ctx context.Context) (int64, error) {
	res, err := c.db.ExecContext(ctx, `UPDATE videos SET generation_after = 0 WHERE generation_after > 0 AND `+previewIndexSQL)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeferGeneration keeps the video out of the due lists until the given
// time. A scan, a retry request or a change of the source file lifts it.
func (c *Catalog) DeferGeneration(ctx context.Context, id string, until time.Time) error {
	_, err := c.db.ExecContext(ctx, `UPDATE videos SET generation_after = ? WHERE id = ?`, until.UnixMilli(), id)
	return err
}
