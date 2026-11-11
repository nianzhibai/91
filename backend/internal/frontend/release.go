package frontend

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/githubrepo"
	"golang.org/x/mod/semver"
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)

type ReleaseRequest struct {
	Repository string `json:"repository"`
	Version    string `json:"version"` // Empty resolves the latest published release.
}

func (request ReleaseRequest) Validate() error {
	if _, err := request.repositoryName(); err != nil {
		return err
	}
	if request.Version != "" && (!strings.HasPrefix(request.Version, "v") || !validVersion(strings.TrimPrefix(request.Version, "v"))) {
		return errors.New("version must be an explicit release tag such as v1.0.0, or omitted for latest")
	}
	return nil
}

func (request ReleaseRequest) repositoryName() (string, error) {
	// Product builds and command-line installs can still pin owner/name and a tag.
	if repositoryPattern.MatchString(request.Repository) {
		return request.Repository, nil
	}
	return githubrepo.Name(request.Repository)
}

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type ReleaseSource struct {
	Client *http.Client
	apiURL string
}

func NewReleaseSource() *ReleaseSource {
	return &ReleaseSource{apiURL: "https://api.github.com", Client: &http.Client{
		Timeout: 90 * time.Second,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 || request.URL.Scheme != "https" {
				return errors.New("invalid frontend download redirect")
			}
			return nil
		},
	}}
}

func (source *ReleaseSource) get(ctx context.Context, address string) (*http.Response, error) {
	parsed, err := url.Parse(address)
	if err != nil || parsed.User != nil || (parsed.Scheme != "https" && !strings.HasPrefix(address, source.apiURL+"/")) {
		return nil, errors.New("frontend downloads require HTTPS")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "91-frontend-manager")
	response, err := source.Client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("frontend release download returned HTTP %d", response.StatusCode)
	}
	return response, nil
}

type resolvedRelease struct {
	Version     string
	ArchiveURL  string
	ChecksumURL string
}

func (source *ReleaseSource) resolve(ctx context.Context, request ReleaseRequest) (resolvedRelease, error) {
	if err := request.Validate(); err != nil {
		return resolvedRelease{}, err
	}
	repository, _ := request.repositoryName()
	releasePath := "latest"
	if request.Version != "" {
		releasePath = "tags/" + url.PathEscape(request.Version)
	}
	response, err := source.get(ctx, source.apiURL+"/repos/"+repository+"/releases/"+releasePath)
	if err != nil {
		return resolvedRelease{}, err
	}
	var metadata struct {
		Tag        string         `json:"tag_name"`
		Draft      bool           `json:"draft"`
		Prerelease bool           `json:"prerelease"`
		Assets     []releaseAsset `json:"assets"`
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&metadata)
	response.Body.Close()
	if err != nil {
		return resolvedRelease{}, err
	}
	if metadata.Draft || (request.Version == "" && metadata.Prerelease) ||
		(request.Version != "" && metadata.Tag != request.Version) ||
		!strings.HasPrefix(metadata.Tag, "v") || !validVersion(strings.TrimPrefix(metadata.Tag, "v")) {
		return resolvedRelease{}, errors.New("release does not match the requested version")
	}
	var archiveURL, checksumURL string
	for _, asset := range metadata.Assets {
		switch asset.Name {
		case ArchiveName:
			archiveURL = asset.URL
		case ArchiveName + ".sha256":
			checksumURL = asset.URL
		}
	}
	if archiveURL == "" || checksumURL == "" {
		return resolvedRelease{}, errors.New("release must contain frontend.tar.gz and frontend.tar.gz.sha256")
	}
	return resolvedRelease{Version: strings.TrimPrefix(metadata.Tag, "v"), ArchiveURL: archiveURL, ChecksumURL: checksumURL}, nil
}

func (source *ReleaseSource) Install(ctx context.Context, manager *Manager, request ReleaseRequest) (Release, error) {
	release, err := source.resolve(ctx, request)
	if err != nil {
		return Release{}, err
	}
	// The latest release can change between checking and installing. Never
	// reinstall or downgrade the same repository through the latest-release flow.
	if request.Version == "" {
		status, err := manager.Status()
		if err != nil {
			return Release{}, err
		}
		if status.Current != nil && sameRepository(status.Current.Repository, request.Repository) &&
			semver.Compare("v"+release.Version, "v"+status.Current.Version) <= 0 {
			return Release{}, ErrNoNewerRelease
		}
	}
	checksumResponse, err := source.get(ctx, release.ChecksumURL)
	if err != nil {
		return Release{}, err
	}
	checksum, err := io.ReadAll(io.LimitReader(checksumResponse.Body, 1025))
	checksumResponse.Body.Close()
	if err != nil {
		return Release{}, err
	}
	fields := strings.Fields(string(checksum))
	if len(checksum) > 1024 || len(fields) != 2 || fields[1] != ArchiveName || len(fields[0]) != 64 {
		return Release{}, errors.New("invalid frontend checksum file")
	}
	if _, err := hex.DecodeString(fields[0]); err != nil {
		return Release{}, errors.New("invalid frontend checksum")
	}
	archiveResponse, err := source.get(ctx, release.ArchiveURL)
	if err != nil {
		return Release{}, err
	}
	defer archiveResponse.Body.Close()
	return manager.Install(archiveResponse.Body, InstallOptions{
		SHA256: fields[0], Repository: request.Repository,
		Version: release.Version,
	})
}

// Enable activates a repository's frontend. Switching to a repository that is
// already installed, such as a bundled frontend, needs no network access. The
// active repository, or one not installed yet, gets its latest release.
func (source *ReleaseSource) Enable(ctx context.Context, manager *Manager, repository string) (Release, error) {
	status, err := manager.Status()
	if err != nil {
		return Release{}, err
	}
	if status.Current == nil || !sameRepository(status.Current.Repository, repository) {
		release, found, err := manager.installedFrom(repository)
		if err != nil {
			return Release{}, err
		}
		if found {
			return release, manager.Activate(release.ID())
		}
	}
	release, err := source.Install(ctx, manager, ReleaseRequest{Repository: repository})
	if err != nil {
		return Release{}, err
	}
	return release, manager.Activate(release.ID())
}
