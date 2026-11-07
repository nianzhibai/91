package pikpak

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/go-resty/resty/v2"

	"github.com/nianzhibai/91/backend/internal/scopedproxy"
)

// PikPak 上传协议（参考 OpenList drivers/pikpak）：
//
//   1. POST https://api-drive.mypikpak.net/drive/v1/files
//      body: {kind, name, size, hash(GCID), upload_type=UPLOAD_TYPE_RESUMABLE,
//             objProvider, parent_id, folder_type}
//
//   2. 服务端响应 newFileResponse：
//      - 命中秒传：resumable=null，file.id 就是新文件 ID（无需上传字节）
//      - 未命中：resumable.params 含 S3 兼容凭证（access_key / secret /
//        bucket / endpoint / key / security_token）
//
//   3. 用 Aliyun OSS SDK 把字节传到 PikPak 返回的临时 OSS endpoint：
//      小文件用 PutObject，大文件用有界并发 multipart
//
//   4. PikPak 服务端轮询 OSS，发现完成后把 resp.File.ID 标记为可用；
//      所以 Upload 完成后直接返回 resp.File.ID 即可（一开始就有，
//      只是文件实体未就绪）。

const (
	ossSecurityTokenHeaderName = "X-OSS-Security-Token"
	ossUserAgent               = "aliyun-sdk-android/2.9.13(Linux/Android 14/M2004j7ac;UKQ1.231108.001)"
	// 首次上传失败后最多再重试 3 次。每次重试都会重新申请 PikPak
	// upload session，以避开偶发不可解析/不可达的临时上传 endpoint。
	pikpakUploadMaxAttempts = 4
)

// newFileResponse 是 POST /drive/v1/files 的通用响应结构。
// PikPak 无论创建目录还是申请文件上传会话，都把新建对象
// 放在 file 字段中，而不是放在响应顶层。
type newFileResponse struct {
	UploadType string         `json:"upload_type"`
	Resumable  *resumableData `json:"resumable"`
	File       file           `json:"file"`
}

type resumableData struct {
	Kind     string   `json:"kind"`
	Params   s3Params `json:"params"`
	Provider string   `json:"provider"`
}

type s3Params struct {
	AccessKeyID     string    `json:"access_key_id"`
	AccessKeySecret string    `json:"access_key_secret"`
	Bucket          string    `json:"bucket"`
	Endpoint        string    `json:"endpoint"`
	Expiration      time.Time `json:"expiration"`
	Key             string    `json:"key"`
	SecurityToken   string    `json:"security_token"`
}

// UploadResult 是 UploadAndReportHash 的返回值。
// FileID 是 PikPak 分配的新文件 ID；Hash 是本次上传的 GCID（HEX 大写）；
// Size 是实际写入的字节数（与传入的 size 应一致）。
type UploadResult struct {
	FileID string
	Hash   string
	Size   int64
}

type preparedUploadBody struct {
	reader   io.ReadSeeker
	readerAt io.ReaderAt
	start    int64
	cleanup  func()
}

func (b preparedUploadBody) rewind() error {
	if b.reader == nil {
		return errors.New("pikpak upload: nil upload body")
	}
	_, err := b.reader.Seek(b.start, io.SeekStart)
	return err
}

// Upload 实现 drives.Drive 接口；只返回 fileID。
// 完整上传元数据见 UploadAndReportHash。
func (d *Driver) Upload(ctx context.Context, parentID, name string, r io.Reader, size int64) (string, error) {
	res, err := d.UploadAndReportHash(ctx, parentID, name, r, size)
	if err != nil {
		return "", err
	}
	return res.FileID, nil
}

