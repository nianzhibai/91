package googledrive

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"

	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/scopedproxy"
)

const (
	Kind                = "googledrive"
	defaultAPIBaseURL   = "https://www.googleapis.com/drive/v3"
	defaultUploadAPIURL = "https://www.googleapis.com/upload/drive/v3"
	defaultOAuthURL     = "https://www.googleapis.com/oauth2/v4/token"
	defaultListInterval = 1 * time.Second
	defaultListCooldown = 5 * time.Minute
	defaultLinkCooldown = 5 * time.Minute
	uploadChunkSize     = int64(8 * 1024 * 1024)

	filesListFields = "files(id,name,mimeType,size,modifiedTime,createdTime,thumbnailLink,shortcutDetails,md5Checksum,sha1Checksum,sha256Checksum),nextPageToken"
	fileInfoFields  = "id,name,mimeType,size,modifiedTime,createdTime,thumbnailLink,shortcutDetails,md5Checksum,sha1Checksum,sha256Checksum"
)

type Driver struct {
	id            string
	rootID        string
	refreshToken  string
	accessToken   string
	clientID      string
	clientSecret  string
	oauthURL      string
	apiBaseURL    string
	uploadBaseURL string
	client        *resty.Client
	httpClient    *http.Client
	onTokenUpdate func(access, refresh string)

	// OAuth refresh tokens can rotate. Keep request snapshots race-free and
	// collapse simultaneous 401 recovery into one exchange.
	tokenMu         sync.RWMutex
	refreshMu       sync.Mutex
	tokenGeneration uint64

	listMu       sync.Mutex
	lastListAt   time.Time
	listInterval time.Duration
	listCooldown time.Duration

	linkCooldownMu       sync.Mutex
	linkCooldownUntil    time.Time
	linkCooldownDuration time.Duration
}

type Config struct {
	ID           string
	RootID       string
	RefreshToken string
	AccessToken  string
	ClientID     string
	ClientSecret string
	OAuthURL     string
	APIBaseURL   string
	UploadAPIURL string

	OnTokenUpdate func(access, refresh string)
}

