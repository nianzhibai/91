package catalog

import (
	"context"
	"reflect"
	"testing"

	"github.com/video-site/backend/internal/tagging"
)

func TestOpenDropsTagAliasesWithoutChangingRulesOrAssignments(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	seedCustomTagRules(t, cat)

	t.Cleanup(func() { _ = cat.Close() })
	if hasColumn(t, cat, "tags", "aliases") {
		t.Fatal("new catalog contains retired aliases column")
	}

	travel, err := cat.EnsureTag(ctx, "旅行", "user")
	if err != nil {
		t.Fatalf("create default tag: %v", err)
	}
	sports, err := cat.ensureTagWithRules(ctx, "运动", tagging.Rule{Keywords: []string{"跑步"}}, "user")
	if err != nil {
		t.Fatalf("create keyword tag: %v", err)
	}
	av := mustTagByLabel(t, ctx, cat, "AV")
	av, err = cat.UpdateTag(ctx, av.ID, tagging.Rule{MatchAVCode: true, AVCodePrefixes: []string{"FHD"}})
	if err != nil {
		t.Fatalf("configure AV prefixes: %v", err)
	}
	if err := cat.UpsertVideo(ctx, &Video{
		ID: "travel-video", DriveID: "drive", FileID: "travel.mp4",
		FileName: "旅行.mp4", Title: "旅行", Size: 1024,
	}); err != nil {
		t.Fatalf("create tagged video: %v", err)
	}
	before, err := cat.ListVideoTagMetadata(ctx, []string{"travel-video"})
	if err != nil {
		t.Fatalf("read assignments: %v", err)
	}
	if len(before["travel-video"]) != 1 {
		t.Fatalf("assignments before migration = %#v, want one travel tag", before)
	}

	// Simulate an existing database that still stores aliases alongside rules.
	if _, err := cat.db.ExecContext(ctx, `ALTER TABLE tags ADD COLUMN aliases TEXT NOT NULL DEFAULT '[]'`); err != nil {
		t.Fatalf("add retired column: %v", err)
	}
	for label, aliases := range map[string]string{
		"旅行": `["旅游"]`,
		"运动": `["健身"]`,
		"AV": `["ZZALIAS"]`,
	} {
		if _, err := cat.db.ExecContext(ctx, `UPDATE tags SET aliases = ? WHERE label = ?`, aliases, label); err != nil {
			t.Fatalf("seed aliases for %s: %v", label, err)
		}
	}

	// Reopening twice also verifies that removing the column is idempotent.
	for pass := 0; pass < 2; pass++ {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
		cat, err = Open(path)
		if err != nil {
			t.Fatalf("reopen catalog: %v", err)
		}
		if hasColumn(t, cat, "tags", "aliases") {
			t.Fatal("reopened catalog contains retired aliases column")
		}
		for _, want := range []Tag{travel, sports, av} {
			got, err := cat.getTagByLabel(ctx, want.Label)
			if err != nil {
				t.Fatalf("read tag %s: %v", want.Label, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("tag after migration = %#v, want %#v", got, want)
			}
		}
		for text, want := range map[string][]string{
			"旅行日记":        {"旅行"},
			"跑步记录":        {"运动"},
			"FHD-123":     {"AV"},
			"旅游":          nil,
			"健身":          nil,
			"运动":          nil,
			"ZZALIAS-123": nil,
		} {
			got, err := cat.MatchTags(ctx, text)
			if err != nil {
				t.Fatalf("match %q: %v", text, err)
			}
			if !sameStrings(got, want) {
				t.Fatalf("MatchTags(%q) = %#v, want %#v", text, got, want)
			}
		}
		after, err := cat.ListVideoTagMetadata(ctx, []string{"travel-video"})
		if err != nil {
			t.Fatalf("read assignments after migration: %v", err)
		}
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("assignments after migration = %#v, want %#v", after, before)
		}
	}
}
