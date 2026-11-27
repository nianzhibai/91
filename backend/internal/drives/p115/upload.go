package p115

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	cipher "github.com/SheltonZhu/115driver/pkg/crypto/ec115"
	sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/nianzhibai/91/backend/internal/applog"
)

const (
	p115UploadPreHashSize        int64 = 128 * 1024
	p115UploadFallbackAppVersion       = "35.6.0.3"
	p115UploadMD5Salt                  = "Qclm8MGWUv59TnrR0XPg"
	p115UploadControlTimeout           = 30 * time.Second
	p115UploadMaxAttempts              = 3
	p115UploadMaxSignChallenges        = 3
	p115UploadLookupAttempts           = 3
	p115UploadResponseLimit      int64 = 2 * 1024 * 1024
)

var errP115UploadResultUnavailable = errors.New("p115 upload result unavailable")

// UploadResult is the exact metadata returned to crawlerupload after a 115
// upload. Sha1 is uppercase because both 115 and catalog.content_hash use that
// representation.
type UploadResult struct {
	FileID string
	Sha1   string
	Size   int64
}

type p115UploadDigest struct {
	Size  int64
	PreID string
	SHA1  string
}

type p115PreparedUploadBody struct {
	reader   io.ReadSeeker
	readerAt io.ReaderAt
	start    int64
	cleanup  func()
}

func (b p115PreparedUploadBody) rewind() error {
	if b.reader == nil {
		return errors.New("p115 upload: nil prepared body")
	}
	_, err := b.reader.Seek(b.start, io.SeekStart)
	return err
}

type p115UploadAuth struct {
	userID    int64
	userKey   string
	sizeLimit int64
}

type p115AppVersionResponse struct {
	Error string `json:"error"`
	Data  struct {
		Win struct {
			Version string `json:"version_code"`
		} `json:"win"`
	} `json:"data"`
}

// Upload implements drives.Drive. The richer UploadAndReportSha1 result is
// used by crawler migration so the already-computed hash is not read again.
func (d *Driver) Upload(ctx context.Context, parentID, name string, r io.Reader, size int64) (string, error) {
	res, err := d.UploadAndReportSha1(ctx, parentID, name, r, size)
	if err != nil {
		return "", err
	}
	return res.FileID, nil
}

// UploadAndReportSha1 owns the 115 upload state machine instead of delegating
// it to the SDK's legacy RapidUploadOrByMultipart helper. That legacy helper
// always copied local files, hashed them twice, split sub-GiB files into 1000
// sequential requests, discarded the callback's file ID and could leave
// multipart goroutines running after an error.
func (d *Driver) UploadAndReportSha1(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	if d.client == nil {
		return UploadResult{}, errors.New("p115 upload: driver not initialized")
	}
	if r == nil {
		return UploadResult{}, errors.New("p115 upload: nil reader")
	}
	if err := validateP115UploadSize(size); err != nil {
		return UploadResult{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return UploadResult{}, errors.New("p115 upload: empty file name")
	}
	parentID = strings.TrimSpace(parentID)
	if parentID == "" {
		parentID = d.rootID
	}

	release, err := d.acquireUploadGate(ctx)
	if err != nil {
		return UploadResult{}, err
	}
	defer release()

	// Resolve account limits before scanning a potentially multi-gigabyte local
	// file. This fails fast for expired credentials and known oversized bodies.
	auth, err := d.p115UploadAuth(ctx)
	if err != nil {
		return UploadResult{}, err
	}
	if auth.sizeLimit > 0 && size > 0 && size > auth.sizeLimit {
		return UploadResult{}, sdk.ErrUploadTooLarge
	}
	appVersion, err := d.p115UploadAppVersionFor(ctx)
	if err != nil {
		return UploadResult{}, err
	}

	body, digest, err := d.prepareP115UploadBody(ctx, r, size)
	if err != nil {
		return UploadResult{}, err
	}
	if body.cleanup != nil {
		defer body.cleanup()
	}
	if err := validateP115UploadSize(digest.Size); err != nil {
		return UploadResult{}, err
	}

	if auth.sizeLimit > 0 && digest.Size > auth.sizeLimit {
		return UploadResult{}, sdk.ErrUploadTooLarge
	}

	var lastErr error
	for attempt := 1; attempt <= p115UploadMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return UploadResult{}, err
		}

		fileID, err := d.uploadP115Attempt(ctx, auth, appVersion, parentID, name, body, digest)
		if err == nil {
			return UploadResult{FileID: fileID, Sha1: digest.SHA1, Size: digest.Size}, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return UploadResult{}, ctxErr
		}
		lastErr = err
		if attempt >= p115UploadMaxAttempts || !isRetryableP115UploadError(err) {
			return UploadResult{}, err
		}
		applog.Warn(ctx, "upload retry", err,
			"component", "p115", "driveId", d.id, "stage", "upload", "name", name, "nextAttempt", attempt+1, "maxAttempts", p115UploadMaxAttempts)
		if err := sleepContext(ctx, time.Duration(attempt)*time.Second); err != nil {
			return UploadResult{}, err
		}
	}
	return UploadResult{}, lastErr
}

