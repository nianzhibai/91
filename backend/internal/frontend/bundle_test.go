package frontend

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	officialRepository  = "https://github.com/owner/web"
	nostalgicRepository = "owner/nostalgic"
)

// testBundle packages two frontends the way release builds do.
func testBundle(t *testing.T) string {
	t.Helper()
	packager, err := New(t.TempDir(), "", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	official, err := packager.Install(bytes.NewReader(testNamedArchive(t, "web", "1.0.0", "1.0.0", "official", nil)), InstallOptions{Repository: officialRepository})
	if err != nil {
		t.Fatal(err)
	}
	nostalgic, err := packager.Install(bytes.NewReader(testNamedArchive(t, "nostalgic", "1.0.0", "1.0.0", "nostalgic", nil)), InstallOptions{Repository: nostalgicRepository})
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "dist")
	if err := packager.WriteBundle(bundle, official.ID(), []string{official.ID(), nostalgic.ID()}); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestBundledFrontendsAreRetainedWithTheirSources(t *testing.T) {
	bundle := testBundle(t)
	site, err := BuiltinSite(bundle)
	if err != nil || site != filepath.Join(bundle, "releases", "web@1.0.0", "site") {
		t.Fatalf("default site = %q, %v", site, err)
	}
	m, err := New(t.TempDir(), bundle, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RetainBuiltin(); err != nil {
		t.Fatal(err)
	}
	// Retention is repeated on every start and must not fail once stored.
	if err := m.RetainBuiltin(); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.Active != "" || status.Builtin == nil || status.Builtin.Name != "web" {
		t.Fatalf("default frontend: %+v", status)
	}
	sources := map[string]string{}
	for _, release := range status.Installed {
		sources[release.ID()] = release.Repository
	}
	if len(sources) != 2 || sources["web@1.0.0"] != officialRepository || sources["nostalgic@1.0.0"] != nostalgicRepository {
		t.Fatalf("retained releases: %v", sources)
	}
	// Assets of every bundled frontend outlive the application directory.
	if err := os.RemoveAll(bundle); err != nil {
		t.Fatal(err)
	}
	directory, err := m.AssetDirectory("nostalgic", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(directory, "assets/app.js")); string(data) != "nostalgic" {
		t.Fatal("bundled asset missing after upgrade")
	}
}

func TestFrontendsListBundledAndInstalledFrontends(t *testing.T) {
	m, err := New(t.TempDir(), testBundle(t), "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RetainBuiltin(); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"1.1.0", "1.3.0", "1.2.0"} {
		if _, err := m.Install(bytes.NewReader(testArchive(t, version, "1.0.0", version, nil)), InstallOptions{Repository: "owner/community"}); err != nil {
			t.Fatal(err)
		}
	}
	// An incompatible newer release is skipped instead of being offered.
	if _, err := m.Install(bytes.NewReader(testArchive(t, "2.0.0", "1.0.0", "future", nil)), InstallOptions{Repository: "owner/community"}); err != nil {
		t.Fatal(err)
	}
	future := Manifest{FormatVersion: 1, Name: "test-web", Version: "2.0.0", APIMin: "1.5.0", APIMax: "2.0.0"}
	if err := writeJSONFile(filepath.Join(m.root, "releases", future.ID(), "site", "frontend.json"), future); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(bytes.NewReader(testNamedArchive(t, "local", "0.1.0", "1.0.0", "local", nil)), InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	want := []Frontend{
		{ID: "nostalgic@1.0.0", Name: "nostalgic", Version: "1.0.0", Repository: nostalgicRepository, Builtin: true},
		// The default site follows application upgrades instead of a pinned version.
		{ID: "", Name: "web", Version: "1.0.0", Repository: officialRepository, Builtin: true, Active: true},
		{ID: "local@0.1.0", Name: "local", Version: "0.1.0"},
		{ID: "test-web@1.3.0", Name: "test-web", Version: "1.3.0", Repository: "owner/community"},
	}
	if frontends, err := m.Frontends(); err != nil || !reflect.DeepEqual(frontends, want) {
		t.Fatalf("frontends:\n got %+v\nwant %+v (%v)", frontends, want, err)
	}
	// An older active version is listed as it is served.
	if err := m.Activate("test-web@1.1.0"); err != nil {
		t.Fatal(err)
	}
	want[1].Active = false
	want[3].ID, want[3].Version, want[3].Active = "test-web@1.1.0", "1.1.0", true
	if frontends, err := m.Frontends(); err != nil || !reflect.DeepEqual(frontends, want) {
		t.Fatalf("pinned frontends:\n got %+v\nwant %+v (%v)", frontends, want, err)
	}
}

func TestDefaultSiteWithoutManifestIsListed(t *testing.T) {
	site := t.TempDir()
	if err := os.WriteFile(filepath.Join(site, "index.html"), []byte("custom"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(t.TempDir(), site, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	frontends, err := m.Frontends()
	if err != nil || !reflect.DeepEqual(frontends, []Frontend{{Builtin: true, Active: true}}) {
		t.Fatalf("frontends: %+v %v", frontends, err)
	}
}

func TestInstallReusesInstalledReleaseOffline(t *testing.T) {
	m := testManager(t)
	for _, version := range []string{"1.1.0", "1.3.0"} {
		if _, err := m.Install(bytes.NewReader(testArchive(t, version, "1.0.0", version, nil)), InstallOptions{Repository: "owner/web"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tag := range []string{"v1.3.0", "v1.2.0"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.EqualFold(r.URL.Path, "/repos/owner/web/releases/latest") {
				t.Errorf("unexpected download: %s", r.URL)
			}
			json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": []releaseAsset{{Name: ArchiveName, URL: "https://github.com/archive"}, {Name: ArchiveName + ".sha256", URL: "https://github.com/checksum"}}})
		}))
		source := &ReleaseSource{Client: server.Client(), apiURL: server.URL}
		release, err := source.Install(context.Background(), m, ReleaseRequest{Repository: "https://github.com/Owner/Web.git"})
		server.Close()
		if err != nil || release.Version != "1.3.0" {
			t.Fatalf("latest %s installed %+v %v", tag, release, err)
		}
	}
}

func TestCorruptedBundleIsRejected(t *testing.T) {
	bundle := testBundle(t)
	if err := os.WriteFile(filepath.Join(bundle, "releases", "nostalgic@1.0.0", "site", "assets", "app.js"), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(t.TempDir(), bundle, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RetainBuiltin(); err == nil {
		t.Fatal("modified bundled frontend was retained")
	}
	status, _ := m.Status()
	for _, release := range status.Installed {
		if release.Name == "nostalgic" {
			t.Fatal("modified bundled frontend became installable")
		}
	}
}

func TestInvalidBundleIndexFailsExplicitly(t *testing.T) {
	bundle := testBundle(t)
	if err := os.WriteFile(filepath.Join(bundle, BundleFile), []byte(`{"formatVersion":1,"default":"../web"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(t.TempDir(), bundle, "1.0.0"); err == nil {
		t.Fatal("invalid default accepted")
	}
	if _, err := BuiltinSite(bundle); err == nil {
		t.Fatal("invalid default resolved")
	}
}
