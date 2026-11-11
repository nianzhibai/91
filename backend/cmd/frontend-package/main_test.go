package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nianzhibai/91/backend/internal/frontend"
	"github.com/nianzhibai/91/backend/internal/httpapi"
)

func writeArchive(t *testing.T, directory, name, version string) string {
	t.Helper()
	manifest := frontend.Manifest{FormatVersion: 1, Name: name, Version: version, APIMin: "1.0.0", APIMax: "2.0.0"}
	metadata, _ := json.Marshal(manifest)
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	for _, file := range []struct{ name, body string }{{"frontend.json", string(metadata)}, {"index.html", `<script src="` + manifest.BaseURL() + `app.js"></script>`}, {"app.js", name}} {
		if err := tw.WriteHeader(&tar.Header{Name: file.name, Mode: 0644, Size: int64(len(file.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(file.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, name+".tar.gz"), buffer.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buffer.Bytes())
	return hex.EncodeToString(sum[:])
}

func writeLock(t *testing.T, lock lockFile) string {
	t.Helper()
	data, _ := json.Marshal(lock)
	path := filepath.Join(t.TempDir(), "frontend.lock.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPrepareBundlesEveryPinnedFrontend(t *testing.T) {
	archives := t.TempDir()
	lock := lockFile{Default: "web", Frontends: []pinnedFrontend{
		{Repository: "https://github.com/owner/web", Name: "web", Version: "v1.0.0", SHA256: writeArchive(t, archives, "web", "1.0.0")},
		{Repository: "https://github.com/owner/nostalgic", Name: "nostalgic", Version: "v1.2.0", SHA256: writeArchive(t, archives, "nostalgic", "1.2.0")},
	}}
	output := filepath.Join(t.TempDir(), "dist")
	if err := prepare(writeLock(t, lock), archives, output); err != nil {
		t.Fatal(err)
	}
	if site, err := frontend.BuiltinSite(output); err != nil || site != filepath.Join(output, "releases", "web@1.0.0", "site") {
		t.Fatalf("default site = %q, %v", site, err)
	}
	manager, err := frontend.New(t.TempDir(), output, httpapi.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RetainBuiltin(); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Status()
	if err != nil || len(status.Installed) != 2 || status.Current == nil || status.Current.Repository != "https://github.com/owner/web" {
		t.Fatalf("bundle: %+v %v", status, err)
	}
}

func TestPrepareRejectsUnpinnedContent(t *testing.T) {
	archives := t.TempDir()
	web := pinnedFrontend{Repository: "owner/web", Name: "web", Version: "v1.0.0", SHA256: writeArchive(t, archives, "web", "1.0.0")}
	other := pinnedFrontend{Repository: "owner/other", Name: "other", Version: "v1.0.0", SHA256: writeArchive(t, archives, "other", "1.0.0")}
	tampered := other
	tampered.SHA256 = web.SHA256
	missing := pinnedFrontend{Repository: "owner/missing", Name: "missing", Version: "v1.0.0", SHA256: web.SHA256}
	for name, lock := range map[string]lockFile{
		"digest mismatch":   {Default: "web", Frontends: []pinnedFrontend{web, tampered}},
		"missing archive":   {Default: "web", Frontends: []pinnedFrontend{web, missing}},
		"unpinned default":  {Default: "other", Frontends: []pinnedFrontend{web}},
		"duplicate entries": {Default: "web", Frontends: []pinnedFrontend{web, web}},
		"empty lock":        {Default: "web"},
	} {
		t.Run(name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "dist")
			if err := prepare(writeLock(t, lock), archives, output); err == nil {
				t.Fatal("unverified bundle prepared")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("failed preparation left an output directory")
			}
		})
	}
}
