package catalog

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
)

func seedRetiredAVSeriesTag(t *testing.T, cat *Catalog, label string) Tag {
	t.Helper()
	ctx := context.Background()
	if _, err := cat.db.ExecContext(ctx, `
INSERT INTO tags (label, match_rules, source, origin, created_at, updated_at)
VALUES (?, '{}', 'generated', 'av_series', 1, 1)`, label); err != nil {
		t.Fatal(err)
	}
	tag, err := cat.getTagByLabel(ctx, label)
	if err != nil {
		t.Fatal(err)
	}
	return tag
}

func TestStartupMergesRetiredAVSeriesIntoAV(t *testing.T) {
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
	seedCustomTagRules(t, cat)
	ssni := seedRetiredAVSeriesTag(t, cat, "SSNI")
	seedRetiredAVSeriesTag(t, cat, "ABP")
	seedRetiredAVSeriesTag(t, cat, "FINAL")
	user, err := cat.EnsureTag(ctx, "OBA", "user")
	if err != nil {
		t.Fatal(err)
	}
	crawler, err := cat.EnsureCrawlerTag(ctx, "FC2PPV")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"auto-series", "manual-series", "existing-av", "user-prefix", "crawler-prefix"} {
		seedTagMaintenanceVideo(t, cat, id, "ordinary clip", id+".mp4")
	}
	if err := cat.SetManualVideoTags(ctx, "manual-series", []string{"SSNI", "ABP", "OBA"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SetManualVideoTags(ctx, "existing-av", []string{"AV"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"auto-series", "existing-av"} {
		if err := cat.insertVideoTag(ctx, id, ssni.ID, "auto", "标题:SSNI-001"); err != nil {
			t.Fatal(err)
		}
		if err := cat.syncVideoTagsJSON(ctx, id, id == "existing-av"); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.SetManualVideoTags(ctx, "user-prefix", []string{user.Label}); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.EnsureCrawlerTagForVideo(ctx, "crawler-prefix", crawler.Label); err != nil {
		t.Fatal(err)
	}
	// Reopening runs the actual startup migration; a second reopen must be inert.
	for attempt := 0; attempt < 2; attempt++ {
		if err := cat.Close(); err != nil {
			t.Fatal(err)
		}
		cat, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		for id, want := range map[string][]string{
			"auto-series": {"AV"}, "manual-series": {"AV", "OBA"},
			"existing-av": {"AV"}, "user-prefix": {"OBA"}, "crawler-prefix": {"FC2PPV"},
		} {
			video, err := cat.GetVideo(ctx, id)
			if err != nil || !sameStrings(video.Tags, want) {
				t.Fatalf("attempt %d: %s tags = %#v, want %#v; err=%v", attempt, id, video, want, err)
			}
			if id == "manual-series" && !cat.hasManualTags(ctx, id) {
				t.Fatal("migration unlocked the manually curated video")
			}
		}
		for _, label := range []string{"SSNI", "ABP", "FINAL"} {
			if _, err := cat.getTagByLabel(ctx, label); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("retired %s tag remains: %v", label, err)
			}
		}
		metadata, err := cat.ListVideoTagMetadata(ctx, []string{"auto-series", "manual-series", "existing-av"})
		if err != nil {
			t.Fatal(err)
		}
		if got := metadata["auto-series"]["AV"]; got.Source != "auto" || got.Evidence != "标题:SSNI-001" {
			t.Fatalf("automatic AV metadata = %#v", got)
		}
		if got := metadata["manual-series"]["AV"]; got.Source != "manual" {
			t.Fatalf("manual AV metadata = %#v", got)
		}
		if got := metadata["existing-av"]["AV"]; got.Source != "manual" || got.Evidence != "" {
			t.Fatalf("existing AV assignment was overwritten: %#v", got)
		}
	}
}

func TestStartupPreservesAVSeriesClassificationOverRetiredAVSources(t *testing.T) {
	for _, test := range []struct {
		name, avSource, seriesSource string
	}{
		{"series-auto", "series", "auto"},
		{"propagated-auto", "propagated", "auto"},
		{"series-legacy", " SERIES ", "legacy"},
		{"propagated-legacy", " Propagated ", "legacy"},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			seedCustomTagRules(t, cat)
			av := mustTagByLabel(t, ctx, cat, "AV")
			ssni := seedRetiredAVSeriesTag(t, cat, "SSNI")
			for _, videoID := range []string{"unlocked", "locked"} {
				seedTagMaintenanceVideo(t, cat, videoID, "ordinary clip", videoID+".mp4")
				// Write the retired source directly; the runtime assignment API
				// normalizes it and cannot reproduce this legacy database state.
				if _, err := cat.db.ExecContext(ctx, `
INSERT INTO video_tags (video_id, tag_id, source, evidence, created_at) VALUES
 (?, ?, ?, '旧聚类关联', 1),
 (?, ?, ?, '标题:SSNI-001', 2)`,
					videoID, av.ID, test.avSource, videoID, ssni.ID, test.seriesSource); err != nil {
					t.Fatal(err)
				}
				if err := cat.syncVideoTagsJSON(ctx, videoID, videoID == "locked"); err != nil {
					t.Fatal(err)
				}
			}

			for attempt := 0; attempt < 2; attempt++ {
				if err := cat.Close(); err != nil {
					t.Fatal(err)
				}
				cat, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
				for _, videoID := range []string{"unlocked", "locked"} {
					video, err := cat.GetVideo(ctx, videoID)
					if err != nil {
						t.Fatal(err)
					}
					if !sameStrings(video.Tags, []string{"AV"}) {
						t.Fatalf("attempt %d: %s lost AV classification: tags=%v", attempt, videoID, video.Tags)
					}
					if cat.hasManualTags(ctx, videoID) != (videoID == "locked") {
						t.Fatalf("migration changed the manual lock for %s", videoID)
					}
				}
				metadata, err := cat.ListVideoTagMetadata(ctx, []string{"unlocked", "locked"})
				if err != nil {
					t.Fatal(err)
				}
				for _, videoID := range []string{"unlocked", "locked"} {
					if got := metadata[videoID]["AV"]; got.Source != test.seriesSource || got.Evidence != "标题:SSNI-001" {
						t.Fatalf("attempt %d: %s AV metadata = %#v", attempt, videoID, got)
					}
				}
				if _, err := cat.getTagByLabel(ctx, "SSNI"); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("retired SSNI tag remains: %v", err)
				}
			}
		})
	}
}

