package frontend

import (
	"context"
	"strings"
	"sync"

	"golang.org/x/mod/semver"
)

// Update reports a published release newer than every installed version of a
// frontend.
type Update struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func sameRepository(left, right string) bool {
	a, errA := (ReleaseRequest{Repository: left}).repositoryName()
	b, errB := (ReleaseRequest{Repository: right}).repositoryName()
	return errA == nil && errB == nil && strings.EqualFold(a, b)
}

// Updates reads only release metadata for listed frontends with a known
// repository. Packages are downloaded after the administrator chooses to
// update. A frontend whose metadata cannot be read reports no update.
func (source *ReleaseSource) Updates(ctx context.Context, manager *Manager) ([]Update, error) {
	frontends, err := manager.Frontends()
	if err != nil {
		return nil, err
	}
	status, err := manager.Status()
	if err != nil {
		return nil, err
	}
	installed := map[string]string{}
	if status.Builtin != nil {
		installed[status.Builtin.Name] = status.Builtin.Version
	}
	for _, release := range status.Installed {
		if semver.Compare("v"+release.Version, "v"+installed[release.Name]) > 0 {
			installed[release.Name] = release.Version
		}
	}
	found := make([]*Update, len(frontends))
	var wait sync.WaitGroup
	for i, frontend := range frontends {
		if frontend.Repository == "" {
			continue
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			release, err := source.resolve(ctx, ReleaseRequest{Repository: frontend.Repository})
			if err == nil && semver.Compare("v"+release.Version, "v"+installed[frontend.Name]) > 0 {
				found[i] = &Update{Name: frontend.Name, Version: release.Version}
			}
		}()
	}
	wait.Wait()
	updates := []Update{}
	for _, update := range found {
		if update != nil {
			updates = append(updates, *update)
		}
	}
	return updates, nil
}
