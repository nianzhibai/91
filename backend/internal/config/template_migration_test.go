package config

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nianzhibai/91/backend"
	"gopkg.in/yaml.v3"
)

func TestTemplateMigrationPreservesValuesAndReplacesDocumentStructure(t *testing.T) {
	manager, path := newManagerForTest(t, `# outdated operator comment
telegram:
  enabled: true
  bot_token: "123:private_token"
  api_base_url: "http://old-bot-api:9999"
  api_id: 1234
  api_hash: "0123456789abcdef0123456789abcdef"
  api_files_root: /old/bot-api
  local_files_root: /old/site
  allowed_user_ids: [42, 43]
  upload_directory: 2026-09-16
  max_pending_jobs: 0
generation:
  preview_concurrency: 4
  future_option: remove
server:
  listen: "0.0.0.0:9999" # outdated port comment
  allowed_origins: []
nightly:
  start_time: "04:20"
logging:
  file_enabled: false
preview:
  enabled: false
  ffmpeg_path: ""
  duration_seconds: 10
  segments: 8
scanner:
  interval_seconds: 60
  max_depth: 2
future_section:
  remove_me: true
`)
	original, _, err := manager.ReadYAML()
	if err != nil {
		t.Fatal(err)
	}
	before, err := Parse(original)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := manager.SyncTemplate()
	if err != nil || !changed {
		t.Fatalf("migration changed=%v err=%v", changed, err)
	}
	written, version, err := manager.ReadYAML()
	if err != nil {
		t.Fatal(err)
	}
	after, err := Parse(written)
	if err != nil {
		t.Fatal(err)
	}
	// These fields are absent from the template and no longer affect behavior.
	before.Scanner.IntervalSeconds = after.Scanner.IntervalSeconds
	before.Scanner.MaxDepth = after.Scanner.MaxDepth
	before.Preview.Segments = after.Preview.Segments
	before.Proxy = after.Proxy // Omitted relay policy becomes explicitly enabled.
	if !reflect.DeepEqual(before, after) {
		t.Fatal("template migration changed an existing effective setting")
	}
	var document map[string]map[string]any
	if err := yaml.Unmarshal(written, &document); err != nil {
		t.Fatal(err)
	}
	for _, field := range []struct {
		section, key string
		want         any
	}{
		{"generation", "thumbnail_concurrency", 1},
		{"generation", "fingerprint_concurrency", 1},
		{"preview", "ffmpeg_threads", 1},
		{"telegram", "upload_proxy", ""},
		{"telegram", "max_pending_jobs", 0},
		{"preview", "ffmpeg_path", ""},
		{"nightly", "disabled", false},
	} {
		got, exists := document[field.section][field.key]
		if !exists || !reflect.DeepEqual(got, field.want) {
			t.Errorf("%s.%s = %#v (exists=%v), want %#v", field.section, field.key, got, exists, field.want)
		}
	}
	if after.Telegram.UploadDirectory != "2026-09-16" {
		t.Fatal("migration changed a string that resembles a date")
	}
	for _, removed := range []string{"outdated", "interval_seconds:", "max_depth:", "duration_seconds:", "segments:", "future_option:", "future_section:", "api_base_url:", "api_files_root:", "local_files_root:"} {
		if strings.Contains(string(written), removed) {
			t.Errorf("retained %s", removed)
		}
	}
	assertTemplateStructure(t, written)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("file permissions changed: info=%v err=%v", info, err)
	}
	if manager.observedVersion != version || !reflect.DeepEqual(manager.current, after) {
		t.Fatal("manager snapshot did not follow the migrated file")
	}
	if changed, err = manager.SyncTemplate(); err != nil || changed {
		t.Fatalf("second migration changed=%v err=%v", changed, err)
	}
	again, _, _ := manager.ReadYAML()
	if !bytes.Equal(written, again) {
		t.Fatal("second migration rewrote the file")
	}
}

