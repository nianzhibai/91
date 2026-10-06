package catalog

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
)

func TestNewCatalogStartsWithoutTags(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	for attempt := 0; attempt < 2; attempt++ {
		cat, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := cat.ReconcileVideoTags(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := cat.MatchTagAssignments(ctx, "SSNI-001", "ABP-123.mp4", "", ""); err != nil {
			t.Fatal(err)
		}
		tags, err := cat.ListTags(ctx)
		if err != nil || len(tags) != 0 {
			t.Fatalf("fresh catalog unexpectedly created tags: %#v, %v", tags, err)
		}
		if err := cat.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuiltinTagsMigrateToUserTagsWithoutChangingRulesOrAssignments(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO tags (id,label,match_rules,source,origin,created_at,updated_at) VALUES
 (21,'我的关键词','{"keywords":["custom phrase"]}','builtin','',1,2),
 (22,'臀','{"keywords":["custom-only"]}',' SYSTEM ','',3,4),
 (23,'AV','{"matchAvCode":true,"avCodePrefixes":["SSNI","FHD"]}','builtin','',5,6),
 (24,'已有自定义','{"keywords":["existing"]}','user','',7,8),
 (25,'爬虫来源','{}','generated','crawler',9,10);
INSERT INTO videos (id,drive_id,file_id,title,tags,tags_manual,published_at,created_at,updated_at)
 VALUES ('legacy','drive','file','custom phrase SSNI-001','["我的关键词","臀","AV","爬虫来源"]',1,1,1,1);
INSERT INTO video_tags (video_id,tag_id,source,evidence,created_at) VALUES
 ('legacy',21,'auto','标题:custom phrase',11),
 ('legacy',22,'manual','人工分类',12),
 ('legacy',23,'auto','标题:SSNI-001',13),
 ('legacy',25,'crawler','爬虫:爬虫来源',14);
INSERT INTO settings (key,value,updated_at) VALUES
 ('tags.builtin_pack_enabled','false',1);`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var previousMetadata map[string]map[string]VideoTagMetadata
	for attempt := 0; attempt < 2; attempt++ {
		cat, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		for label, wantID := range map[string]int64{"我的关键词": 21, "臀": 22, "AV": 23, "已有自定义": 24} {
			tag, err := cat.getTagByLabel(ctx, label)
			if err != nil || tag.ID != wantID || tag.Source != "user" {
				t.Fatalf("migrated tag %s = %#v, %v", label, tag, err)
			}
		}
		for id, want := range map[int64]string{21: `{"keywords":["custom phrase"]}`, 22: `{"keywords":["custom-only"]}`, 23: `{"matchAvCode":true,"avCodePrefixes":["SSNI","FHD"]}`} {
			var raw string
			if err := cat.db.QueryRowContext(ctx, `SELECT match_rules FROM tags WHERE id=?`, id).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if raw != want {
				t.Fatalf("tag %d rule changed: %s", id, raw)
			}
		}
		video, err := cat.GetVideo(ctx, "legacy")
		if err != nil || !sameStrings(video.Tags, []string{"我的关键词", "臀", "AV", "爬虫来源"}) || !cat.hasManualTags(ctx, "legacy") {
			t.Fatalf("legacy video was changed: %#v, %v", video, err)
		}
		metadata, err := cat.ListVideoTagMetadata(ctx, []string{"legacy"})
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			previousMetadata = metadata
		} else if !reflect.DeepEqual(previousMetadata, metadata) {
			t.Fatal("reopen changed assignment sources or evidence")
		}
		selectable, err := cat.ListUserSelectableTags(ctx)
		if err != nil || len(selectable) != 4 {
			t.Fatalf("migrated custom tag choices = %#v, %v", selectable, err)
		}
		for _, key := range []string{"tags.builtin_pack_enabled", "tags.builtin_pack_initialized_v1"} {
			if value, err := cat.GetSetting(ctx, key, "missing"); err != nil || value != "missing" {
				t.Fatalf("retired setting remains: %s=%s, %v", key, value, err)
			}
		}
		if got, err := cat.MatchTags(ctx, "custom phrase SSNI-001"); err != nil || !sameStrings(got, []string{"我的关键词", "AV"}) {
			t.Fatalf("migrated rules no longer match: %#v, %v", got, err)
		}
		if err := cat.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuiltinTagMigrationRollsBackOnFailure(t *testing.T) {
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	ctx := context.Background()
	if _, err := cat.db.Exec(`
INSERT INTO tags (label,match_rules,source,created_at,updated_at) VALUES
 ('first','{"keywords":["first"]}','builtin',1,1),('second','{}','builtin',1,1);
INSERT INTO settings (key,value,updated_at) VALUES ('tags.builtin_pack_enabled','true',1);
CREATE TRIGGER reject_tag_conversion BEFORE UPDATE OF source ON tags
WHEN OLD.label='second' BEGIN SELECT RAISE(ABORT,'injected migration failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := cat.normalizeStoredTagSources(ctx); err == nil {
		t.Fatal("expected migration failure")
	}
	var count int
	if err := cat.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tags WHERE source='builtin'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("migration was partially applied: count=%d err=%v", count, err)
	}
	if value, err := cat.GetSetting(ctx, "tags.builtin_pack_enabled", ""); err != nil || value != "true" {
		t.Fatalf("failed migration removed legacy settings: %q, %v", value, err)
	}
}
