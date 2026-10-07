package catalog

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/video-site/backend/internal/tagging"
)

func openTagMaintenanceTestCatalog(t *testing.T) (*Catalog, context.Context) {
	t.Helper()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	seedCustomTagRules(t, cat)
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	return cat, context.Background()
}

func seedTagMaintenanceVideo(t *testing.T, cat *Catalog, id, title, fileName string) {
	t.Helper()
	now := time.Now()
	if err := cat.UpsertVideo(context.Background(), &Video{
		ID:          id,
		DriveID:     "drive",
		FileID:      "file-" + id,
		FileName:    fileName,
		Title:       title,
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video %s: %v", id, err)
	}
}

func seedTagMaintenanceVideoRaw(t *testing.T, cat *Catalog, id, title, fileName string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := cat.db.ExecContext(context.Background(), `
INSERT INTO videos (id, drive_id, file_id, file_name, title, tags, tags_manual, published_at, created_at, updated_at)
VALUES (?, 'drive', ?, ?, ?, '[]', 0, ?, ?, ?)`,
		id, "file-"+id, fileName, title, now, now, now); err != nil {
		t.Fatalf("seed raw video %s: %v", id, err)
	}
}

func TestReplaceAutoVideoTagsPreservesIndependentSourcesAndManualLock(t *testing.T) {
	cat, ctx := openTagMaintenanceTestCatalog(t)
	seedTagMaintenanceVideo(t, cat, "replace", "ordinary", "ordinary.mp4")
	seedTagMaintenanceVideo(t, cat, "manual", "ordinary", "manual.mp4")

	for _, label := range []string{"old-auto", "new-auto", "crawler-tag", "manual-tag"} {
		if _, err := cat.EnsureTag(ctx, label, "user"); err != nil {
			t.Fatalf("ensure %s: %v", label, err)
		}
	}
	if _, err := cat.AddVideoTagAssignments(ctx, "replace", []TagAssignment{
		{Label: "old-auto", Source: "legacy", Evidence: "old"},
		{Label: "crawler-tag", Source: "crawler", Evidence: "script"},
	}); err != nil {
		t.Fatalf("seed assignments: %v", err)
	}
	if _, err := cat.ReplaceAutoVideoTags(ctx, "replace", []TagAssignment{
		{Label: "new-auto", Source: "auto", Evidence: "标题:new-auto"},
	}); err != nil {
		t.Fatalf("replace auto tags: %v", err)
	}
	got, err := cat.GetVideo(ctx, "replace")
	if err != nil {
		t.Fatalf("get replace video: %v", err)
	}
	if !sameStrings(got.Tags, []string{"new-auto", "crawler-tag"}) {
		t.Fatalf("tags = %#v, want new-auto + crawler-tag", got.Tags)
	}
	metadata, err := cat.ListVideoTagMetadata(ctx, []string{"replace"})
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if metadata["replace"]["crawler-tag"].Source != "crawler" || metadata["replace"]["new-auto"].Source != "auto" {
		t.Fatalf("metadata = %#v", metadata["replace"])
	}

	if err := cat.SetManualVideoTags(ctx, "manual", []string{"manual-tag"}); err != nil {
		t.Fatalf("lock manual video: %v", err)
	}
	if _, err := cat.ReplaceAutoVideoTags(ctx, "manual", []TagAssignment{{Label: "new-auto", Source: "auto"}}); err != nil {
		t.Fatalf("replace locked video: %v", err)
	}
	locked, err := cat.GetVideo(ctx, "manual")
	if err != nil {
		t.Fatalf("get manual video: %v", err)
	}
	if !sameStrings(locked.Tags, []string{"manual-tag"}) {
		t.Fatalf("manual tags = %#v, want unchanged", locked.Tags)
	}

	if _, err := cat.db.ExecContext(ctx, `UPDATE videos SET updated_at = 123 WHERE id = 'replace'`); err != nil {
		t.Fatalf("set stable timestamp: %v", err)
	}
	if _, err := cat.ReplaceAutoVideoTags(ctx, "replace", []TagAssignment{{Label: "new-auto", Source: "auto"}}); err != nil {
		t.Fatalf("idempotent replace: %v", err)
	}
	var updatedAt int64
	if err := cat.db.QueryRowContext(ctx, `SELECT updated_at FROM videos WHERE id = 'replace'`).Scan(&updatedAt); err != nil {
		t.Fatalf("read timestamp: %v", err)
	}
	if updatedAt != 123 {
		t.Fatalf("idempotent replacement updated video timestamp to %d", updatedAt)
	}
}

func TestReplaceAutoVideoTagsRespectsSourcePriorityAndRefreshesEvidence(t *testing.T) {
	cat, ctx := openTagMaintenanceTestCatalog(t)
	seedTagMaintenanceVideo(t, cat, "priority", "source-tag clip", "priority.mp4")
	seedTagMaintenanceVideo(t, cat, "evidence", "source-tag clip", "evidence.mp4")
	if _, err := cat.EnsureTag(ctx, "source-tag", "user"); err != nil {
		t.Fatalf("ensure source tag: %v", err)
	}
	if _, err := cat.AddVideoTagAssignments(ctx, "priority", []TagAssignment{{
		Label: "source-tag", Source: "crawler", Evidence: "脚本标签",
	}}); err != nil {
		t.Fatalf("seed crawler tag: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx, `UPDATE videos SET updated_at = 321 WHERE id = 'priority'`); err != nil {
		t.Fatalf("set priority timestamp: %v", err)
	}
	changed, err := cat.ReplaceAutoVideoTags(ctx, "priority", []TagAssignment{{
		Label: "source-tag", Source: "auto", Evidence: "标题:source-tag",
	}})
	if err != nil {
		t.Fatalf("replace priority auto: %v", err)
	}
	if changed {
		t.Fatal("auto replacement changed a crawler-owned tag")
	}
	var updatedAt int64
	if err := cat.db.QueryRowContext(ctx, `SELECT updated_at FROM videos WHERE id = 'priority'`).Scan(&updatedAt); err != nil {
		t.Fatalf("read priority timestamp: %v", err)
	}
	if updatedAt != 321 {
		t.Fatalf("priority timestamp = %d, want unchanged", updatedAt)
	}
	metadata, err := cat.ListVideoTagMetadata(ctx, []string{"priority"})
	if err != nil {
		t.Fatalf("priority metadata: %v", err)
	}
	if got := metadata["priority"]["source-tag"]; got.Source != "crawler" || got.Evidence != "脚本标签" {
		t.Fatalf("priority metadata = %#v, want crawler evidence", got)
	}

	if _, err := cat.ReplaceAutoVideoTags(ctx, "evidence", []TagAssignment{{
		Label: "source-tag", Source: "auto", Evidence: "标题:source-tag",
	}}); err != nil {
		t.Fatalf("seed auto evidence: %v", err)
	}
	changed, err = cat.ReplaceAutoVideoTags(ctx, "evidence", []TagAssignment{{
		Label: "source-tag", Source: "auto", Evidence: "文件名:source-tag",
	}})
	if err != nil {
		t.Fatalf("refresh auto evidence: %v", err)
	}
	if !changed {
		t.Fatal("evidence refresh was not reported as a change")
	}
	metadata, err = cat.ListVideoTagMetadata(ctx, []string{"evidence"})
	if err != nil {
		t.Fatalf("evidence metadata: %v", err)
	}
	if got := metadata["evidence"]["source-tag"]; got.Source != "auto" || got.Evidence != "文件名:source-tag" {
		t.Fatalf("evidence metadata = %#v, want refreshed auto evidence", got)
	}
}

func TestRetagVideosBatchRefreshesExistingTagMatches(t *testing.T) {
	cat, ctx := openTagMaintenanceTestCatalog(t)
	seedTagMaintenanceVideo(t, cat, "a-auto", "fresh-keyword clip", "a.mp4")
	seedTagMaintenanceVideo(t, cat, "b-manual", "fresh-keyword clip", "b.mp4")

	fresh, err := cat.EnsureTag(ctx, "fresh-keyword", "user")
	if err != nil {
		t.Fatalf("ensure fresh tag: %v", err)
	}
	stale, err := cat.EnsureTag(ctx, "stale-tag", "user")
	if err != nil {
		t.Fatalf("ensure stale tag: %v", err)
	}
	if err := cat.insertVideoTag(ctx, "a-auto", stale.ID, "legacy", "legacy"); err != nil {
		t.Fatalf("seed stale legacy: %v", err)
	}
	if err := cat.syncVideoTagsJSON(ctx, "a-auto", false); err != nil {
		t.Fatalf("sync stale legacy: %v", err)
	}
	if err := cat.SetManualVideoTags(ctx, "b-manual", []string{"stale-tag"}); err != nil {
		t.Fatalf("lock manual: %v", err)
	}

	matcher := tagging.NewMatcher([]tagging.TagRule{{
		Label: fresh.Label,
		Rule:  tagging.Rule{Keywords: []string{"fresh-keyword"}},
	}})
	processed, lastID, done, err := cat.RetagVideosBatch(ctx, matcher, "", 10)
	if err != nil {
		t.Fatalf("retag: %v", err)
	}
	if processed != 2 || lastID != "b-manual" || !done {
		t.Fatalf("retag result = %d/%q/%v", processed, lastID, done)
	}
	autoVideo, _ := cat.GetVideo(ctx, "a-auto")
	if !sameStrings(autoVideo.Tags, []string{"fresh-keyword"}) {
		t.Fatalf("auto tags = %#v, want fresh-keyword", autoVideo.Tags)
	}
	manualVideo, _ := cat.GetVideo(ctx, "b-manual")
	if !sameStrings(manualVideo.Tags, []string{"stale-tag"}) {
		t.Fatalf("manual tags = %#v", manualVideo.Tags)
	}

	processed, _, done, err = cat.RetagVideosBatch(ctx, matcher, "", 10)
	if err != nil || processed != 2 || !done {
		t.Fatalf("idempotent retag = %d/%v/%v", processed, done, err)
	}
}

func hasTagLabel(tags []Tag, label string) bool {
	for _, tag := range tags {
		if tag.Label == label {
			return true
		}
	}
	return false
}

func TestAVCodesOnlyAssignAVAndDoNotCreateTags(t *testing.T) {
	cat, ctx := openTagMaintenanceTestCatalog(t)
	codes := []string{"FC2PPV-3259498", "FC2PPV-4162750", "FC2PPV-4768873"}
	for i, code := range codes {
		id := "fc2ppv-" + string(rune('a'+i))
		seedTagMaintenanceVideo(t, cat, id, code, code+".mp4")
		assignments, err := cat.MatchTagAssignments(ctx, code, code+".mp4", "", "")
		if err != nil {
			t.Fatalf("match assignments for %s: %v", code, err)
		}
		if !sameStrings(assignmentLabels(assignments), []string{"AV"}) {
			t.Fatalf("assignments for %s = %#v, want AV only", code, assignments)
		}
		if _, err := cat.ReplaceAutoVideoTags(ctx, id, assignments); err != nil {
			t.Fatalf("attach AV tags for %s: %v", id, err)
		}
	}
	for i := range codes {
		id := "fc2ppv-" + string(rune('a'+i))
		video, err := cat.GetVideo(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if !hasTag(video.Tags, "AV") {
			t.Fatalf("%s tags = %#v, want AV", id, video.Tags)
		}
		if hasTag(video.Tags, "FC2PPV") {
			t.Fatalf("%s unexpectedly has a series tag: %#v", id, video.Tags)
		}
	}
	metadata, err := cat.ListVideoTagMetadata(ctx, []string{"fc2ppv-a"})
	if err != nil {
		t.Fatalf("FC2PPV metadata: %v", err)
	}
	if got := metadata["fc2ppv-a"]["AV"]; got.Source != "auto" || got.Evidence != "标题:FC2PPV-3259498" {
		t.Fatalf("AV metadata = %#v, want auto title evidence", got)
	}
	if _, err := cat.getTagByLabel(ctx, "FC2PPV"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("matching created FC2PPV tag: %v", err)
	}
	if err := cat.ReconcileVideoTags(ctx); err != nil {
		t.Fatalf("retag videos: %v", err)
	}
	if _, err := cat.getTagByLabel(ctx, "FC2PPV"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("retagging created FC2PPV tag: %v", err)
	}
}

func TestUpdateAVTagAndReconcileOnlyChangesAVScope(t *testing.T) {
	cat, ctx := openTagMaintenanceTestCatalog(t)
	seedTagMaintenanceVideo(t, cat, "old-av-prefix", "OBA-334456", "OBA-334456.mp4")
	seedTagMaintenanceVideo(t, cat, "new-av-prefix", "FHD-78824", "FHD-78824.mp4")

	assignments, err := cat.MatchTagAssignments(ctx, "OBA-334456", "OBA-334456.mp4", "", "")
	if err != nil {
		t.Fatalf("match old AV prefix: %v", err)
	}
	if !sameStrings(assignmentLabels(assignments), []string{"AV"}) {
		t.Fatalf("old AV assignments = %#v", assignments)
	}
	if _, err := cat.ReplaceAutoVideoTags(ctx, "old-av-prefix", assignments); err != nil {
		t.Fatalf("seed old AV assignments: %v", err)
	}
	if _, err := cat.EnsureTag(ctx, "unrelated-tag", "user"); err != nil {
		t.Fatalf("ensure unrelated tag: %v", err)
	}
	if _, err := cat.AddVideoTagAssignments(ctx, "old-av-prefix", []TagAssignment{{
		Label: "unrelated-tag", Source: "auto", Evidence: "must survive AV reconcile",
	}}); err != nil {
		t.Fatalf("seed unrelated assignment: %v", err)
	}
	orphan, err := cat.ensureTagDefinition(ctx, "unrelated-generated", tagging.Rule{}, "generated")
	if err != nil {
		t.Fatalf("ensure unrelated generated tag: %v", err)
	}

	av := mustTagByLabel(t, ctx, cat, avTagLabel)
	prefixes := make([]string, 0, len(av.MatchRules.AVCodePrefixes)+1)
	for _, prefix := range av.MatchRules.AVCodePrefixes {
		if prefix != "OBA" {
			prefixes = append(prefixes, prefix)
		}
	}
	prefixes = append(prefixes, "FHD")
	updated, changed, err := cat.UpdateTagAndReconcile(ctx, av.ID, tagging.Rule{
		MatchAVCode: true, AVCodePrefixes: prefixes,
	})
	if err != nil {
		t.Fatalf("update and reconcile AV prefixes: %v", err)
	}
	if changed != 2 || stringSliceContains(updated.MatchRules.AVCodePrefixes, "OBA") || !stringSliceContains(updated.MatchRules.AVCodePrefixes, "FHD") {
		t.Fatalf("updated AV = %#v, changed = %d", updated, changed)
	}

	oldVideo, err := cat.GetVideo(ctx, "old-av-prefix")
	if err != nil {
		t.Fatalf("get old-prefix video: %v", err)
	}
	if !sameStrings(oldVideo.Tags, []string{"unrelated-tag"}) {
		t.Fatalf("old-prefix tags = %#v, want unrelated tag only", oldVideo.Tags)
	}
	newVideo, err := cat.GetVideo(ctx, "new-av-prefix")
	if err != nil {
		t.Fatalf("get new-prefix video: %v", err)
	}
	if !sameStrings(newVideo.Tags, []string{"AV"}) {
		t.Fatalf("new-prefix tags = %#v, want AV only", newVideo.Tags)
	}
	if _, err := cat.getTagByLabel(ctx, "OBA"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("removed AV series OBA still exists: %v", err)
	}
	if _, err := cat.getTagByLabel(ctx, "FHD"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("editing AV created FHD series tag: %v", err)
	}
	if _, err := cat.getTagByID(ctx, orphan.ID); err != nil {
		t.Fatalf("AV reconcile removed unrelated generated tag: %v", err)
	}
	metadata, err := cat.ListVideoTagMetadata(ctx, []string{"old-av-prefix", "new-av-prefix"})
	if err != nil {
		t.Fatalf("list reconciled metadata: %v", err)
	}
	if got := metadata["old-av-prefix"]["unrelated-tag"]; got.Source != "auto" || got.Evidence != "must survive AV reconcile" {
		t.Fatalf("unrelated metadata = %#v", got)
	}
	if got := metadata["new-av-prefix"]["AV"]; got.Source != "auto" || got.Evidence != "标题:FHD-78824" {
		t.Fatalf("AV metadata = %#v", got)
	}
}

func TestUpdateAVTagAndReconcileCanDisableAllPrefixes(t *testing.T) {
	cat, ctx := openTagMaintenanceTestCatalog(t)
	seedTagMaintenanceVideo(t, cat, "disable-av-prefixes", "SSNI-001", "SSNI-001.mp4")
	assignments, err := cat.MatchTagAssignments(ctx, "SSNI-001", "SSNI-001.mp4", "", "")
	if err != nil {
		t.Fatalf("match AV before disabling: %v", err)
	}
	if _, err := cat.ReplaceAutoVideoTags(ctx, "disable-av-prefixes", assignments); err != nil {
		t.Fatalf("seed AV assignments: %v", err)
	}

	av := mustTagByLabel(t, ctx, cat, avTagLabel)
	updated, _, err := cat.UpdateTagAndReconcile(ctx, av.ID, tagging.Rule{})
	if err != nil {
		t.Fatalf("disable all AV prefixes: %v", err)
	}
	if !updated.MatchRules.IsEmpty() {
		t.Fatalf("disabled AV rule = %#v, want empty", updated.MatchRules)
	}
	video, err := cat.GetVideo(ctx, "disable-av-prefixes")
	if err != nil {
		t.Fatalf("get video after disabling AV: %v", err)
	}
	if len(video.Tags) != 0 {
		t.Fatalf("disabled AV video tags = %#v", video.Tags)
	}
	if _, err := cat.getTagByLabel(ctx, "SSNI"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("disabled AV retained SSNI series: %v", err)
	}
	if assignments, err := cat.MatchTagAssignments(ctx, "SSNI-001", "SSNI-001.mp4", "", ""); err != nil || len(assignments) != 0 {
		t.Fatalf("disabled AV assignments = %#v, %v", assignments, err)
	}
}

func TestUpdateAVTagAndReconcilePreservesUserPrefixTags(t *testing.T) {
	cat, ctx := openTagMaintenanceTestCatalog(t)
	oba, err := cat.EnsureTag(ctx, "OBA", "user")
	if err != nil {
		t.Fatalf("ensure user OBA tag: %v", err)
	}
	if _, err := cat.UpdateTag(ctx, oba.ID, tagging.Rule{Keywords: []string{"keep-oba-tag"}}); err != nil {
		t.Fatalf("set user OBA rule: %v", err)
	}
	seedTagMaintenanceVideo(t, cat, "oba-code", "OBA-334456", "OBA-334456.mp4")
	seedTagMaintenanceVideo(t, cat, "oba-ordinary", "keep-oba-tag", "ordinary.mp4")
	if _, err := cat.ClassifyTagByID(ctx, oba.ID); err != nil {
		t.Fatalf("classify user OBA tag: %v", err)
	}
	assignments, err := cat.MatchTagAssignments(ctx, "OBA-334456", "OBA-334456.mp4", "", "")
	if err != nil {
		t.Fatalf("match colliding AV series: %v", err)
	}
	if _, err := cat.ReplaceAutoVideoTags(ctx, "oba-code", assignments); err != nil {
		t.Fatalf("seed colliding AV assignments: %v", err)
	}

	av := mustTagByLabel(t, ctx, cat, avTagLabel)
	prefixes := make([]string, 0, len(av.MatchRules.AVCodePrefixes))
	for _, prefix := range av.MatchRules.AVCodePrefixes {
		if prefix != "OBA" {
			prefixes = append(prefixes, prefix)
		}
	}
	if _, _, err := cat.UpdateTagAndReconcile(ctx, av.ID, tagging.Rule{
		MatchAVCode: true, AVCodePrefixes: prefixes,
	}); err != nil {
		t.Fatalf("remove colliding AV prefix: %v", err)
	}

	codeVideo, err := cat.GetVideo(ctx, "oba-code")
	if err != nil {
		t.Fatalf("get OBA code video: %v", err)
	}
	if len(codeVideo.Tags) != 0 {
		t.Fatalf("code video retained stale collision tags: %#v", codeVideo.Tags)
	}
	ordinaryVideo, err := cat.GetVideo(ctx, "oba-ordinary")
	if err != nil {
		t.Fatalf("get ordinary OBA video: %v", err)
	}
	if !sameStrings(ordinaryVideo.Tags, []string{"OBA"}) {
		t.Fatalf("ordinary OBA tags = %#v", ordinaryVideo.Tags)
	}
	remaining := mustTagByLabel(t, ctx, cat, "OBA")
	if remaining.ID != oba.ID || remaining.Source != "user" {
		t.Fatalf("user OBA tag changed = %#v", remaining)
	}
}

func TestDeletingAVTagDisablesAVCodeMatching(t *testing.T) {
	cat, ctx := openTagMaintenanceTestCatalog(t)
	seedTagMaintenanceVideo(t, cat, "disabled-av", "FC2PPV-4162750", "FC2PPV-4162750.mp4")
	av := mustTagByLabel(t, ctx, cat, "AV")
	if _, err := cat.DeleteTag(ctx, av.ID); err != nil {
		t.Fatalf("delete AV tag: %v", err)
	}

	assignments, err := cat.MatchTagAssignments(ctx, "FC2PPV-4162750", "FC2PPV-4162750.mp4", "", "")
	if err != nil {
		t.Fatalf("match disabled AV assignments: %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("assignments = %#v, want none after deleting AV", assignments)
	}
	if _, err := cat.getTagByLabel(ctx, "FC2PPV"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("FC2PPV tag exists after disabled AV matching: %v", err)
	}
	if err := cat.ReconcileVideoTags(ctx); err != nil {
		t.Fatalf("post-startup maintenance with deleted AV: %v", err)
	}
	video, err := cat.GetVideo(ctx, "disabled-av")
	if err != nil {
		t.Fatalf("get disabled AV video: %v", err)
	}
	if len(video.Tags) != 0 {
		t.Fatalf("disabled AV video tags = %#v, want none", video.Tags)
	}
	if _, err := cat.getTagByLabel(ctx, "AV"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("AV tag was reseeded after delete: %v", err)
	}
	if _, err := cat.getTagByLabel(ctx, "FC2PPV"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("FC2PPV tag was generated after AV delete: %v", err)
	}
}

func TestPostStartupMaintenanceRemovesInvalidAVSeriesTags(t *testing.T) {
	cat, ctx := openTagMaintenanceTestCatalog(t)
	seedTagMaintenanceVideo(t, cat, "invalid-av-series", "ordinary title", "ordinary.mp4")
	seedRetiredAVSeriesTag(t, cat, "FINAL")
	if _, err := cat.AddVideoTagAssignments(ctx, "invalid-av-series", []TagAssignment{{
		Label: "FINAL", Source: "auto", Evidence: "旧版本误生成",
	}}); err != nil {
		t.Fatalf("seed invalid AV series assignment: %v", err)
	}

	if err := cat.ReconcileVideoTags(ctx); err != nil {
		t.Fatalf("post-startup maintenance: %v", err)
	}
	if _, err := cat.getTagByLabel(ctx, "FINAL"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("invalid AV series tag retained: %v", err)
	}
	video, err := cat.GetVideo(ctx, "invalid-av-series")
	if err != nil {
		t.Fatalf("get invalid AV series video: %v", err)
	}
	if len(video.Tags) != 0 {
		t.Fatalf("invalid AV series video tags = %#v, want none", video.Tags)
	}
}

func TestUpdateTagAndReconcileOnlyChangesEditedTag(t *testing.T) {
	cat, ctx := openTagMaintenanceTestCatalog(t)
	seedTagMaintenanceVideo(t, cat, "rule-target", "special phrase", "target.mp4")

	target, err := cat.EnsureTag(ctx, "display-label", "user")
	if err != nil {
		t.Fatalf("ensure target tag: %v", err)
	}
	if _, err := cat.EnsureTag(ctx, "other-label", "user"); err != nil {
		t.Fatalf("ensure unrelated tag: %v", err)
	}
	if _, err := cat.AddVideoTagAssignments(ctx, "rule-target", []TagAssignment{{
		Label: "other-label", Source: "auto", Evidence: "existing",
	}}); err != nil {
		t.Fatalf("seed unrelated assignment: %v", err)
	}
	orphan, err := cat.ensureTagDefinition(ctx, "unrelated-generated", tagging.Rule{}, "generated")
	if err != nil {
		t.Fatalf("ensure unrelated generated tag: %v", err)
	}

	updated, changed, err := cat.UpdateTagAndReconcile(ctx, target.ID, tagging.Rule{
		Keywords: []string{"special phrase"},
	})
	if err != nil {
		t.Fatalf("update and reconcile target tag: %v", err)
	}
	if changed != 1 || !sameStrings(updated.MatchRules.Keywords, []string{"special phrase"}) {
		t.Fatalf("updated tag = %#v, changed = %d", updated, changed)
	}
	video, err := cat.GetVideo(ctx, "rule-target")
	if err != nil {
		t.Fatalf("get matched video: %v", err)
	}
	if !sameStrings(video.Tags, []string{"display-label", "other-label"}) {
		t.Fatalf("matched video tags = %#v", video.Tags)
	}
	if _, err := cat.getTagByID(ctx, orphan.ID); err != nil {
		t.Fatalf("targeted reconcile pruned unrelated tag: %v", err)
	}

	_, changed, err = cat.UpdateTagAndReconcile(ctx, target.ID, tagging.Rule{
		Keywords: []string{"other phrase"},
	})
	if err != nil {
		t.Fatalf("remove stale target match: %v", err)
	}
	if changed != 1 {
		t.Fatalf("removed assignment count = %d, want 1", changed)
	}
	video, err = cat.GetVideo(ctx, "rule-target")
	if err != nil {
		t.Fatalf("get unmatched video: %v", err)
	}
	if !sameStrings(video.Tags, []string{"other-label"}) {
		t.Fatalf("targeted reconcile changed unrelated assignments: %#v", video.Tags)
	}
}

func TestUpdateTagSavesMatchRulesAndClassifiesExistingVideos(t *testing.T) {
	cat, ctx := openTagMaintenanceTestCatalog(t)
	seedTagMaintenanceVideo(t, cat, "rule-video", "special phrase", "rule.mp4")
	userTag, err := cat.EnsureTag(ctx, "display-label", "user")
	if err != nil {
		t.Fatalf("ensure user tag: %v", err)
	}
	updated, err := cat.UpdateTag(ctx, userTag.ID, tagging.Rule{Keywords: []string{"special phrase"}})
	if err != nil {
		t.Fatalf("update tag: %v", err)
	}
	if len(updated.MatchRules.Keywords) != 1 {
		t.Fatalf("updated tag = %#v", updated)
	}
	classified, err := cat.ClassifyTagByID(ctx, userTag.ID)
	if err != nil || classified != 1 {
		t.Fatalf("classify updated tag = %d, %v", classified, err)
	}
	video, _ := cat.GetVideo(ctx, "rule-video")
	if !sameStrings(video.Tags, []string{"display-label"}) {
		t.Fatalf("classified tags = %#v, want display-label", video.Tags)
	}
}

func hasTag(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

func assignmentLabels(assignments []TagAssignment) []string {
	labels := make([]string, 0, len(assignments))
	for _, assignment := range assignments {
		labels = append(labels, assignment.Label)
	}
	return labels
}