func (d *Driver) acquireUploadGate(ctx context.Context) (func(), error) {
	if d.uploadGate == nil {
		return func() {}, nil
	}
	select {
	case d.uploadGate <- struct{}{}:
		return func() { <-d.uploadGate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *Driver) uploadP115Attempt(
	ctx context.Context,
	auth p115UploadAuth,
	appVersion, parentID, name string,
	body p115PreparedUploadBody,
	digest p115UploadDigest,
) (string, error) {
	initResp, err := d.rapidUploadP115(ctx, auth, appVersion, parentID, name, body, digest)
	if err != nil {
		return "", fmt.Errorf("p115 upload init: %w", err)
	}
	matched, err := initResp.Ok()
	if err != nil {
		return "", fmt.Errorf("p115 upload init result: %w", err)
	}
	if matched {
		if pickCode := strings.TrimSpace(initResp.PickCode); pickCode != "" {
			if fileID, err := d.p115FileIDByPickCode(ctx, pickCode); err == nil && fileID != "" {
				return fileID, nil
			}
		}
		return d.findUploadedFileID(ctx, parentID, name, digest.SHA1)
	}

	callbackBody, err := d.uploadP115BodyToOSS(ctx, &initResp.UploadOSSParams, body, digest.Size)
	if err != nil {
		return "", fmt.Errorf("p115 oss upload: %w", err)
	}
	fileID, err := parseP115UploadCallback(callbackBody, digest.SHA1)
	if err == nil {
		return fileID, nil
	}
	if !errors.Is(err, errP115UploadResultUnavailable) {
		return "", err
	}

	// The object is already complete. A missing/malformed callback must not turn
	// into a blind duplicate upload; first ask 115 for the exact name+SHA1 row.
	fileID, lookupErr := d.findUploadedFileID(ctx, parentID, name, digest.SHA1)
	if lookupErr == nil {
		return fileID, nil
	}
	return "", errors.Join(err, lookupErr)
}

func parseP115UploadCallback(body []byte, expectedSHA1 string) (string, error) {
	if len(body) == 0 {
		return "", fmt.Errorf("%w: empty callback body", errP115UploadResultUnavailable)
	}
	var result sdk.UploadResult
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("%w: decode callback: %v", errP115UploadResultUnavailable, err)
	}
	if err := result.Err(string(body)); err != nil {
		return "", fmt.Errorf("p115 upload callback: %w", err)
	}
	if result.Data.Sha1 != "" && expectedSHA1 != "" && !strings.EqualFold(result.Data.Sha1, expectedSHA1) {
		return "", fmt.Errorf("p115 upload callback: SHA1 mismatch got=%s want=%s", result.Data.Sha1, expectedSHA1)
	}
	fileID := strings.TrimSpace(result.Data.FileID)
	if fileID == "" {
		return "", fmt.Errorf("%w: callback has empty file ID", errP115UploadResultUnavailable)
	}
	return fileID, nil
}

func (d *Driver) p115UploadAuth(ctx context.Context) (p115UploadAuth, error) {
	if d.client.UserID != 0 && d.client.Userkey != "" && d.client.UploadMetaInfo != nil {
		return p115UploadAuth{
			userID:    d.client.UserID,
			userKey:   d.client.Userkey,
			sizeLimit: d.client.UploadMetaInfo.SizeLimit,
		}, nil
	}

	var result sdk.UploadInfoResp
	requestCtx, cancel := context.WithTimeout(ctx, p115UploadControlTimeout)
	defer cancel()
	resp, err := d.client.Client.R().
		SetContext(requestCtx).
		SetResult(&result).
		ForceContentType("application/json;charset=UTF-8").
		Post(sdk.ApiUploadInfo)
	if err = sdk.CheckErr(err, &result, resp); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return p115UploadAuth{}, ctxErr
		}
		return p115UploadAuth{}, fmt.Errorf("p115 upload info: %w", err)
	}
	if result.UserID == 0 || result.Userkey == "" {
		return p115UploadAuth{}, errors.New("p115 upload info: empty user credentials")
	}
	d.client.UserID = result.UserID
	d.client.Userkey = result.Userkey
	d.client.UploadMetaInfo = &result.UploadMetaInfo
	return p115UploadAuth{userID: result.UserID, userKey: result.Userkey, sizeLimit: result.SizeLimit}, nil
}

