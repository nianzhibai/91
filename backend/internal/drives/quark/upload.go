package quark

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/scopedproxy"
)

const (
	quarkUploadPartAttempts = 3
	quarkOSSUserAgent       = "aliyun-sdk-js/6.6.1 Chrome 98.0.4758.80 on Windows 10 64-bit"
)

// UploadResult is the durable identity returned by a Quark upload. Hash is the
// SHA-1 calculated while preparing the replayable body; Size is the number of
// bytes actually validated and uploaded.
type UploadResult struct {
	FileID string
	Hash   string
	Size   int64
}

type uploadPreResp struct {
	Data struct {
		TaskID    string `json:"task_id"`
		Finish    bool   `json:"finish"`
		UploadID  string `json:"upload_id"`
		ObjKey    string `json:"obj_key"`
		UploadURL string `json:"upload_url"`
		Fid       string `json:"fid"`
		Bucket    string `json:"bucket"`
		Callback  struct {
			CallbackURL  string `json:"callbackUrl"`
			CallbackBody string `json:"callbackBody"`
		} `json:"callback"`
		FormatType string `json:"format_type"`
		Size       int64  `json:"size"`
		AuthInfo   string `json:"auth_info"`
	} `json:"data"`
	Metadata struct {
		PartSize int64 `json:"part_size"`
	} `json:"metadata"`
}

type uploadHashResp struct {
	Data struct {
		Finish bool   `json:"finish"`
		Fid    string `json:"fid"`
	} `json:"data"`
}

type uploadAuthResp struct {
	Data struct {
		AuthKey string `json:"auth_key"`
	} `json:"data"`
}

type uploadFinishResp struct {
	Data struct {
		Finish bool   `json:"finish"`
		Fid    string `json:"fid"`
	} `json:"data"`
}

type preparedUpload struct {
	readerAt io.ReaderAt
	start    int64
	size     int64
	md5Hex   string
	sha1Hex  string
	cleanup  func()
}

// Upload implements the drive write contract and returns the new Quark fid.
func (d *Driver) Upload(ctx context.Context, parentID, name string, r io.Reader, size int64) (string, error) {
	result, err := d.UploadAndReportHash(ctx, parentID, name, r, size)
	if err != nil {
		return "", err
	}
	return result.FileID, nil
}

// UploadAndReportHash implements Quark's cookie-account upload protocol:
// preflight, hash rapid-upload, signed OSS multipart upload, commit and finish.
// The source is made replayable before any remote mutation, so a failed part can
// be retried without silently uploading the wrong byte range.
func (d *Driver) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	if r == nil {
		return UploadResult{}, errors.New("quark upload: nil reader")
	}
	if size < 0 {
		return UploadResult{}, fmt.Errorf("quark upload: invalid size %d", size)
	}
	name = strings.TrimSpace(name)
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return UploadResult{}, errors.New("quark upload: invalid file name")
	}
	parentID = strings.TrimSpace(parentID)
	if parentID == "" || parentID == "/" {
		parentID = d.rootID
	}

	prepared, err := d.prepareUpload(ctx, r, size)
	if err != nil {
		return UploadResult{}, err
	}
	if prepared.cleanup != nil {
		defer prepared.cleanup()
	}
	result := UploadResult{Hash: prepared.sha1Hex, Size: prepared.size}

	pre, err := d.uploadPreflight(ctx, parentID, name, prepared.size)
	if err != nil {
		return UploadResult{}, err
	}
	if strings.TrimSpace(pre.Data.TaskID) == "" {
		return UploadResult{}, errors.New("quark upload: preflight returned empty task id")
	}
	hashResult, err := d.uploadHash(ctx, pre.Data.TaskID, prepared.md5Hex, prepared.sha1Hex)
	if err != nil {
		return UploadResult{}, err
	}
	if hashResult.Data.Finish || pre.Data.Finish {
		result.FileID = firstNonEmptyString(hashResult.Data.Fid, pre.Data.Fid)
		if result.FileID == "" {
			result.FileID, err = d.findUploadedFile(ctx, parentID, name, prepared.size)
		}
		if err != nil || result.FileID == "" {
			return UploadResult{}, fmt.Errorf("quark rapid upload: resolve file id: %w", nonNilError(err, errors.New("empty file id")))
		}
		return result, nil
	}

	if pre.Metadata.PartSize <= 0 {
		return UploadResult{}, fmt.Errorf("quark upload: invalid part size %d", pre.Metadata.PartSize)
	}
	if prepared.size == 0 {
		return UploadResult{}, errors.New("quark upload: zero-byte multipart upload is unsupported")
	}
	objectURL, err := quarkOSSObjectURL(pre)
	if err != nil {
		return UploadResult{}, err
	}
	mimeType := strings.TrimSpace(pre.Data.FormatType)
	if mimeType == "" {
		mimeType = mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	partCount := int((prepared.size + pre.Metadata.PartSize - 1) / pre.Metadata.PartSize)
	etags := make([]string, partCount)
	for partIndex := 0; partIndex < partCount; partIndex++ {
		if err := ctx.Err(); err != nil {
			return UploadResult{}, err
		}
		offset := int64(partIndex) * pre.Metadata.PartSize
		partSize := minInt64(pre.Metadata.PartSize, prepared.size-offset)
		var lastErr error
		for attempt := 1; attempt <= quarkUploadPartAttempts; attempt++ {
			etag, partErr := d.uploadPart(ctx, pre, objectURL, mimeType, prepared.readerAt, prepared.start+offset, partSize, partIndex+1)
			if partErr == nil {
				etags[partIndex] = etag
				lastErr = nil
				break
			}
			lastErr = partErr
			if attempt < quarkUploadPartAttempts {
				if err := sleepContext(ctx, time.Duration(attempt)*500*time.Millisecond); err != nil {
					return UploadResult{}, err
				}
			}
		}
		if lastErr != nil {
			return UploadResult{}, fmt.Errorf("quark upload part %d/%d: %w", partIndex+1, partCount, lastErr)
		}
	}
	if err := d.commitUpload(ctx, pre, objectURL, etags); err != nil {
		return UploadResult{}, err
	}
	finish, err := d.finishUpload(ctx, pre)
	if err != nil {
		return UploadResult{}, err
	}
	result.FileID = firstNonEmptyString(finish.Data.Fid, hashResult.Data.Fid, pre.Data.Fid)
	if result.FileID == "" {
		result.FileID, err = d.findUploadedFile(ctx, parentID, name, prepared.size)
	}
	if err != nil || result.FileID == "" {
		return UploadResult{}, fmt.Errorf("quark upload: resolve file id: %w", nonNilError(err, errors.New("empty file id")))
	}
	return result, nil
}

