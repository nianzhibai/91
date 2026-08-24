package wopan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	sdk "github.com/OpenListTeam/wopan-sdk-go"
	"github.com/go-resty/resty/v2"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/scopedproxy"
)

// Driver 封装联通网盘
type Driver struct {
	id            string
	rootID        string
	familyID      string
	accessToken   string
	refreshToken  string
	client        *sdk.WoClient
	onTokenUpdate func(access, refresh string)
	uploadTempDir string

	// The upstream SDK stores encryption and rotating-token state on WoClient
	// without synchronization. Serialize SDK calls at the adapter boundary so a
	// refresh cannot race another request or consume the same refresh token twice.
	clientMu sync.Mutex

	listMu       sync.Mutex
	lastListAt   time.Time
	listInterval time.Duration
	listCooldown time.Duration

	fileIDMu sync.RWMutex
	fidToID  map[string]string
}

type Config struct {
	ID            string
	AccessToken   string
	RefreshToken  string
	FamilyID      string // 空则走个人空间，有值则走家庭空间
	RootID        string // 根目录 ID，默认 "0"
	UploadTempDir string
	// 当 SDK 刷新 token 时回调，便于持久化
	OnTokenUpdate func(access, refresh string)
}

func New(c Config) *Driver {
	rootID := c.RootID
	if rootID == "" {
		rootID = "0"
	}
	return &Driver{
		id:            c.ID,
		rootID:        rootID,
		familyID:      c.FamilyID,
		accessToken:   c.AccessToken,
		refreshToken:  c.RefreshToken,
		onTokenUpdate: c.OnTokenUpdate,
		uploadTempDir: strings.TrimSpace(c.UploadTempDir),
		listInterval:  800 * time.Millisecond,
		listCooldown:  5 * time.Minute,
		fidToID:       make(map[string]string),
	}
}

func (d *Driver) Kind() string { return "wopan" }
func (d *Driver) ID() string   { return d.id }
func (d *Driver) RootID() string {
	return d.rootID
}

func (d *Driver) Init(ctx context.Context) error {
	d.client = sdk.New(
		sdk.WithUA(sdk.DefaultUA),
		sdk.WithClient(scopedproxy.NewHTTPClient(0)),
		sdk.WithRefreshToken(d.refreshToken),
	)
	d.client.SetAccessToken(d.accessToken)
	d.client.OnRefreshToken(func(access, refresh string) {
		d.accessToken = access
		d.refreshToken = refresh
		if d.onTokenUpdate != nil {
			d.onTokenUpdate(access, refresh)
		}
	})
	// InitData 会触发一次 token 校验
	d.clientMu.Lock()
	defer d.clientMu.Unlock()
	return d.client.InitData()
}

func (d *Driver) spaceType() string {
	if d.familyID != "" {
		return sdk.SpaceTypeFamily
	}
	return sdk.SpaceTypePersonal
}

func (d *Driver) List(ctx context.Context, dirID string) ([]drives.Entry, error) {
	d.listMu.Lock()
	defer d.listMu.Unlock()

	var result []drives.Entry
	pageNum := 0
	pageSize := 100
	for {
		var data *sdk.QueryAllFilesData
		for attempt := 0; ; attempt++ {
			if err := d.waitForListSlotLocked(ctx); err != nil {
				return nil, err
			}
			var err error
			d.clientMu.Lock()
			data, err = d.client.QueryAllFiles(d.spaceType(), dirID, pageNum, pageSize, 0, d.familyID, func(req *resty.Request) {
				req.SetContext(ctx)
			})
			d.clientMu.Unlock()
			if err == nil {
				break
			}
			err = wopanRequestError("list", err)
			wait, ok := drives.RateLimitRetryAfter(err)
			if !ok {
				return nil, err
			}
			if wait <= 0 {
				wait = d.listCooldown
			}
			log.Printf("[wopan] list cooling down drive=%s dir=%s page=%d cooldown=%s attempt=%d err=%v",
				d.id, dirID, pageNum, wait, attempt+1, err)
			if err := sleepContext(ctx, wait); err != nil {
				return nil, err
			}
		}
		for _, f := range data.Files {
			d.rememberFileID(f)
			result = append(result, fileToEntry(f, dirID))
		}
		if len(data.Files) < pageSize {
			break
		}
		pageNum++
	}
	return result, nil
}

func (d *Driver) Stat(ctx context.Context, fileID string) (*drives.Entry, error) {
	// 沃盘 SDK 没有单文件查询，退化为遍历父目录 —— 这里第一版只在 scanner 路径使用 List，Stat 保留 stub
	return nil, drives.ErrNotSupported
}

