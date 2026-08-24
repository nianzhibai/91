package onedrive

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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
	maxSmallUploadSize         = 250 * 1024 * 1024
	defaultUploadSessionChunk  = 10 * 1024 * 1024
	uploadSessionRetryAttempts = 3
	defaultRenewAPIURL         = "https://api.oplist.org/onedrive/renewapi"
	onedriveListCooldown       = 5 * time.Minute
	onedriveListInterval       = 1 * time.Second
	authModeOpenListAPI        = "openlist_api"
	authModeCustomApp          = "custom_app"
)

var (
	smallUploadThreshold = int64(maxSmallUploadSize)
	uploadSessionChunk   = int64(defaultUploadSessionChunk)
)

type Driver struct {
	id            string
	rootID        string
	region        string
	accessToken   string
	refreshToken  string
	authMode      string
	clientID      string
	clientSecret  string
	isSharePoint  bool
	siteID        string
	apiBaseURL    string
	renewAPIURL   string
	oauthURL      string
	client        *resty.Client
	onTokenUpdate func(access, refresh string)

	// tokenMu protects request snapshots while refreshMu makes refresh-token
	// rotation single-flight. OAuth providers may invalidate a refresh token as
	// soon as it is exchanged, so concurrent refreshes cannot be allowed.
	tokenMu         sync.RWMutex
	refreshMu       sync.Mutex
	tokenGeneration uint64

	listMu       sync.Mutex
	lastListAt   time.Time
	listInterval time.Duration
	listCooldown time.Duration
}

type Config struct {
	ID            string
	RootID        string
	Region        string
	AccessToken   string
	RefreshToken  string
	AuthMode      string
	ClientID      string
	ClientSecret  string
	IsSharePoint  bool
	SiteID        string
	OnTokenUpdate func(access, refresh string)

	RenewAPIURL string
	OAuthURL    string
	APIBaseURL  string
}

func New(c Config) *Driver {
	rootID := strings.TrimSpace(c.RootID)
	if rootID == "" {
		rootID = "root"
	}
	region := strings.ToLower(strings.TrimSpace(c.Region))
	if region == "" {
		region = "global"
	}
	h, ok := hostMap[region]
	if !ok {
		h = hostMap["global"]
	}
	apiBaseURL := strings.TrimRight(strings.TrimSpace(c.APIBaseURL), "/")
	if apiBaseURL == "" {
		apiBaseURL = h.api
	}
	renewAPIURL := strings.TrimSpace(c.RenewAPIURL)
	if renewAPIURL == "" {
		renewAPIURL = defaultRenewAPIURL
	}
	oauthURL := strings.TrimSpace(c.OAuthURL)
	if oauthURL == "" {
		oauthURL = strings.TrimRight(h.oauth, "/") + "/common/oauth2/v2.0/token"
	}
	clientID := strings.TrimSpace(c.ClientID)
	clientSecret := strings.TrimSpace(c.ClientSecret)
	authMode := strings.ToLower(strings.TrimSpace(c.AuthMode))
	if authMode == "" {
		if clientID != "" || clientSecret != "" {
			authMode = authModeCustomApp
		} else {
			authMode = authModeOpenListAPI
		}
	}
	return &Driver{
		id:              c.ID,
		rootID:          rootID,
		region:          region,
		accessToken:     strings.TrimSpace(c.AccessToken),
		refreshToken:    strings.TrimSpace(c.RefreshToken),
		authMode:        authMode,
		clientID:        clientID,
		clientSecret:    clientSecret,
		isSharePoint:    c.IsSharePoint,
		siteID:          strings.TrimSpace(c.SiteID),
		apiBaseURL:      apiBaseURL,
		renewAPIURL:     renewAPIURL,
		oauthURL:        oauthURL,
		onTokenUpdate:   c.OnTokenUpdate,
		tokenGeneration: 1,
		client: resty.New().
			SetTransport(scopedproxy.NewTransport(nil)).
			SetTimeout(30*time.Second).
			SetHeader("Accept", "application/json, text/plain, */*"),
		listInterval: onedriveListInterval,
		listCooldown: onedriveListCooldown,
	}
}