func New(c Config) *Driver {
	rootID := strings.TrimSpace(c.RootID)
	if rootID == "" {
		rootID = "root"
	}
	oauthURL := strings.TrimSpace(c.OAuthURL)
	if oauthURL == "" {
		oauthURL = defaultOAuthURL
	}
	apiBaseURL := strings.TrimRight(strings.TrimSpace(c.APIBaseURL), "/")
	if apiBaseURL == "" {
		apiBaseURL = defaultAPIBaseURL
	}
	uploadBaseURL := strings.TrimRight(strings.TrimSpace(c.UploadAPIURL), "/")
	if uploadBaseURL == "" {
		uploadBaseURL = deriveUploadBaseURL(apiBaseURL)
	}
	return &Driver{
		id:              c.ID,
		rootID:          rootID,
		refreshToken:    strings.TrimSpace(c.RefreshToken),
		accessToken:     strings.TrimSpace(c.AccessToken),
		clientID:        strings.TrimSpace(c.ClientID),
		clientSecret:    strings.TrimSpace(c.ClientSecret),
		oauthURL:        oauthURL,
		apiBaseURL:      apiBaseURL,
		uploadBaseURL:   uploadBaseURL,
		onTokenUpdate:   c.OnTokenUpdate,
		tokenGeneration: 1,
		client: resty.New().
			SetTransport(scopedproxy.NewTransport(nil)).
			SetTimeout(30*time.Second).
			SetHeader("Accept", "application/json, text/plain, */*"),
		httpClient: &http.Client{
			Timeout:   0,
			Transport: scopedproxy.NewTransport(nil),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		listInterval:         defaultListInterval,
		listCooldown:         defaultListCooldown,
		linkCooldownDuration: defaultLinkCooldown,
	}
}

func deriveUploadBaseURL(apiBaseURL string) string {
	apiBaseURL = strings.TrimRight(strings.TrimSpace(apiBaseURL), "/")
	if apiBaseURL == "" || apiBaseURL == defaultAPIBaseURL {
		return defaultUploadAPIURL
	}
	if strings.HasSuffix(apiBaseURL, "/drive/v3") {
		return strings.TrimSuffix(apiBaseURL, "/drive/v3") + "/upload/drive/v3"
	}
	return apiBaseURL
}

func (d *Driver) Kind() string   { return Kind }
func (d *Driver) ID() string     { return d.id }
func (d *Driver) RootID() string { return d.rootID }

func (d *Driver) Init(ctx context.Context) error {
	if d.tokenSnapshot().refresh == "" {
		return errors.New("googledrive init: refresh_token is required")
	}
	return d.refresh(ctx)
}

func (d *Driver) List(ctx context.Context, dirID string) ([]drives.Entry, error) {
	if dirID == "" {
		dirID = d.rootID
	}
	d.listMu.Lock()
	defer d.listMu.Unlock()

	pageToken := ""
	out := make([]drives.Entry, 0)
	for {
		if err := d.waitForListSlotLocked(ctx); err != nil {
			return nil, err
		}
		var resp filesResp
		err := d.request(ctx, d.filesURL(), http.MethodGet, func(req *resty.Request) {
			params := map[string]string{
				"fields":   filesListFields,
				"pageSize": "1000",
				"q":        fmt.Sprintf("'%s' in parents and trashed = false", strings.ReplaceAll(dirID, "'", "\\'")),
				"orderBy":  "folder,name,modifiedTime desc",
			}
			if pageToken != "" {
				params["pageToken"] = pageToken
			}
			req.SetQueryParams(params)
		}, &resp)
		if err != nil {
			if wait, ok := drives.RateLimitRetryAfter(err); ok {
				if wait <= 0 {
					wait = d.listCooldown
				}
				if sleepErr := sleepContext(ctx, wait); sleepErr != nil {
					return nil, sleepErr
				}
				continue
			}
			return nil, fmt.Errorf("googledrive list: %w", err)
		}
		if err := d.fillShortcutFileMetadata(ctx, resp.Files); err != nil {
			return nil, fmt.Errorf("googledrive shortcut metadata: %w", err)
		}
		for _, f := range resp.Files {
			out = append(out, fileToEntry(f, dirID))
		}
		pageToken = resp.NextPageToken
		if pageToken == "" {
			return out, nil
		}
	}
}

func (d *Driver) waitForListSlotLocked(ctx context.Context) error {
	if d.listInterval <= 0 || d.lastListAt.IsZero() {
		d.lastListAt = time.Now()
		return ctx.Err()
	}
	next := d.lastListAt.Add(d.listInterval)
	now := time.Now()
	if now.Before(next) {
		if err := sleepContext(ctx, next.Sub(now)); err != nil {
			return err
		}
	}
	d.lastListAt = time.Now()
	return ctx.Err()
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (d *Driver) Stat(ctx context.Context, fileID string) (*drives.Entry, error) {
	var f driveFile
	if err := d.request(ctx, d.fileURL(fileID), http.MethodGet, func(req *resty.Request) {
		req.SetQueryParam("fields", fileInfoFields)
	}, &f); err != nil {
		return nil, fmt.Errorf("googledrive stat: %w", err)
	}
	e := fileToEntry(f, "")
	return &e, nil
}

func (d *Driver) StreamURL(ctx context.Context, fileID string) (*drives.StreamLink, error) {
	if fileID == "" {
		return nil, errors.New("googledrive stream: empty file id")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := d.linkCooldownError(time.Now()); err != nil {
		return nil, err
	}
	if _, err := d.Stat(ctx, fileID); err != nil {
		err = fmt.Errorf("googledrive stream: %w", err)
		if wait, ok := drives.RateLimitRetryAfter(err); ok {
			until := d.pauseLinkCooldown(wait)
			log.Printf("[googledrive] stream link cooling down drive=%s until=%s err=%v", d.id, until.Format(time.RFC3339), err)
		}
		return nil, err
	}
	u := d.fileURL(fileID) + "?alt=media&acknowledgeAbuse=true&supportsAllDrives=true"
	tokens := d.tokenSnapshot()
	return &drives.StreamLink{
		URL: u,
		Headers: http.Header{
			"Authorization": []string{"Bearer " + tokens.access},
		},
		Expires: time.Now().Add(30 * time.Minute),
	}, nil
}

func (d *Driver) linkCooldownError(now time.Time) error {
	d.linkCooldownMu.Lock()
	defer d.linkCooldownMu.Unlock()
	if d.linkCooldownUntil.IsZero() {
		return nil
	}
	if !now.Before(d.linkCooldownUntil) {
		d.linkCooldownUntil = time.Time{}
		return nil
	}
	wait := d.linkCooldownUntil.Sub(now)
	if wait <= 0 {
		return nil
	}
	return &drives.RateLimitError{
		Provider:   Kind,
		RetryAfter: wait,
		Err:        fmt.Errorf("googledrive stream link cooling down until %s", d.linkCooldownUntil.Format(time.RFC3339)),
	}
}

func (d *Driver) pauseLinkCooldown(wait time.Duration) time.Time {
	if wait <= 0 {
		wait = d.linkCooldownDuration
	}
	if wait <= 0 {
		wait = defaultLinkCooldown
	}
	until := time.Now().Add(wait)
	d.linkCooldownMu.Lock()
	if until.After(d.linkCooldownUntil) {
		d.linkCooldownUntil = until
	} else {
		until = d.linkCooldownUntil
	}
	d.linkCooldownMu.Unlock()
	return until
}

func (d *Driver) Upload(ctx context.Context, parentID, name string, r io.Reader, size int64) (string, error) {
	res, err := d.UploadAndReportHash(ctx, parentID, name, r, size)
	if err != nil {
		return "", err
	}
	return res.FileID, nil
}

func (d *Driver) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	parentID, name, err := d.normalizeUploadArgs(parentID, name, r, size)
	if err != nil {
		return UploadResult{}, err
	}
	sessionURL, err := d.createUploadSession(ctx, parentID, name, size)
	if err != nil {
		return UploadResult{}, err
	}
	if strings.TrimSpace(sessionURL) == "" {
		return UploadResult{}, errors.New("googledrive upload session: empty upload url")
	}

	hasher := md5.New()
	var item driveFile
	var copied int64
	if size == 0 {
		completed, err := d.putUploadSessionChunkWithRetry(ctx, sessionURL, 0, 0, nil, hasher)
		if err != nil {
			return UploadResult{}, err
		}
		if completed != nil {
			item = *completed
		}
	} else {
		chunkSize := uploadChunkSize
		if chunkSize <= 0 {
			chunkSize = 8 * 1024 * 1024
		}
		if chunkSize > int64(math.MaxInt32) {
			chunkSize = int64(math.MaxInt32)
		}
		buf := make([]byte, int(chunkSize))
		for copied < size {
			partSize := minInt64(chunkSize, size-copied)
			chunk := buf[:int(partSize)]
			n, err := io.ReadFull(r, chunk)
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					return UploadResult{}, fmt.Errorf("googledrive upload: size mismatch: declared %d, copied %d", size, copied+int64(n))
				}
				return UploadResult{}, fmt.Errorf("googledrive upload: read body: %w", err)
			}
			chunk = chunk[:n]
			completed, err := d.putUploadSessionChunkWithRetry(ctx, sessionURL, copied, size, chunk, hasher)
			if err != nil {
				return UploadResult{}, err
			}
			if completed != nil {
				item = *completed
			}
			copied += int64(n)
		}
	}

	hashHex := hex.EncodeToString(hasher.Sum(nil))
	if item.ID == "" {
		fileID, err := d.findUploadedFileID(ctx, parentID, name, hashHex)
		if err != nil {
			return UploadResult{}, err
		}
		item.ID = fileID
	}
	return UploadResult{FileID: item.ID, Hash: hashHex, Size: copied}, nil
}