func (d *Driver) StreamURL(ctx context.Context, fileID string) (*drives.StreamLink, error) {
	d.clientMu.Lock()
	data, err := d.client.GetDownloadUrlV2([]string{fileID}, func(req *resty.Request) {
		req.SetContext(ctx)
	})
	d.clientMu.Unlock()
	if err != nil {
		return nil, wopanRequestError("download url", err)
	}
	if len(data.List) == 0 {
		return nil, fmt.Errorf("wopan download url: empty response")
	}
	return &drives.StreamLink{
		URL:                data.List[0].DownloadUrl,
		Headers:            http.Header{},
		Expires:            time.Now().Add(10 * time.Minute),
		ClientRedirectSafe: true,
	}, nil
}

func (d *Driver) Upload(ctx context.Context, parentID, name string, r io.Reader, size int64) (string, error) {
	// wopan SDK 要求 *os.File，先把流落到临时文件再上传
	if d.uploadTempDir != "" {
		if err := os.MkdirAll(d.uploadTempDir, 0o755); err != nil {
			return "", fmt.Errorf("wopan upload: create tmp dir: %w", err)
		}
	}
	tmp, err := os.CreateTemp(d.uploadTempDir, "wopan-upload-*.tmp")
	if err != nil {
		return "", err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	if _, err := io.Copy(tmp, r); err != nil {
		return "", err
	}
	if _, err := tmp.Seek(0, 0); err != nil {
		return "", err
	}
	d.clientMu.Lock()
	fid, err := d.client.Upload2C(d.spaceType(), sdk.Upload2CFile{
		Name:        name,
		Size:        size,
		Content:     tmp,
		ContentType: "application/octet-stream",
	}, parentID, d.familyID, sdk.Upload2COption{Ctx: ctx})
	d.clientMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("wopan upload: %w", err)
	}
	if fid != "" {
		if objectID, err := d.findDeleteFileIDInParent(ctx, parentID, drives.SourceFile{
			FileID: fid,
			Name:   name,
			Size:   size,
		}); err == nil {
			d.rememberFIDMapping(fid, objectID)
		} else {
			log.Printf("[wopan] upload drive=%s parent=%s fid=%s resolve object id: %v", d.id, parentID, fid, err)
		}
	}
	return fid, nil
}

func (d *Driver) Rename(ctx context.Context, fileID, newName string) error {
	if d.client == nil {
		return fmt.Errorf("wopan rename: driver not initialized")
	}
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return fmt.Errorf("wopan rename: empty file id")
	}
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return fmt.Errorf("wopan rename: empty new name")
	}
	renameID := fileID
	if cached := d.cachedDeleteFileID(fileID); cached != "" {
		renameID = cached
	}
	d.clientMu.Lock()
	err := d.client.RenameFileOrDirectory(d.spaceType(), 1, renameID, newName, d.familyID, func(req *resty.Request) {
		req.SetContext(ctx)
	})
	d.clientMu.Unlock()
	if err != nil {
		return wopanRequestError("rename", err)
	}
	return nil
}

func (d *Driver) Remove(ctx context.Context, fileID string) error {
	if d.client == nil {
		return fmt.Errorf("wopan remove: driver not initialized")
	}
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return fmt.Errorf("wopan remove: empty file id")
	}
	deleteID := fileID
	if cached := d.cachedDeleteFileID(fileID); cached != "" {
		deleteID = cached
	}
	if err := d.deleteFileByObjectID(ctx, deleteID); err != nil {
		return fmt.Errorf("wopan remove: %w", err)
	}
	return nil
}

func (d *Driver) RemoveSource(ctx context.Context, source drives.SourceFile) error {
	if d.client == nil {
		return fmt.Errorf("wopan remove: driver not initialized")
	}
	fileID := strings.TrimSpace(source.FileID)
	if fileID == "" {
		return fmt.Errorf("wopan remove: empty file id")
	}
	deleteID, err := d.resolveDeleteFileID(ctx, source)
	if err != nil {
		return err
	}
	if err := d.deleteFileByObjectID(ctx, deleteID); err != nil {
		return fmt.Errorf("wopan remove: %w", err)
	}
	return nil
}

func (d *Driver) deleteFileByObjectID(ctx context.Context, fileID string) error {
	d.clientMu.Lock()
	err := d.client.DeleteFile(d.spaceType(), nil, []string{fileID}, func(req *resty.Request) {
		req.SetContext(ctx)
	})
	d.clientMu.Unlock()
	return err
}

func (d *Driver) resolveDeleteFileID(ctx context.Context, source drives.SourceFile) (string, error) {
	fileID := strings.TrimSpace(source.FileID)
	if fileID == "" {
		return "", fmt.Errorf("wopan remove: empty file id")
	}
	if cached := d.cachedDeleteFileID(fileID); cached != "" {
		return cached, nil
	}
	parentID := strings.TrimSpace(source.ParentID)
	if parentID == "" {
		return fileID, nil
	}
	return d.findDeleteFileIDInParent(ctx, parentID, source)
}

