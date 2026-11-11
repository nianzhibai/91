package frontend

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/atomicfile"
	"github.com/nianzhibai/91/backend/internal/filelock"
	"golang.org/x/mod/semver"
)

type Release struct {
	Manifest
	Repository  string `json:"repository,omitempty"`
	SHA256      string `json:"sha256"`
	ContentHash string `json:"contentHash"`
	InstalledAt string `json:"installedAt"`
}

type Selection struct {
	Active   string  `json:"active"` // Empty selects the default bundled frontend.
	Previous *string `json:"previous,omitempty"`
}

type Current struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Repository string `json:"repository"`
}

type Status struct {
	Selection
	Builtin   *Manifest `json:"builtin,omitempty"`
	Current   *Current  `json:"current,omitempty"`
	Installed []Release `json:"installed"`
}

type Manager struct {
	root       string
	builtin    string // Site served when no release is selected.
	bundle     string // Shipped bundle, when the builtin directory is one.
	apiVersion string
}

// InstallOptions describes the expected archive identity. All supplied
// constraints are checked in staging before the release becomes visible.
type InstallOptions struct {
	SHA256     string
	Name       string
	Version    string // Manifest version, without the release tag's v prefix.
	Repository string
}

func New(dataDirectory, builtinDirectory, apiVersion string) (*Manager, error) {
	m := &Manager{root: filepath.Join(dataDirectory, "frontends"), builtin: builtinDirectory, apiVersion: apiVersion}
	index, ok, err := readBundleIndex(builtinDirectory)
	if err != nil {
		return nil, err
	}
	if ok {
		m.bundle = builtinDirectory
		m.builtin = filepath.Join(builtinDirectory, "releases", index.Default, "site")
	}
	if err := os.MkdirAll(filepath.Join(m.root, "releases"), 0755); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) withLock(fn func() error) error {
	file, err := os.OpenFile(filepath.Join(m.root, "install.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := filelock.TryLock(file); err != nil {
		return fmt.Errorf("another frontend operation is running: %w", err)
	}
	defer filelock.Unlock(file)
	return fn()
}

func writeJSONFile(name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(name), ".frontend-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), name); err != nil {
		return err
	}
	return atomicfile.SyncDirectory(filepath.Dir(name))
}

func (m *Manager) selection() (Selection, error) {
	var selection Selection
	data, err := os.ReadFile(filepath.Join(m.root, "selection.json"))
	if errors.Is(err, os.ErrNotExist) {
		return selection, nil
	}
	if err != nil {
		return selection, err
	}
	if err := json.Unmarshal(data, &selection); err != nil {
		return selection, err
	}
	if selection.Active != "" && !validID(selection.Active) || selection.Previous != nil && *selection.Previous != "" && !validID(*selection.Previous) {
		return selection, errors.New("invalid frontend selection")
	}
	return selection, nil
}

func (m *Manager) Status() (Status, error) {
	selection, err := m.selection()
	if err != nil {
		return Status{}, err
	}
	status := Status{Selection: selection, Installed: []Release{}}
	if manifest, err := readManifest(m.builtin, m.apiVersion); err == nil {
		status.Builtin = &manifest
	}
	entries, err := os.ReadDir(filepath.Join(m.root, "releases"))
	if err != nil {
		return status, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validID(entry.Name()) {
			continue
		}
		var release Release
		data, err := os.ReadFile(filepath.Join(m.root, "releases", entry.Name(), "release.json"))
		if err != nil {
			return status, err
		}
		if err := json.Unmarshal(data, &release); err != nil {
			return status, err
		}
		status.Installed = append(status.Installed, release)
	}
	if status.Active == "" && status.Builtin != nil {
		// Retained bundled releases record the repository they were built from.
		repository := ""
		for _, release := range status.Installed {
			if release.ID() == status.Builtin.ID() {
				repository = release.Repository
				break
			}
		}
		status.Current = &Current{Name: status.Builtin.Name, Version: status.Builtin.Version, Repository: repository}
	} else {
		for _, release := range status.Installed {
			if release.ID() == status.Active {
				status.Current = &Current{Name: release.Name, Version: release.Version, Repository: release.Repository}
				break
			}
		}
	}
	return status, nil
}