func (d *Driver) Kind() string   { return "onedrive" }
func (d *Driver) ID() string     { return d.id }
func (d *Driver) RootID() string { return d.rootID }

func (d *Driver) Init(ctx context.Context) error {
	if d.tokenSnapshot().refresh == "" {
		return errors.New("onedrive init: refresh_token is required")
	}
	if d.isSharePoint && d.siteID == "" {
		return errors.New("onedrive init: site_id is required for SharePoint")
	}
	return d.refresh(ctx)
}

func (d *Driver) List(ctx context.Context, dirID string) ([]drives.Entry, error) {
	if dirID == "" {
		dirID = d.rootID
	}
	d.listMu.Lock()
	defer d.listMu.Unlock()

	nextLink := d.childrenURL(dirID)
	first := true
	out := make([]drives.Entry, 0)
	for nextLink != "" {
		if err := d.waitForListSlotLocked(ctx); err != nil {
			return nil, err
		}
		var resp filesResp
		err := d.request(ctx, nextLink, http.MethodGet, func(req *resty.Request) {
			if first {
				req.SetQueryParams(map[string]string{
					"$top":    "1000",
					"$select": "id,name,size,fileSystemInfo,content.downloadUrl,file,parentReference,folder",
				})
			}
		}, &resp)
		if err != nil {
			if wait, ok := drives.RateLimitRetryAfter(err); ok {
				if wait <= 0 {
					wait = d.listCooldown
					if wait <= 0 {
						wait = onedriveListCooldown
					}
				}
				log.Printf("[onedrive] list cooling down drive=%s dir=%s cooldown=%s err=%v", d.id, dirID, wait, err)
				if err := sleepContext(ctx, wait); err != nil {
					return nil, err
				}
				continue
			}
			return nil, fmt.Errorf("onedrive list: %w", err)
		}
		for _, item := range resp.Value {
			out = append(out, itemToEntry(item, dirID))
		}
		nextLink = resp.NextLink
		first = false
	}
	return out, nil
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
	var item graphItem
	if err := d.request(ctx, d.itemURL(fileID), http.MethodGet, nil, &item); err != nil {
		return nil, fmt.Errorf("onedrive stat: %w", err)
	}
	e := itemToEntry(item, "")
	return &e, nil
}

func (d *Driver) StreamURL(ctx context.Context, fileID string) (*drives.StreamLink, error) {
	var item graphItem
	if err := d.request(ctx, d.itemURL(fileID), http.MethodGet, nil, &item); err != nil {
		return nil, fmt.Errorf("onedrive download url: %w", err)
	}
	if item.DownloadURL == "" {
		return nil, errors.New("onedrive download url: empty")
	}
	return &drives.StreamLink{
		URL:                item.DownloadURL,
		Headers:            http.Header{},
		Expires:            time.Now().Add(10 * time.Minute),
		ClientRedirectSafe: true,
	}, nil
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
	threshold := smallUploadThreshold
	if threshold <= 0 {
		threshold = maxSmallUploadSize
	}
	if size <= threshold {
		return d.uploadSmallAndReportHash(ctx, parentID, name, r, size, threshold)
	}
	return d.uploadSessionAndReportHash(ctx, parentID, name, r, size)
}

func (d *Driver) normalizeUploadArgs(parentID, name string, r io.Reader, size int64) (string, string, error) {
	if r == nil {
		return "", "", errors.New("onedrive upload: body is required")
	}
	if size < 0 {
		return "", "", fmt.Errorf("onedrive upload: invalid size %d", size)
	}
	if parentID == "" {
		parentID = d.rootID
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", errors.New("onedrive upload: empty file name")
	}
	return parentID, name, nil
}