func (d *Driver) p115UploadAppVersionFor(ctx context.Context) (string, error) {
	if d.uploadAppVersionResolved {
		return d.uploadAppVersion, nil
	}
	version := p115UploadFallbackAppVersion
	var result p115AppVersionResponse
	requestCtx, cancel := context.WithTimeout(ctx, p115UploadControlTimeout)
	// The version endpoint returns JSON with a text/html content type.
	resp, err := d.client.Client.R().
		SetContext(requestCtx).
		SetResult(&result).
		ForceContentType("application/json").
		Get(sdk.ApiGetVersion)
	cancel()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	if err == nil && resp != nil && !resp.IsError() && strings.TrimSpace(result.Data.Win.Version) != "" {
		version = strings.TrimSpace(result.Data.Win.Version)
	} else {
		if err == nil && resp != nil && resp.IsError() {
			err = fmt.Errorf("HTTP %d", resp.StatusCode())
		}
		if err == nil && strings.TrimSpace(result.Error) != "" {
			err = errors.New(strings.TrimSpace(result.Error))
		}
		if err == nil {
			err = errors.New("empty version response")
		}
		applog.Warn(ctx, "upload app version lookup failed; using fallback", err,
			"component", "p115", "driveId", d.id, "stage", "upload", "fallback", version)
	}
	d.uploadAppVersion = version
	d.uploadAppVersionResolved = true
	return version, nil
}