func (d *Driver) normalizeUploadArgs(parentID, name string, r io.Reader, size int64) (string, string, error) {
	if r == nil {
		return "", "", errors.New("googledrive upload: body is required")
	}
	if size < 0 {
		return "", "", fmt.Errorf("googledrive upload: invalid size %d", size)
	}
	parentID = strings.TrimSpace(parentID)
	if parentID == "" || parentID == "/" {
		parentID = d.rootID
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", errors.New("googledrive upload: empty file name")
	}
	return parentID, name, nil
}

func (d *Driver) createUploadSession(ctx context.Context, parentID, name string, size int64) (string, error) {
	return d.createUploadSessionOnce(ctx, parentID, name, size, true)
}

func (d *Driver) createUploadSessionOnce(ctx context.Context, parentID, name string, size int64, retry bool) (string, error) {
	tokens := d.tokenSnapshot()
	var apiErr apiErrorResp
	res, err := d.client.R().
		SetContext(ctx).
		SetHeader("Authorization", "Bearer "+tokens.access).
		SetHeader("X-Upload-Content-Type", mimeType(driveFile{Name: name})).
		SetHeader("X-Upload-Content-Length", strconv.FormatInt(size, 10)).
		SetQueryParams(map[string]string{
			"uploadType":        "resumable",
			"supportsAllDrives": "true",
			"fields":            fileInfoFields,
		}).
		SetBody(map[string]any{
			"name":    name,
			"parents": []string{parentID},
		}).
		SetError(&apiErr).
		Post(d.uploadFilesURL())
	if err != nil {
		return "", fmt.Errorf("googledrive upload session: %w", err)
	}
	if isGoogleRateLimit(res, apiErr.Error) {
		return "", googleRateLimitError(res, apiErr.Error.Message)
	}
	if apiErr.Error.Code != 0 {
		if apiErr.Error.Code == http.StatusUnauthorized && retry {
			if err := d.refresh(ctx, tokens); err != nil {
				return "", err
			}
			return d.createUploadSessionOnce(ctx, parentID, name, size, false)
		}
		return "", googleAPIError(apiErr.Error)
	}
	if res.IsError() {
		return "", fmt.Errorf("googledrive upload session: status=%d body=%s", res.StatusCode(), strings.TrimSpace(res.String()))
	}
	return strings.TrimSpace(res.Header().Get("Location")), nil
}

