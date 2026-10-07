package catalog

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

func finalizeTaggedImport(t *testing.T, c *Catalog, id, kind string, manual []string, auto []TagAssignment) *Video {
	t.Helper()
	ctx := context.Background()
	if kind == "telegram" {
		enqueueTelegram(t, c, 1, id, id)
	} else if _, err := c.CreateRemoteUploadJob(ctx, id, "https://example.com/video.mp4", "example.com", "TG video", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.TransitionRemoteUploadJob(ctx, id, RemoteUploadQueued, RemoteUploadSaving); err != nil {
		t.Fatal(err)
	}
	v := &Video{ID: "video-" + id, DriveID: "local-upload", FileID: id + ".mp4", FileName: id + ".mp4", Title: "TG video", Size: 5, Ext: "mp4"}
	if err := c.FinalizeRemoteUpload(ctx, id, v, manual, auto); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTelegramSourceTagCoexistsWithContentTags(t *testing.T) {
	for _, manual := range []bool{false, true} {
		name := "automatic"
		if manual {
			name = "manual"
		}
		t.Run(name, func(t *testing.T) {
			c := telegramTestCatalog(t)
			ctx := context.Background()
			if _, err := c.EnsureTag(ctx, "content", "user"); err != nil {
				t.Fatal(err)
			}
			var selected []string
			if manual {
				selected = []string{"content"}
			}
			v := finalizeTaggedImport(t, c, "tg", "telegram", selected, []TagAssignment{{Label: "content", Source: "auto"}})
			tag := mustTagByLabel(t, ctx, c, TelegramTagLabel)
			if tag.Source != "generated" {
				t.Fatalf("Telegram tag source = %q, want generated", tag.Source)
			}
			if _, selectable, err := c.LookupUserSelectableTagLabel(ctx, TelegramTagLabel); err != nil || selectable {
				t.Fatalf("Telegram tag selectable = %v, err=%v; want false", selectable, err)
			}
			choices, err := c.ListUserSelectableTags(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, choice := range choices {
				if choice.Label == TelegramTagLabel {
					t.Fatal("Telegram source tag appeared in manual upload choices")
				}
			}
			if len(v.Tags) != 2 || !slices.Contains(v.Tags, "TG") || !slices.Contains(v.Tags, "content") {
				t.Fatalf("content tags lost: %v", v.Tags)
			}
			var locked bool
			if err := c.db.QueryRow(`SELECT tags_manual FROM videos WHERE id=?`, v.ID).Scan(&locked); err != nil || locked != manual {
				t.Fatalf("source tag changed manual lock: %v %v", locked, err)
			}
			var source string
			if err := c.db.QueryRow(`SELECT vt.source FROM video_tags vt JOIN tags t ON t.id=vt.tag_id WHERE vt.video_id=? AND t.label='TG'`, v.ID).Scan(&source); err != nil || source != "telegram" {
				t.Fatalf("wrong source metadata: %q %v", source, err)
			}
			matched, err := c.MatchTagAssignments(ctx, "TG video", "TG.mp4", "", "")
			if err != nil {
				t.Fatal(err)
			}
			for _, tag := range matched {
				if tag.Label == "TG" {
					t.Fatal("source tag matched an unrelated title")
				}
			}
			other := finalizeTaggedImport(t, c, "http", "http", nil, matched)
			if slices.Contains(other.Tags, "TG") {
				t.Fatal("HTTP import received Telegram source tag")
			}
			if _, err := c.ReplaceAutoVideoTags(ctx, v.ID, nil); err != nil {
				t.Fatal(err)
			}
			if err := c.ReconcileVideoTags(ctx); err != nil {
				t.Fatal(err)
			}
			saved, err := c.GetVideo(ctx, v.ID)
			if err != nil || !slices.Contains(saved.Tags, "TG") {
				t.Fatalf("source tag lost during maintenance: %v %v", saved, err)
			}
		})
	}
}

func TestTelegramImportReclassifiesExistingUserTag(t *testing.T) {
	c := telegramTestCatalog(t)
	ctx := context.Background()
	if _, err := c.CreateTagAndClassify(ctx, TelegramTagLabel, "user"); err != nil {
		t.Fatal(err)
	}
	before := mustTagByLabel(t, ctx, c, TelegramTagLabel)
	video := finalizeTaggedImport(t, c, "existing-tg", "telegram", nil, nil)
	after := mustTagByLabel(t, ctx, c, TelegramTagLabel)
	if after.ID != before.ID || after.Source != "generated" {
		t.Fatalf("existing Telegram tag was not reclassified in place: before=%#v after=%#v", before, after)
	}
	if !slices.Contains(video.Tags, TelegramTagLabel) {
		t.Fatalf("import lost its Telegram tag: %v", video.Tags)
	}
}

func TestTelegramTagSurvivesLastVideoDeletionAndRestore(t *testing.T) {
	for _, deleteTag := range []bool{false, true} {
		name := "retain tag"
		if deleteTag {
			name = "explicitly delete tag"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			c := telegramTestCatalog(t)
			video := finalizeTaggedImport(t, c, "restore-tg", "telegram", nil, nil)
			original := mustTagByLabel(t, ctx, c, TelegramTagLabel)
			if err := c.DeleteVideoWithTombstone(ctx, video.ID); err != nil {
				t.Fatal(err)
			}
			if err := c.ReconcileVideoTags(ctx); err != nil {
				t.Fatal(err)
			}
			retained := mustTagByLabel(t, ctx, c, TelegramTagLabel)
			if retained.ID != original.ID || retained.Source != "generated" || retained.Count != 0 {
				t.Fatalf("unreferenced Telegram tag = %#v, want original generated definition with zero videos", retained)
			}
			if deleteTag {
				if _, err := c.DeleteTag(ctx, original.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := c.RestoreDeletedVideo(ctx, video.ID, func(string, string) (DeletedVideoSourceInfo, error) {
				return DeletedVideoSourceInfo{Size: video.Size}, nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := c.ReconcileVideoTags(ctx); err != nil {
				t.Fatal(err)
			}
			saved, err := c.GetVideo(ctx, video.ID)
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(saved.Tags, TelegramTagLabel) == deleteTag {
				t.Fatalf("restored Telegram tags = %v, explicitly deleted tag=%v", saved.Tags, deleteTag)
			}
			if _, found, err := c.LookupTagLabel(ctx, TelegramTagLabel); err != nil || found == deleteTag {
				t.Fatalf("Telegram definition after restore: found=%v err=%v, explicitly deleted=%v", found, err, deleteTag)
			}
			if !deleteTag {
				metadata, err := c.ListVideoTagMetadata(ctx, []string{video.ID})
				if err != nil {
					t.Fatal(err)
				}
				if assignment := metadata[video.ID][TelegramTagLabel]; assignment.Source != "telegram" || assignment.Evidence != "Telegram 视频导入" {
					t.Fatalf("restored Telegram provenance = %#v", assignment)
				}
				if c.hasManualTags(ctx, video.ID) {
					t.Fatal("restoring a source tag locked automatic content tags")
				}
			}
		})
	}
}

func TestStartupPreservesAndReclassifiesTelegramSourceTag(t *testing.T) {
	for _, source := range []string{"user", "generated"} {
		t.Run(source, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "catalog.db")
			c, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if c != nil {
					_ = c.Close()
				}
			})
			if _, _, err := c.TelegramOffset(ctx, 123); err != nil {
				t.Fatal(err)
			}
			if _, err := c.EnsureTag(ctx, "content", "user"); err != nil {
				t.Fatal(err)
			}
			video := finalizeTaggedImport(t, c, "restart-tg", "telegram", []string{"content"}, nil)
			tag := mustTagByLabel(t, ctx, c, TelegramTagLabel)
			if _, err := c.db.ExecContext(ctx, `UPDATE tags SET source = ? WHERE id = ?`, source, tag.ID); err != nil {
				t.Fatal(err)
			}
			// Startup migration and repeated startup must preserve the source tag,
			// existing assignments, and the independent manual content tag.
			for attempt := 0; attempt < 2; attempt++ {
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
				c, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
				migrated := mustTagByLabel(t, ctx, c, TelegramTagLabel)
				if migrated.ID != tag.ID || migrated.Source != "generated" {
					t.Fatalf("attempt %d: Telegram tag = %#v, want ID %d and generated source", attempt, migrated, tag.ID)
				}
				if err := c.ReconcileVideoTags(ctx); err != nil {
					t.Fatal(err)
				}
				saved, err := c.GetVideo(ctx, video.ID)
				if err != nil || !sameStrings(saved.Tags, []string{TelegramTagLabel, "content"}) {
					t.Fatalf("attempt %d: video = %#v, err=%v", attempt, saved, err)
				}
				if !c.hasManualTags(ctx, video.ID) {
					t.Fatal("classification migration unlocked manual content tags")
				}
				metadata, err := c.ListVideoTagMetadata(ctx, []string{video.ID})
				if err != nil {
					t.Fatal(err)
				}
				if got := metadata[video.ID][TelegramTagLabel]; got.Source != "telegram" || got.Evidence != "Telegram 视频导入" {
					t.Fatalf("attempt %d: Telegram assignment metadata = %#v", attempt, got)
				}
				if got := metadata[video.ID]["content"].Source; got != "manual" {
					t.Fatalf("attempt %d: content tag source = %q, want manual", attempt, got)
				}
			}
		})
	}
}