func (d *Driver) rapidUploadP115(
	ctx context.Context,
	auth p115UploadAuth,
	appVersion, parentID, name string,
	body p115PreparedUploadBody,
	digest p115UploadDigest,
) (*sdk.UploadInitResp, error) {
	ecdhCipher, err := cipher.NewEcdhCipher()
	if err != nil {
		return nil, err
	}
	target := "U_1_" + parentID
	fileSize := strconv.FormatInt(digest.Size, 10)
	userID := strconv.FormatInt(auth.userID, 10)
	form := url.Values{}
	form.Set("appid", "0")
	form.Set("appversion", appVersion)
	form.Set("userid", userID)
	form.Set("filename", name)
	form.Set("filesize", fileSize)
	form.Set("fileid", digest.SHA1)
	form.Set("target", target)
	form.Set("sig", generateP115UploadSignature(auth, digest.SHA1, target))
	form.Set("topupload", "true")

	signKey, signValue := "", ""
	for challenge := 0; challenge <= p115UploadMaxSignChallenges; challenge++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		timestamp := sdk.NowMilli()
		encodedToken, err := ecdhCipher.EncodeToken(timestamp.ToInt64())
		if err != nil {
			return nil, err
		}
		form.Set("t", timestamp.String())
		form.Set("token", generateP115UploadToken(auth.userID, digest.SHA1, digest.PreID, timestamp.String(), fileSize, signKey, signValue, appVersion))
		if signKey != "" && signValue != "" {
			form.Set("sign_key", signKey)
			form.Set("sign_val", signValue)
		}
		encrypted, err := ecdhCipher.Encrypt([]byte(form.Encode()))
		if err != nil {
			return nil, err
		}

		bodyBytes, err := d.doP115RapidUploadRequest(ctx, encodedToken, appVersion, encrypted)
		if err != nil {
			return nil, err
		}
		decrypted, err := ecdhCipher.Decrypt(bodyBytes)
		if err != nil {
			return nil, err
		}
		var result sdk.UploadInitResp
		if err := json.Unmarshal(decrypted, &result); err != nil {
			return nil, err
		}
		if err := result.Err(string(decrypted)); err != nil {
			return nil, err
		}
		result.SHA1 = digest.SHA1
		if result.Status != 7 {
			return &result, nil
		}
		if challenge == p115UploadMaxSignChallenges {
			return nil, fmt.Errorf("too many rapid-upload sign challenges")
		}
		signKey = result.SignKey
		signValue, err = hashP115UploadRange(ctx, body, digest.Size, result.SignCheck)
		if err != nil {
			return nil, err
		}
	}
	return nil, sdk.ErrUnexpected
}

// doP115RapidUploadRequest keeps the request context alive until the raw
// encrypted body has been consumed. Resty returns immediately after receiving
// headers in DoNotParseResponse mode, so canceling before ReadAll truncates a
// valid response and makes rapid upload fail intermittently.
func (d *Driver) doP115RapidUploadRequest(ctx context.Context, encodedToken, appVersion string, encrypted []byte) ([]byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, p115UploadControlTimeout)
	defer cancel()
	resp, err := d.client.Client.R().
		SetContext(requestCtx).
		SetQueryParam("k_ec", encodedToken).
		SetBody(encrypted).
		SetHeaderVerbatim("Content-Type", "application/x-www-form-urlencoded").
		SetHeader("User-Agent", "Mozilla/5.0 115Browser/"+appVersion).
		SetDoNotParseResponse(true).
		Post(sdk.ApiUploadInit)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("empty rapid-upload response")
	}
	raw := resp.RawBody()
	if raw == nil {
		return nil, errors.New("empty rapid-upload response body")
	}
	bodyBytes, readErr := io.ReadAll(io.LimitReader(raw, p115UploadResponseLimit+1))
	closeErr := raw.Close()
	if readErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(bodyBytes)) > p115UploadResponseLimit {
		return nil, errors.New("rapid-upload response too large")
	}
	if resp.IsError() {
		return nil, fmt.Errorf("rapid-upload HTTP %d", resp.StatusCode())
	}
	return bodyBytes, nil
}

func generateP115UploadSignature(auth p115UploadAuth, fileID, target string) string {
	first := sha1.Sum([]byte(strconv.FormatInt(auth.userID, 10) + fileID + target + "0"))
	second := sha1.Sum([]byte(auth.userKey + hex.EncodeToString(first[:]) + "000000"))
	return strings.ToUpper(hex.EncodeToString(second[:]))
}

func generateP115UploadToken(userID int64, fileID, preID, timestamp, fileSize, signKey, signValue, appVersion string) string {
	uid := strconv.FormatInt(userID, 10)
	uidMD5 := md5.Sum([]byte(uid))
	payload := p115UploadMD5Salt + fileID + fileSize + signKey + signValue + uid + timestamp + hex.EncodeToString(uidMD5[:]) + appVersion
	token := md5.Sum([]byte(payload))
	return hex.EncodeToString(token[:])
}