func (d *Driver) putUploadSessionChunkWithRetry(ctx context.Context, uploadURL string, start, total int64, data []byte, hasher hash.Hash) (*driveFile, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err := sleepContext(ctx, time.Duration(attempt)*time.Second); err != nil {
				return nil, err
			}
		}
		item, retryable, err := d.putUploadSessionChunk(ctx, uploadURL, start, total, data)
		if err == nil {
			if hasher != nil && len(data) > 0 {
				_, _ = hasher.Write(data)
			}
			return item, nil
		}
		last = err
		if !retryable {
			return nil, err
		}
	}
	if last == nil {
		last = errors.New("googledrive upload session: retry attempts exhausted")
	}
	return nil, last
}

func (d *Driver) putUploadSessionChunk(ctx context.Context, uploadURL string, start, total int64, data []byte) (*driveFile, bool, error) {
	tokens := d.tokenSnapshot()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(data))
	if err != nil {
		return nil, false, err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Authorization", "Bearer "+tokens.access)
	req.Header.Set("Content-Length", strconv.Itoa(len(data)))
	if total == 0 {
		req.Header.Set("Content-Range", "bytes */0")
	} else {
		end := start + int64(len(data)) - 1
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	}
	hc := d.httpClient
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("googledrive upload session: put chunk: %w", err)
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var item driveFile
		if err := json.NewDecoder(res.Body).Decode(&item); err != nil {
			return nil, false, fmt.Errorf("googledrive upload session: decode completed file: %w", err)
		}
		return &item, false, nil
	case http.StatusPermanentRedirect:
		return nil, false, nil
	case http.StatusUnauthorized:
		if err := d.refresh(ctx, tokens); err != nil {
			return nil, false, err
		}
		return nil, true, fmt.Errorf("googledrive upload session: unauthorized")
	default:
		body, _ := io.ReadAll(io.LimitReader(res.Body, 64*1024))
		var apiErr apiErrorResp
		_ = json.Unmarshal(body, &apiErr)
		if isGoogleUploadHTTPRateLimit(res.StatusCode, res.Header, body, apiErr.Error) {
			return nil, false, googleUploadRateLimitError(res.StatusCode, res.Header, body, apiErr.Error.Message)
		}
		retryable := res.StatusCode == http.StatusTooManyRequests || (res.StatusCode >= 500 && res.StatusCode <= 504)
		return nil, retryable, fmt.Errorf("googledrive upload session: status=%d body=%s", res.StatusCode, strings.TrimSpace(string(body)))
	}
}