func (d *Driver) uploadSmallAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size, limit int64) (UploadResult, error) {
	data, hash, actualSize, err := readSmallUpload(r, size, limit)
	if err != nil {
		return UploadResult{}, err
	}
	u := fmt.Sprintf("%s/items/%s:/%s:/content", d.driveBaseURL(), url.PathEscape(parentID), url.PathEscape(name))
	var item graphItem
	err = d.request(ctx, u, http.MethodPut, func(req *resty.Request) {
		req.SetBody(bytes.NewReader(data))
		req.SetContentLength(true)
	}, &item)
	if err != nil {
		return UploadResult{}, fmt.Errorf("onedrive upload: %w", err)
	}
	if item.ID == "" {
		return UploadResult{}, errors.New("onedrive upload: empty item id")
	}
	return UploadResult{FileID: item.ID, Hash: hash, Size: actualSize}, nil
}

func (d *Driver) uploadSessionAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	session, err := d.createUploadSession(ctx, parentID, name)
	if err != nil {
		return UploadResult{}, err
	}
	if strings.TrimSpace(session.UploadURL) == "" {
		return UploadResult{}, errors.New("onedrive upload session: empty upload url")
	}

	chunkSize := uploadSessionChunk
	if chunkSize <= 0 {
		chunkSize = defaultUploadSessionChunk
	}
	buf := make([]byte, int(chunkSize))
	hasher := sha1.New()
	var finalItem graphItem
	var offset int64
	for offset < size {
		partSize := minInt64(chunkSize, size-offset)
		chunk := buf[:int(partSize)]
		n, err := io.ReadFull(r, chunk)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return UploadResult{}, fmt.Errorf("onedrive upload: size mismatch: declared %d, copied %d", size, offset+int64(n))
			}
			return UploadResult{}, fmt.Errorf("onedrive upload: read body: %w", err)
		}
		chunk = chunk[:n]
		_, _ = hasher.Write(chunk)
		item, err := d.putUploadSessionChunkWithRetry(ctx, session.UploadURL, offset, size, chunk)
		if err != nil {
			return UploadResult{}, err
		}
		if item != nil {
			finalItem = *item
		}
		offset += int64(n)
	}
	if finalItem.ID == "" {
		return UploadResult{}, errors.New("onedrive upload session: empty item id")
	}
	return UploadResult{
		FileID: finalItem.ID,
		Hash:   hex.EncodeToString(hasher.Sum(nil)),
		Size:   offset,
	}, nil
}

func (d *Driver) createUploadSession(ctx context.Context, parentID, name string) (uploadSessionResp, error) {
	u := fmt.Sprintf("%s/items/%s:/%s:/createUploadSession", d.driveBaseURL(), url.PathEscape(parentID), url.PathEscape(name))
	body := map[string]any{
		"item": map[string]any{
			"@microsoft.graph.conflictBehavior": "rename",
		},
	}
	var out uploadSessionResp
	err := d.request(ctx, u, http.MethodPost, func(req *resty.Request) {
		req.SetBody(body)
	}, &out)
	if err != nil {
		return uploadSessionResp{}, fmt.Errorf("onedrive upload session: %w", err)
	}
	return out, nil
}

func (d *Driver) putUploadSessionChunkWithRetry(ctx context.Context, uploadURL string, start, total int64, data []byte) (*graphItem, error) {
	var last error
	for attempt := 0; attempt < uploadSessionRetryAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepContext(ctx, time.Duration(attempt)*time.Second); err != nil {
				return nil, err
			}
		}
		item, retryable, err := d.putUploadSessionChunk(ctx, uploadURL, start, total, data)
		if err == nil {
			return item, nil
		}
		last = err
		if !retryable {
			return nil, err
		}
	}
	if last == nil {
		last = errors.New("onedrive upload session: retry attempts exhausted")
	}
	return nil, last
}