func hashP115UploadRange(ctx context.Context, body p115PreparedUploadBody, size int64, rangeSpec string) (string, error) {
	var start, end int64
	if _, err := fmt.Sscanf(rangeSpec, "%d-%d", &start, &end); err != nil {
		return "", fmt.Errorf("invalid sign-check range %q: %w", rangeSpec, err)
	}
	if start < 0 || end < start || end >= size {
		return "", fmt.Errorf("sign-check range %q outside file size %d", rangeSpec, size)
	}
	h := sha1.New()
	section := io.NewSectionReader(body.readerAt, body.start+start, end-start+1)
	if _, err := io.Copy(h, &p115ReaderWithContext{ctx: ctx, reader: section}); err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil))), nil
}

func (d *Driver) p115FileIDByPickCode(ctx context.Context, pickCode string) (string, error) {
	var result sdk.GetFileInfoResponse
	requestCtx, cancel := context.WithTimeout(ctx, p115UploadControlTimeout)
	defer cancel()
	resp, err := d.client.Client.R().
		SetContext(requestCtx).
		SetQueryParam("pick_code", pickCode).
		ForceContentType("application/json;charset=UTF-8").
		SetResult(&result).
		Get(sdk.ApiFileInfo)
	if err = sdk.CheckErr(err, &result, resp); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", err
	}
	if len(result.Files) == 0 || result.Files[0] == nil || strings.TrimSpace(result.Files[0].FileID) == "" {
		return "", fmt.Errorf("%w: pickcode %q returned no file", errP115UploadResultUnavailable, pickCode)
	}
	fileID := strings.TrimSpace(result.Files[0].FileID)
	d.rememberPickCode(fileID, pickCode)
	return fileID, nil
}

// findUploadedFileID is now an eventual-consistency fallback only. Normal
// uploads use the callback's exact file ID, and rapid uploads use PickCode. The
// fallback deliberately requires both name and SHA1; name-only/SHA1-only
// guesses can bind the catalog row to a different copy in a busy directory.
func (d *Driver) findUploadedFileID(ctx context.Context, parentID, name, sha1Hex string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < p115UploadLookupAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepContext(ctx, time.Duration(attempt)*time.Second); err != nil {
				return "", err
			}
		}
		requestCtx, cancel := context.WithTimeout(ctx, p115UploadControlTimeout)
		req := d.client.Client.R().SetContext(requestCtx).ForceContentType("application/json;charset=UTF-8")
		resp, err := sdk.GetFiles(req, parentID,
			sdk.WithOrder(sdk.FileOrderByTime),
			sdk.WithShowDirEnable(false),
			sdk.WithAsc(false),
			sdk.WithLimit(500),
		)
		cancel()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", ctxErr
			}
			lastErr = err
			continue
		}
		if resp == nil {
			lastErr = errors.New("empty list response")
			continue
		}
		for _, file := range resp.Files {
			if file.FileID != "" && file.Name == name && strings.EqualFold(file.Sha1, sha1Hex) {
				return file.FileID, nil
			}
		}
		lastErr = fmt.Errorf("file %q with SHA1 %s not visible in parent %q", name, sha1Hex, parentID)
	}
	return "", fmt.Errorf("%w: %v", errP115UploadResultUnavailable, lastErr)
}

type p115ReadSeekAt interface {
	io.ReadSeeker
	io.ReaderAt
}