func (d *Driver) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	currentID := d.rootID
	for _, name := range splitPath(pathFromRoot) {
		childID, err := d.findChildDir(ctx, currentID, name)
		if err != nil {
			return "", err
		}
		if childID == "" {
			childID, err = d.makeDir(ctx, currentID, name)
			if err != nil {
				return "", err
			}
		}
		currentID = childID
	}
	return currentID, nil
}

func (d *Driver) findChildDir(ctx context.Context, parentID, name string) (string, error) {
	entries, err := d.List(ctx, parentID)
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

func (d *Driver) makeDir(ctx context.Context, parentID, name string) (string, error) {
	var item driveFile
	err := d.request(ctx, d.filesURL(), http.MethodPost, func(req *resty.Request) {
		req.SetQueryParam("fields", fileInfoFields)
		req.SetBody(map[string]any{
			"name":     name,
			"parents":  []string{parentID},
			"mimeType": "application/vnd.google-apps.folder",
		})
	}, &item)
	if err != nil {
		return "", fmt.Errorf("googledrive mkdir %s: %w", name, err)
	}
	if item.ID == "" {
		return "", fmt.Errorf("googledrive mkdir %s: empty file id", name)
	}
	return item.ID, nil
}

func (d *Driver) Rename(ctx context.Context, fileID, newName string) error {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return errors.New("googledrive rename: empty file id")
	}
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return errors.New("googledrive rename: empty new name")
	}
	var item driveFile
	err := d.request(ctx, d.fileURL(fileID), http.MethodPatch, func(req *resty.Request) {
		req.SetQueryParam("fields", fileInfoFields)
		req.SetBody(map[string]string{"name": newName})
	}, &item)
	if err != nil {
		return fmt.Errorf("googledrive rename: %w", err)
	}
	return nil
}

func (d *Driver) Remove(ctx context.Context, fileID string) error {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return errors.New("googledrive remove: empty file id")
	}
	if err := d.request(ctx, d.fileURL(fileID), http.MethodDelete, nil, nil); err != nil {
		return fmt.Errorf("googledrive remove: %w", err)
	}
	return nil
}

func (d *Driver) findUploadedFileID(ctx context.Context, parentID, name, md5Hex string) (string, error) {
	entries, err := d.List(ctx, parentID)
	if err != nil {
		return "", fmt.Errorf("googledrive upload verify: %w", err)
	}
	var hashHit string
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		if !strings.EqualFold(e.Hash, md5Hex) {
			continue
		}
		if e.Name == name {
			return e.ID, nil
		}
		if hashHit == "" {
			hashHit = e.ID
		}
	}
	if hashHit != "" {
		return hashHit, nil
	}
	for _, e := range entries {
		if !e.IsDir && e.Name == name {
			return e.ID, nil
		}
	}
	return "", fmt.Errorf("googledrive upload: uploaded file %q not found in parent %q", name, parentID)
}