func (d *Driver) prepareUpload(ctx context.Context, source io.Reader, declaredSize int64) (preparedUpload, error) {
	md5Hash := md5.New()
	sha1Hash := sha1.New()

	if seekable, ok := source.(interface {
		io.Reader
		io.ReaderAt
		io.Seeker
	}); ok {
		start, err := seekable.Seek(0, io.SeekCurrent)
		if err == nil {
			actual, hashErr := copyExactWithContext(ctx, io.MultiWriter(md5Hash, sha1Hash), seekable, declaredSize)
			_, rewindErr := seekable.Seek(start, io.SeekStart)
			if hashErr != nil {
				return preparedUpload{}, fmt.Errorf("quark upload: hash source: %w", hashErr)
			}
			if rewindErr != nil {
				return preparedUpload{}, fmt.Errorf("quark upload: rewind source: %w", rewindErr)
			}
			return preparedUpload{
				readerAt: seekable,
				start:    start,
				size:     actual,
				md5Hex:   hex.EncodeToString(md5Hash.Sum(nil)),
				sha1Hex:  hex.EncodeToString(sha1Hash.Sum(nil)),
			}, nil
		}
	}

	tempDir := strings.TrimSpace(d.uploadTempDir)
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return preparedUpload{}, fmt.Errorf("quark upload: create temp dir: %w", err)
	}
	tmp, err := os.CreateTemp(tempDir, "quark-upload-*.bin")
	if err != nil {
		return preparedUpload{}, fmt.Errorf("quark upload: create temp file: %w", err)
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}
	actual, err := copyExactWithContext(ctx, io.MultiWriter(tmp, md5Hash, sha1Hash), source, declaredSize)
	if err != nil {
		cleanup()
		return preparedUpload{}, fmt.Errorf("quark upload: stage source: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return preparedUpload{}, fmt.Errorf("quark upload: sync temp file: %w", err)
	}
	return preparedUpload{
		readerAt: tmp,
		size:     actual,
		md5Hex:   hex.EncodeToString(md5Hash.Sum(nil)),
		sha1Hex:  hex.EncodeToString(sha1Hash.Sum(nil)),
		cleanup:  cleanup,
	}, nil
}

