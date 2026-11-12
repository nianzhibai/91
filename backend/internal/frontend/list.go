package frontend

import (
	"cmp"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/semver"
)

// Frontend is one selectable frontend. Installed releases sharing a manifest
// name are versions of the same frontend; only the version that is served, or
// would be served after switching, is listed.
type Frontend struct {
	// ID is passed to Activate. Empty selects the application's default site,
	// which follows application upgrades instead of pinning a version.
	ID         string `json:"id"`
	Name       string `json:"name"` // Empty for a default site without frontend.json.
	Version    string `json:"version,omitempty"`
	Repository string `json:"repository,omitempty"`
	Builtin    bool   `json:"builtin"`
	Active     bool   `json:"active"`
}

// Frontends lists every frontend this server can serve, builtin ones first.
func (m *Manager) Frontends() ([]Frontend, error) {
	status, err := m.Status()
	if err != nil {
		return nil, err
	}
	bundled, err := m.bundledNames()
	if err != nil {
		return nil, err
	}
	defaultID := ""
	if status.Builtin != nil {
		defaultID = status.Builtin.ID()
		bundled[status.Builtin.Name] = true
	}
	// The default site is served when nothing, or nothing servable, is selected.
	served := status.Active
	if _, err := m.directory(served); served == "" || err != nil {
		served = defaultID
	}

	versions := map[string][]Release{}
	for _, release := range status.Installed {
		if _, err := m.directory(release.ID()); err == nil {
			versions[release.Name] = append(versions[release.Name], release)
		}
	}
	if status.Builtin != nil && !slices.ContainsFunc(versions[status.Builtin.Name], func(release Release) bool { return release.ID() == defaultID }) {
		versions[status.Builtin.Name] = append(versions[status.Builtin.Name], Release{Manifest: *status.Builtin})
	}

	frontends := []Frontend{}
	if _, err := m.directory(""); err == nil && status.Builtin == nil {
		frontends = append(frontends, Frontend{Builtin: true, Active: served == ""})
	}
	for name, releases := range versions {
		slices.SortFunc(releases, func(a, b Release) int { return semver.Compare("v"+b.Version, "v"+a.Version) })
		frontend := Frontend{Name: name, Builtin: bundled[name]}
		chosen := releases[0]
		if index := slices.IndexFunc(releases, func(release Release) bool { return release.ID() == served }); index >= 0 {
			chosen, frontend.Active = releases[index], true
		}
		frontend.ID, frontend.Version = chosen.ID(), chosen.Version
		if frontend.ID == defaultID {
			frontend.ID = ""
		}
		for _, release := range append([]Release{chosen}, releases...) {
			if release.Repository != "" {
				frontend.Repository = release.Repository
				break
			}
		}
		frontends = append(frontends, frontend)
	}
	slices.SortStableFunc(frontends, func(a, b Frontend) int {
		if a.Builtin != b.Builtin {
			if a.Builtin {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return frontends, nil
}

// bundledNames returns the frontends shipped with this application version.
func (m *Manager) bundledNames() (map[string]bool, error) {
	names := map[string]bool{}
	if m.bundle == "" {
		return names, nil
	}
	entries, err := os.ReadDir(filepath.Join(m.bundle, "releases"))
	if errors.Is(err, os.ErrNotExist) {
		return names, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if name, _, ok := strings.Cut(entry.Name(), "@"); ok && validID(entry.Name()) {
			names[name] = true
		}
	}
	return names, nil
}
