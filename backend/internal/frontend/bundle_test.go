package frontend

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// offlineSource fails the test when any release metadata or package is requested.
func offlineSource(t *testing.T) *ReleaseSource {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected network request: %s", r.URL)
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	return &ReleaseSource{Client: server.Client(), apiURL: server.URL}
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
	if status.Active != "" || status.Current == nil || status.Current.Name != "web" || status.Current.Repository != officialRepository {
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

func TestEnableSwitchesBetweenInstalledRepositoriesOffline(t *testing.T) {
	m, err := New(t.TempDir(), testBundle(t), "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RetainBuiltin(); err != nil {
		t.Fatal(err)
	}
	source := offlineSource(t)
	for _, step := range []struct{ repository, active string }{
		{"https://github.com/Owner/Nostalgic.git", "nostalgic@1.0.0"},
		{officialRepository, "web@1.0.0"},
	} {
		release, err := source.Enable(context.Background(), m, step.repository)
		if err != nil || release.ID() != step.active {
			t.Fatalf("enable %s: %+v %v", step.repository, release, err)
		}
		status, _ := m.Status()
		if status.Active != step.active {
			t.Fatalf("enable %s selected %q", step.repository, status.Active)
		}
	}
}

func TestEnablePrefersTheNewestInstalledRelease(t *testing.T) {
	m := testManager(t)
	for _, version := range []string{"1.1.0", "1.3.0", "1.2.0"} {
		if _, err := m.Install(bytes.NewReader(testArchive(t, version, "1.0.0", version, nil)), InstallOptions{Repository: "owner/web"}); err != nil {
			t.Fatal(err)
		}
	}
	// An incompatible newer release is skipped instead of failing the switch.
	if _, err := m.Install(bytes.NewReader(testArchive(t, "2.0.0", "1.0.0", "future", nil)), InstallOptions{Repository: "owner/web"}); err != nil {
		t.Fatal(err)
	}
	future := Manifest{FormatVersion: 1, Name: "test-web", Version: "2.0.0", APIMin: "1.5.0", APIMax: "2.0.0"}
	if err := writeJSONFile(filepath.Join(m.root, "releases", future.ID(), "site", "frontend.json"), future); err != nil {
		t.Fatal(err)
	}
	release, err := offlineSource(t).Enable(context.Background(), m, "https://github.com/owner/web")
	if err != nil || release.Version != "1.3.0" {
		t.Fatalf("enabled %+v %v", release, err)
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
