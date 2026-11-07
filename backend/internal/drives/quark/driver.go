package quark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/generationsource"
	"github.com/nianzhibai/91/backend/internal/scopedproxy"
)

const (
	defaultUA      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) quark-cloud-drive/2.5.20 Chrome/100.0.4896.160 Electron/18.3.5.4-b478491100 Safari/537.36 Channel/pckk_other_ch"
	defaultReferer = "https://pan.quark.cn"
	defaultAPI     = "https://drive.quark.cn/1/clouddrive"
	defaultPR      = "ucpro"
)

type Driver struct {
	generation     generationsource.Cache
	id             string
	cookie         string
	rootID         string
	ua             string
	referer        string
	apiBase        string
	playbackAPI    string
	pr             string
	client         *resty.Client
	uploadClient   *http.Client
	onCookieUpdate func(string)
	uploadTempDir  string
	requestMu      sync.Mutex
	cookieMu       sync.RWMutex
	ensureDirMu    sync.Mutex
}

type Config struct {
	ID             string
	Cookie         string
	RootID         string
	UploadTempDir  string
	OnCookieUpdate func(cookie string)
}

func New(c Config) *Driver {
	rootID := c.RootID
	if rootID == "" {
		rootID = "0"
	}
	d := &Driver{
		id:             c.ID,
		cookie:         c.Cookie,
		rootID:         rootID,
		ua:             defaultUA,
		referer:        defaultReferer,
		apiBase:        defaultAPI,
		playbackAPI:    "https://drive-pc.quark.cn/1/clouddrive",
		pr:             defaultPR,
		uploadTempDir:  c.UploadTempDir,
		onCookieUpdate: c.OnCookieUpdate,
	}
	d.client = resty.New().
		SetTransport(scopedproxy.NewTransport(nil)).
		SetTimeout(30*time.Second).
		SetHeader("Accept", "application/json, text/plain, */*").
		SetHeader("Referer", d.referer).
		SetHeader("User-Agent", d.ua)
	d.uploadClient = newQuarkUploadHTTPClient(nil)
	return d
}

func (d *Driver) Kind() string   { return "quark" }
func (d *Driver) ID() string     { return d.id }
func (d *Driver) RootID() string { return d.rootID }

// ---------- 公共请求 ----------

type resp struct {
	Status  int    `json:"status"`
	Code    int    `json:"code"`
	Message string `json:"message"`
	Msg     string `json:"msg"`
}

func (d *Driver) request(ctx context.Context, path, method string, query map[string]string, body any, out any) error {
	return d.requestAt(ctx, d.apiBase, path, method, query, body, out)
}

func (d *Driver) requestAt(ctx context.Context, apiBase, path, method string, query map[string]string, body any, out any) error {
	// Quark can rotate credential cookies on any response. Serializing provider
	// requests prevents two concurrent responses from applying rotations out of
	// order; cookieMu protects playback readers that only need a snapshot.
	d.requestMu.Lock()
	defer d.requestMu.Unlock()

	req := d.client.R().
		SetContext(ctx).
		SetHeader("Cookie", d.cookieSnapshot()).
		SetQueryParam("pr", d.pr).
		SetQueryParam("fr", "pc")
	if query != nil {
		req.SetQueryParams(query)
	}
	if body != nil {
		req.SetBody(body)
	}
	res, err := req.Execute(method, apiBase+path)
	if err != nil {
		return err
	}

	if cookie, changed := d.applyResponseCookies(res.Cookies()); changed && d.onCookieUpdate != nil {
		// Keep persistence callbacks in request order as well. A later rotation
		// must never be overwritten in storage by an earlier slow callback.
		d.onCookieUpdate(cookie)
	}

	raw := res.Body()
	var envelope resp
	jsonErr := error(nil)
	if len(raw) > 0 {
		jsonErr = json.Unmarshal(raw, &envelope)
	}
	statusCode := res.StatusCode()
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		apiErr := quarkResponseError(statusCode, envelope, raw)
		if statusCode == http.StatusTooManyRequests || envelope.Status == http.StatusTooManyRequests || envelope.Code == http.StatusTooManyRequests {
			return &drives.RateLimitError{Provider: d.Kind(), RetryAfter: parseRetryAfter(res.Header().Get("Retry-After")), Err: apiErr}
		}
		return apiErr
	}
	if jsonErr != nil {
		return fmt.Errorf("quark api %s: decode response: %w", path, jsonErr)
	}
	if envelope.Status >= http.StatusBadRequest || envelope.Code != 0 {
		apiErr := quarkResponseError(statusCode, envelope, raw)
		if envelope.Status == http.StatusTooManyRequests || envelope.Code == http.StatusTooManyRequests {
			return &drives.RateLimitError{Provider: d.Kind(), RetryAfter: parseRetryAfter(res.Header().Get("Retry-After")), Err: apiErr}
		}
		return apiErr
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("quark api %s: decode result: %w", path, err)
		}
	}
	return nil
}