// UploadAndReportHash 上传并返回 file ID + GCID + 实际字节数。
//
// 用于 crawler upload worker：上传完后直接把 hash 写回 catalog
// 的 content_hash 字段，避免再读一次本地文件做 hash。
//
// 参数：
//   - parentID：PikPak 目录 fileID。空字符串或 "/" 时回退到 driver 自身的 rootID。
//   - name：上传后的文件名（含扩展名）。
//   - r：字节流。会被先全量缓冲到临时文件以便算 GCID + 重试。
//   - size：流的总字节数。必须准确（PikPak API 要求 size 字段）。
//
// 实现要点：
//   - 必须先算 GCID 再申请上传会话（PikPak API 要求 hash 字段），
//     所以这里先 io.Copy 到临时文件并同步算 GCID。
//   - 命中秒传时不发任何字节。
//   - 非秒传时，小文件用 PutObject，大文件用有界并发 multipart；
//     分片失败只重试当前分片，整个 session 失败后仍由外层换新 session 重试。
func (d *Driver) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	if r == nil {
		return UploadResult{}, errors.New("pikpak upload: nil reader")
	}
	if err := validatePikPakUploadSize(size); err != nil {
		return UploadResult{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return UploadResult{}, errors.New("pikpak upload: empty file name")
	}
	parentID = strings.TrimSpace(parentID)
	if parentID == "" || parentID == "/" {
		parentID = d.rootID
	}

	// 1) 算 GCID，并准备一个可重试读取的 body。爬虫迁移传入的是
	// *os.File，可直接复用原文件，避免再占用一份视频大小的临时空间。
	body, gcidHex, actualSize, err := d.prepareUploadBody(r, size)
	if err != nil {
		return UploadResult{}, err
	}
	if body.cleanup != nil {
		defer body.cleanup()
	}

	result := UploadResult{Hash: gcidHex, Size: actualSize}
	var lastErr error
	for attempt := 1; attempt <= pikpakUploadMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return UploadResult{}, err
		}

		resp, err := d.requestUploadSession(ctx, parentID, name, actualSize, gcidHex)
		if err != nil {
			lastErr = fmt.Errorf("pikpak upload: request session: %w", err)
			if !shouldRetryPikPakUploadAttempt(lastErr, attempt) {
				return UploadResult{}, lastErr
			}
			d.logUploadRetry(name, attempt, lastErr)
			if err := pikpakSleepContext(ctx, pikpakUploadRetryDelay(attempt)); err != nil {
				return UploadResult{}, err
			}
			continue
		}

		out, err := d.completeUploadAttempt(ctx, body, parentID, name, result, resp)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !shouldRetryPikPakUploadAttempt(lastErr, attempt) {
			return UploadResult{}, lastErr
		}
		d.logUploadRetry(name, attempt, lastErr)
		if err := pikpakSleepContext(ctx, pikpakUploadRetryDelay(attempt)); err != nil {
			return UploadResult{}, err
		}
	}
	return UploadResult{}, lastErr
}

func (d *Driver) requestUploadSession(ctx context.Context, parentID, name string, size int64, gcidHex string) (newFileResponse, error) {
	var resp newFileResponse
	if err := d.request(ctx, filesURL, http.MethodPost, func(req *resty.Request) {
		req.SetBody(map[string]any{
			"kind":        "drive#file",
			"name":        name,
			"size":        size,
			"hash":        gcidHex,
			"upload_type": "UPLOAD_TYPE_RESUMABLE",
			"objProvider": map[string]any{"provider": "UPLOAD_TYPE_UNKNOWN"},
			"parent_id":   parentID,
			"folder_type": "NORMAL",
		})
	}, &resp); err != nil {
		return newFileResponse{}, err
	}
	return resp, nil
}

func (d *Driver) completeUploadAttempt(ctx context.Context, body preparedUploadBody, parentID, name string, result UploadResult, resp newFileResponse) (UploadResult, error) {
	// 命中秒传：服务端已经知道这个 hash，直接返回新文件 ID。
	if resp.Resumable == nil {
		if resp.File.ID != "" {
			result.FileID = resp.File.ID
			return result, nil
		}
		// 极少数情况下 file.id 不在响应里，回退到列父目录找名字。
		fid, err := d.findFileIDByName(ctx, parentID, name)
		if err != nil {
			return UploadResult{}, err
		}
		result.FileID = fid
		return result, nil
	}

	// 未命中秒传：把字节传到 S3 兼容存储。
	if err := d.uploadToOSS(ctx, &resp.Resumable.Params, body, result.Size); err != nil {
		return UploadResult{}, fmt.Errorf("pikpak upload: oss upload: %w", err)
	}

	// 拿到 fileID。优先走响应里的预分配 ID；为空就回查目录。
	if resp.File.ID != "" {
		result.FileID = resp.File.ID
		return result, nil
	}
	fid, err := d.findFileIDByName(ctx, parentID, name)
	if err != nil {
		return UploadResult{}, err
	}
	result.FileID = fid
	return result, nil
}