func (m *Manager) directory(id string) (string, error) {
	if id == "" {
		if m.builtin == "" {
			return "", errors.New("bundled frontend is unavailable")
		}
		if info, err := os.Stat(filepath.Join(m.builtin, "index.html")); err != nil || !info.Mode().IsRegular() {
			return "", errors.New("bundled frontend index.html is unavailable")
		}
		return m.builtin, nil
	}
	if !validID(id) {
		return "", errors.New("invalid frontend identifier")
	}
	directory := filepath.Join(m.root, "releases", id, "site")
	manifest, err := readManifest(directory, m.apiVersion)
	if err != nil {
		return "", err
	}
	if manifest.ID() != id {
		return "", errors.New("frontend identity mismatch")
	}
	return directory, nil
}

func (m *Manager) ActiveDirectory() (string, error) {
	selection, err := m.selection()
	if err != nil {
		return "", err
	}
	return m.directory(selection.Active)
}

// AssetDirectory resolves retained immutable releases, including the bundled
// version. Old tabs can finish loading their chunks after a frontend switch.
func (m *Manager) AssetDirectory(name, version string) (string, error) {
	id := name + "@" + version
	if !validID(id) {
		return "", os.ErrNotExist
	}
	if dir, err := m.directory(id); err == nil {
		return dir, nil
	}
	if manifest, err := readManifest(m.builtin, m.apiVersion); err == nil && manifest.ID() == id {
		return m.builtin, nil
	}
	return "", os.ErrNotExist
}

func (m *Manager) Activate(id string) error {
	return m.withLock(func() error {
		if _, err := m.directory(id); err != nil {
			return err
		}
		selection, err := m.selection()
		if err != nil {
			if id == "" {
				return writeJSONFile(filepath.Join(m.root, "selection.json"), Selection{})
			}
			return err
		}
		if selection.Active == id {
			return nil
		}
		previous := selection.Active
		return writeJSONFile(filepath.Join(m.root, "selection.json"), Selection{Active: id, Previous: &previous})
	})
}

func (m *Manager) Rollback() error {
	return m.withLock(func() error {
		selection, err := m.selection()
		if err != nil {
			return err
		}
		if selection.Previous == nil {
			return errors.New("no previous frontend is available")
		}
		if _, err := m.directory(*selection.Previous); err != nil {
			return err
		}
		previous := selection.Active
		return writeJSONFile(filepath.Join(m.root, "selection.json"), Selection{Active: *selection.Previous, Previous: &previous})
	})
}

func (m *Manager) Install(reader io.Reader, options InstallOptions) (Release, error) {
	var release Release
	err := m.withLock(func() error {
		staging, err := os.MkdirTemp(m.root, ".install-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(staging)
		archive, err := os.CreateTemp(staging, "archive-*")
		if err != nil {
			return err
		}
		defer archive.Close()
		hash := sha256.New()
		n, err := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(reader, MaxArchiveBytes+1))
		if err != nil {
			return err
		}
		if n > MaxArchiveBytes {
			return errors.New("frontend archive exceeds download limit")
		}
		digest := hex.EncodeToString(hash.Sum(nil))
		if options.SHA256 != "" && !strings.EqualFold(options.SHA256, digest) {
			return errors.New("frontend archive SHA-256 mismatch")
		}
		if _, err := archive.Seek(0, 0); err != nil {
			return err
		}
		site := filepath.Join(staging, "site")
		if err := os.Mkdir(site, 0755); err != nil {
			return err
		}
		if err := unpack(archive, site); err != nil {
			return err
		}
		if err := archive.Close(); err != nil {
			return err
		}
		if err := os.Remove(archive.Name()); err != nil {
			return err
		}
		manifest, err := readManifest(site, m.apiVersion)
		if err != nil {
			return err
		}
		if options.Name != "" && manifest.Name != options.Name {
			return fmt.Errorf("frontend manifest name %q does not match expected %q", manifest.Name, options.Name)
		}
		if options.Version != "" && manifest.Version != options.Version {
			return fmt.Errorf("frontend manifest version %q does not match expected %q", manifest.Version, options.Version)
		}
		contentHash, err := treeDigest(site)
		if err != nil {
			return err
		}
		release = Release{Manifest: manifest, Repository: options.Repository, SHA256: digest, ContentHash: contentHash, InstalledAt: time.Now().UTC().Format(time.RFC3339)}
		return m.store(staging, release)
	})
	return release, err
}

