package guangyapan

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"

	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/scopedproxy"
)

const (
	Kind = "guangyapan"

	defaultAccountBaseURL  = "https://account.guangyapan.com"
	defaultAPIBaseURL      = "https://api.guangyapan.com"
	defaultClientID        = "aMe-8VSlkrbQXpUR"
	defaultPageSize        = 100
	defaultAPIRateInterval = 500 * time.Millisecond
	defaultAPICooldown     = 10 * time.Minute
)

var errGuangYaFolderNotFound = errors.New("guangyapan folder not found")

type Driver struct {
	id             string
	rootID         string
	rootPath       string
	phoneNumber    string
	captchaToken   string
	sendCode       bool
	verifyCode     string
	verificationID string
	accessToken    string
	refreshToken   string
	clientID       string
	deviceID       string
	pageSize       int
	orderBy        int
	sortType       int
	accountBaseURL string
	apiBaseURL     string
	accountClient  *resty.Client
	apiClient      *resty.Client
	uploadTempDir  string

	onCredentialsUpdate func(map[string]string)

	credentialsMu   sync.RWMutex
	tokenGeneration uint64
	refreshMu       sync.Mutex
	loginMu         sync.Mutex
	ensureDirMu     sync.Mutex
	apiRateMu       sync.Mutex
	apiNext         map[string]time.Time
	apiCooldown     time.Time
	apiRateInterval time.Duration
	authRetryDelay  time.Duration
	uploadGate      chan struct{}

	// newOSSBucket is intentionally unset in production. Tests inject the
	// narrow OSS interface to exercise retries and aborts without a live bucket.
	newOSSBucket func(uploadTokenData) (guangYaOSSBucket, error)

	fileMu sync.RWMutex
	files  map[string]drives.Entry
}

type Config struct {
	ID             string
	RootID         string
	RootPath       string
	PhoneNumber    string
	CaptchaToken   string
	SendCode       bool
	VerifyCode     string
	VerificationID string
	AccessToken    string
	RefreshToken   string
	ClientID       string
	DeviceID       string
	PageSize       int
	OrderBy        int
	SortType       int
	AccountBaseURL string
	APIBaseURL     string
	UploadTempDir  string

	OnCredentialsUpdate func(map[string]string)
}

func New(c Config) *Driver {
	rootID := strings.TrimSpace(c.RootID)
	if rootID == "0" {
		rootID = ""
	}
	clientID := strings.TrimSpace(c.ClientID)
	if clientID == "" {
		clientID = defaultClientID
	}
	deviceID := normalizeDeviceID(c.DeviceID)
	if deviceID == "" {
		deviceID = randomDeviceID()
	}
	pageSize := c.PageSize
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	orderBy := c.OrderBy
	if orderBy < 0 {
		orderBy = 3
	}
	sortType := c.SortType
	if sortType != 0 && sortType != 1 {
		sortType = 1
	}
	accountBaseURL := strings.TrimRight(strings.TrimSpace(c.AccountBaseURL), "/")
	if accountBaseURL == "" {
		accountBaseURL = defaultAccountBaseURL
	}
	apiBaseURL := strings.TrimRight(strings.TrimSpace(c.APIBaseURL), "/")
	if apiBaseURL == "" {
		apiBaseURL = defaultAPIBaseURL
	}
	d := &Driver{
		id:                  strings.TrimSpace(c.ID),
		rootID:              rootID,
		rootPath:            strings.TrimSpace(c.RootPath),
		phoneNumber:         strings.TrimSpace(c.PhoneNumber),
		captchaToken:        strings.TrimSpace(c.CaptchaToken),
		sendCode:            c.SendCode,
		verifyCode:          strings.TrimSpace(c.VerifyCode),
		verificationID:      strings.TrimSpace(c.VerificationID),
		accessToken:         normalizeAccessToken(c.AccessToken),
		refreshToken:        strings.TrimSpace(c.RefreshToken),
		clientID:            clientID,
		deviceID:            deviceID,
		pageSize:            pageSize,
		orderBy:             orderBy,
		sortType:            sortType,
		accountBaseURL:      accountBaseURL,
		apiBaseURL:          apiBaseURL,
		uploadTempDir:       strings.TrimSpace(c.UploadTempDir),
		onCredentialsUpdate: c.OnCredentialsUpdate,
		apiNext:             make(map[string]time.Time),
		apiRateInterval:     defaultAPIRateInterval,
		authRetryDelay:      500 * time.Millisecond,
		uploadGate:          make(chan struct{}, 1),
		files:               make(map[string]drives.Entry),
	}
	if d.accessToken != "" {
		d.tokenGeneration = 1
	}
	d.accountClient = d.newAccountClient()
	d.apiClient = d.newAPIClient()
	return d
}

func (d *Driver) Kind() string   { return Kind }
func (d *Driver) ID() string     { return d.id }
func (d *Driver) RootID() string { return d.rootID }