func shouldRetryPikPakUploadAttempt(err error, attempt int) bool {
	return attempt < pikpakUploadMaxAttempts && isRetryablePikPakUploadError(err)
}

func pikpakUploadRetryDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	return time.Duration(attempt) * time.Second
}

func (d *Driver) logUploadRetry(name string, attempt int, err error) {
	log.Printf("[pikpak] upload retry drive=%s name=%q next_attempt=%d/%d err=%v",
		d.id, name, attempt+1, pikpakUploadMaxAttempts, err)
}

func isRetryablePikPakUploadError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var serviceErr oss.ServiceError
	if errors.As(err, &serviceErr) {
		return serviceErr.StatusCode == http.StatusTooManyRequests || serviceErr.StatusCode >= 500
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "no such host") ||
		strings.Contains(text, "temporary failure in name resolution") ||
		strings.Contains(text, "server misbehaving") ||
		strings.Contains(text, "connection reset") ||
		strings.Contains(text, "connection refused") ||
		strings.Contains(text, "broken pipe") ||
		strings.Contains(text, "eof") ||
		strings.Contains(text, "i/o timeout") ||
		strings.Contains(text, "tls handshake timeout") ||
		strings.Contains(text, "http 429") ||
		strings.Contains(text, "http 500") ||
		strings.Contains(text, "http 502") ||
		strings.Contains(text, "http 503") ||
		strings.Contains(text, "http 504") ||
		strings.Contains(text, "http 509") ||
		strings.Contains(text, "too many requests") ||
		strings.Contains(text, "temporarily unavailable") ||
		strings.Contains(text, "service unavailable")
}

type readSeekAt interface {
	io.ReadSeeker
	io.ReaderAt
}

func (d *Driver) prepareUploadBody(r io.Reader, size int64) (preparedUploadBody, string, int64, error) {
	if rs, ok := r.(readSeekAt); ok {
		gcidHex, actualSize, start, err := hashGCIDFromReadSeeker(rs, size)
		if err != nil {
			return preparedUploadBody{}, "", 0, err
		}
		return preparedUploadBody{reader: rs, readerAt: rs, start: start, cleanup: func() {}}, gcidHex, actualSize, nil
	}

	tmp, gcidHex, actualSize, err := bufferAndHashGCID(d.uploadTempDir, r, size)
	if err != nil {
		return preparedUploadBody{}, "", 0, err
	}
	return preparedUploadBody{
		reader:   tmp,
		readerAt: tmp,
		start:    0,
		cleanup: func() {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		},
	}, gcidHex, actualSize, nil
}

func hashGCIDFromReadSeeker(r io.ReadSeeker, size int64) (string, int64, int64, error) {
	start, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return "", 0, 0, fmt.Errorf("pikpak upload: seek body: %w", err)
	}

	h := NewGCID(size)
	written, copyErr := io.Copy(h, r)
	_, seekErr := r.Seek(start, io.SeekStart)
	if copyErr != nil {
		return "", 0, start, fmt.Errorf("pikpak upload: hash body: %w", copyErr)
	}
	if seekErr != nil {
		return "", 0, start, fmt.Errorf("pikpak upload: rewind body: %w", seekErr)
	}
	if size > 0 && written != size {
		return "", 0, start, fmt.Errorf("pikpak upload: size mismatch: declared %d, copied %d", size, written)
	}
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil))), written, start, nil
}

