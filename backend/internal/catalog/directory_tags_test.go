package catalog

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/video-site/backend/internal/tagging"
)

func TestMatchTagAssignmentsKeepsDirectoryBoundariesAndPriority(t *testing.T) {
	cat, ctx := openTagMaintenanceTestCatalog(t)
	for label, keywords := range map[string][]string{
		"旅行":  {"旅行", "旅游"},
		"跨目录": {"旅行2026", "2026旅行"},
	} {
		if _, err := cat.ensureTagWithRules(ctx, label, tagging.Rule{Keywords: keywords}, "user"); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name   string
		title  string
		parent string
		dirs   []string
		want   []TagAssignment
	}{
		{"ancestor", "clip", "2026", []string{"旅行", "2026"}, []TagAssignment{{Label: "旅行", Source: "auto", Evidence: "上级目录:旅行"}}},
		{"parent priority", "clip", "旅游", []string{"旅行", "旅游"}, []TagAssignment{{Label: "旅行", Source: "auto", Evidence: "目录:旅游"}}},
		{"title priority", "旅行", "旅游", []string{"旅游"}, []TagAssignment{{Label: "旅行", Source: "auto", Evidence: "标题:旅行"}}},
		{"no code across directories", "clip", "123", []string{"ABP", "123"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cat.MatchTagAssignments(ctx, tc.title, "clip.mp4", "", tc.parent, tc.dirs...)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) || (len(got) > 0 && !reflect.DeepEqual(got, tc.want)) {
				t.Fatalf("assignments = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestStoredDirectoryNamesSupportTagEditsAndRetagging(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	cat, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seedCustomTagRules(t, cat)

	t.Cleanup(func() {
		if cat != nil {
			_ = cat.Close()
		}
	})
	dirNames := []string{"旅行 ABP-123", "2026"}
	if err := cat.UpsertVideo(ctx, &Video{
		ID: "directory-video", DriveID: "drive", FileID: "clip", FileName: "clip.mp4",
		Title: "clip", DirName: "2026", AncestorDirNames: dirNames, Size: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	cat, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	assertTags := func(want ...string) {
		t.Helper()
		video, err := cat.GetVideo(ctx, "directory-video")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(video.AncestorDirNames, dirNames) || video.DirName != "2026" {
			t.Fatalf("directory metadata lost: %#v", video)
		}
		slices.Sort(video.Tags)
		slices.Sort(want)
		if !sameStrings(video.Tags, want) {
			t.Fatalf("tags = %#v, want %#v", video.Tags, want)
		}
	}
	assertTags("AV")
	if count, err := cat.CreateTagAndClassify(ctx, "旅行", "user"); err != nil || count != 1 {
		t.Fatalf("classify new tag = %d, %v", count, err)
	}
	assertTags("AV", "旅行")
	travel := mustTagByLabel(t, ctx, cat, "旅行")
	for _, keyword := range []string{"别处", "旅行"} {
		if _, _, err := cat.UpdateTagAndReconcile(ctx, travel.ID, tagging.Rule{Keywords: []string{keyword}}); err != nil {
			t.Fatal(err)
		}
		if keyword == "别处" {
			assertTags("AV")
		} else {
			assertTags("AV", "旅行")
		}
	}
	av := mustTagByLabel(t, ctx, cat, "AV")
	for _, prefix := range []string{"SSNI", "ABP"} {
		if _, _, err := cat.UpdateTagAndReconcile(ctx, av.ID, tagging.Rule{MatchAVCode: true, AVCodePrefixes: []string{prefix}}); err != nil {
			t.Fatal(err)
		}
		if prefix == "SSNI" {
			assertTags("旅行")
		} else {
			assertTags("AV", "旅行")
		}
	}
	if _, err := cat.ReplaceAutoVideoTags(ctx, "directory-video", nil); err != nil {
		t.Fatal(err)
	}
	if err := cat.ReconcileVideoTags(ctx); err != nil {
		t.Fatal(err)
	}
	assertTags("AV", "旅行")
}

func TestVideoDriveMigrationReplacesAncestorDirectoryNames(t *testing.T) {
	for name, names := range map[string][]string{"unknown": nil, "known": {"新目录", "目标"}} {
		t.Run(name, func(t *testing.T) {
			cat, ctx := openTagMaintenanceTestCatalog(t)
			if err := cat.UpsertVideo(ctx, &Video{
				ID: "move", DriveID: "old", FileID: "old-file", Title: "clip",
				DirName: "2026", AncestorDirNames: []string{"旧目录", "2026"},
			}); err != nil {
				t.Fatal(err)
			}
			if err := cat.MigrateVideoToDrive(ctx, "move", VideoDriveMigration{
				DriveID: "new", FileID: "new-file", DirName: "目标", AncestorDirNames: names,
			}); err != nil {
				t.Fatal(err)
			}
			video, err := cat.GetVideo(ctx, "move")
			if err != nil {
				t.Fatal(err)
			}
			if !sameStrings(video.AncestorDirNames, names) {
				t.Fatalf("migrated names = %#v, want %#v", video.AncestorDirNames, names)
			}
		})
	}
}

func TestOpenAddsAncestorDirectoryNamesToExistingVideos(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	cat, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cat != nil {
			_ = cat.Close()
		}
	})
	if _, err := cat.EnsureTag(ctx, "旅行", "user"); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpsertVideo(ctx, &Video{
		ID: "old-video", DriveID: "drive", FileID: "clip", Title: "clip", DirName: "旅行",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.db.ExecContext(ctx, `ALTER TABLE videos DROP COLUMN ancestor_dir_names`); err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	cat, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !hasColumn(t, cat, "videos", "ancestor_dir_names") {
		t.Fatal("existing catalog has no directory names column")
	}
	if err := cat.ReconcileVideoTags(ctx); err != nil {
		t.Fatal(err)
	}
	video, err := cat.GetVideo(ctx, "old-video")
	if err != nil {
		t.Fatal(err)
	}
	if video.DirName != "旅行" || len(video.AncestorDirNames) != 0 || !sameStrings(video.Tags, []string{"旅行"}) {
		t.Fatalf("old video lost parent-directory matching before rescan: %#v", video)
	}
}