func (d *Driver) Init(ctx context.Context) error {
	d.saveCredentials()

	accessToken, refreshToken := d.tokenSnapshot()
	var authErr error
	if accessToken != "" {
		authErr = d.validateToken(ctx)
		if authErr == nil {
			return d.prepareRootFolder(ctx)
		}
		if !isRejectedCredential(authErr) {
			return fmt.Errorf("guangyapan init: %w", authErr)
		}
		d.clearAccessToken(accessToken)
	}
	if refreshToken != "" {
		authErr = d.refresh(ctx)
		if authErr == nil {
			if err := d.validateToken(ctx); err != nil {
				return fmt.Errorf("guangyapan init after refresh: %w", err)
			}
			return d.prepareRootFolder(ctx)
		}
		if !isRejectedCredential(authErr) {
			return fmt.Errorf("guangyapan init: %w", authErr)
		}
	}
	if d.phoneNumber != "" && d.verifyCode != "" {
		if err := d.loginBySMSCode(ctx); err != nil {
			return err
		}
		if err := d.validateToken(ctx); err != nil {
			return err
		}
		return d.prepareRootFolder(ctx)
	}
	if d.phoneNumber != "" && d.sendCode {
		if err := d.prepareSMSCode(ctx); err != nil {
			return err
		}
		return errors.New("光鸭验证码已发送，请填写 verify_code 后再次保存")
	}
	if authErr != nil {
		return fmt.Errorf("guangyapan init: %w", authErr)
	}
	return &drives.ProviderError{Kind: drives.ProviderErrorAuth, Err: errors.New("guangyapan init: no credentials; provide access_token / refresh_token, or use QR login in admin")}
}

func (d *Driver) List(ctx context.Context, dirID string) ([]drives.Entry, error) {
	return d.list(ctx, dirID, true)
}

func (d *Driver) list(ctx context.Context, dirID string, applyDefaultRoot bool) ([]drives.Entry, error) {
	if applyDefaultRoot && strings.TrimSpace(dirID) == "" {
		dirID = d.rootID
	}
	if err := d.ensureAccessToken(ctx); err != nil {
		return nil, err
	}
	out := make([]drives.Entry, 0, d.pageSize)
	for pageNo := 0; ; pageNo++ {
		var resp listResp
		if err := d.postAPI(ctx, "/userres/v1/file/get_file_list", map[string]any{
			"parentId":  dirID,
			"page":      pageNo,
			"pageSize":  d.pageSize,
			"orderBy":   d.orderBy,
			"sortType":  d.sortType,
			"fileTypes": []int{},
		}, &resp); err != nil {
			return nil, err
		}
		for _, item := range resp.Data.List {
			entry := fileItemToEntry(item, dirID)
			out = append(out, entry)
			d.remember(entry)
		}
		if len(resp.Data.List) < d.pageSize {
			return out, nil
		}
		if resp.Data.Total > 0 && len(out) >= resp.Data.Total {
			return out, nil
		}
	}
}

func (d *Driver) Stat(ctx context.Context, fileID string) (*drives.Entry, error) {
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
		return nil, errors.New("guangyapan stream: empty file id")
	}
	if err := d.ensureAccessToken(ctx); err != nil {
		return nil, err
	}
	var resp downloadResp
	if err := d.postAPI(ctx, "/nd.bizuserres.s/v1/get_res_download_url", map[string]any{
		"fileId": fileID,
	}, &resp); err != nil {
		return nil, err
	}
	u := strings.TrimSpace(resp.Data.SignedURL)
	if u == "" {
		u = strings.TrimSpace(resp.Data.DownloadURL)
	}
	if u == "" {
		return nil, errors.New("guangyapan stream: empty download url")
	}
	return &drives.StreamLink{URL: u, Headers: http.Header{}, Expires: time.Now().Add(10 * time.Minute), ClientRedirectSafe: true}, nil
}

func (d *Driver) Upload(ctx context.Context, parentID, name string, r io.Reader, size int64) (string, error) {
	parentID = strings.TrimSpace(parentID)
	if parentID == "" {
		parentID = d.rootID
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("guangyapan upload: empty file name")
	}
	if r == nil {
		return "", errors.New("guangyapan upload: nil reader")
	}
	if err := validateGuangYaUploadSize(size); err != nil {
		return "", err
	}
	if err := d.ensureAccessToken(ctx); err != nil {
		return "", err
	}
	release, err := d.acquireUpload(ctx)
	if err != nil {
		return "", err
	}
	defer release()

	token, _, err := d.getUploadToken(ctx, parentID, name, size)
	if err != nil {
		return "", err
	}
	taskID := strings.TrimSpace(token.TaskID)
	if token.AlreadyDone {
		fileID, err := d.waitUploadTaskInfo(ctx, taskID)
		if err != nil {
			return "", err
		}
		d.remember(drives.Entry{ID: fileID, ParentID: parentID, Name: name, Size: size})
		return fileID, nil
	}
	if strings.TrimSpace(token.ObjectPath) == "" || strings.TrimSpace(token.BucketName) == "" || strings.TrimSpace(token.EndPoint) == "" || strings.TrimSpace(token.AccessKeyID) == "" || strings.TrimSpace(token.SecretAccessKey) == "" {
		return "", errors.New("guangyapan upload: incomplete upload token")
	}
	body, err := d.prepareUploadBody(ctx, r, size)
	if err != nil {
		return "", err
	}
	if body.cleanup != nil {
		defer body.cleanup()
	}

	bucket, err := d.openUploadBucket(token)
	if err != nil {
		return "", err
	}
	if size == 0 {
		if err := putEmptyGuangYaObject(ctx, bucket, token.ObjectPath); err != nil {
			return "", d.classifyUploadError(err)
		}
	} else if err := uploadGuangYaMultipart(ctx, bucket, token.ObjectPath, body, size); err != nil {
		return "", d.classifyUploadError(err)
	}
	fileID, err := d.waitUploadTaskInfo(ctx, taskID)
	if err != nil {
		return "", err
	}
	d.remember(drives.Entry{ID: fileID, ParentID: parentID, Name: name, Size: size})
	return fileID, nil
}