func (d *Driver) Init(ctx context.Context) error {
	return d.request(ctx, "/config", http.MethodGet, nil, nil, nil)
}

// ---------- 列目录 ----------

type file struct {
	Fid       string `json:"fid"`
	FileName  string `json:"file_name"`
	Size      int64  `json:"size"`
	Category  int    `json:"category"`
	File      bool   `json:"file"`
	UpdatedAt int64  `json:"updated_at"`
	MD5       string `json:"md5"`
	SHA1      string `json:"sha1"`
	ParentID  string `json:"pdir_fid"`
	Status    *int   `json:"status"`
}

type sortResp struct {
	Data struct {
		List []file `json:"list"`
	} `json:"data"`
	Metadata struct {
		Total int `json:"_total"`
	} `json:"metadata"`
}

func (d *Driver) List(ctx context.Context, dirID string) ([]drives.Entry, error) {
	var out []drives.Entry
	page := 1
	size := 100
	for {
		q := map[string]string{
			"pdir_fid":             dirID,
			"_size":                strconv.Itoa(size),
			"_page":                strconv.Itoa(page),
			"_fetch_total":         "1",
			"fetch_all_file":       "1",
			"fetch_risk_file_name": "1",
		}
		var r sortResp
		if err := d.request(ctx, "/file/sort", http.MethodGet, q, nil, &r); err != nil {
			return nil, fmt.Errorf("quark list: %w", err)
		}
		for _, f := range r.Data.List {
			out = append(out, fileToEntry(&f, dirID))
		}
		if page*size >= r.Metadata.Total {
			break
		}
		page++
	}
	return out, nil
}

func (d *Driver) Stat(ctx context.Context, fileID string) (*drives.Entry, error) {
	var out struct {
		Data file `json:"data"`
	}
	if err := d.requestAt(ctx, d.playbackAPI, "/file/info", http.MethodGet, map[string]string{"fid": fileID}, nil, &out); err != nil {
		return nil, fmt.Errorf("quark stat: %w", err)
	}
	if out.Data.Fid == "" || (out.Data.Status != nil && *out.Data.Status == 0) {
		return nil, fmt.Errorf("quark stat: %w", os.ErrNotExist)
	}
	entry := fileToEntry(&out.Data, out.Data.ParentID)
	return &entry, nil
}

// ---------- 下载直链 ----------

type downResp struct {
	Data []struct {
		DownloadUrl string `json:"download_url"`
	} `json:"data"`
}

func (d *Driver) StreamURL(ctx context.Context, fileID string) (*drives.StreamLink, error) {
	body := map[string]any{"fids": []string{fileID}}
	var r downResp
	if err := d.request(ctx, "/file/download", http.MethodPost, nil, body, &r); err != nil {
		return nil, fmt.Errorf("quark download: %w", err)
	}
	if len(r.Data) == 0 || r.Data[0].DownloadUrl == "" {
		return nil, errors.New("quark download: empty url")
	}

	headers := http.Header{}
	headers.Set("User-Agent", d.ua)
	headers.Set("Referer", d.referer)
	headers.Set("Cookie", d.cookieSnapshot())

	return &drives.StreamLink{
		URL:       r.Data[0].DownloadUrl,
		Headers:   headers,
		URLSource: "quark-download",
		Expires:   time.Now().Add(10 * time.Minute),
	}, nil
}

// ---------- 创建目录 ----------

type mkdirResp struct {
	Data struct {
		Fid string `json:"fid"`
	} `json:"data"`
}

func (d *Driver) MakeDir(ctx context.Context, parentID, name string) (string, error) {
	body := map[string]any{
		"dir_init_lock": false,
		"dir_path":      "",
		"file_name":     name,
		"pdir_fid":      parentID,
	}
	var r mkdirResp
	if err := d.request(ctx, "/file", http.MethodPost, nil, body, &r); err != nil {
		return "", fmt.Errorf("quark mkdir: %w", err)
	}
	return r.Data.Fid, nil
}

func (d *Driver) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	d.ensureDirMu.Lock()
	defer d.ensureDirMu.Unlock()

	parts := splitPath(pathFromRoot)
	currentID := d.rootID
	for _, name := range parts {
		childID, err := d.findChildDir(ctx, currentID, name)
		if err != nil {
			return "", err
		}
		if childID == "" {
			id, err := d.MakeDir(ctx, currentID, name)
			if err != nil {
				// A competing process/account client may have created the same
				// directory. Re-list before treating the conflict as fatal.
				if existing, listErr := d.findChildDir(ctx, currentID, name); listErr == nil && existing != "" {
					childID = existing
					currentID = childID
					continue
				}
				return "", err
			}
			if strings.TrimSpace(id) == "" {
				return "", errors.New("quark mkdir: empty directory id")
			}
			childID = id
		}
		currentID = childID
	}
	return currentID, nil
}

