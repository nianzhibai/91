package frontend

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// BundleFile marks a directory of frontends shipped with the application. A
// bundle uses the same release layout as the managed data directory:
//
//	bundle.json
//	releases/<name>@<version>/release.json
//	releases/<name>@<version>/site/
const BundleFile = "bundle.json"

type bundleIndex struct {
	FormatVersion int    `json:"formatVersion"`
	Default       string `json:"default"` // Served until an administrator selects another frontend.
}

func readBundleIndex(directory string) (bundleIndex, bool, error) {
	var index bundleIndex
	if directory == "" {
		return index, false, nil
	}
	data, err := os.ReadFile(filepath.Join(directory, BundleFile))
	if errors.Is(err, os.ErrNotExist) {
		return index, false, nil
	}
	if err != nil {
		return index, false, err
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return index, false, fmt.Errorf("read frontend bundle: %w", err)
	}
	if index.FormatVersion != 1 || !validID(index.Default) {
		return index, false, errors.New("invalid frontend bundle index")
	}
	return index, true, nil
}

// BuiltinSite returns the site served when no managed release is selected. A
// bundle resolves to its default release; any other directory is a site.
func BuiltinSite(directory string) (string, error) {
	index, ok, err := readBundleIndex(directory)
	if err != nil || !ok {
		return directory, err
	}
	return filepath.Join(directory, "releases", index.Default, "site"), nil
}

// WriteBundle copies installed releases into a new bundle directory.
func (m *Manager) WriteBundle(output, defaultID string, ids []string) error {
	if !slices.Contains(ids, defaultID) {
		return errors.New("the default frontend must be part of the bundle")
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		return errors.New("bundle output directory must not already exist")
	}
	if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		return err
	}
	staged, err := os.MkdirTemp(filepath.Dir(output), ".frontend-bundle-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staged)
	if err := os.Chmod(staged, 0755); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := m.directory(id); err != nil {
			return err
		}
		if err := copyTree(filepath.Join(m.root, "releases", id), filepath.Join(staged, "releases", id)); err != nil {
			return err
		}
	}
	index, err := json.MarshalIndent(bundleIndex{FormatVersion: 1, Default: defaultID}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staged, BundleFile), index, 0644); err != nil {
		return err
	}
	return os.Rename(staged, output)
}

// retainBundle copies every bundled release, including its source repository,
// into the managed data directory so it can be enabled without downloading.
func (m *Manager) retainBundle() error {
	entries, err := os.ReadDir(filepath.Join(m.bundle, "releases"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validID(entry.Name()) {
			return fmt.Errorf("unexpected frontend bundle entry %q", entry.Name())
		}
		directory := filepath.Join(m.bundle, "releases", entry.Name())
		var release Release
		data, err := os.ReadFile(filepath.Join(directory, "release.json"))
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &release); err != nil {
			return err
		}
		if release.ID() != entry.Name() || release.ContentHash == "" {
			return fmt.Errorf("bundled frontend %s has invalid release metadata", entry.Name())
		}
		if err := m.retain(filepath.Join(directory, "site"), release); err != nil {
			return fmt.Errorf("bundled frontend %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func copyTree(source, target string) error {
	return filepath.WalkDir(source, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0755)
		}
		if !entry.Type().IsRegular() {
			return errors.New("frontend contains non-regular files")
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, data, 0644)
	})
}