func (d *Driver) prepareP115UploadBody(ctx context.Context, r io.Reader, declaredSize int64) (p115PreparedUploadBody, p115UploadDigest, error) {
	if reusable, ok := r.(p115ReadSeekAt); ok {
		start, err := reusable.Seek(0, io.SeekCurrent)
		if err != nil {
			return p115PreparedUploadBody{}, p115UploadDigest{}, fmt.Errorf("p115 upload: seek body: %w", err)
		}
		digest, hashErr := hashAndCopyP115Upload(ctx, reusable, nil, declaredSize)
		_, seekErr := reusable.Seek(start, io.SeekStart)
		if hashErr != nil {
			return p115PreparedUploadBody{}, p115UploadDigest{}, hashErr
		}
		if seekErr != nil {
			return p115PreparedUploadBody{}, p115UploadDigest{}, fmt.Errorf("p115 upload: rewind body: %w", seekErr)
		}
		return p115PreparedUploadBody{reader: reusable, readerAt: reusable, start: start}, digest, nil
	}

	tempDir := strings.TrimSpace(d.uploadTempDir)
	if tempDir != "" {
		if err := os.MkdirAll(tempDir, 0o755); err != nil {
			return p115PreparedUploadBody{}, p115UploadDigest{}, fmt.Errorf("p115 upload: create tmp dir: %w", err)
		}
	}
	tmp, err := os.CreateTemp(tempDir, "p115-upload-*.bin")
	if err != nil {
		return p115PreparedUploadBody{}, p115UploadDigest{}, fmt.Errorf("p115 upload: create tmp: %w", err)
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}
	digest, err := hashAndCopyP115Upload(ctx, r, tmp, declaredSize)
	if err != nil {
		cleanup()
		return p115PreparedUploadBody{}, p115UploadDigest{}, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return p115PreparedUploadBody{}, p115UploadDigest{}, fmt.Errorf("p115 upload: rewind tmp: %w", err)
	}
	return p115PreparedUploadBody{reader: tmp, readerAt: tmp, cleanup: cleanup}, digest, nil
}

func hashAndCopyP115Upload(ctx context.Context, src io.Reader, dst io.Writer, declaredSize int64) (p115UploadDigest, error) {
	fullHash := sha1.New()
	preHash := sha1.New()
	remainingPreHash := p115UploadPreHashSize
	buffer := make([]byte, 1024*1024)
	var written int64
	emptyReads := 0

	for {
		if err := ctx.Err(); err != nil {
			return p115UploadDigest{}, err
		}
		n, readErr := src.Read(buffer)
		if n > 0 {
			emptyReads = 0
			chunk := buffer[:n]
			if dst != nil {
				if err := writeAllP115Upload(dst, chunk); err != nil {
					return p115UploadDigest{}, fmt.Errorf("p115 upload: buffer body: %w", err)
				}
			}
			_, _ = fullHash.Write(chunk)
			if remainingPreHash > 0 {
				preBytes := int64(n)
				if preBytes > remainingPreHash {
					preBytes = remainingPreHash
				}
				_, _ = preHash.Write(chunk[:int(preBytes)])
				remainingPreHash -= preBytes
			}
			written += int64(n)
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return p115UploadDigest{}, fmt.Errorf("p115 upload: read body: %w", readErr)
			}
			break
		}
		if n == 0 {
			emptyReads++
			if emptyReads >= 100 {
				return p115UploadDigest{}, fmt.Errorf("p115 upload: read body: %w", io.ErrNoProgress)
			}
		}
	}
	if declaredSize > 0 && written != declaredSize {
		return p115UploadDigest{}, fmt.Errorf("p115 upload: size mismatch: declared %d, copied %d", declaredSize, written)
	}
	return p115UploadDigest{
		Size:  written,
		PreID: strings.ToUpper(hex.EncodeToString(preHash.Sum(nil))),
		SHA1:  strings.ToUpper(hex.EncodeToString(fullHash.Sum(nil))),
	}, nil
}

func writeAllP115Upload(dst io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := dst.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

type p115ReaderWithContext struct {
	ctx       context.Context
	reader    io.Reader
	remaining int64
}

func (r *p115ReaderWithContext) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if n > 0 && r.remaining > 0 {
		r.remaining -= int64(n)
		if r.remaining < 0 {
			r.remaining = 0
		}
	}
	return n, err
}

// Len lets the Aliyun SDK determine Content-Length through the context-aware
// wrapper. Without it, EnableMD5 treats every part as an unknown-length body,
// copies it to another temporary file and only then starts the request.
func (r *p115ReaderWithContext) Len() int {
	if r.remaining <= 0 {
		return 0
	}
	maxInt := int64(^uint(0) >> 1)
	if r.remaining > maxInt {
		return int(maxInt)
	}
	return int(r.remaining)
}
