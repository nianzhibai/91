package config

import (
	"os"
	"testing"
)

func TestFrontendRepositoryUsesLiveYAMLConfiguration(t *testing.T) {
	manager, path := newManagerForTest(t, "{}\n")
	if got := manager.FrontendRepository(); got != DefaultFrontendRepository {
		t.Fatal(got)
	}
	_, version, _ := manager.ReadYAML()
	result, err := manager.ReplaceYAML([]byte("frontend:\n  repository: https://github.com/community/web\n"), version)
	if err != nil || result.RestartRequired || manager.FrontendRepository() != "https://github.com/community/web" {
		t.Fatalf("save repository: %+v %v", result, err)
	}
	if _, err := manager.SyncTemplate(); err != nil {
		t.Fatal(err)
	}
	if manager.FrontendRepository() != "https://github.com/community/web" {
		t.Fatal("template lost repository")
	}
	if err := os.WriteFile(path, []byte("frontend: {repository: 'https://github.com/other/web.git'}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if changed, err := manager.Reload(); !changed || err != nil {
		t.Fatalf("reload: %v %v", changed, err)
	}
	if manager.FrontendRepository() != "https://github.com/other/web.git" {
		t.Fatal("source edit did not apply")
	}
	if err := os.WriteFile(path, []byte("frontend: {repository: owner/web}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Reload(); err == nil {
		t.Fatal("accepted incomplete repository URL")
	}
	if manager.FrontendRepository() != "https://github.com/other/web.git" {
		t.Fatal("invalid edit replaced last valid setting")
	}
}