func copyExactWithContext(ctx context.Context, dst io.Writer, src io.Reader, declaredSize int64) (int64, error) {
	limited := &io.LimitedReader{R: &contextReader{ctx: ctx, r: src}, N: declaredSize + 1}
	n, err := io.Copy(dst, limited)
	if err != nil {
		return n, err
	}
	if n != declaredSize {
		return n, fmt.Errorf("size mismatch: declared=%d actual=%d", declaredSize, n)
	}
	return n, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func (d *Driver) uploadPreflight(ctx context.Context, parentID, name string, size int64) (uploadPreResp, error) {
	now := time.Now().UnixMilli()
	var out uploadPreResp
	err := d.request(ctx, "/file/upload/pre", http.MethodPost, nil, map[string]any{
		"ccp_hash_update": true,
		"dir_name":        "",
		"file_name":       name,
		"format_type":     firstNonEmptyString(mime.TypeByExtension(strings.ToLower(filepath.Ext(name))), "application/octet-stream"),
		"l_created_at":    now,
		"l_updated_at":    now,
		"pdir_fid":        parentID,
		"size":            size,
	}, &out)
	if err != nil {
		return uploadPreResp{}, fmt.Errorf("quark upload preflight: %w", err)
	}
	return out, nil
}

func (d *Driver) uploadHash(ctx context.Context, taskID, md5Hex, sha1Hex string) (uploadHashResp, error) {
	var out uploadHashResp
	err := d.request(ctx, "/file/update/hash", http.MethodPost, nil, map[string]any{
		"md5": md5Hex, "sha1": sha1Hex, "task_id": taskID,
	}, &out)
	if err != nil {
		return uploadHashResp{}, fmt.Errorf("quark upload hash: %w", err)
	}
	return out, nil
}

func (d *Driver) uploadPart(ctx context.Context, pre uploadPreResp, objectURL *url.URL, mimeType string, readerAt io.ReaderAt, offset, size int64, partNumber int) (string, error) {
	timeString := time.Now().UTC().Format(http.TimeFormat)
	authMeta := fmt.Sprintf("PUT\n\n%s\n%s\nx-oss-date:%s\nx-oss-user-agent:%s\n/%s/%s?partNumber=%d&uploadId=%s",
		mimeType, timeString, timeString, quarkOSSUserAgent, pre.Data.Bucket, pre.Data.ObjKey, partNumber, pre.Data.UploadID)
	auth, err := d.uploadAuth(ctx, pre, authMeta)
	if err != nil {
		return "", err
	}
	u := cloneURL(objectURL)
	query := u.Query()
	query.Set("partNumber", strconv.Itoa(partNumber))
	query.Set("uploadId", pre.Data.UploadID)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), io.NewSectionReader(readerAt, offset, size))
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	req.Header.Set("Authorization", auth.Data.AuthKey)
	req.Header.Set("Content-Type", mimeType)
	req.Header.Set("Referer", defaultReferer+"/")
	req.Header.Set("x-oss-date", timeString)
	req.Header.Set("x-oss-user-agent", quarkOSSUserAgent)
	res, err := d.uploadClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("oss HTTP %d: %s", res.StatusCode, readLimitedBody(res.Body))
	}
	etag := strings.TrimSpace(res.Header.Get("ETag"))
	if etag == "" {
		return "", errors.New("oss response missing ETag")
	}
	return etag, nil
}

type completeMultipartUpload struct {
	XMLName xml.Name       `xml:"CompleteMultipartUpload"`
	Parts   []completePart `xml:"Part"`
}

type completePart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

