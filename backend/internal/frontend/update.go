package frontend

import (
	"context"
	"errors"
	"strings"

	"golang.org/x/mod/semver"
)

var ErrNoNewerRelease = errors.New("当前前端已是最新版本，请重新检查更新")

var ErrRepositoryNotActive = errors.New("configured frontend repository is not active; reload frontend status")

type UpdateCheck struct {
	Repository      string  `json:"repository"`
	Current         Current `json:"current"`
	LatestVersion   string  `json:"latestVersion"`
	UpdateAvailable bool    `json:"updateAvailable"`
}

func sameRepository(left, right string) bool {
	a, errA := (ReleaseRequest{Repository: left}).repositoryName()
	b, errB := (ReleaseRequest{Repository: right}).repositoryName()
	return errA == nil && errB == nil && strings.EqualFold(a, b)
}

// CheckUpdate only reads release metadata. Packages are downloaded after the
// administrator chooses to install, and older releases are never offered as updates.
func (source *ReleaseSource) CheckUpdate(ctx context.Context, manager *Manager, repository string) (UpdateCheck, error) {
	status, err := manager.Status()
	if err != nil {
		return UpdateCheck{}, err
	}
	if status.Current == nil || !sameRepository(status.Current.Repository, repository) {
		return UpdateCheck{}, ErrRepositoryNotActive
	}
	release, err := source.resolve(ctx, ReleaseRequest{Repository: repository})
	if err != nil {
		return UpdateCheck{}, err
	}
	// An installation can finish while the metadata request is in flight.
	status, err = manager.Status()
	if err != nil {
		return UpdateCheck{}, err
	}
	if status.Current == nil || !sameRepository(status.Current.Repository, repository) {
		return UpdateCheck{}, ErrRepositoryNotActive
	}
	return UpdateCheck{
		Repository: repository, Current: *status.Current, LatestVersion: release.Version,
		UpdateAvailable: semver.Compare("v"+release.Version, "v"+status.Current.Version) > 0,
	}, nil
}