func (d *Driver) findChildDir(ctx context.Context, parent, name string) (string, error) {
	entries, err := d.List(ctx, parent)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir && e.Name == name {
			return e.ID, nil
		}
	}
	return "", nil
}

// Upload is implemented in upload.go. Keeping the protocol isolated makes the
// replayable staging and multipart lifecycle auditable independently from list
// and playback operations.

func (d *Driver) Remove(ctx context.Context, fileID string) error {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return errors.New("quark remove: empty file id")
	}
	body := map[string]any{
		"action_type":  1,
		"exclude_fids": []string{},
		"filelist":     []string{fileID},
	}
	if err := d.request(ctx, "/file/delete", http.MethodPost, nil, body, nil); err != nil {
		return fmt.Errorf("quark remove: %w", err)
	}
	return nil
}

func (d *Driver) Rename(ctx context.Context, fileID, newName string) error {
	fileID = strings.TrimSpace(fileID)
	newName = strings.TrimSpace(newName)
	if fileID == "" || newName == "" {
		return errors.New("quark rename: empty file id or name")
	}
	body := map[string]any{"fid": fileID, "file_name": newName}
	if err := d.request(ctx, "/file/rename", http.MethodPost, nil, body, nil); err != nil {
		return fmt.Errorf("quark rename: %w", err)
	}
	return nil
}

// ---------- helpers ----------

func fileToEntry(f *file, parentID string) drives.Entry {
	return drives.Entry{
		ID: f.Fid,
		// Quark escapes names in directory listings. Decode before directory
		// matching and upload reconciliation so "A&B" is not treated as a
		// different object from "A&amp;B".
		Name:     html.UnescapeString(f.FileName),
		Size:     f.Size,
		IsDir:    !f.File,
		ParentID: parentID,
		MimeType: guessMime(f.FileName),
		ModTime:  time.UnixMilli(f.UpdatedAt),
		Category: f.Category,
		Hash:     firstNonEmptyString(f.SHA1, f.MD5),
	}
}

func guessMime(name string) string {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".mp4":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".mov":
		return "video/quicktime"
	case ".webm":
		return "video/webm"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	}
	return "application/octet-stream"
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// setCookieValue 替换 cookie 字符串中某个 key 的值，不存在则追加
func setCookieValue(cookie, key, value string) string {
	if cookie == "" {
		return key + "=" + value
	}
	parts := strings.Split(cookie, ";")
	var out []string
	found := false
	for _, p := range parts {
		kv := strings.TrimSpace(p)
		if kv == "" {
			continue
		}
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			out = append(out, kv)
			continue
		}
		if kv[:eq] == key {
			out = append(out, key+"="+value)
			found = true
		} else {
			out = append(out, kv)
		}
	}
	if !found {
		out = append(out, key+"="+value)
	}
	return strings.Join(out, "; ")
}

func (d *Driver) cookieSnapshot() string {
	d.cookieMu.RLock()
	defer d.cookieMu.RUnlock()
	return d.cookie
}

func (d *Driver) applyResponseCookies(cookies []*http.Cookie) (string, bool) {
	d.cookieMu.Lock()
	defer d.cookieMu.Unlock()
	next := d.cookie
	for _, ck := range cookies {
		if ck == nil || (ck.Name != "__puus" && ck.Name != "__pus") || ck.Value == "" {
			continue
		}
		next = setCookieValue(next, ck.Name, ck.Value)
	}
	if next == d.cookie {
		return d.cookie, false
	}
	d.cookie = next
	return next, true
}

func quarkResponseError(httpStatus int, envelope resp, raw []byte) error {
	message := strings.TrimSpace(envelope.Message)
	if message == "" {
		message = strings.TrimSpace(envelope.Msg)
	}
	if message == "" {
		message = strings.TrimSpace(string(raw))
		if len(message) > 256 {
			message = message[:256] + "..."
		}
	}
	if message == "" {
		message = http.StatusText(httpStatus)
	}
	return fmt.Errorf("quark api error: http=%d status=%d code=%d: %s", httpStatus, envelope.Status, envelope.Code, message)
}

func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(raw); err == nil {
		if wait := time.Until(deadline); wait > 0 {
			return wait
		}
	}
	return 0
}

var _ drives.Drive = (*Driver)(nil)
var _ drives.Remover = (*Driver)(nil)
var _ drives.Uploader = (*Driver)(nil)
