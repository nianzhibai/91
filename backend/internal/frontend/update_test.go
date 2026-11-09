package frontend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCheckUpdateReadsOnlyMetadataAndComparesVersions(t *testing.T) {
	for _, tc := range []struct {
		tag       string
		available bool
	}{
		{"v1.10.0", true}, {"v1.2.0", false}, {"v1.1.0", false},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			manager := testManager(t)
			release, err := manager.Install(bytes.NewReader(testArchive(t, "1.2.0", "1.0.0", "installed", nil)), InstallOptions{Repository: "Owner/Web"})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Activate(release.ID()); err != nil {
				t.Fatal(err)
			}
			before, _ := manager.Status()
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads++
				if r.URL.Path != "/repos/owner/web/releases/latest" {
					t.Errorf("unexpected download: %s", r.URL.Path)
				}
				json.NewEncoder(w).Encode(map[string]any{"tag_name": tc.tag, "assets": []releaseAsset{{Name: ArchiveName, URL: "https://github.com/archive"}, {Name: ArchiveName + ".sha256", URL: "https://github.com/checksum"}}})
			}))
			defer server.Close()
			source := &ReleaseSource{Client: server.Client(), apiURL: server.URL}
			result, err := source.CheckUpdate(context.Background(), manager, "https://github.com/owner/web.git/")
			if err != nil || result.UpdateAvailable != tc.available || result.Current.Version != "1.2.0" {
				t.Fatalf("check: %+v %v", result, err)
			}
			after, _ := manager.Status()
			if reads != 1 || !reflect.DeepEqual(before, after) {
				t.Fatalf("check changed state: reads=%d before=%+v after=%+v", reads, before, after)
			}
		})
	}
}

func TestCheckUpdateRejectsChangedRepositoryAndFailedLookup(t *testing.T) {
	manager := testManager(t)
	release, err := manager.Install(bytes.NewReader(testArchive(t, "1.2.0", "1.0.0", "installed", nil)), InstallOptions{Repository: "owner/web"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Activate(release.ID()); err != nil {
		t.Fatal(err)
	}
	changeSelection := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if changeSelection {
			if err := manager.Activate(""); err != nil {
				t.Error(err)
			}
			json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.3.0", "assets": []releaseAsset{{Name: ArchiveName, URL: "https://github.com/archive"}, {Name: ArchiveName + ".sha256", URL: "https://github.com/checksum"}}})
		} else {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
		}
	}))
	defer server.Close()
	source := &ReleaseSource{Client: server.Client(), apiURL: server.URL}
	if _, err := source.CheckUpdate(context.Background(), manager, "https://github.com/other/web"); !errors.Is(err, ErrRepositoryNotActive) {
		t.Fatal(err)
	}
	if _, err := source.CheckUpdate(context.Background(), manager, "https://github.com/owner/web"); err == nil {
		t.Fatal("failed lookup reported success")
	}
	changeSelection = true
	if _, err := source.CheckUpdate(context.Background(), manager, "https://github.com/owner/web"); !errors.Is(err, ErrRepositoryNotActive) {
		t.Fatalf("stale lookup: %v", err)
	}
}

func TestCurrentFrontendTracksBuiltinAndInstalledSource(t *testing.T) {
	builtin := t.TempDir()
	manifest := Manifest{FormatVersion: 1, Name: "91-web", Version: "1.0.0", APIMin: "1.0.0", APIMax: "2.0.0"}
	metadata, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(builtin, "frontend.json"), metadata, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(builtin, "index.html"), []byte(manifest.BaseURL()), 0644); err != nil {
		t.Fatal(err)
	}
	manager, err := New(t.TempDir(), builtin, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	status, err := manager.Status()
	if err != nil || status.Current == nil || status.Current.Repository != "https://github.com/nianzhibai/91-web" {
		t.Fatalf("builtin: %+v %v", status, err)
	}
	archive := testArchive(t, "1.2.0", "1.0.0", "local", nil)
	local, err := manager.Install(bytes.NewReader(archive), InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Activate(local.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install(bytes.NewReader(archive), InstallOptions{Repository: "https://github.com/community/web"}); err != nil {
		t.Fatal(err)
	}
	status, err = manager.Status()
	if err != nil || status.Current.Repository != "https://github.com/community/web" {
		t.Fatalf("installed source: %+v %v", status, err)
	}
	if _, err := manager.Install(bytes.NewReader(archive), InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	status, _ = manager.Status()
	if status.Current.Repository != "https://github.com/community/web" {
		t.Fatal("lost repository")
	}
}