func (d *Driver) findDeleteFileIDInParent(ctx context.Context, parentID string, source drives.SourceFile) (string, error) {
	d.listMu.Lock()
	defer d.listMu.Unlock()

	pageNum := 0
	pageSize := 100
	for {
		var data *sdk.QueryAllFilesData
		for attempt := 0; ; attempt++ {
			if err := d.waitForListSlotLocked(ctx); err != nil {
				return "", err
			}
			var err error
			d.clientMu.Lock()
			data, err = d.client.QueryAllFiles(d.spaceType(), parentID, pageNum, pageSize, 0, d.familyID, func(req *resty.Request) {
				req.SetContext(ctx)
			})
			d.clientMu.Unlock()
			if err == nil {
				break
			}
			err = wopanRequestError("resolve delete id", err)
			wait, ok := drives.RateLimitRetryAfter(err)
			if !ok {
				return "", err
			}
			if wait <= 0 {
				wait = d.listCooldown
			}
			log.Printf("[wopan] resolve delete id cooling down drive=%s parent=%s page=%d cooldown=%s attempt=%d err=%v",
				d.id, parentID, pageNum, wait, attempt+1, err)
			if err := sleepContext(ctx, wait); err != nil {
				return "", err
			}
		}
		for _, f := range data.Files {
			d.rememberFileID(f)
			if id, ok := deleteFileIDFromWopanFile(f, source); ok {
				return id, nil
			}
		}
		if len(data.Files) < pageSize {
			break
		}
		pageNum++
	}
	return "", fmt.Errorf("wopan remove: source file %q not found under parent %q", source.FileID, parentID)
}

func (d *Driver) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	parts := splitPath(pathFromRoot)
	currentID := d.rootID
	for _, name := range parts {
		childID, err := d.findChildDir(ctx, currentID, name)
		if err != nil {
			return "", err
		}
		if childID == "" {
			d.clientMu.Lock()
			resp, err := d.client.CreateDirectory(d.spaceType(), currentID, name, d.familyID, func(req *resty.Request) {
				req.SetContext(ctx)
			})
			d.clientMu.Unlock()
			if err != nil {
				return "", wopanRequestError("mkdir "+name, err)
			}
			childID = resp.Id
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

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func fileToEntry(f *sdk.File, parentID string) drives.Entry {
	mod, _ := time.Parse("2006-01-02 15:04:05", f.CreateTime)
	name := f.Name
	isDir := f.Type == 0
	id := f.Id
	if !isDir && f.Fid != "" {
		id = f.Fid
	}
	if id == "" {
		id = f.Fid
	}
	if isDir && !strings.HasSuffix(name, "/") {
		// 不改 name，只标志
	}
	return drives.Entry{
		ID:       id,
		Name:     name,
		Size:     f.Size,
		IsDir:    isDir,
		ParentID: parentID,
		MimeType: guessMime(name),
		ModTime:  mod,
	}
}

func (d *Driver) rememberFileID(f *sdk.File) {
	if f == nil || f.Type == 0 {
		return
	}
	objectID := strings.TrimSpace(f.Id)
	fid := strings.TrimSpace(f.Fid)
	if objectID == "" {
		return
	}
	d.fileIDMu.Lock()
	if d.fidToID == nil {
		d.fidToID = make(map[string]string)
	}
	d.fidToID[objectID] = objectID
	if fid != "" {
		d.fidToID[fid] = objectID
	}
	d.fileIDMu.Unlock()
}

func (d *Driver) rememberFIDMapping(fid, objectID string) {
	fid = strings.TrimSpace(fid)
	objectID = strings.TrimSpace(objectID)
	if fid == "" || objectID == "" {
		return
	}
	d.fileIDMu.Lock()
	if d.fidToID == nil {
		d.fidToID = make(map[string]string)
	}
	d.fidToID[fid] = objectID
	d.fidToID[objectID] = objectID
	d.fileIDMu.Unlock()
}

func (d *Driver) cachedDeleteFileID(fileID string) string {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return ""
	}
	d.fileIDMu.RLock()
	defer d.fileIDMu.RUnlock()
	return strings.TrimSpace(d.fidToID[fileID])
}

func deleteFileIDFromWopanFile(f *sdk.File, source drives.SourceFile) (string, bool) {
	if f == nil || f.Type == 0 {
		return "", false
	}
	sourceID := strings.TrimSpace(source.FileID)
	if sourceID == "" {
		return "", false
	}
	objectID := strings.TrimSpace(f.Id)
	fid := strings.TrimSpace(f.Fid)
	if objectID == "" {
		return "", false
	}
	if sourceID != objectID && sourceID != fid {
		return "", false
	}
	return objectID, true
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

func wopanRequestError(step string, err error) error {
	if err == nil {
		return nil
	}
	wrapped := fmt.Errorf("wopan %s: %w", step, err)
	if isWopanRateLimitError(err) {
		return &drives.RateLimitError{
			Provider: "wopan",
			Err:      wrapped,
		}
	}
	return wrapped
}

func isWopanRateLimitError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return drives.ErrorMentionsHTTPStatus(err,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		509,
	)
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

// 确保实现接口
var _ drives.Drive = (*Driver)(nil)
var _ drives.Remover = (*Driver)(nil)
var _ drives.SourceRemover = (*Driver)(nil)
