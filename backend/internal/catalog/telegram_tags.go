package catalog

import (
	"context"
	"database/sql"
	"time"
)

const TelegramTagLabel = "TG"
const telegramTagOrigin = "telegram"

// The import source owns this tag independently of manual or matched content
// tags. Create it in the same transaction as the video so an import cannot be
// completed without its source label.
func ensureTelegramTagTx(ctx context.Context, tx *sql.Tx) (Tag, error) {
	now := time.Now().UnixMilli()
	res, err := tx.ExecContext(ctx, `
INSERT INTO tags (label, match_rules, source, origin, created_at, updated_at)
VALUES (?, '{}', 'generated', ?, ?, ?)
ON CONFLICT(label) DO UPDATE SET source='generated', origin=excluded.origin, updated_at=excluded.updated_at
WHERE tags.source!='generated' OR COALESCE(tags.origin,'')!=excluded.origin`, TelegramTagLabel, telegramTagOrigin, now, now)
	if err != nil {
		return Tag{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return Tag{}, err
	} else if n > 0 {
		if err := bumpTagRulesVersionTx(ctx, tx); err != nil {
			return Tag{}, err
		}
	}
	return getTagByLabelTxRaw(ctx, tx, TelegramTagLabel)
}