var _ drives.Remover = (*Driver)(nil)

func isGoogleUploadHTTPRateLimit(status int, header http.Header, body []byte, apiErr apiErrorBody) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	if status == http.StatusForbidden && strings.TrimSpace(header.Get("Retry-After")) != "" {
		return true
	}
	if isGoogleRateLimit(nil, apiErr) {
		return true
	}
	return false
}

func googleUploadRateLimitError(status int, header http.Header, body []byte, message string) error {
	if strings.TrimSpace(message) == "" {
		message = "google drive upload rate limited"
	}
	bodyText := strings.TrimSpace(string(body))
	if bodyText != "" {
		message = fmt.Sprintf("%s: status=%d body=%s", message, status, bodyText)
	}
	return &drives.RateLimitError{
		Provider:   Kind,
		RetryAfter: parseRetryAfterHeader(header.Get("Retry-After")),
		Err:        errors.New(message),
	}
}

type tokenSnapshot struct {
	access     string
	refresh    string
	generation uint64
}

func (d *Driver) tokenSnapshot() tokenSnapshot {
	d.tokenMu.RLock()
	defer d.tokenMu.RUnlock()
	return tokenSnapshot{
		access:     d.accessToken,
		refresh:    d.refreshToken,
		generation: d.tokenGeneration,
	}
}

func (d *Driver) refresh(ctx context.Context, rejected ...tokenSnapshot) error {
	if d.clientID == "" || d.clientSecret == "" {
		return errors.New("googledrive refresh token: client_id and client_secret are required")
	}
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()

	current := d.tokenSnapshot()
	if len(rejected) > 0 &&
		(current.generation != rejected[0].generation || current.access != rejected[0].access) {
		return nil
	}
	if current.refresh == "" {
		return errors.New("googledrive refresh token: refresh_token is required")
	}

	var out tokenResp
	res, err := d.client.R().
		SetContext(ctx).
		SetFormData(map[string]string{
			"client_id":     d.clientID,
			"client_secret": d.clientSecret,
			"refresh_token": current.refresh,
			"grant_type":    "refresh_token",
		}).
		SetResult(&out).
		SetError(&out).
		Post(d.oauthURL)
	if err != nil {
		return fmt.Errorf("googledrive refresh token: %w", err)
	}
	if err := tokenResponseError("googledrive refresh token", res, out); err != nil {
		return err
	}
	d.applyToken(out, current.refresh)
	return nil
}

func (d *Driver) applyToken(out tokenResp, previousRefresh string) {
	accessToken := strings.TrimSpace(out.AccessToken)
	refreshToken := strings.TrimSpace(out.RefreshToken)
	if refreshToken == "" {
		refreshToken = previousRefresh
	}
	d.tokenMu.Lock()
	d.accessToken = accessToken
	d.refreshToken = refreshToken
	d.tokenGeneration++
	d.tokenMu.Unlock()
	if d.onTokenUpdate != nil {
		d.onTokenUpdate(accessToken, refreshToken)
	}
}

func tokenResponseError(prefix string, res *resty.Response, out tokenResp) error {
	if isGoogleTokenRateLimit(res, out) {
		message := strings.TrimSpace(out.Text)
		if message == "" {
			message = strings.TrimSpace(out.ErrorDescription)
		}
		if message == "" {
			message = strings.TrimSpace(out.Error)
		}
		if message == "" {
			message = "google drive token refresh rate limited"
		}
		if res != nil && strings.TrimSpace(res.String()) != "" {
			message = fmt.Sprintf("%s: status=%d body=%s", message, res.StatusCode(), strings.TrimSpace(res.String()))
		}
		return &drives.RateLimitError{
			Provider:   Kind,
			RetryAfter: parseRetryAfter(res),
			Err:        fmt.Errorf("%s: %s", prefix, message),
		}
	}
	if out.Text != "" {
		return fmt.Errorf("%s: %s", prefix, out.Text)
	}
	if out.Error != "" {
		if out.ErrorDescription != "" {
			return fmt.Errorf("%s: %s", prefix, out.ErrorDescription)
		}
		return fmt.Errorf("%s: %s", prefix, out.Error)
	}
	if res != nil && res.IsError() {
		return fmt.Errorf("%s: status=%d body=%s", prefix, res.StatusCode(), strings.TrimSpace(res.String()))
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return fmt.Errorf("%s: empty token", prefix)
	}
	return nil
}

