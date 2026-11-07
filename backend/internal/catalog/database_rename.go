package catalog

import (
	"context"
	"fmt"
	"strings"
)

// PrepareDatabaseRename checkpoints pending writes and closes SQLite handles.
// The caller must stop other database users before renaming the file.
func PrepareDatabaseRename(ctx context.Context, path string) error {
	cat, err := OpenExisting(ctx, path)
	if err != nil {
		return err
	}
	defer cat.Close()
	cat.db.SetMaxOpenConns(1)
	if _, err := cat.db.ExecContext(ctx, `PRAGMA locking_mode = EXCLUSIVE`); err != nil {
		return err
	}
	// SQLite checkpoints WAL and rejects the mode change while other WAL
	// connections are open: https://www.sqlite.org/walformat.html
	var mode string
	if err := cat.db.QueryRowContext(ctx, `PRAGMA journal_mode = DELETE`).Scan(&mode); err != nil {
		return err
	}
	if !strings.EqualFold(mode, "delete") {
		return fmt.Errorf("cannot checkpoint database: journal mode is %q", mode)
	}
	if _, err := cat.db.ExecContext(ctx, `BEGIN EXCLUSIVE; COMMIT`); err != nil {
		return err
	}
	// All SQLite handles must be closed before changing filesystem names.
	return cat.Close()
}