func (d *Driver) putUploadSessionChunk(ctx context.Context, uploadURL string, start, total int64, data []byte) (*graphItem, bool, error) {
	end := start + int64(len(data)) - 1
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(data))
	if err != nil {
		return nil, false, err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var item graphItem
		if err := json.NewDecoder(res.Body).Decode(&item); err != nil {
			return nil, false, fmt.Errorf("onedrive upload session: decode completed item: %w", err)
		}
		return &item, false, nil
	case http.StatusAccepted:
		return nil, false, nil
	default:
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		err := fmt.Errorf("onedrive upload session: status=%d body=%s", res.StatusCode, strings.TrimSpace(string(body)))
		retryable := res.StatusCode == http.StatusTooManyRequests || (res.StatusCode >= 500 && res.StatusCode <= 504)
		return nil, retryable, err
	}
}

func readSmallUpload(r io.Reader, declaredSize, limit int64) ([]byte, string, int64, error) {
	if r == nil {
		return nil, "", 0, errors.New("onedrive upload: body is required")
	}
	if limit <= 0 {
		limit = maxSmallUploadSize
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, "", 0, fmt.Errorf("onedrive upload: read body: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, "", 0, fmt.Errorf("onedrive upload: files over %d bytes require upload session", limit)
	}
	if declaredSize >= 0 && int64(len(data)) != declaredSize {
		return nil, "", 0, fmt.Errorf("onedrive upload: size mismatch: declared %d, copied %d", declaredSize, len(data))
	}
	sum := sha1.Sum(data)
	return data, hex.EncodeToString(sum[:]), int64(len(data)), nil
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
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
	body := map[string]any{
		"name":                              name,
		"folder":                            map[string]any{},
		"@microsoft.graph.conflictBehavior": "rename",
	}
	var item graphItem
	err := d.request(ctx, d.childrenURL(parentID), http.MethodPost, func(req *resty.Request) {
		req.SetBody(body)
	}, &item)
	if err != nil {
		return "", fmt.Errorf("onedrive mkdir %s: %w", name, err)
	}
	if item.ID == "" {
		return "", fmt.Errorf("onedrive mkdir %s: empty item id", name)
	}
	return item.ID, nil
}

func (d *Driver) Rename(ctx context.Context, fileID, newName string) error {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return errors.New("onedrive rename: empty file id")
	}
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return errors.New("onedrive rename: empty new name")
	}
	var item graphItem
	err := d.request(ctx, d.itemURL(fileID), http.MethodPatch, func(req *resty.Request) {
		req.SetBody(map[string]string{"name": newName})
	}, &item)
	if err != nil {
		return fmt.Errorf("onedrive rename: %w", err)
	}
	return nil
}

