package catalog

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
)

const settingScannerParsedAuthorsCleaned = "scanner.parsed_authors_cleaned_v1"

var (
	legacyFilenameBracketPrefix = regexp.MustCompile(`^\s*\[([^\]]+)\]\s*`)
	legacyFilenameAuthorSuffix  = regexp.MustCompile(`\s*-\s*([^-]+?)\s*$`)
)

func legacyParsedFilenameAuthor(fileName string) string {
	name := strings.TrimSuffix(fileName, path.Ext(fileName))
	if m := legacyFilenameBracketPrefix.FindStringSubmatch(name); m != nil {
		name = strings.TrimSpace(name[len(m[0]):])
	}
	if m := legacyFilenameAuthorSuffix.FindStringSubmatch(name); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// clearScannerParsedAuthorsOnce removes author values historically guessed from
// scanned filenames ("... - suffix.ext"). Crawler videos keep their explicit
// source authors, and any non-crawler author that does not match the retired
// filename parser is left untouched.
func (c *Catalog) clearScannerParsedAuthorsOnce(ctx context.Context) (int64, error) {
	marker, err := c.GetSetting(ctx, settingScannerParsedAuthorsCleaned, "")
	if err != nil {
		return 0, err
	}
	if marker == "1" {
		return 0, nil
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
SELECT v.id, COALESCE(v.file_name, ''), COALESCE(v.author, '')
  FROM videos v
 WHERE trim(COALESCE(v.author, '')) != ''
   AND v.id NOT LIKE 'scriptcrawler-%'
   AND NOT EXISTS (
       SELECT 1
         FROM drives d
        WHERE d.id = v.drive_id
          AND d.kind = 'scriptcrawler'
   )`)
	if err != nil {
		return 0, fmt.Errorf("query scanner parsed authors: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id, fileName, author string
		if err := rows.Scan(&id, &fileName, &author); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan scanner parsed author: %w", err)
		}
		if parsed := legacyParsedFilenameAuthor(fileName); parsed != "" && strings.TrimSpace(author) == parsed {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("iterate scanner parsed authors: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close scanner parsed author rows: %w", err)
	}

	var removed int64
	if len(ids) > 0 {
		stmt, err := tx.PrepareContext(ctx, `UPDATE videos SET author = '' WHERE id = ?`)
		if err != nil {
			return 0, fmt.Errorf("prepare clear scanner parsed authors: %w", err)
		}
		for _, id := range ids {
			result, err := stmt.ExecContext(ctx, id)
			if err != nil {
				_ = stmt.Close()
				return 0, fmt.Errorf("clear scanner parsed author %s: %w", id, err)
			}
			n, err := result.RowsAffected()
			if err != nil {
				_ = stmt.Close()
				return 0, fmt.Errorf("count cleared scanner parsed author %s: %w", id, err)
			}
			removed += n
		}
		if err := stmt.Close(); err != nil {
			return 0, fmt.Errorf("close clear scanner parsed authors statement: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO settings (key, value, updated_at) VALUES (?, '1', ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		settingScannerParsedAuthorsCleaned, time.Now().UnixMilli()); err != nil {
		return 0, fmt.Errorf("mark scanner parsed authors cleaned: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if removed > 0 {
		applog.Info(ctx, "cleared scanner filename-parsed author values", "component", "catalog", "stage", "migrate", "removed", removed)
	}
	return removed, nil
}