func (d *Driver) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	if err := d.ensureAccessToken(ctx); err != nil {
		return "", err
	}
	d.ensureDirMu.Lock()
	defer d.ensureDirMu.Unlock()

	clean := strings.Trim(strings.ReplaceAll(strings.TrimSpace(pathFromRoot), "\\", "/"), "/")
	if clean == "" {
		return d.rootID, nil
	}
	parentID := d.rootID
	for _, name := range strings.Split(clean, "/") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		childID, err := d.findChildFolderID(ctx, parentID, name)
		if err == nil {
			parentID = childID
			continue
		}
		if !errors.Is(err, errGuangYaFolderNotFound) {
			return "", err
		}
		created, err := d.createDir(ctx, parentID, name)
		if err != nil {
			return "", err
		}
		parentID = created
	}
	return parentID, nil
}

func (d *Driver) Remove(ctx context.Context, fileID string) error {
	if err := d.ensureAccessToken(ctx); err != nil {
		return err
	}
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return errors.New("guangyapan remove: empty file id")
	}
	var resp deleteResp
	if err := d.postAPI(ctx, "/nd.bizuserres.s/v1/file/delete_file", map[string]any{
		"fileIds": []string{fileID},
	}, &resp); err != nil {
		return err
	}
	if !successMessage(resp.Msg) {
		return fmt.Errorf("guangyapan remove: %s", strings.TrimSpace(resp.Msg))
	}
	if taskID := strings.TrimSpace(resp.Data.TaskID); taskID != "" {
		return d.waitTaskDone(ctx, taskID)
	}
	return nil
}

func (d *Driver) Rename(ctx context.Context, fileID, newName string) error {
	if err := d.ensureAccessToken(ctx); err != nil {
		return err
	}
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return errors.New("guangyapan rename: empty file id")
	}
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return errors.New("guangyapan rename: empty new name")
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := d.postAPI(ctx, "/nd.bizuserres.s/v1/file/rename", map[string]any{
		"fileId":  fileID,
		"newName": newName,
	}, &resp); err != nil {
		return err
	}
	if !successMessage(resp.Msg) {
		return fmt.Errorf("guangyapan rename: %s", strings.TrimSpace(resp.Msg))
	}
	return nil
}

func (d *Driver) prepareRootFolder(ctx context.Context) error {
	if d.rootPath == "" {
		return nil
	}
	rootID, err := d.resolveFolderPath(ctx, d.rootPath)
	if err != nil {
		return err
	}
	d.rootID = rootID
	return nil
}

func (d *Driver) resolveFolderPath(ctx context.Context, rootPath string) (string, error) {
	clean := strings.Trim(strings.ReplaceAll(strings.TrimSpace(rootPath), "\\", "/"), "/")
	if clean == "" {
		return "", nil
	}
	parentID := ""
	for _, name := range strings.Split(clean, "/") {
		if name == "" {
			continue
		}
		childID, err := d.findChildFolderID(ctx, parentID, name)
		if err != nil {
			return "", err
		}
		parentID = childID
	}
	return parentID, nil
}

func (d *Driver) findChildFolderID(ctx context.Context, parentID, name string) (string, error) {
	entries, err := d.list(ctx, parentID, false)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir && entry.Name == name {
			return entry.ID, nil
		}
	}
	if parentID == "" {
		return "", fmt.Errorf("%w: %q under /", errGuangYaFolderNotFound, name)
	}
	return "", fmt.Errorf("%w: %q under parent %s", errGuangYaFolderNotFound, name, parentID)
}

func (d *Driver) createDir(ctx context.Context, parentID, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("guangyapan create dir: empty name")
	}
	var resp createDirResp
	if err := d.postAPI(ctx, "/nd.bizuserres.s/v1/file/create_dir", map[string]any{
		"parentId": parentID,
		"dirName":  name,
	}, &resp); err != nil {
		return "", err
	}
	if !successMessage(resp.Msg) {
		return "", fmt.Errorf("guangyapan create dir: %s", strings.TrimSpace(resp.Msg))
	}
	id := strings.TrimSpace(resp.Data.FileID)
	if id == "" {
		return "", errors.New("guangyapan create dir: empty file id")
	}
	d.remember(drives.Entry{ID: id, ParentID: parentID, Name: name, IsDir: true})
	return id, nil
}

func (d *Driver) ensureAccessToken(ctx context.Context) error {
	accessToken, refreshToken := d.tokenSnapshot()
	if accessToken != "" {
		return nil
	}
	if refreshToken != "" {
		return d.refresh(ctx)
	}
	d.credentialsMu.RLock()
	verifyCode := d.verifyCode
	d.credentialsMu.RUnlock()
	if d.phoneNumber != "" && verifyCode != "" {
		return d.loginBySMSCode(ctx)
	}
	return errors.New("guangyapan auth: access token is empty; use QR login in admin or provide refresh_token")
}