func assertTemplateStructure(t *testing.T, data []byte) {
	t.Helper()
	var actual, template yaml.Node
	if err := yaml.Unmarshal(data, &actual); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(backend.ConfigTemplate(), &template); err != nil {
		t.Fatal(err)
	}
	var compare func(*yaml.Node, *yaml.Node)
	compare = func(got, want *yaml.Node) {
		t.Helper()
		if got.HeadComment != want.HeadComment || got.LineComment != want.LineComment || got.FootComment != want.FootComment {
			t.Errorf("comments for %q do not match template", want.Value)
		}
		if want.Kind != yaml.MappingNode && want.Kind != yaml.DocumentNode {
			return
		}
		if got.Kind != want.Kind || len(got.Content) != len(want.Content) {
			t.Fatalf("document structure differs from template at %q", want.Value)
		}
		for i, child := range want.Content {
			if want.Kind == yaml.MappingNode && i%2 == 0 && got.Content[i].Value != child.Value {
				t.Fatalf("field order = %q, want %q", got.Content[i].Value, child.Value)
			}
			compare(got.Content[i], child)
		}
	}
	compare(&actual, &template)
}

func TestTemplateMigrationUsesBundledDefaultsWithoutExternalExample(t *testing.T) {
	want, err := Parse(backend.ConfigTemplate())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, source string }{
		{"empty", ""},
		{"empty_mapping", "{}\n"},
		{"null", "null\n"},
		{"comment_only", "# empty config\n"},
		{"null_section", "server: null\n"},
		{"complete", string(backend.ConfigTemplate())},
	} {
		t.Run(test.name, func(t *testing.T) {
			// The deployment contains only config.yaml, no external template.
			manager, _ := newManagerForTest(t, test.source)
			if _, err := manager.SyncTemplate(); err != nil {
				t.Fatal(err)
			}
			data, _, _ := manager.ReadYAML()
			got, err := Parse(data)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("empty config did not receive template defaults: %v", err)
			}
			assertTemplateStructure(t, data)
			if changed, err := manager.SyncTemplate(); err != nil || changed {
				t.Fatalf("second migration changed=%v err=%v", changed, err)
			}
		})
	}
}

func TestTemplateMigrationResolvesAliasesBeforeRemovingOldFields(t *testing.T) {
	manager, _ := newManagerForTest(t, `defaults: &defaults
  generation:
    preview_concurrency: 3
    unused: remove
  server:
    listen: ":9876"
<<: *defaults
preview_defaults: &preview
  enabled: false
preview: *preview
owner: &owner 42
telegram:
  allowed_user_ids: [*owner]
  upload_proxy: "socks5h://proxy.example:1080"
`)
	if _, err := manager.SyncTemplate(); err != nil {
		t.Fatal(err)
	}
	data, _, _ := manager.ReadYAML()
	assertTemplateStructure(t, data)
	cfg, err := Parse(data)
	if err != nil || cfg.Preview.Enabled || cfg.Generation.PreviewConcurrency != 3 || cfg.Server.Listen != ":9876" ||
		cfg.Telegram.UploadProxy != "socks5h://proxy.example:1080" || len(cfg.Telegram.AllowedUserIDs) != 1 || cfg.Telegram.AllowedUserIDs[0] != 42 {
		t.Fatalf("migration lost inherited settings: %v", err)
	}
}

func TestTemplateMigrationRejectsInvalidConfigWithoutChangingFileOrSnapshot(t *testing.T) {
	for _, source := range []string{
		"generation: {preview_concurrency: 99}\n",
		"telegram: {enabled: true}\n",
		"server: {admin: {username: owner, password: secret}}\n",
		"preview: [\n",
	} {
		t.Run(source, func(t *testing.T) {
			manager, path := newManagerForTest(t, "{}\n")
			before, version := manager.current, manager.observedVersion
			if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
				t.Fatal(err)
			}
			if changed, err := manager.SyncTemplate(); err == nil || changed {
				t.Fatalf("invalid migration changed=%v err=%v", changed, err)
			}
			data, _ := os.ReadFile(path)
			if string(data) != source || manager.current != before || manager.observedVersion != version {
				t.Fatal("failed migration changed config")
			}
		})
	}
}

func TestTemplateMigrationRollsBackWhenLiveApplyFails(t *testing.T) {
	manager, path := newManagerForTest(t, "{}\n")
	if err := manager.SetApply(func(settings LiveSettings) error {
		if !settings.PreviewEnabled {
			return errors.New("cannot disable preview")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	original := []byte("preview: {enabled: false}\n")
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}
	if changed, err := manager.SyncTemplate(); err == nil || changed {
		t.Fatalf("failed apply changed=%v err=%v", changed, err)
	}
	data, _ := os.ReadFile(path)
	if !bytes.Equal(data, original) || !manager.LiveSettings().PreviewEnabled {
		t.Fatal("failed live apply did not restore the file and snapshot")
	}
}