func (d *Driver) Remove(ctx context.Context, fileID string) error {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return errors.New("onedrive remove: empty file id")
	}
	if err := d.request(ctx, d.itemURL(fileID), http.MethodDelete, nil, nil); err != nil {
		return fmt.Errorf("onedrive remove: %w", err)
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
		SetHeader("Authorization", "Bearer "+tokens.access)
	if configure != nil {
		configure(req)
	}
	if out != nil {
		req.SetResult(out)
	}
	var graphErr graphErrorResp
	req.SetError(&graphErr)
	res, err := req.Execute(method, rawURL)
	if err != nil {
		return err
	}
	if isRateLimitResponse(res, graphErr.Error.Code, graphErr.Error.Message) {
		return onedriveRateLimitError(res, graphErr.Error.Message)
	}
	if graphErr.Error.Code != "" {
		if graphErr.Error.Code == "InvalidAuthenticationToken" && retry {
			if err := d.refresh(ctx, tokens); err != nil {
				return err
			}
			return d.requestOnce(ctx, rawURL, method, configure, out, false)
		}
		if graphErr.Error.Message != "" {
			return errors.New(graphErr.Error.Message)
		}
		return fmt.Errorf("graph api error: %s", graphErr.Error.Code)
	}
	if res.IsError() {
		return fmt.Errorf("graph api error: status=%d body=%s", res.StatusCode(), strings.TrimSpace(res.String()))
	}
	return nil
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
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()

	current := d.tokenSnapshot()
	if len(rejected) > 0 &&
		(current.generation != rejected[0].generation || current.access != rejected[0].access) {
		// Another request already advanced the token while this caller waited.
		return nil
	}
	if current.refresh == "" {
		return errors.New("onedrive refresh token: refresh_token is required")
	}

	out, err := d.exchangeRefreshToken(ctx, current.refresh)
	if err != nil {
		return err
	}
	accessToken := strings.TrimSpace(out.AccessToken)
	refreshToken := strings.TrimSpace(out.RefreshToken)
	if accessToken == "" {
		return errors.New("onedrive refresh token: empty access token")
	}
	if refreshToken == "" {
		refreshToken = current.refresh
	}
	d.tokenMu.Lock()
	d.accessToken = accessToken
	d.refreshToken = refreshToken
	d.tokenGeneration++
	d.tokenMu.Unlock()
	if d.onTokenUpdate != nil {
		d.onTokenUpdate(accessToken, refreshToken)
	}
	return nil
}

func (d *Driver) exchangeRefreshToken(ctx context.Context, refreshToken string) (tokenResp, error) {
	switch d.authMode {
	case authModeCustomApp:
		return d.exchangeCustomOAuthToken(ctx, refreshToken)
	case authModeOpenListAPI:
		return d.exchangeOpenListToken(ctx, refreshToken)
	default:
		return tokenResp{}, fmt.Errorf("onedrive refresh token: unsupported auth_mode %q", d.authMode)
	}
}

func (d *Driver) exchangeOpenListToken(ctx context.Context, refreshToken string) (tokenResp, error) {
	var out tokenResp
	res, err := d.client.R().
		SetContext(ctx).
		SetQueryParams(map[string]string{
			"refresh_ui": refreshToken,
			"server_use": "true",
			"driver_txt": "onedrive_pr",
		}).
		SetResult(&out).
		SetError(&out).
		Get(d.renewAPIURL)
	if err != nil {
		return tokenResp{}, fmt.Errorf("onedrive refresh token: %w", err)
	}
	if res.StatusCode() == http.StatusTooManyRequests {
		return tokenResp{}, onedriveRateLimitError(res, "token refresh throttled")
	}
	if out.Text != "" {
		return tokenResp{}, fmt.Errorf("onedrive refresh token: %s", out.Text)
	}
	if out.Error != "" {
		if out.Description != "" {
			return tokenResp{}, fmt.Errorf("onedrive refresh token: %s", out.Description)
		}
		return tokenResp{}, fmt.Errorf("onedrive refresh token: %s", out.Error)
	}
	if res.IsError() {
		return tokenResp{}, fmt.Errorf("onedrive refresh token: status=%d body=%s", res.StatusCode(), strings.TrimSpace(res.String()))
	}
	if strings.TrimSpace(out.AccessToken) == "" || strings.TrimSpace(out.RefreshToken) == "" {
		return tokenResp{}, errors.New("onedrive refresh token: empty token")
	}
	return out, nil
}

