// Package yun139 implements China Mobile's new personal cloud API. Endpoints,
// request signing and token renewal follow OpenList's drivers/139 implementation.
package yun139

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/generationsource"
	"github.com/nianzhibai/91/backend/internal/scopedproxy"
)

const Kind = "yun139"

type Config struct {
	ID                    string
	RootID                string
	Authorization         string
	UploadTempDir         string
	OnAuthorizationUpdate func(string)
}

type Driver struct {
	generation               generationsource.Cache
	id, rootID, personalHost string
	client, uploadClient     *http.Client
	routeURL, refreshURL     string
	uploadTempDir            string
	partSize                 int64

	authMu                sync.Mutex
	authorization         string
	refreshAfter          time.Time
	onAuthorizationUpdate func(string)

	fileMu sync.RWMutex
	files  map[string]drives.Entry
	dirMu  sync.Mutex
}

func New(c Config) *Driver {
	root := strings.TrimSpace(c.RootID)
	if root == "" {
		root = "/"
	}
	client := scopedproxy.NewHTTPClient(30 * time.Second)
	// Signed API requests must not forward account headers through redirects.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	uploadClient := scopedproxy.NewHTTPClient(30 * time.Minute)
	uploadClient.CheckRedirect = client.CheckRedirect
	return &Driver{
		id: c.ID, rootID: root, authorization: normalizeAuthorization(c.Authorization),
		client: client, uploadClient: uploadClient, uploadTempDir: strings.TrimSpace(c.UploadTempDir),
		routeURL:              "https://user-njs.yun.139.com/user/route/qryRoutePolicy",
		refreshURL:            "https://aas.caiyun.feixin.10086.cn:443/tellin/authTokenRefresh.do",
		onAuthorizationUpdate: c.OnAuthorizationUpdate,
		files:                 make(map[string]drives.Entry),
	}
}

func (d *Driver) Kind() string   { return Kind }
func (d *Driver) ID() string     { return d.id }
func (d *Driver) RootID() string { return d.rootID }

func (d *Driver) Init(ctx context.Context) error {
	_, account, err := d.ensureAuthorization(ctx)
	if err != nil {
		return err
	}
	var result struct {
		Data struct {
			Policies []struct {
				Name string `json:"modName"`
				URL  string `json:"httpsUrl"`
			} `json:"routePolicyList"`
		} `json:"data"`
	}
	if err := d.signedPost(ctx, d.routeURL, map[string]any{
		"userInfo":    map[string]any{"userType": 1, "accountType": 1, "accountName": account},
		"modAddrType": 1,
	}, &result); err != nil {
		return fmt.Errorf("yun139 route: %w", err)
	}
	for _, policy := range result.Data.Policies {
		if policy.Name == "personal" && validHTTPURL(policy.URL) {
			d.personalHost = strings.TrimRight(policy.URL, "/")
			return nil
		}
	}
	return errors.New("yun139 route: missing personal cloud host")
}

