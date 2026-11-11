package frontend

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testArchive(t *testing.T, version, apiMin, script string, extra *tar.Header) []byte {
	t.Helper()
	return testNamedArchive(t, "test-web", version, apiMin, script, extra)
}

func testNamedArchive(t *testing.T, name, version, apiMin, script string, extra *tar.Header) []byte {
	t.Helper()
	manifest := Manifest{FormatVersion: 1, Name: name, Version: version, APIMin: apiMin, APIMax: "2.0.0"}
	metadata, _ := json.Marshal(manifest)
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	for _, file := range []struct{ name, body string }{{"frontend.json", string(metadata)}, {"index.html", `<script src="` + manifest.BaseURL() + `assets/app.js"></script>`}, {"assets/app.js", script}} {
		if err := tw.WriteHeader(&tar.Header{Name: file.name, Mode: 0644, Size: int64(len(file.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(file.body)); err != nil {
			t.Fatal(err)
		}
	}
	if extra != nil {
		if err := tw.WriteHeader(extra); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func testManager(t *testing.T) *Manager {
	t.Helper()
	builtin := t.TempDir()
	if err := unpack(bytes.NewReader(testArchive(t, "1.0.0", "1.0.0", "builtin", nil)), builtin); err != nil {
		t.Fatal(err)
	}
	m, err := New(t.TempDir(), builtin, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RetainBuiltin(); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestInstallActivateRollbackAndRetainedAssets(t *testing.T) {
	m := testManager(t)
	first, err := m.Install(bytes.NewReader(testArchive(t, "1.1.0", "1.0.0", "first", nil)), InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if directory, _ := m.ActiveDirectory(); directory != m.builtin {
		t.Fatal("installation changed the active site")
	}
	if err := m.Activate(first.ID()); err != nil {
		t.Fatal(err)
	}
	second, err := m.Install(bytes.NewReader(testArchive(t, "1.2.0", "1.0.0", "second", nil)), InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(second.ID()); err != nil {
		t.Fatal(err)
	}
	old, err := m.AssetDirectory(first.Name, first.Version)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(old, "assets/app.js")); string(data) != "first" {
		t.Fatal("old chunks disappeared")
	}
	if err := m.Rollback(); err != nil {
		t.Fatal(err)
	}
	status, _ := m.Status()
	if status.Active != first.ID() {
		t.Fatalf("rollback selected %s", status.Active)
	}
	if err := m.Activate(""); err != nil {
		t.Fatal(err)
	}
	if directory, _ := m.ActiveDirectory(); directory != m.builtin {
		t.Fatal("reset did not select builtin")
	}
	// CLI and server instances observe the same durable selection without restart.
	other, err := New(filepath.Dir(m.root), m.builtin, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Activate(second.ID()); err != nil {
		t.Fatal(err)
	}
	status, _ = m.Status()
	if status.Active != second.ID() {
		t.Fatal("selection did not propagate")
	}
}

func TestRejectedPackagesLeaveCurrentVersionUntouched(t *testing.T) {
	m := testManager(t)
	valid := testArchive(t, "1.1.0", "1.0.0", "original", nil)
	installed, err := m.Install(bytes.NewReader(valid), InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(installed.ID()); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		data []byte
		hash string
	}{
		{"checksum", valid, strings.Repeat("0", 64)},
		{"incompatible", testArchive(t, "1.2.0", "1.5.0", "incompatible", nil), ""},
		{"replace-version", testArchive(t, "1.1.0", "1.0.0", "changed", nil), ""},
		{"traversal", testArchive(t, "1.2.0", "1.0.0", "", &tar.Header{Name: "../escape", Mode: 0644, Typeflag: tar.TypeReg}), ""},
		{"symlink", testArchive(t, "1.2.0", "1.0.0", "", &tar.Header{Name: "escape", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink}), ""},
		{"duplicate", testArchive(t, "1.2.0", "1.0.0", "", &tar.Header{Name: "index.html", Mode: 0644, Typeflag: tar.TypeReg}), ""},
		{"truncated", valid[:len(valid)-5], ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := m.Install(bytes.NewReader(test.data), InstallOptions{SHA256: test.hash}); err == nil {
				t.Fatal("invalid package accepted")
			}
			status, err := m.Status()
			if err != nil || status.Active != installed.ID() {
				t.Fatalf("active frontend changed: %+v %v", status, err)
			}
		})
	}
}

func TestBundledFilesSurviveApplicationUpgrade(t *testing.T) {
	m := testManager(t)
	if err := os.RemoveAll(m.builtin); err != nil {
		t.Fatal(err)
	}
	directory, err := m.AssetDirectory("test-web", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "assets/app.js"))
	if err != nil || string(data) != "builtin" {
		t.Fatalf("old bundled asset missing: %v", err)
	}
}

func TestFixedReleaseDownloadChecksVersionAndDigest(t *testing.T) {
	m := testManager(t)
	archive := testArchive(t, "1.3.0", "1.0.0", "remote", nil)
	digest := sha256.Sum256(archive)
	var base string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/web/releases/tags/v1.3.0":
			json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.3.0", "assets": []releaseAsset{{Name: ArchiveName, URL: base + "/archive"}, {Name: ArchiveName + ".sha256", URL: base + "/checksum"}}})
		case "/archive":
			w.Write(archive)
		case "/checksum":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(digest[:]), ArchiveName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base = server.URL
	source := &ReleaseSource{Client: server.Client(), apiURL: server.URL}
	release, err := source.Install(context.Background(), m, ReleaseRequest{Repository: "owner/web", Version: "v1.3.0"})
	if err != nil {
		t.Fatal(err)
	}
	if release.Repository != "owner/web" || release.Version != "1.3.0" {
		t.Fatalf("unexpected release: %+v", release)
	}
	for _, version := range []string{"latest", "main", "1.3.0", "v../evil"} {
		if _, err := source.Install(context.Background(), m, ReleaseRequest{Repository: "owner/web", Version: version}); err == nil {
			t.Fatalf("accepted %s", version)
		}
	}
}

func TestInstallChecksExpectedIdentityBeforePublishing(t *testing.T) {
	m := testManager(t)
	before, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	archive := testArchive(t, "1.3.0", "1.0.0", "candidate", nil)
	for _, options := range []InstallOptions{{Name: "other-web"}, {Version: "1.2.0"}} {
		if _, err := m.Install(bytes.NewReader(archive), options); err == nil {
			t.Fatalf("accepted mismatched identity: %+v", options)
		}
		after, err := m.Status()
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("rejected archive changed status: %+v %v", after, err)
		}
		if err := m.Activate("test-web@1.3.0"); err == nil {
			t.Fatal("rejected archive can be activated")
		}
		if _, err := m.AssetDirectory("test-web", "1.3.0"); err == nil {
			t.Fatal("rejected archive exposes assets")
		}
	}
	if _, err := m.Install(bytes.NewReader(archive), InstallOptions{Name: "test-web", Version: "1.3.0"}); err != nil {
		t.Fatalf("corrected install failed: %v", err)
	}
}

func TestReleaseTagMismatchLeavesNoInstalledVersion(t *testing.T) {
	m := testManager(t)
	before, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	archive := testArchive(t, "1.4.0", "1.0.0", "wrong-tag", nil)
	digest := sha256.Sum256(archive)
	var base string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/web/releases/tags/v1.3.0":
			json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.3.0", "assets": []releaseAsset{{Name: ArchiveName, URL: base + "/archive"}, {Name: ArchiveName + ".sha256", URL: base + "/checksum"}}})
		case "/archive":
			w.Write(archive)
		case "/checksum":
			fmt.Fprintf(w, "%x  %s\n", digest, ArchiveName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base = server.URL
	source := &ReleaseSource{Client: server.Client(), apiURL: server.URL}
	if _, err := source.Install(context.Background(), m, ReleaseRequest{Repository: "owner/web", Version: "v1.3.0"}); err == nil {
		t.Fatal("accepted mismatched release tag")
	}
	after, err := m.Status()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected release changed status: %+v %v", after, err)
	}
	if err := m.Activate("test-web@1.4.0"); err == nil {
		t.Fatal("rejected release can be activated")
	}
}

func TestLatestReleaseResolvesTagAndVerifiesManifest(t *testing.T) {
	for _, scenario := range []struct {
		name, tag                  string
		draft, prerelease, success bool
	}{
		{name: "latest", tag: "v1.3.0", success: true},
		{name: "manifest mismatch", tag: "v1.4.0"},
		{name: "invalid tag", tag: "main"},
		{name: "draft", tag: "v1.3.0", draft: true},
		{name: "prerelease", tag: "v1.3.0", prerelease: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			m := testManager(t)
			before, err := m.Status()
			if err != nil {
				t.Fatal(err)
			}
			archive := testArchive(t, "1.3.0", "1.0.0", "latest", nil)
			digest := sha256.Sum256(archive)
			var base string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/owner/web/releases/latest":
					json.NewEncoder(w).Encode(map[string]any{"tag_name": scenario.tag, "draft": scenario.draft, "prerelease": scenario.prerelease, "assets": []releaseAsset{{Name: ArchiveName, URL: base + "/archive"}, {Name: ArchiveName + ".sha256", URL: base + "/checksum"}}})
				case "/archive":
					w.Write(archive)
				case "/checksum":
					fmt.Fprintf(w, "%x  %s\n", digest, ArchiveName)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			base = server.URL
			source := &ReleaseSource{Client: server.Client(), apiURL: server.URL}
			release, err := source.Install(context.Background(), m, ReleaseRequest{Repository: "https://github.com/owner/web.git"})
			if scenario.success {
				if err != nil || release.Version != "1.3.0" {
					t.Fatalf("latest: %+v %v", release, err)
				}
			} else {
				if err == nil {
					t.Fatal("accepted invalid latest release")
				}
				status, err := m.Status()
				if err != nil || !reflect.DeepEqual(before, status) {
					t.Fatalf("failed install published release: %+v %v", status, err)
				}
			}
		})
	}
}
