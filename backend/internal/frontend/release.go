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
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)

type ReleaseRequest struct {
	Repository string `json:"repository"`
	Version    string `json:"version"`
}

func (request ReleaseRequest) Validate() error {
	if !repositoryPattern.MatchString(request.Repository) {
		return errors.New("repository must be a GitHub owner/name")
	}
	if !strings.HasPrefix(request.Version, "v") || !validVersion(strings.TrimPrefix(request.Version, "v")) {
		return errors.New("version must be an explicit release tag such as v1.0.0; latest is not supported")
	}
	return nil
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

func (source *ReleaseSource) Install(ctx context.Context, manager *Manager, request ReleaseRequest) (Release, error) {
	if err := request.Validate(); err != nil {
		return Release{}, err
	}
	response, err := source.get(ctx, source.apiURL+"/repos/"+request.Repository+"/releases/tags/"+url.PathEscape(request.Version))
	if err != nil {
		return Release{}, err
	}
	var metadata struct {
		Tag    string         `json:"tag_name"`
		Draft  bool           `json:"draft"`
		Assets []releaseAsset `json:"assets"`
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&metadata)
	response.Body.Close()
	if err != nil {
		return Release{}, err
	}
	if metadata.Draft || metadata.Tag != request.Version {
		return Release{}, errors.New("release does not match the requested version")
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
		return Release{}, errors.New("release must contain frontend.tar.gz and frontend.tar.gz.sha256")
	}
	checksumResponse, err := source.get(ctx, checksumURL)
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
	archiveResponse, err := source.get(ctx, archiveURL)
	if err != nil {
		return Release{}, err
	}
	defer archiveResponse.Body.Close()
	return manager.Install(archiveResponse.Body, InstallOptions{
		SHA256: fields[0], Repository: request.Repository,
		Version: strings.TrimPrefix(request.Version, "v"),
	})
}