func (d *Driver) validateToken(ctx context.Context) error {
	accessToken, _ := d.tokenSnapshot()
	if accessToken == "" {
		return errors.New("guangyapan validate token: access token is empty")
	}
	var out userMeResp
	if err := d.requestAccount(ctx, "validate token", http.MethodGet, "/v1/user/me", accessToken, nil, &out); err != nil {
		return err
	}
	if strings.TrimSpace(out.Sub) == "" {
		return &drives.ProviderError{Kind: drives.ProviderErrorOther, Err: errors.New("guangyapan validate token: invalid response: empty user sub")}
	}
	return nil
}

type guangYaRejectedToken struct {
	value      string
	generation uint64
}

func (d *Driver) refresh(ctx context.Context, rejectedToken ...guangYaRejectedToken) error {
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()

	accessToken, refreshToken, generation := d.tokenState()
	if accessToken != "" {
		// An empty rejectedToken means a concurrent caller only needed any valid
		// token. With a rejected token, skip refresh when another caller has
		// already advanced the credential generation while we waited for refreshMu
		// (even if the provider reused the same access-token string).
		if len(rejectedToken) == 0 ||
			accessToken != strings.TrimSpace(rejectedToken[0].value) ||
			generation != rejectedToken[0].generation {
			return nil
		}
	}
	if refreshToken == "" {
		return errors.New("guangyapan refresh: refresh_token is empty")
	}
	var out tokenResp
	if err := d.requestAccount(ctx, "refresh", http.MethodPost, "/v1/auth/token", "", map[string]any{
		"client_id":     d.clientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	}, &out); err != nil {
		return err
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return &drives.ProviderError{Kind: drives.ProviderErrorOther, Err: errors.New("guangyapan refresh: invalid response: empty access_token")}
	}
	newRefreshToken := strings.TrimSpace(out.RefreshToken)
	if newRefreshToken == "" {
		newRefreshToken = refreshToken
	}
	d.setTokenPair(strings.TrimSpace(out.AccessToken), newRefreshToken)
	d.saveCredentials()
	return nil
}

func (d *Driver) loginBySMSCode(ctx context.Context) error {
	d.loginMu.Lock()
	defer d.loginMu.Unlock()
	if accessToken, _ := d.tokenSnapshot(); accessToken != "" {
		return nil
	}

	d.credentialsMu.RLock()
	verificationID := strings.TrimSpace(d.verificationID)
	verifyCode := d.verifyCode
	d.credentialsMu.RUnlock()
	if verificationID == "" {
		var err error
		verificationID, err = d.requestVerificationID(ctx)
		if err != nil {
			return err
		}
	}

	var step2 verifyResp
	resp, err := d.accountClient.R().
		SetContext(ctx).
		SetBody(map[string]any{
			"verification_id":   verificationID,
			"verification_code": verifyCode,
			"client_id":         d.clientID,
		}).
		SetResult(&step2).
		Post("/v1/auth/verification/verify")
	if err != nil {
		return err
	}
	if resp.IsError() || step2.Error != "" || strings.TrimSpace(step2.VerificationToken) == "" {
		return fmt.Errorf("guangyapan verify code: %s", accountErr(step2.ErrorDesc, step2.Error, resp))
	}

	var out tokenResp
	resp, err = d.accountClient.R().
		SetContext(ctx).
		SetBody(map[string]any{
			"verification_code":  verifyCode,
			"verification_token": step2.VerificationToken,
			"username":           normalizePhoneE164(d.phoneNumber),
			"client_id":          d.clientID,
		}).
		SetResult(&out).
		Post("/v1/auth/signin")
	if err != nil {
		return err
	}
	if resp.IsError() || out.Error != "" || strings.TrimSpace(out.AccessToken) == "" {
		return fmt.Errorf("guangyapan signin: %s", accountErr(out.ErrorDesc, out.Error, resp))
	}
	d.credentialsMu.Lock()
	d.accessToken = strings.TrimSpace(out.AccessToken)
	d.refreshToken = strings.TrimSpace(out.RefreshToken)
	d.tokenGeneration++
	d.verificationID = ""
	d.verifyCode = ""
	d.sendCode = false
	d.credentialsMu.Unlock()
	d.saveCredentials()
	return nil
}

func (d *Driver) prepareSMSCode(ctx context.Context) error {
	d.credentialsMu.Lock()
	d.verificationID = ""
	d.credentialsMu.Unlock()
	if err := d.ensureCaptchaToken(ctx, false); err != nil {
		return err
	}
	id, err := d.requestVerificationID(ctx)
	if err != nil {
		return err
	}
	d.credentialsMu.Lock()
	d.verificationID = id
	d.sendCode = false
	d.credentialsMu.Unlock()
	d.saveCredentials()
	return nil
}

