package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSourceRemovalPreventsRebindingDuringProviderCall(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	owner := &Video{ID: "owner", DriveID: "drive", FileID: "file"}
	other := &Video{ID: "crawler", DriveID: "local", FileID: "other"}
	for _, v := range []*Video{owner, other} {
		if err := writer.UpsertVideo(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	target := VideoDriveMigration{DriveID: owner.DriveID, FileID: owner.FileID}
	providerErr := errors.New("provider busy")
	err = cat.DeleteVideoGroup(ctx, owner.ID, VideoDeletionOptions{DeleteSource: true}, time.Time{}, func(ctx context.Context, _ *Video, _ bool) error {
		if err := writer.MigrateVideoToDrive(ctx, other.ID, target); err == nil {
			t.Fatal("migration bound a source while its removal was in flight")
		}
		if err := writer.MigrateVideoToDrive(ctx, owner.ID, VideoDriveMigration{DriveID: "new", FileID: "new"}); err == nil {
			t.Fatal("live row moved while its removal was in flight")
		}
		if err := writer.UpsertVideo(ctx, &Video{ID: "inserted", DriveID: owner.DriveID, FileID: owner.FileID}); err == nil {
			t.Fatal("new row acquired the reserved source")
		}
		if err := writer.SetSetting(ctx, "unrelated", "value"); err != nil {
			t.Fatalf("provider call blocked an unrelated write: %v", err)
		}
		return providerErr
	}, nil)
	if !errors.Is(err, providerErr) {
		t.Fatalf("provider error = %v", err)
	}
	if err := writer.MigrateVideoToDrive(ctx, other.ID, target); err != nil {
		t.Fatalf("provider error did not release reservation: %v", err)
	}
	if err := cat.DeleteVideoWithTombstone(ctx, owner.ID); err != nil {
		t.Fatal(err)
	}
	err = cat.WithBlacklistedSourceRemoval(ctx, &DeletedVideo{ID: owner.ID, DriveID: owner.DriveID, FileID: owner.FileID}, func(context.Context) error {
		t.Fatal("provider was called after another row acquired the file")
		return nil
	})
	if !errors.Is(err, ErrVideoSourceInUse) {
		t.Fatalf("shared removal = %v", err)
	}
}

func TestSourceRemovalReclaimsExpiredReservation(t *testing.T) {
	ctx := context.Background()
	cat := openCanonicalTestCatalog(t)
	if _, err := cat.db.Exec(`INSERT INTO video_source_removals VALUES ('drive', 'file', 'abandoned', 0)`); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpsertVideo(ctx, &Video{ID: "id", DriveID: "drive", FileID: "file"}); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := cat.DeleteVideoGroup(ctx, "id", VideoDeletionOptions{DeleteSource: true}, time.Time{}, func(context.Context, *Video, bool) error {
		called = true
		return nil
	}, nil); err != nil || !called {
		t.Fatalf("retry called=%t, err=%v", called, err)
	}
}
