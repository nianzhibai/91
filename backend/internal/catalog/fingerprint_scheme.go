package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/nianzhibai/91/backend/internal/dedupe"
)

// sampledFingerprintSchemeKey records the sampling scheme whose fingerprints
// the catalog holds. Its value is dedupe.SampledFingerprintPrefix, so a new
// scheme retires the previous fingerprints once.
const sampledFingerprintSchemeKey = "videos.sampled_sha256.scheme"

// currentFingerprintSQL matches a column holding a fingerprint of the current
// sampling scheme. The prefix is a constant without quotes or LIKE wildcards.
// Only such a fingerprint may join two rows as the same content; see
// dedupe.ExactKeys.
func currentFingerprintSQL(column string) string {
	return "COALESCE(" + column + ", '') LIKE '" + dedupe.SampledFingerprintPrefix + "%'"
}

// currentFingerprint returns sampled when the current scheme made it. Records
// copied out of a video keep only current fingerprints: nothing recomputes
// them, so an earlier scheme's value would never match again.
func currentFingerprint(sampled string) string {
	sampled = normalizeContentHash(sampled)
	if !dedupe.IsCurrentFingerprint(sampled) {
		return ""
	}
	return sampled
}

func (c *Catalog) retireLegacyFingerprintsOnce(ctx context.Context) error {
	scheme, err := c.GetSetting(ctx, sampledFingerprintSchemeKey, "")
	if err != nil {
		return fmt.Errorf("read %s: %w", sampledFingerprintSchemeKey, err)
	}
	if strings.TrimSpace(scheme) == dedupe.SampledFingerprintPrefix {
		return nil
	}
	if err := RetireLegacyFingerprints(ctx, c.db); err != nil {
		return err
	}
	if err := c.SetSetting(ctx, sampledFingerprintSchemeKey, dedupe.SampledFingerprintPrefix); err != nil {
		return fmt.Errorf("write %s: %w", sampledFingerprintSchemeKey, err)
	}
	return nil
}

// RetireLegacyFingerprints moves stored fingerprints to the current sampling
// scheme without losing the evidence they hold.
//
// A video keeps its earlier fingerprint and becomes due to be fingerprinted
// again. Until the fingerprint step replaces it, the old value still separates the
// video from same-named files of the same size that the old scheme proved
// different, so the file name and size fallback cannot group them, and it
// never equals a current fingerprint. A video awaiting content verification
// needs no change: its new fingerprint cannot equal the old one it is checked
// against, so the worker treats the bytes as replaced and clears everything
// derived from them, including the provider hash.
//
// Blacklist, merge and crawler records cannot be fingerprinted again. Their
// earlier fingerprints are cleared, and they keep their other identity
// evidence. Restore runs this on a staged archive, which may predate some
// columns; a missing column holds no fingerprints.
func RetireLegacyFingerprints(ctx context.Context, db *sql.DB) error {
	legacy := func(column string) string {
		return "COALESCE(" + column + ", '') != '' AND NOT " + currentFingerprintSQL(column)
	}
	statements := []struct{ table, column, query string }{
		{"videos", "sampled_sha256", "UPDATE videos SET fingerprint_status = 'pending', fingerprint_error = '' WHERE " + legacy("sampled_sha256")},
		{"deleted_videos", "sampled_sha256", "UPDATE deleted_videos SET sampled_sha256 = '' WHERE " + legacy("sampled_sha256")},
		// A restored blacklist entry is fingerprinted again like any new row.
		{"deleted_videos", "restore_payload", `UPDATE deleted_videos
			SET restore_payload = json_set(restore_payload,
				'$.video.sampledSha256', '', '$.video.fingerprintStatus', 'pending', '$.video.fingerprintError', '')
			WHERE json_valid(restore_payload) AND ` + legacy("json_extract(restore_payload, '$.video.sampledSha256')")},
		{"merged_videos", "sampled_sha256", "UPDATE merged_videos SET sampled_sha256 = '' WHERE " + legacy("sampled_sha256")},
		{"crawler_seen_sources", "sampled_sha256", "UPDATE crawler_seen_sources SET sampled_sha256 = '' WHERE " + legacy("sampled_sha256")},
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range statements {
		var present bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name = ?)",
			statement.table, statement.column).Scan(&present); err != nil {
			return fmt.Errorf("retire legacy fingerprints: %w", err)
		}
		if !present {
			continue
		}
		if _, err := tx.ExecContext(ctx, statement.query); err != nil {
			return fmt.Errorf("retire legacy %s.%s fingerprints: %w", statement.table, statement.column, err)
		}
	}
	return tx.Commit()
}
