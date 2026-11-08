// Package frontend manages independently released web applications.
package frontend

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/mod/semver"
)

const AssetPrefix = "/_frontend/"
const ArchiveName = "frontend.tar.gz"

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

type Manifest struct {
	FormatVersion int    `json:"formatVersion"`
	Name          string `json:"name"`
	Version       string `json:"version"`
	APIMin        string `json:"apiMin"`
	APIMax        string `json:"apiMaxExclusive"`
}

func validVersion(version string) bool {
	return !strings.HasPrefix(version, "v") && !strings.ContainsAny(version, "/\\") && semver.IsValid("v"+version) && semver.Canonical("v"+version) == "v"+version
}

func (m Manifest) ID() string      { return m.Name + "@" + m.Version }
func (m Manifest) BaseURL() string { return AssetPrefix + m.Name + "/" + m.Version + "/" }

func (m Manifest) Validate(apiVersion string) error {
	if m.FormatVersion != 1 || !namePattern.MatchString(m.Name) || !validVersion(m.Version) {
		return errors.New("invalid frontend manifest format, name or version")
	}
	if !validVersion(m.APIMin) || !validVersion(m.APIMax) || semver.Compare("v"+m.APIMin, "v"+m.APIMax) >= 0 {
		return errors.New("invalid frontend API compatibility range")
	}
	if semver.Compare("v"+apiVersion, "v"+m.APIMin) < 0 || semver.Compare("v"+apiVersion, "v"+m.APIMax) >= 0 {
		return fmt.Errorf("frontend requires API >= %s and < %s; server provides %s", m.APIMin, m.APIMax, apiVersion)
	}
	return nil
}

func readManifest(directory, apiVersion string) (Manifest, error) {
	var manifest Manifest
	if directory == "" {
		return manifest, os.ErrNotExist
	}
	data, err := os.ReadFile(filepath.Join(directory, "frontend.json"))
	if err != nil {
		return manifest, err
	}
	if len(data) > 8192 {
		return manifest, errors.New("frontend manifest is too large")
	}
	if err = json.Unmarshal(data, &manifest); err != nil {
		return manifest, err
	}
	if err = manifest.Validate(apiVersion); err != nil {
		return manifest, err
	}
	index, err := os.ReadFile(filepath.Join(directory, "index.html"))
	if err != nil {
		return manifest, err
	}
	if !strings.Contains(string(index), manifest.BaseURL()) {
		return manifest, fmt.Errorf("index.html must reference versioned resources under %s", manifest.BaseURL())
	}
	return manifest, nil
}

func validID(id string) bool {
	name, version, ok := strings.Cut(id, "@")
	return ok && namePattern.MatchString(name) && validVersion(version)
}
