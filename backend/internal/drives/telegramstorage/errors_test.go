package telegramstorage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

func TestLocalPathDistinguishesMissingRecordFromStorageAndDatabaseFailures(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	ctx := context.Background()
	if _, err := cat.ReserveTelegramLocalFile(ctx, catalog.TelegramLocalFile{FileID: "video.media", JobID: "job"}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "unavailable-library")
	driver := New(cat, func() string { return root })
	if _, err := driver.LocalPath(ctx, "unregistered.media"); !errors.Is(err, sql.ErrNoRows) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing registry identity was lost: %v", err)
	}
	if _, err := driver.LocalPath(ctx, "video.media"); err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unavailable storage was treated as an unregistered file: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := driver.LocalPath(canceled, "video.media"); !errors.Is(err, context.Canceled) || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancellation was treated as a missing file: %v", err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.LocalPath(ctx, "video.media"); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database failure was treated as a missing file: %v", err)
	}
}