func (d *Driver) request(ctx context.Context, rawURL, method string, configure func(*resty.Request), out any) error {
	return d.requestOnce(ctx, rawURL, method, configure, out, true)
}

func (d *Driver) requestOnce(ctx context.Context, rawURL, method string, configure func(*resty.Request), out any, retry bool) error {
	tokens := d.tokenSnapshot()
	req := d.client.R().
		SetContext(ctx).
		SetHeader("Authorization", "Bearer "+tokens.access).
		SetQueryParam("includeItemsFromAllDrives", "true").
		SetQueryParam("supportsAllDrives", "true")
	if configure != nil {
		configure(req)
	}
	if out != nil {
		req.SetResult(out)
	}
	var apiErr apiErrorResp
	req.SetError(&apiErr)
	res, err := req.Execute(method, rawURL)
	if err != nil {
		return err
	}
	if isGoogleRateLimit(res, apiErr.Error) {
		return googleRateLimitError(res, apiErr.Error.Message)
	}
	if apiErr.Error.Code != 0 {
		if apiErr.Error.Code == http.StatusUnauthorized && retry {
			if err := d.refresh(ctx, tokens); err != nil {
				return err
			}
			return d.requestOnce(ctx, rawURL, method, configure, out, false)
		}
		return googleAPIError(apiErr.Error)
	}
	if res.IsError() {
		return fmt.Errorf("google drive api error: status=%d body=%s", res.StatusCode(), strings.TrimSpace(res.String()))
	}
	return nil
}

func (d *Driver) fillShortcutFileMetadata(ctx context.Context, files []driveFile) error {
	for i := range files {
		f := &files[i]
		if f.MimeType != "application/vnd.google-apps.shortcut" ||
			f.Shortcut.TargetID == "" ||
			f.Shortcut.TargetMimeType == "application/vnd.google-apps.folder" {
			continue
		}
		var target driveFile
		if err := d.request(ctx, d.fileURL(f.Shortcut.TargetID), http.MethodGet, func(req *resty.Request) {
			req.SetQueryParam("fields", fileInfoFields)
		}, &target); err != nil {
			return err
		}
		if target.Size != "" {
			f.Size = target.Size
		}
		if target.MD5Checksum != "" {
			f.MD5Checksum = target.MD5Checksum
		}
		if target.SHA1Checksum != "" {
			f.SHA1Checksum = target.SHA1Checksum
		}
		if target.SHA256Checksum != "" {
			f.SHA256Checksum = target.SHA256Checksum
		}
	}
	return nil
}

func (d *Driver) filesURL() string {
	return d.apiBaseURL + "/files"
}

func (d *Driver) uploadFilesURL() string {
	return d.uploadBaseURL + "/files"
}

func (d *Driver) fileURL(fileID string) string {
	return d.filesURL() + "/" + url.PathEscape(fileID)
}