func TestAVSeriesRetirementDoesNotRestoreDisabledAV(t *testing.T) {
	for _, mode := range []string{"deleted", "empty-prefixes"} {
		t.Run(mode, func(t *testing.T) {
			cat, ctx := openTagMaintenanceTestCatalog(t)
			av := mustTagByLabel(t, ctx, cat, "AV")
			switch mode {
			case "deleted":
				if _, err := cat.DeleteTag(ctx, av.ID); err != nil {
					t.Fatal(err)
				}
			case "empty-prefixes":
				if _, err := cat.UpdateTag(ctx, av.ID, avRuleFromPrefixes(nil)); err != nil {
					t.Fatal(err)
				}
			}
			seedTagMaintenanceVideo(t, cat, "disabled-series", "ordinary", "ordinary.mp4")
			seedRetiredAVSeriesTag(t, cat, "SSNI")
			if err := cat.SetManualVideoTags(ctx, "disabled-series", []string{"SSNI"}); err != nil {
				t.Fatal(err)
			}
			if err := cat.ReconcileVideoTags(ctx); err != nil {
				t.Fatal(err)
			}
			video, err := cat.GetVideo(ctx, "disabled-series")
			if err != nil || len(video.Tags) != 0 {
				t.Fatalf("disabled video = %#v, err=%v", video, err)
			}
			if mode != "empty-prefixes" {
				if _, err := cat.getTagByLabel(ctx, "AV"); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("disabled AV was restored: %v", err)
				}
			}
		})
	}
}

func TestAVSeriesRetirementRollsBackWhenCleanupFails(t *testing.T) {
	cat, ctx := openTagMaintenanceTestCatalog(t)
	seedTagMaintenanceVideo(t, cat, "rollback-series", "ordinary", "ordinary.mp4")
	seedRetiredAVSeriesTag(t, cat, "SSNI")
	if err := cat.SetManualVideoTags(ctx, "rollback-series", []string{"SSNI"}); err != nil {
		t.Fatal(err)
	}
	before, err := cat.GetVideo(ctx, "rollback-series")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.db.ExecContext(ctx, `
CREATE TRIGGER reject_series_delete BEFORE DELETE ON tags
WHEN OLD.origin = 'av_series'
BEGIN SELECT RAISE(ABORT, 'injected cleanup failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := cat.removeAutomaticTaggingArtifacts(ctx); err == nil {
		t.Fatal("expected injected cleanup failure")
	}
	after, err := cat.GetVideo(ctx, "rollback-series")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("failed migration changed video: before=%#v after=%#v err=%v", before, after, err)
	}
	metadata, err := cat.ListVideoTagMetadata(ctx, []string{"rollback-series"})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := metadata["rollback-series"]["AV"]; exists {
		t.Fatal("failed migration left a partial AV assignment")
	}
	if _, err := cat.getTagByLabel(ctx, "SSNI"); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationRemovesRetiredTagSettingsAndAssignments(t *testing.T) {
	cat, ctx := openTagMaintenanceTestCatalog(t)
	seedTagMaintenanceVideo(t, cat, "old-tags", "ordinary clip", "clip.mp4")
	userTag, err := cat.EnsureTag(ctx, "user-label", "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.db.ExecContext(ctx, `
INSERT INTO tags (label, source, created_at, updated_at) VALUES ('old-generated', 'generated', 1, 1);
INSERT INTO video_tags (video_id, tag_id, source, created_at)
SELECT 'old-tags', id, 'auto', 1 FROM tags WHERE label = 'old-generated';`); err != nil {
		t.Fatal(err)
	}
	if err := cat.insertVideoTag(ctx, "old-tags", userTag.ID, "propagated", "标题聚类"); err != nil {
		t.Fatal(err)
	}
	keys := []string{"tags.auto_generate_enabled", "tags.retag.v2_done", "tags.maintenance.last_run_ms"}
	for _, key := range keys {
		if err := cat.SetSetting(ctx, key, "1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cat.ReconcileVideoTags(ctx); err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if value, err := cat.GetSetting(ctx, key, "missing"); err != nil || value != "missing" {
			t.Fatalf("retired setting %q = %q, %v", key, value, err)
		}
	}
	if _, err := cat.getTagByLabel(ctx, "old-generated"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("retired generated tag lookup = %v", err)
	}
	if _, err := cat.getTagByID(ctx, userTag.ID); err != nil {
		t.Fatalf("user tag was removed: %v", err)
	}
	video, err := cat.GetVideo(ctx, "old-tags")
	if err != nil || len(video.Tags) != 0 {
		t.Fatalf("retired assignments survived migration: video=%#v, err=%v", video, err)
	}
}