func (d *Driver) commitUpload(ctx context.Context, pre uploadPreResp, objectURL *url.URL, etags []string) error {
	parts := make([]completePart, len(etags))
	for i, etag := range etags {
		parts[i] = completePart{PartNumber: i + 1, ETag: etag}
	}
	xmlBody, err := xml.Marshal(completeMultipartUpload{Parts: parts})
	if err != nil {
		return fmt.Errorf("quark upload commit XML: %w", err)
	}
	body := append([]byte(xml.Header), xmlBody...)
	contentDigest := md5.Sum(body)
	contentMD5 := base64.StdEncoding.EncodeToString(contentDigest[:])
	callbackJSON, err := json.Marshal(pre.Data.Callback)
	if err != nil {
		return fmt.Errorf("quark upload callback: %w", err)
	}
	callback := base64.StdEncoding.EncodeToString(callbackJSON)
	timeString := time.Now().UTC().Format(http.TimeFormat)
	authMeta := fmt.Sprintf("POST\n%s\napplication/xml\n%s\nx-oss-callback:%s\nx-oss-date:%s\nx-oss-user-agent:%s\n/%s/%s?uploadId=%s",
		contentMD5, timeString, callback, timeString, quarkOSSUserAgent, pre.Data.Bucket, pre.Data.ObjKey, pre.Data.UploadID)
	auth, err := d.uploadAuth(ctx, pre, authMeta)
	if err != nil {
		return err
	}
	u := cloneURL(objectURL)
	query := u.Query()
	query.Set("uploadId", pre.Data.UploadID)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Authorization", auth.Data.AuthKey)
	req.Header.Set("Content-MD5", contentMD5)
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("Referer", defaultReferer+"/")
	req.Header.Set("x-oss-callback", callback)
	req.Header.Set("x-oss-date", timeString)
	req.Header.Set("x-oss-user-agent", quarkOSSUserAgent)
	res, err := d.uploadClient.Do(req)
	if err != nil {
		return fmt.Errorf("quark upload commit: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("quark upload commit: OSS HTTP %d: %s", res.StatusCode, readLimitedBody(res.Body))
	}
	return nil
}

func (d *Driver) uploadAuth(ctx context.Context, pre uploadPreResp, authMeta string) (uploadAuthResp, error) {
	var out uploadAuthResp
	err := d.request(ctx, "/file/upload/auth", http.MethodPost, nil, map[string]any{
		"auth_info": pre.Data.AuthInfo,
		"auth_meta": authMeta,
		"task_id":   pre.Data.TaskID,
	}, &out)
	if err != nil {
		return uploadAuthResp{}, fmt.Errorf("quark upload auth: %w", err)
	}
	if strings.TrimSpace(out.Data.AuthKey) == "" {
		return uploadAuthResp{}, errors.New("quark upload auth: empty authorization")
	}
	return out, nil
}

func (d *Driver) finishUpload(ctx context.Context, pre uploadPreResp) (uploadFinishResp, error) {
	var out uploadFinishResp
	err := d.request(ctx, "/file/upload/finish", http.MethodPost, nil, map[string]any{
		"obj_key": pre.Data.ObjKey,
		"task_id": pre.Data.TaskID,
	}, &out)
	if err != nil {
		return uploadFinishResp{}, fmt.Errorf("quark upload finish: %w", err)
	}
	return out, nil
}

func (d *Driver) findUploadedFile(ctx context.Context, parentID, name string, size int64) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		entries, err := d.List(ctx, parentID)
		if err == nil {
			for _, entry := range entries {
				if !entry.IsDir && entry.Name == name && entry.Size == size && strings.TrimSpace(entry.ID) != "" {
					return entry.ID, nil
				}
			}
			lastErr = errors.New("uploaded file is not visible yet")
		} else {
			lastErr = err
		}
		if attempt < 3 {
			if err := sleepContext(ctx, time.Duration(attempt+1)*300*time.Millisecond); err != nil {
				return "", err
			}
		}
	}
	return "", lastErr
}

func quarkOSSObjectURL(pre uploadPreResp) (*url.URL, error) {
	raw := strings.TrimSpace(pre.Data.UploadURL)
	if raw == "" || strings.TrimSpace(pre.Data.Bucket) == "" || strings.TrimSpace(pre.Data.ObjKey) == "" || strings.TrimSpace(pre.Data.UploadID) == "" {
		return nil, errors.New("quark upload: incomplete OSS session")
	}
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	} else if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("quark upload: invalid OSS URL %q", raw)
	}
	if u.User != nil || (!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) {
		return nil, errors.New("quark upload: OSS endpoint must be credential-free HTTP(S)")
	}
	// Quark currently returns an http:// endpoint in some upload sessions (the
	// OpenList driver also rewrites it). Never send signed OSS credentials over
	// plaintext: normalize both accepted provider forms to HTTPS.
	u.Scheme = "https"
	host := u.Host
	if !strings.HasPrefix(strings.ToLower(u.Hostname()), strings.ToLower(pre.Data.Bucket)+".") {
		host = pre.Data.Bucket + "." + u.Host
	}
	u.Host = host
	u.Path = "/" + strings.TrimLeft(pre.Data.ObjKey, "/")
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u, nil
}

func newQuarkUploadHTTPClient(base *http.Client) *http.Client {
	client := &http.Client{Timeout: 2 * time.Minute}
	directTransport := http.RoundTripper(nil)
	if base != nil {
		directTransport = base.Transport
		if base.Timeout > 0 {
			client.Timeout = base.Timeout
		}
	}
	client.Transport = scopedproxy.NewTransport(directTransport)
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		// Authorization is scoped to the exact signed OSS request and must
		// never be replayed to a redirected host.
		return http.ErrUseLastResponse
	}
	return client
}

func readLimitedBody(r io.Reader) string {
	body, _ := io.ReadAll(io.LimitReader(r, 1024))
	return strings.TrimSpace(string(body))
}

func cloneURL(source *url.URL) *url.URL {
	copy := *source
	return &copy
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func nonNilError(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