// bufferAndHashGCID 把 r 复制到一个临时文件，同时计算 GCID。
// 返回临时文件（位置在末尾，需要调用方 Seek 回 start）、GCID hex 大写、实际写入字节数。
//
// 调用方负责 Close + Remove 临时文件。
func bufferAndHashGCID(tempDir string, r io.Reader, size int64) (*os.File, string, int64, error) {
	tempDir = strings.TrimSpace(tempDir)
	if tempDir != "" {
		if err := os.MkdirAll(tempDir, 0o755); err != nil {
			return nil, "", 0, fmt.Errorf("pikpak upload: create tmp dir: %w", err)
		}
	}
	tmp, err := os.CreateTemp(tempDir, "pikpak-upload-*.bin")
	if err != nil {
		return nil, "", 0, fmt.Errorf("pikpak upload: create tmp: %w", err)
	}

	h := NewGCID(size)
	mw := io.MultiWriter(tmp, h)
	written, err := io.Copy(mw, r)
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, "", 0, fmt.Errorf("pikpak upload: buffer body: %w", err)
	}
	if size > 0 && written != size {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, "", 0, fmt.Errorf("pikpak upload: size mismatch: declared %d, copied %d", size, written)
	}
	gcidHex := strings.ToUpper(hex.EncodeToString(h.Sum(nil)))
	return tmp, gcidHex, written, nil
}

// uploadToOSS 用 Aliyun OSS SDK 把 body 上传到 PikPak 提供的 S3 端点。
// 小文件用单次 PutObject，大文件用有界并发 multipart。
//
// 参数复用 PikPak 的临时凭证；必须带 Security Token 头部 + UserAgent，与 OpenList 一致。
func (d *Driver) uploadToOSS(ctx context.Context, p *s3Params, body preparedUploadBody, size int64) error {
	if p == nil {
		return errors.New("pikpak upload: nil s3 params")
	}
	if d.uploadToOSSFunc != nil {
		if err := body.rewind(); err != nil {
			return fmt.Errorf("rewind body: %w", err)
		}
		return d.uploadToOSSFunc(ctx, p, body.reader)
	}
	client, err := newPikPakOSSClient(p)
	if err != nil {
		return fmt.Errorf("oss client: %w", err)
	}
	bucket, err := client.Bucket(p.Bucket)
	if err != nil {
		return fmt.Errorf("oss bucket: %w", err)
	}
	// 单次和分片请求都通过 oss.WithContext 绑定取消，并用
	// readerWithCtx 在读链路上再做一层保护。
	return uploadPreparedBodyToOSS(ctx, bucket, p, body, size)
}

func newPikPakOSSClient(p *s3Params, options ...oss.ClientOption) (*oss.Client, error) {
	if p == nil {
		return nil, errors.New("pikpak upload: nil s3 params")
	}
	clientOptions := make([]oss.ClientOption, 0, len(options)+1)
	if isPikPakCNAMEEndpoint(p.Endpoint) {
		clientOptions = append(clientOptions, oss.UseCname(true))
	}
	clientOptions = append(clientOptions, oss.HTTPClient(scopedproxy.NewHTTPClient(0)))
	clientOptions = append(clientOptions, options...)
	return oss.New(p.Endpoint, p.AccessKeyID, p.AccessKeySecret, clientOptions...)
}

func isPikPakCNAMEEndpoint(endpoint string) bool {
	host := endpointHost(endpoint)
	if host == "" {
		return false
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return host != "mypikpak.com" && host != "mypikpak.net" &&
		(strings.HasSuffix(host, ".mypikpak.com") || strings.HasSuffix(host, ".mypikpak.net"))
}

func endpointHost(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		endpoint = u.Host
	} else if idx := strings.IndexByte(endpoint, '/'); idx >= 0 {
		endpoint = endpoint[:idx]
	}
	if host, _, err := net.SplitHostPort(endpoint); err == nil {
		endpoint = host
	}
	return strings.Trim(endpoint, "[]")
}

type readerWithCtx struct {
	ctx context.Context
	r   io.Reader
}

func (rc *readerWithCtx) Read(p []byte) (int, error) {
	if err := rc.ctx.Err(); err != nil {
		return 0, err
	}
	return rc.r.Read(p)
}

// findFileIDByName 列出 parentID 目录，返回名字完全匹配 name 的第一个文件的 ID。
// 用于秒传或上传后兜底取 fileID 的情况；带短暂重试以等待服务端持久化。
func (d *Driver) findFileIDByName(ctx context.Context, parentID, name string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		files, err := d.getFiles(ctx, parentID)
		if err != nil {
			lastErr = err
			continue
		}
		for _, f := range files {
			if f.Name == name && f.Kind != "drive#folder" {
				return f.ID, nil
			}
		}
		lastErr = fmt.Errorf("uploaded file %q not found in parent %q", name, parentID)
	}
	return "", lastErr
}