func (m *Manager) store(staging string, release Release) error {
	target := filepath.Join(m.root, "releases", release.ID())
	if data, err := os.ReadFile(filepath.Join(target, "release.json")); err == nil {
		var existing Release
		if json.Unmarshal(data, &existing) != nil || existing.ContentHash != release.ContentHash {
			return errors.New("frontend version already exists with different content; publish a new version")
		}
		// An identical bundled/local copy may acquire its source when installed
		// from GitHub. Retaining the builtin must not erase a known source.
		if release.Repository != "" && release.Repository != existing.Repository {
			existing.Repository = release.Repository
			existing.SHA256 = release.SHA256
			return writeJSONFile(filepath.Join(target, "release.json"), existing)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := writeJSONFile(filepath.Join(staging, "release.json"), release); err != nil {
		return err
	}
	if err := os.Rename(staging, target); err != nil {
		return err
	}
	return atomicfile.SyncDirectory(filepath.Dir(target))
}

// RetainBuiltin preserves shipped frontends before later application upgrades
// replace the bundled directory. No selection is changed by this operation.
func (m *Manager) RetainBuiltin() error {
	if m.bundle != "" {
		return m.retainBundle()
	}
	if m.builtin == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(m.builtin, "frontend.json")); errors.Is(err, os.ErrNotExist) {
		return nil // A custom site without a manifest is served but not versioned.
	}
	return m.retain(m.builtin, Release{})
}

// retain stores a shipped site. Metadata from a bundle must match its files.
func (m *Manager) retain(site string, release Release) error {
	manifest, err := readManifest(site, m.apiVersion)
	if err != nil {
		return err
	}
	if release.Name != "" && release.Manifest != manifest {
		return errors.New("release metadata does not match frontend.json")
	}
	release.Manifest = manifest
	return m.withLock(func() error {
		contentHash, err := treeDigest(site)
		if err != nil {
			return err
		}
		if release.ContentHash != "" && release.ContentHash != contentHash {
			return errors.New("files do not match the recorded content hash")
		}
		release.ContentHash = contentHash
		release.InstalledAt = time.Now().UTC().Format(time.RFC3339)
		staging, err := os.MkdirTemp(m.root, ".builtin-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(staging)
		if err := copyTree(site, filepath.Join(staging, "site")); err != nil {
			return err
		}
		return m.store(staging, release)
	})
}

// installedFrom returns the newest installed release from repository that this
// server can still serve.
func (m *Manager) installedFrom(repository string) (Release, bool, error) {
	status, err := m.Status()
	if err != nil {
		return Release{}, false, err
	}
	var newest Release
	found := false
	for _, release := range status.Installed {
		if !sameRepository(release.Repository, repository) {
			continue
		}
		if _, err := m.directory(release.ID()); err != nil {
			continue
		}
		if !found || semver.Compare("v"+release.Version, "v"+newest.Version) > 0 {
			newest, found = release, true
		}
	}
	return newest, found, nil
}
