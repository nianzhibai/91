package catalog

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestTelegramLibraryMigrationPreservesFileIdentityWithoutHostPaths(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	file := TelegramLocalFile{FileID: "tg-existing.media", JobID: "tg-existing"}
	if _, err := cat.ReserveTelegramLocalFile(ctx, file); err != nil {
		cat.Close()
		t.Fatal(err)
	}
	// Simulate the old schema and the absolute host path captured at import.
	if _, err := cat.db.Exec(`ALTER TABLE telegram_local_files ADD COLUMN root TEXT NOT NULL DEFAULT '/old/location/library'`); err != nil {
		cat.Close()
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP INDEX idx_telegram_local_files_group`,
		`ALTER TABLE telegram_local_files DROP COLUMN directory`,
		`ALTER TABLE telegram_local_files DROP COLUMN group_key`,
	} {
		if _, err := cat.db.Exec(statement); err != nil {
			cat.Close()
			t.Fatal(err)
		}
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		cat, err = Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if hasColumn(t, cat, "telegram_local_files", "root") {
			t.Fatal("retained absolute storage root")
		}
		got, err := cat.TelegramLocalFile(ctx, file.FileID)
		if err != nil || got != file {
			t.Fatalf("migration changed identity: %+v %v", got, err)
		}
		if err := cat.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTelegramAlbumDirectoryIsFrozenAndSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	first := TelegramLocalFile{FileID: "first.media", JobID: "first", Directory: "海边旅行-1234", GroupKey: "album"}
	if _, err := cat.ReserveTelegramLocalFile(ctx, first); err != nil {
		cat.Close()
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	cat, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	second, err := cat.ReserveTelegramLocalFile(ctx, TelegramLocalFile{FileID: "second.media", JobID: "second", Directory: "后到的说明-1234", GroupKey: "album"})
	if err != nil || second.Directory != first.Directory {
		t.Fatalf("album directory changed after restart: %+v %v", second, err)
	}
	changed := first
	changed.Directory = "must-not-move"
	if got, err := cat.ReserveTelegramLocalFile(ctx, changed); err != nil || got != first {
		t.Fatalf("retry changed the reserved location: %+v %v", got, err)
	}
	files, err := cat.AbandonedTelegramLocalFiles(ctx)
	if err != nil || len(files) != 2 {
		t.Fatalf("abandoned files lost: %v %v", files, err)
	}
	for _, file := range files {
		if file.Directory != first.Directory || file.GroupKey != first.GroupKey {
			t.Fatalf("cleanup lost the album location: %+v", file)
		}
	}
}

func TestConcurrentTelegramReservationsChooseOneAlbumDirectory(t *testing.T) {
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	results := make(chan TelegramLocalFile, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			file, err := cat.ReserveTelegramLocalFile(context.Background(), TelegramLocalFile{FileID: fmt.Sprintf("%d.media", i), JobID: fmt.Sprint(i), Directory: fmt.Sprintf("说明-%d", i), GroupKey: "same-album"})
			if err != nil {
				t.Error(err)
				return
			}
			results <- file
		}()
	}
	wg.Wait()
	close(results)
	directory := ""
	for file := range results {
		if directory == "" {
			directory = file.Directory
		}
		if file.Directory != directory {
			t.Fatalf("concurrent reservations split the album: %q and %q", directory, file.Directory)
		}
	}
}

func TestExistingTelegramVideosReceiveTheirActualDirectoryOnReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, directory := range []string{"", "", "海边旅行-a1b2", "海边旅行-a1b2"} {
		id := fmt.Sprintf("video-%d", i)
		if _, err := cat.ReserveTelegramLocalFile(ctx, TelegramLocalFile{FileID: id + ".media", JobID: id, Directory: directory}); err != nil {
			cat.Close()
			t.Fatal(err)
		}
		if err := cat.UpsertVideo(ctx, &Video{ID: id, DriveID: TelegramLocalDriveID, FileID: id + ".media", FileName: id + ".mp4", Title: id, ParentID: "", DirName: ""}); err != nil {
			cat.Close()
			t.Fatal(err)
		}
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		cat, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, directory := range []string{TelegramLibraryRootID, "海边旅行-a1b2"} {
			videos, err := cat.ListVisibleVideosByDirectory(ctx, TelegramLocalDriveID, directory)
			if err != nil || len(videos) != 2 {
				cat.Close()
				t.Fatalf("directory %q contains %d videos, want 2: %v", directory, len(videos), err)
			}
			for _, video := range videos {
				if video.ParentID != directory || video.DirName != directory {
					cat.Close()
					t.Fatalf("incorrect directory metadata after migration: %+v", video)
				}
			}
		}
		if err := cat.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