func (d *Driver) exchangeCustomOAuthToken(ctx context.Context, refreshToken string) (tokenResp, error) {
	if d.clientID == "" || d.clientSecret == "" {
		return tokenResp{}, errors.New("onedrive refresh token: client_id and client_secret must be provided together")
	}

	var out tokenResp
	res, err := d.client.R().
		SetContext(ctx).
		SetFormData(map[string]string{
			"grant_type":    "refresh_token",
			"client_id":     d.clientID,
			"client_secret": d.clientSecret,
			"refresh_token": refreshToken,
		}).
		SetResult(&out).
		SetError(&out).
		Post(d.oauthURL)
	if err != nil {
		return tokenResp{}, fmt.Errorf("onedrive refresh token: %w", err)
	}
	if res.StatusCode() == http.StatusTooManyRequests {
		message := strings.TrimSpace(out.Description)
		if message == "" {
			message = "token refresh throttled"
		}
		return tokenResp{}, onedriveRateLimitError(res, message)
	}
	if out.Error != "" {
		if out.Description != "" {
			return tokenResp{}, fmt.Errorf("onedrive refresh token: %s", out.Description)
		}
		return tokenResp{}, fmt.Errorf("onedrive refresh token: %s", out.Error)
	}
	if res.IsError() {
		return tokenResp{}, fmt.Errorf("onedrive refresh token: status=%d body=%s", res.StatusCode(), strings.TrimSpace(res.String()))
	}
	return out, nil
}

func isRateLimitResponse(res *resty.Response, code, _ string) bool {
	if isRateLimitCode(code) {
		return true
	}
	if res == nil {
		return false
	}
	if res.StatusCode() == http.StatusTooManyRequests {
		return true
	}
	if res.Header().Get("Retry-After") == "" {
		return false
	}
	switch res.StatusCode() {
	case http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func isRateLimitCode(code string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(code), "_", ""))
	normalized = strings.ReplaceAll(normalized, "-", "")
	switch normalized {
	case "toomanyrequests",
		"activitylimitreached",
		"throttledrequest",
		"requestthrottled",
		"resourcethrottled",
		"applicationthrottled",
		"tenantthrottled":
		return true
	default:
		return false
	}
}

func onedriveRateLimitError(res *resty.Response, message string) error {
	if strings.TrimSpace(message) == "" {
		message = "onedrive rate limited"
	}
	if res != nil && strings.TrimSpace(res.String()) != "" {
		message = fmt.Sprintf("%s: status=%d body=%s", message, res.StatusCode(), strings.TrimSpace(res.String()))
	}
	return &drives.RateLimitError{
		Provider:   "onedrive",
		RetryAfter: parseRetryAfter(res),
		Err:        errors.New(message),
	}
}

func parseRetryAfter(res *resty.Response) time.Duration {
	if res == nil {
		return 0
	}
	raw := strings.TrimSpace(res.Header().Get("Retry-After"))
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

func (d *Driver) driveBaseURL() string {
	if d.isSharePoint {
		return fmt.Sprintf("%s/v1.0/sites/%s/drive", d.apiBaseURL, url.PathEscape(d.siteID))
	}
	return d.apiBaseURL + "/v1.0/me/drive"
}

func (d *Driver) itemURL(itemID string) string {
	if itemID == "" {
		itemID = d.rootID
	}
	return d.driveBaseURL() + "/items/" + url.PathEscape(itemID)
}

func (d *Driver) childrenURL(parentID string) string {
	return d.itemURL(parentID) + "/children"
}

func itemToEntry(item graphItem, fallbackParentID string) drives.Entry {
	parentID := item.ParentRef.ID
	if parentID == "" {
		parentID = fallbackParentID
	}
	isDir := item.Folder != nil
	mod := time.Time{}
	if item.FileSystemInfo != nil {
		mod = item.FileSystemInfo.LastModifiedDateTime
	}
	mimeType := ""
	if item.File != nil {
		mimeType = item.File.MimeType
	}
	if mimeType == "" && !isDir {
		mimeType = guessMime(item.Name)
	}
	return drives.Entry{
		ID:       item.ID,
		Name:     item.Name,
		Size:     item.Size,
		IsDir:    isDir,
		ParentID: parentID,
		MimeType: mimeType,
		ModTime:  mod,
	}
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
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

var _ drives.Drive = (*Driver)(nil)
var _ drives.Remover = (*Driver)(nil)