func fileToEntry(f driveFile, fallbackParentID string) drives.Entry {
	id := f.ID
	isDir := f.MimeType == "application/vnd.google-apps.folder"
	if f.MimeType == "application/vnd.google-apps.shortcut" && f.Shortcut.TargetID != "" {
		id = f.Shortcut.TargetID
		isDir = f.Shortcut.TargetMimeType == "application/vnd.google-apps.folder"
	}
	size, _ := strconv.ParseInt(f.Size, 10, 64)
	hash := f.MD5Checksum
	if hash == "" {
		hash = f.SHA1Checksum
	}
	if hash == "" {
		hash = f.SHA256Checksum
	}
	return drives.Entry{
		ID:           id,
		Name:         f.Name,
		Size:         size,
		Hash:         hash,
		IsDir:        isDir,
		ParentID:     fallbackParentID,
		MimeType:     mimeType(f),
		ModTime:      f.ModifiedTime,
		ThumbnailURL: f.ThumbnailLink,
	}
}

func mimeType(f driveFile) string {
	if f.MimeType != "" && f.MimeType != "application/vnd.google-apps.shortcut" {
		return f.MimeType
	}
	if f.Shortcut.TargetMimeType != "" {
		return f.Shortcut.TargetMimeType
	}
	ext := strings.ToLower(path.Ext(f.Name))
	switch ext {
	case ".mp4":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".mov":
		return "video/quicktime"
	case ".webm":
		return "video/webm"
	case ".avi":
		return "video/x-msvideo"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

func isGoogleRateLimit(res *resty.Response, body apiErrorBody) bool {
	if res != nil && res.StatusCode() == http.StatusTooManyRequests {
		return true
	}
	if res != nil && res.StatusCode() == http.StatusForbidden && strings.TrimSpace(res.Header().Get("Retry-After")) != "" {
		return true
	}
	if body.Code == http.StatusTooManyRequests {
		return true
	}
	for _, e := range body.Errors {
		if googleLimitReason(e.Reason) {
			return true
		}
		domain := compactGoogleLimitText(e.Domain)
		if domain == "usagelimits" && (body.Code == http.StatusForbidden || body.Code == http.StatusTooManyRequests) {
			return true
		}
	}
	return false
}

func isGoogleTokenRateLimit(res *resty.Response, out tokenResp) bool {
	if res != nil {
		if res.StatusCode() == http.StatusTooManyRequests {
			return true
		}
		if res.StatusCode() == http.StatusForbidden && strings.TrimSpace(res.Header().Get("Retry-After")) != "" {
			return true
		}
	}
	return googleLimitReason(out.Error)
}

func googleLimitReason(reason string) bool {
	switch compactGoogleLimitText(reason) {
	case "ratelimitexceeded",
		"userratelimitexceeded",
		"dailylimitexceeded",
		"dailylimitexceededunreg",
		"downloadquotaexceeded",
		"sharingratelimitexceeded",
		"quotaexceeded",
		"uploadlimitexceeded",
		"storagelimitexceeded",
		"storagequotaexceeded":
		return true
	default:
		return false
	}
}

func compactGoogleLimitText(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	replacer := strings.NewReplacer("_", "", "-", "", " ", "", ".", "", ":", "")
	return replacer.Replace(text)
}

func googleRateLimitError(res *resty.Response, message string) error {
	if strings.TrimSpace(message) == "" {
		message = "google drive rate limited"
	}
	if res != nil && strings.TrimSpace(res.String()) != "" {
		message = fmt.Sprintf("%s: status=%d body=%s", message, res.StatusCode(), strings.TrimSpace(res.String()))
	}
	return &drives.RateLimitError{
		Provider:   Kind,
		RetryAfter: parseRetryAfter(res),
		Err:        errors.New(message),
	}
}

func googleAPIError(body apiErrorBody) error {
	if body.Message != "" {
		return errors.New(body.Message)
	}
	if body.Code != 0 {
		return fmt.Errorf("google drive api error: code=%d", body.Code)
	}
	return errors.New("google drive api error")
}

func parseRetryAfter(res *resty.Response) time.Duration {
	if res == nil {
		return 0
	}
	return parseRetryAfterHeader(res.Header().Get("Retry-After"))
}

func parseRetryAfterHeader(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		d := time.Until(when)
		if d > 0 {
			return d
		}
	}
	return 0
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

var _ drives.Drive = (*Driver)(nil)