func (d *Driver) requestVerificationID(ctx context.Context) (string, error) {
	d.credentialsMu.RLock()
	captchaToken := d.captchaToken
	d.credentialsMu.RUnlock()
	if captchaToken != "" {
		d.accountClient.SetHeader("X-Captcha-Token", captchaToken)
	}
	var out verificationResp
	resp, err := d.accountClient.R().
		SetContext(ctx).
		SetBody(map[string]any{
			"phone_number": normalizePhoneE164(d.phoneNumber),
			"target":       "ANY",
			"client_id":    d.clientID,
		}).
		SetResult(&out).
		Post("/v1/auth/verification")
	if err != nil {
		return "", err
	}
	if resp.IsError() || out.Error != "" || strings.TrimSpace(out.VerificationID) == "" {
		if strings.Contains(out.Error, "captcha_invalid") || strings.Contains(out.ErrorDesc, "captcha_token expired") {
			if err := d.ensureCaptchaToken(ctx, true); err == nil {
				return d.requestVerificationID(ctx)
			}
		}
		return "", fmt.Errorf("guangyapan request verification: %s", accountErr(out.ErrorDesc, out.Error, resp))
	}
	return strings.TrimSpace(out.VerificationID), nil
}

func (d *Driver) ensureCaptchaToken(ctx context.Context, force bool) error {
	d.credentialsMu.RLock()
	captchaToken := d.captchaToken
	d.credentialsMu.RUnlock()
	if !force && captchaToken != "" {
		d.accountClient.SetHeader("X-Captcha-Token", captchaToken)
		return nil
	}
	var out captchaInitResp
	resp, err := d.accountClient.R().
		SetContext(ctx).
		SetBody(map[string]any{
			"client_id": d.clientID,
			"action":    "POST:/v1/auth/verification",
			"device_id": d.deviceID,
			"meta": map[string]any{
				"username":           normalizePhoneE164(d.phoneNumber),
				"phone_number":       normalizePhoneE164(d.phoneNumber),
				"VERIFICATION_PHONE": normalizePhoneE164(d.phoneNumber),
			},
		}).
		SetResult(&out).
		Post("/v1/shield/captcha/init")
	if err != nil {
		return err
	}
	if resp.IsError() || out.Error != "" || strings.TrimSpace(out.CaptchaToken) == "" {
		return fmt.Errorf("guangyapan captcha init: %s", accountErr(out.ErrorDesc, out.Error, resp))
	}
	captchaToken = strings.TrimSpace(out.CaptchaToken)
	d.credentialsMu.Lock()
	d.captchaToken = captchaToken
	d.credentialsMu.Unlock()
	d.accountClient.SetHeader("X-Captcha-Token", captchaToken)
	d.saveCredentials()
	return nil
}

func (d *Driver) postAPI(ctx context.Context, p string, body any, out any) error {
	if err := d.ensureAccessToken(ctx); err != nil {
		return err
	}
	accessToken, _, accessGeneration := d.tokenState()
	request := func(token string) (*resty.Response, error) {
		if err := d.waitAPIRate(ctx, p); err != nil {
			return nil, err
		}
		// A concurrent request may have refreshed while this call was waiting
		// for the endpoint limiter. Always send the newest available token.
		if latestToken, _, latestGeneration := d.tokenState(); latestToken != "" {
			token = latestToken
			accessToken = latestToken
			accessGeneration = latestGeneration
		}
		return d.apiClient.R().
			SetContext(ctx).
			SetHeader("Authorization", "Bearer "+token).
			SetBody(body).
			SetResult(out).
			Post(p)
	}

	resp, err := request(accessToken)
	if err != nil {
		return err
	}
	if resp.StatusCode() == http.StatusUnauthorized || resp.StatusCode() == http.StatusForbidden {
		_, refreshToken := d.tokenSnapshot()
		if refreshToken == "" {
			code, msg := guangYaPanResponseCodeMsg(resp, out)
			if guangYaPanLooksRateLimited(resp.StatusCode(), code, msg) {
				return d.guangYaPanRateLimitError(p, resp.Header().Get("Retry-After"), resp.StatusCode(), code, msg)
			}
			return fmt.Errorf("guangyapan api: status=%d body=%s", resp.StatusCode(), resp.String())
		}
		if err := d.refresh(ctx, guangYaRejectedToken{value: accessToken, generation: accessGeneration}); err != nil {
			return err
		}
		accessToken, _, accessGeneration = d.tokenState()
		resetGuangYaResponse(out)
		resp, err = request(accessToken)
		if err != nil {
			return err
		}
	}
	if resp.IsError() {
		code, msg := guangYaPanResponseCodeMsg(resp, out)
		if guangYaPanLooksRateLimited(resp.StatusCode(), code, msg) {
			return d.guangYaPanRateLimitError(p, resp.Header().Get("Retry-After"), resp.StatusCode(), code, msg)
		}
		return fmt.Errorf("guangyapan api: status=%d body=%s", resp.StatusCode(), resp.String())
	}
	code, msg := guangYaPanResponseCodeMsg(resp, out)
	if guangYaPanLooksRateLimited(resp.StatusCode(), code, msg) {
		return d.guangYaPanRateLimitError(p, resp.Header().Get("Retry-After"), resp.StatusCode(), code, msg)
	}
	return nil
}

func resetGuangYaResponse(out any) {
	rv := reflect.ValueOf(out)
	if !rv.IsValid() || rv.Kind() != reflect.Pointer || rv.IsNil() || !rv.Elem().CanSet() {
		return
	}
	rv.Elem().Set(reflect.Zero(rv.Elem().Type()))
}

