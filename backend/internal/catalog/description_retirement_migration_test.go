package catalog

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestOpenDropsVideoDescriptionWithoutLosingMetadata(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		name := "column"
		if indexed {
			name = "indexed column"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dbPath := t.TempDir() + "/catalog.db"
			cat, err := Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cat.Close() })
			if hasColumn(t, cat, "videos", "description") {
				t.Fatal("new catalog has a description column")
			}
			now := time.Now().Truncate(time.Millisecond)
			if err := cat.UpsertVideo(ctx, &Video{
				ID: "video-1", DriveID: "local-upload", FileID: "original.mp4",
				FileName: "original.mp4", Title: "Original title", Author: "Original author",
				Size: 1234, Ext: "mp4", DurationSeconds: 42,
				Badges: []string{"featured"}, Views: 7, Likes: 3,
				PublishedAt: now, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			before, err := cat.GetVideo(ctx, "video-1")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cat.db.ExecContext(ctx, `ALTER TABLE videos ADD COLUMN description TEXT`); err != nil {
				t.Fatal(err)
			}
			if _, err := cat.db.ExecContext(ctx, `UPDATE videos SET description = 'Legacy description'`); err != nil {
				t.Fatal(err)
			}
			if indexed {
				if _, err := cat.db.ExecContext(ctx, `CREATE INDEX idx_legacy_description ON videos(description)`); err != nil {
					t.Fatal(err)
				}
			}
			if err := cat.Close(); err != nil {
				t.Fatal(err)
			}

			migrated, err := Open(dbPath)
			if err != nil {
				t.Fatalf("migrate catalog: %v", err)
			}
			cat = migrated
			if hasColumn(t, cat, "videos", "description") {
				t.Fatal("retired description column still exists")
			}
			after, err := cat.GetVideo(ctx, "video-1")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("migration changed video metadata: before=%#v after=%#v", before, after)
			}
			if err := cat.migrate(ctx); err != nil {
				t.Fatalf("repeat migration: %v", err)
			}
			after.Title = "Updated title"
			if err := cat.UpsertVideo(ctx, after); err != nil {
				t.Fatalf("update migrated video: %v", err)
			}
		})
	}
}

func TestDeletedVideoRestoreDiscardsLegacyDescription(t *testing.T) {
	payload, err := decodeDeletedVideoRestorePayload("video-1", `{
		"version": 1,
		"video": {"id": "video-1", "driveId": "local-upload", "fileId": "original.mp4", "title": "Original title", "tags": ["travel"], "description": "Legacy description"}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Video.Title != "Original title" || !reflect.DeepEqual(payload.Video.Tags, []string{"travel"}) {
		t.Fatalf("legacy restore lost retained metadata: %#v", payload.Video)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"description"`) {
		t.Fatalf("retired description survived restore: %s", encoded)
	}
}