type fileItem struct {
	ID         string `json:"fileId"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	Type       string `json:"type"`
	UpdatedAt  string `json:"updatedAt"`
	Thumbnails []struct {
		URL string `json:"url"`
	} `json:"thumbnailUrls"`
}

func (d *Driver) List(ctx context.Context, dirID string) ([]drives.Entry, error) {
	if strings.TrimSpace(dirID) == "" {
		dirID = d.rootID
	}
	entries := make([]drives.Entry, 0)
	cursor := ""
	seen := make(map[string]bool)
	for {
		var result struct {
			Data struct {
				Items  []fileItem `json:"items"`
				Cursor string     `json:"nextPageCursor"`
			} `json:"data"`
		}
		if err := d.post(ctx, "/file/list", map[string]any{
			"parentFileId": dirID, "orderBy": "updated_at", "orderDirection": "DESC",
			"imageThumbnailStyleList": []string{"Small", "Large"},
			"pageInfo":                map[string]any{"pageCursor": cursor, "pageSize": 100},
		}, &result); err != nil {
			return nil, err
		}
		for _, item := range result.Data.Items {
			if item.ID == "" {
				return nil, errors.New("yun139 list: empty file ID")
			}
			modified, _ := time.Parse(time.RFC3339Nano, item.UpdatedAt)
			entry := drives.Entry{ID: item.ID, Name: item.Name, Size: item.Size, IsDir: item.Type == "folder", ParentID: dirID, ModTime: modified}
			if len(item.Thumbnails) > 0 {
				entry.ThumbnailURL = item.Thumbnails[len(item.Thumbnails)-1].URL
			}
			entries = append(entries, entry)
		}
		cursor = result.Data.Cursor
		if cursor == "" {
			break
		}
		if seen[cursor] {
			return nil, errors.New("yun139 list: repeated pagination cursor")
		}
		seen[cursor] = true
	}
	d.fileMu.Lock()
	for id, entry := range d.files {
		if entry.ParentID == dirID {
			delete(d.files, id)
		}
	}
	for _, entry := range entries {
		d.files[entry.ID] = entry
	}
	d.fileMu.Unlock()
	return entries, nil
}

func (d *Driver) Stat(ctx context.Context, fileID string) (*drives.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.fileMu.RLock()
	entry, ok := d.files[fileID]
	d.fileMu.RUnlock()
	if !ok {
		return nil, drives.ErrNotSupported
	}
	return &entry, nil
}

func (d *Driver) StreamURL(ctx context.Context, fileID string) (*drives.StreamLink, error) {
	if strings.TrimSpace(fileID) == "" {
		return nil, errors.New("yun139 stream: empty file ID")
	}
	var result struct {
		Data struct {
			URL       string `json:"url"`
			CDNURL    string `json:"cdnUrl"`
			CDNSwitch bool   `json:"cdnSwitch"`
		} `json:"data"`
	}
	if err := d.post(ctx, "/file/getDownloadUrl", map[string]string{"fileId": fileID}, &result); err != nil {
		return nil, err
	}
	link := result.Data.URL
	if result.Data.CDNSwitch && result.Data.CDNURL != "" {
		link = result.Data.CDNURL
	}
	if !validHTTPURL(link) {
		return nil, errors.New("yun139 stream: invalid download URL")
	}
	return &drives.StreamLink{URL: link, Headers: http.Header{}, Expires: time.Now().Add(time.Minute), ClientRedirectSafe: true}, nil
}

func (d *Driver) Remove(ctx context.Context, fileID string) error {
	if strings.TrimSpace(fileID) == "" || fileID == d.rootID || fileID == "/" {
		return errors.New("yun139 remove: invalid file ID")
	}
	if err := d.post(ctx, "/recyclebin/batchTrash", map[string]any{"fileIds": []string{fileID}}, nil); err != nil {
		return err
	}
	d.fileMu.Lock()
	delete(d.files, fileID)
	d.fileMu.Unlock()
	return nil
}

func (d *Driver) Rename(ctx context.Context, fileID, name string) error {
	if strings.TrimSpace(fileID) == "" || !validName(name) {
		return errors.New("yun139 rename: invalid file ID or name")
	}
	if err := d.post(ctx, "/file/update", map[string]string{"fileId": fileID, "name": name, "description": ""}, nil); err != nil {
		return err
	}
	d.fileMu.Lock()
	if entry, ok := d.files[fileID]; ok {
		entry.Name = name
		d.files[fileID] = entry
	}
	d.fileMu.Unlock()
	return nil
}

func (d *Driver) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	names := strings.Split(strings.Trim(pathFromRoot, "/"), "/")
	for _, name := range names {
		if name != "" && name != "." && !validName(name) {
			return "", errors.New("yun139 mkdir: invalid directory name")
		}
	}
	d.dirMu.Lock()
	defer d.dirMu.Unlock()
	parent := d.rootID
	for _, name := range names {
		if name == "" || name == "." {
			continue
		}
		entries, err := d.List(ctx, parent)
		if err != nil {
			return "", err
		}
		found := ""
		for _, entry := range entries {
			if entry.IsDir && entry.Name == name {
				found = entry.ID
				break
			}
		}
		if found == "" {
			var result struct {
				Data struct {
					ID string `json:"fileId"`
				} `json:"data"`
			}
			if err := d.post(ctx, "/file/create", map[string]string{
				"parentFileId": parent, "name": name, "description": "", "type": "folder", "fileRenameMode": "force_rename",
			}, &result); err != nil {
				return "", err
			}
			found = result.Data.ID
			if found == "" {
				return "", errors.New("yun139 mkdir: empty directory ID")
			}
		}
		parent = found
	}
	return parent, nil
}

func validName(name string) bool {
	return strings.TrimSpace(name) != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

func validHTTPURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.Fragment == ""
}

var _ drives.Drive = (*Driver)(nil)
var _ drives.Uploader = (*Driver)(nil)
var _ drives.Remover = (*Driver)(nil)