// waitAPIRate serializes bursts per endpoint. A provider cooldown, learned
// from either API or OSS failures, gates every endpoint so separate workers do
// not immediately amplify an upstream throttle response.
func (d *Driver) waitAPIRate(ctx context.Context, path string) error {
	for {
		d.apiRateMu.Lock()
		now := time.Now()
		if d.apiCooldown.After(now) {
			wait := time.Until(d.apiCooldown)
			d.apiRateMu.Unlock()
			return &drives.RateLimitError{
				Provider:   Kind,
				RetryAfter: wait,
				Err:        fmt.Errorf("guangyapan provider cooldown active for %s", wait.Round(time.Second)),
			}
		}
		readyAt := d.apiNext[path]
		if !readyAt.After(now) {
			if d.apiRateInterval > 0 {
				d.apiNext[path] = now.Add(d.apiRateInterval)
			}
			d.apiRateMu.Unlock()
			return nil
		}
		d.apiRateMu.Unlock()

		timer := time.NewTimer(time.Until(readyAt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (d *Driver) setAPICooldown(wait time.Duration) time.Duration {
	if wait <= 0 {
		wait = defaultAPICooldown
	}
	until := time.Now().Add(wait)
	d.apiRateMu.Lock()
	if until.After(d.apiCooldown) {
		d.apiCooldown = until
	}
	d.apiRateMu.Unlock()
	return wait
}

func guangYaPanResponseCodeMsg(resp *resty.Response, out any) (int, string) {
	if resp != nil {
		body := resp.Body()
		if len(body) > 0 {
			var env struct {
				Code int    `json:"code"`
				Msg  string `json:"msg"`
			}
			if err := json.Unmarshal(body, &env); err == nil && (env.Code != 0 || strings.TrimSpace(env.Msg) != "") {
				return env.Code, strings.TrimSpace(env.Msg)
			}
			if resp.IsError() {
				return 0, strings.TrimSpace(resp.String())
			}
		}
	}
	if code, msg, ok := guangYaPanCodeMsgFromValue(out); ok {
		return code, msg
	}
	if resp != nil && resp.IsError() {
		return 0, strings.TrimSpace(resp.String())
	}
	return 0, ""
}

func guangYaPanCodeMsgFromValue(v any) (int, string, bool) {
	rv := reflect.ValueOf(v)
	for rv.IsValid() && rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return 0, "", false
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() || rv.Kind() != reflect.Struct {
		return 0, "", false
	}
	codeField := rv.FieldByName("Code")
	msgField := rv.FieldByName("Msg")
	if !codeField.IsValid() && !msgField.IsValid() {
		return 0, "", false
	}
	code := 0
	if codeField.IsValid() && codeField.CanInt() {
		code = int(codeField.Int())
	}
	msg := ""
	if msgField.IsValid() && msgField.Kind() == reflect.String {
		msg = strings.TrimSpace(msgField.String())
	}
	return code, msg, true
}

func guangYaPanLooksRateLimited(status int, code int, _ string) bool {
	if status == http.StatusTooManyRequests || code == http.StatusTooManyRequests {
		return true
	}
	switch status {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 509:
		return true
	}
	return false
}

func (d *Driver) guangYaPanRateLimitError(step, retryAfter string, status int, code int, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "guangyapan api rate limited"
	}
	if len(message) > 1024 {
		message = message[:1024] + "...(truncated)"
	}
	wait := d.setAPICooldown(parseRetryAfterHeader(retryAfter))
	return &drives.RateLimitError{
		Provider:   Kind,
		RetryAfter: wait,
		Err:        fmt.Errorf("guangyapan api rate limited: step=%s status=%d code=%d msg=%s", step, status, code, message),
	}
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

func (d *Driver) waitTaskDone(ctx context.Context, taskID string) error {
	const (
		maxTry   = 30
		interval = 300 * time.Millisecond
	)
	for i := 0; i < maxTry; i++ {
		var out taskStatusResp
		if err := d.postAPI(ctx, "/nd.bizuserres.s/v1/get_task_status", map[string]any{"taskId": taskID}, &out); err != nil {
			return err
		}
		if !successMessage(out.Msg) {
			return fmt.Errorf("guangyapan task status: %s", strings.TrimSpace(out.Msg))
		}
		switch out.Data.Status {
		case 2:
			return nil
		case -1, 3:
			return fmt.Errorf("guangyapan task %s failed with status=%d", taskID, out.Data.Status)
		}
		if i == maxTry-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return fmt.Errorf("guangyapan task %s timeout", taskID)
}

func (d *Driver) getUploadToken(ctx context.Context, parentID, name string, size int64) (*uploadTokenData, int, error) {
	var out uploadTokenResp
	if err := d.postAPI(ctx, "/nd.bizuserres.s/v1/get_res_center_token", map[string]any{
		"capacity": 2,
		"name":     name,
		"parentId": parentID,
		"res":      map[string]any{"fileSize": size},
	}, &out); err != nil {
		return nil, 0, err
	}
	msg := strings.TrimSpace(out.Msg)
	if out.Code != 156 && msg != "" && !successMessage(msg) && !isUploadAlreadyDone(msg) {
		return nil, out.Code, fmt.Errorf("guangyapan upload token: %s", msg)
	}
	if strings.TrimSpace(out.Data.TaskID) == "" {
		return nil, out.Code, errors.New("guangyapan upload token: empty task id")
	}
	// Instant-upload responses intentionally omit OSS credentials. Preserve the
	// task and let Upload wait for its file ID instead of rejecting the token.
	if out.Code == 156 || isUploadAlreadyDone(msg) {
		out.Data.AlreadyDone = true
	}
	if out.Data.AccessKeyID == "" {
		out.Data.AccessKeyID = out.Data.Creds.AccessKeyID
	}
	if out.Data.SecretAccessKey == "" {
		out.Data.SecretAccessKey = out.Data.Creds.SecretAccessKey
	}
	if out.Data.SessionToken == "" {
		out.Data.SessionToken = out.Data.Creds.SessionToken
	}
	if strings.TrimSpace(out.Data.EndPoint) == "" {
		out.Data.EndPoint = strings.TrimSpace(out.Data.FullEndPoint)
	}
	if strings.TrimSpace(out.Data.EndPoint) != "" && !strings.HasPrefix(out.Data.EndPoint, "http://") && !strings.HasPrefix(out.Data.EndPoint, "https://") {
		if strings.TrimSpace(out.Data.FullEndPoint) != "" {
			out.Data.EndPoint = strings.TrimSpace(out.Data.FullEndPoint)
		} else if strings.TrimSpace(out.Data.BucketName) != "" {
			host := strings.TrimSpace(out.Data.EndPoint)
			prefix := strings.TrimSpace(out.Data.BucketName) + "."
			if strings.HasPrefix(host, prefix) {
				out.Data.EndPoint = "https://" + host
			} else {
				out.Data.EndPoint = "https://" + strings.TrimSpace(out.Data.BucketName) + "." + host
			}
		} else {
			out.Data.EndPoint = "https://" + strings.TrimSpace(out.Data.EndPoint)
		}
	}
	return &out.Data, out.Code, nil
}

func (d *Driver) waitUploadTaskInfo(ctx context.Context, taskID string) (string, error) {
	const (
		maxTry   = 300
		interval = time.Second
	)
	for i := 0; i < maxTry; i++ {
		var out taskInfoResp
		if err := d.postAPI(ctx, "/nd.bizuserres.s/v1/file/get_info_by_task_id", map[string]any{"taskId": taskID}, &out); err != nil {
			return "", err
		}
		if out.Data.FileID != "" {
			return out.Data.FileID, nil
		}
		switch out.Code {
		case 0, 145, 146, 147, 155, 163:
		default:
			if strings.TrimSpace(out.Msg) != "" {
				return "", fmt.Errorf("guangyapan upload task failed: code=%d msg=%s", out.Code, strings.TrimSpace(out.Msg))
			}
		}
		if i == maxTry-1 {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
	}
	return "", fmt.Errorf("guangyapan upload task %s timeout", taskID)
}

func (d *Driver) newAccountClient() *resty.Client {
	client := resty.New().
		SetTransport(scopedproxy.NewTransport(nil)).
		SetTimeout(30*time.Second).
		SetBaseURL(d.accountBaseURL).
		SetHeader("Accept", "application/json, text/plain, */*").
		SetHeader("Content-Type", "application/json").
		SetHeader("X-Device-Model", "chrome%2F147.0.0.0").
		SetHeader("X-Device-Name", "PC-Chrome").
		SetHeader("X-Device-Sign", "wdi10."+d.deviceID+"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx").
		SetHeader("X-Net-Work-Type", "NONE").
		SetHeader("X-OS-Version", "MacIntel").
		SetHeader("X-Platform-Version", "1").
		SetHeader("X-Protocol-Version", "301").
		SetHeader("X-Provider-Name", "NONE").
		SetHeader("X-SDK-Version", "9.0.2").
		SetHeader("X-Client-Id", d.clientID).
		SetHeader("X-Client-Version", "0.0.1").
		SetHeader("X-Device-Id", d.deviceID)
	if d.captchaToken != "" {
		client.SetHeader("X-Captcha-Token", d.captchaToken)
	}
	return client
}

func (d *Driver) newAPIClient() *resty.Client {
	return resty.New().
		SetTransport(scopedproxy.NewTransport(nil)).
		SetTimeout(30*time.Second).
		SetBaseURL(d.apiBaseURL).
		SetHeader("Accept", "application/json, text/plain, */*").
		SetHeader("Content-Type", "application/json").
		SetHeader("Did", d.deviceID).
		SetHeader("Dt", "4")
}

func (d *Driver) saveCredentials() {
	if d.onCredentialsUpdate == nil {
		return
	}
	d.credentialsMu.RLock()
	values := map[string]string{
		"access_token":    d.accessToken,
		"refresh_token":   d.refreshToken,
		"captcha_token":   d.captchaToken,
		"device_id":       d.deviceID,
		"client_id":       d.clientID,
		"verification_id": d.verificationID,
		"verify_code":     d.verifyCode,
		"send_code":       strconv.FormatBool(d.sendCode),
	}
	d.credentialsMu.RUnlock()
	d.onCredentialsUpdate(values)
}

func (d *Driver) tokenSnapshot() (accessToken, refreshToken string) {
	accessToken, refreshToken, _ = d.tokenState()
	return accessToken, refreshToken
}

func (d *Driver) tokenState() (accessToken, refreshToken string, generation uint64) {
	d.credentialsMu.RLock()
	accessToken = strings.TrimSpace(d.accessToken)
	refreshToken = strings.TrimSpace(d.refreshToken)
	generation = d.tokenGeneration
	d.credentialsMu.RUnlock()
	return accessToken, refreshToken, generation
}

func (d *Driver) clearAccessToken(expected string) {
	d.credentialsMu.Lock()
	if strings.TrimSpace(d.accessToken) == strings.TrimSpace(expected) {
		d.accessToken = ""
		d.tokenGeneration++
	}
	d.credentialsMu.Unlock()
}

func (d *Driver) setTokenPair(accessToken, refreshToken string) {
	d.credentialsMu.Lock()
	d.accessToken = normalizeAccessToken(accessToken)
	d.refreshToken = strings.TrimSpace(refreshToken)
	d.tokenGeneration++
	d.credentialsMu.Unlock()
}

func (d *Driver) remember(entry drives.Entry) {
	if entry.ID == "" {
		return
	}
	d.fileMu.Lock()
	d.files[entry.ID] = entry
	d.fileMu.Unlock()
}

func fileItemToEntry(item fileItem, parentID string) drives.Entry {
	if item.ParentID != "" {
		parentID = item.ParentID
	}
	return drives.Entry{
		ID:       item.FileID,
		Name:     item.FileName,
		Size:     item.FileSize,
		Hash:     normalizeGCID(item.GCID),
		IsDir:    item.ResType == 2,
		ParentID: parentID,
		ModTime:  unixOrZero(item.UTime),
	}
}

func normalizeGCID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) != 40 {
		return ""
	}
	for _, ch := range value {
		switch {
		case ch >= '0' && ch <= '9':
		case ch >= 'a' && ch <= 'f':
		case ch >= 'A' && ch <= 'F':
		default:
			return ""
		}
	}
	return strings.ToUpper(value)
}

func successMessage(msg string) bool {
	return strings.EqualFold(strings.TrimSpace(msg), "success")
}

func isUploadAlreadyDone(msg string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(msg)), ""))
	if normalized == "" {
		return false
	}
	for _, marker := range []string{
		"上传已完成",
		"上传完成",
		"秒传成功",
		"已秒传",
		"instantuploadsuccess",
		"alreadyuploaded",
		"uploadalreadycompleted",
		"uploadcompleted",
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func accountErr(desc, short string, resp *resty.Response) string {
	msg := strings.TrimSpace(desc)
	if msg == "" {
		msg = strings.TrimSpace(short)
	}
	if msg == "" && resp != nil {
		msg = strings.TrimSpace(resp.String())
	}
	if msg == "" && resp != nil {
		msg = fmt.Sprintf("status=%d", resp.StatusCode())
	}
	if msg == "" {
		msg = "unknown error"
	}
	return msg
}

func normalizeAccessToken(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(strings.ToLower(v), "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return v
}

func normalizeCaptchaUsername(phone string) string {
	p := strings.TrimSpace(phone)
	p = strings.ReplaceAll(p, " ", "")
	p = strings.TrimPrefix(p, "+")
	b := make([]rune, 0, len(p))
	for _, ch := range p {
		if ch >= '0' && ch <= '9' {
			b = append(b, ch)
		}
	}
	digits := string(b)
	if strings.HasPrefix(digits, "86") && len(digits) > 11 {
		digits = digits[2:]
	}
	return digits
}

func normalizePhoneE164(phone string) string {
	p := strings.TrimSpace(phone)
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, " ", "")
	if strings.HasPrefix(p, "+") {
		if strings.HasPrefix(p, "+86") && len(p) > 3 {
			return "+86 " + strings.TrimPrefix(p, "+86")
		}
		return p
	}
	digits := normalizeCaptchaUsername(p)
	if len(digits) == 11 {
		return "+86 " + digits
	}
	return p
}

func normalizeDeviceID(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.ReplaceAll(v, "-", "")
	if len(v) != 32 {
		return ""
	}
	for _, ch := range v {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return ""
		}
	}
	return v
}

func randomDeviceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "0123456789abcdef0123456789abcdef"
	}
	return hex.EncodeToString(b)
}

func normalizeOSSEndpoint(endpoint, bucket string) string {
	ep := strings.TrimSpace(endpoint)
	if ep == "" {
		return ep
	}
	if !strings.HasPrefix(ep, "http://") && !strings.HasPrefix(ep, "https://") {
		ep = "https://" + ep
	}
	u, err := url.Parse(ep)
	if err != nil || u.Host == "" {
		return ep
	}
	prefix := strings.TrimSpace(bucket)
	if prefix != "" && strings.HasPrefix(u.Host, prefix+".") {
		u.Host = strings.TrimPrefix(u.Host, prefix+".")
	}
	return u.String()
}

var _ drives.Drive = (*Driver)(nil)
var _ drives.Uploader = (*Driver)(nil)
var _ drives.Remover = (*Driver)(nil)
