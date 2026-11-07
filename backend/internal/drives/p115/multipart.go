package p115

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"

	sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"

	"github.com/nianzhibai/91/backend/internal/scopedproxy"
)

const (
	p115MultipartThreshold             int64 = 10 * 1024 * 1024
	p115MultipartTargetPartSize        int64 = 8 * 1024 * 1024
	p115MultipartMaxParts                    = 10000
	p115MultipartPartAttempts                = 3
	p115MultipartPartNoProgressTimeout       = 60 * time.Second
	p115MultipartAbortTimeout                = 30 * time.Second
	p115OSSTokenRefreshBefore                = 10 * time.Minute

	p115MaxMultipartObjectSize int64 = int64(oss.MaxPartSize) * p115MultipartMaxParts
)

var errP115MultipartPartNoProgress = errors.New("p115 multipart: part upload made no progress")

// p115OSSBucket is deliberately smaller than *oss.Bucket. It makes the 115
// protocol state machine testable without live credentials and prevents the
// driver from falling back to the SDK's unsafe multipart helper.
type p115OSSBucket interface {
	PutObject(objectKey string, reader io.Reader, options ...oss.Option) error
	InitiateMultipartUpload(objectKey string, options ...oss.Option) (oss.InitiateMultipartUploadResult, error)
	UploadPart(imur oss.InitiateMultipartUploadResult, reader io.Reader, partSize int64, partNumber int, options ...oss.Option) (oss.UploadPart, error)
	CompleteMultipartUpload(imur oss.InitiateMultipartUploadResult, parts []oss.UploadPart, options ...oss.Option) (oss.CompleteMultipartUploadResult, error)
	AbortMultipartUpload(imur oss.InitiateMultipartUploadResult, options ...oss.Option) error
}

type p115MultipartChunk struct {
	number int
	offset int64
	size   int64
}

type p115OSSAccess struct {
	bucket p115OSSBucket
	token  sdk.UploadOSSTokenResp
}

type p115OSSAccessProvider func(context.Context) (p115OSSAccess, error)

func validateP115UploadSize(size int64) error {
	if size < 0 {
		return fmt.Errorf("p115 upload: invalid size %d", size)
	}
	if size > p115MaxMultipartObjectSize {
		return fmt.Errorf("p115 upload: file size %d exceeds multipart limit %d", size, p115MaxMultipartObjectSize)
	}
	return nil
}

// planP115Multipart uses stable 8 MiB parts for ordinary video sizes. 115 asks
// OSS to calculate a sequential SHA1, so parts must remain ordered and cannot
// safely be uploaded concurrently. Larger parts remove hundreds of RTT-bound
// requests while preserving that protocol requirement.
func planP115Multipart(size int64) ([]p115MultipartChunk, error) {
	if size <= 0 {
		return nil, fmt.Errorf("p115 multipart: invalid size %d", size)
	}
	if err := validateP115UploadSize(size); err != nil {
		return nil, err
	}

	partSize := p115MultipartTargetPartSize
	if count := (size + partSize - 1) / partSize; count > p115MultipartMaxParts {
		partSize = (size + p115MultipartMaxParts - 1) / p115MultipartMaxParts
		// Round upward to MiB boundaries so plans are stable and readable.
		const alignment int64 = 1024 * 1024
		partSize = ((partSize + alignment - 1) / alignment) * alignment
	}
	if partSize > int64(oss.MaxPartSize) {
		return nil, fmt.Errorf("p115 multipart: part size %d exceeds OSS limit %d", partSize, oss.MaxPartSize)
	}

	partCount := (size + partSize - 1) / partSize
	if partCount > p115MultipartMaxParts {
		return nil, fmt.Errorf("p115 multipart: part count %d exceeds OSS limit %d", partCount, p115MultipartMaxParts)
	}
	chunks := make([]p115MultipartChunk, 0, int(partCount))
	for offset, number := int64(0), 1; offset < size; offset, number = offset+partSize, number+1 {
		chunkSize := partSize
		if remaining := size - offset; remaining < chunkSize {
			chunkSize = remaining
		}
		chunks = append(chunks, p115MultipartChunk{number: number, offset: offset, size: chunkSize})
	}
	return chunks, nil
}

func (d *Driver) uploadP115BodyToOSS(ctx context.Context, params *sdk.UploadOSSParams, body p115PreparedUploadBody, size int64) ([]byte, error) {
	if params == nil {
		return nil, errors.New("p115 oss upload: nil parameters")
	}
	if strings.TrimSpace(params.Bucket) == "" || strings.TrimSpace(params.Object) == "" {
		return nil, errors.New("p115 oss upload: empty bucket or object")
	}
	provider := d.p115OSSAccessProvider(params)
	return uploadPreparedP115BodyToOSS(ctx, params, body, size, provider)
}

func uploadPreparedP115BodyToOSS(
	ctx context.Context,
	params *sdk.UploadOSSParams,
	body p115PreparedUploadBody,
	size int64,
	provider p115OSSAccessProvider,
) ([]byte, error) {
	if size <= p115MultipartThreshold {
		return putP115Object(ctx, params, body, size, provider)
	}
	return uploadP115Multipart(ctx, params, body, size, provider)
}

func putP115Object(
	ctx context.Context,
	params *sdk.UploadOSSParams,
	body p115PreparedUploadBody,
	size int64,
	provider p115OSSAccessProvider,
) ([]byte, error) {
	access, err := provider(ctx)
	if err != nil {
		return nil, fmt.Errorf("get OSS credentials: %w", err)
	}
	var lastErr error
	attemptsMade := 0
	for attempt := 1; attempt <= p115MultipartPartAttempts; attempt++ {
		attemptsMade = attempt
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if attempt > 1 && (p115OSSAccessNeedsRefresh(access.token, time.Now()) || isP115OSSCredentialError(lastErr)) {
			access, err = provider(ctx)
			if err != nil {
				return nil, fmt.Errorf("refresh OSS credentials: %w", err)
			}
		}
		if err := body.rewind(); err != nil {
			return nil, fmt.Errorf("rewind body: %w", err)
		}
		var callbackBody []byte
		reader := io.LimitReader(body.reader, size)
		err = access.bucket.PutObject(
			params.Object,
			&p115ReaderWithContext{ctx: ctx, reader: reader, remaining: size},
			p115OSSCallbackOptions(ctx, params, &access.token, &callbackBody)...,
		)
		if err == nil {
			return callbackBody, nil
		}
		lastErr = err
		if attempt >= p115MultipartPartAttempts || (!isRetryableP115UploadError(err) && !isP115OSSCredentialError(err)) {
			break
		}
		if err := sleepContext(ctx, time.Duration(attempt)*time.Second); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("put object after %d attempt(s): %w", attemptsMade, lastErr)
}

func uploadP115Multipart(
	ctx context.Context,
	params *sdk.UploadOSSParams,
	body p115PreparedUploadBody,
	size int64,
	provider p115OSSAccessProvider,
) (callbackBody []byte, retErr error) {
	chunks, err := planP115Multipart(size)
	if err != nil {
		return nil, err
	}
	if body.readerAt == nil {
		return nil, errors.New("p115 multipart: upload body does not support random access")
	}
	access, err := provider(ctx)
	if err != nil {
		return nil, fmt.Errorf("get OSS credentials: %w", err)
	}

	var imur oss.InitiateMultipartUploadResult
	for attempt := 1; attempt <= p115MultipartPartAttempts; attempt++ {
		imur, err = access.bucket.InitiateMultipartUpload(
			params.Object,
			append(p115OSSRequestOptions(ctx, &access.token), oss.EnableSha1(), oss.Sequential())...,
		)
		if err == nil {
			break
		}
		if attempt >= p115MultipartPartAttempts || (!isRetryableP115UploadError(err) && !isP115OSSCredentialError(err)) {
			return nil, fmt.Errorf("initiate multipart: %w", err)
		}
		if isP115OSSCredentialError(err) || p115OSSAccessNeedsRefresh(access.token, time.Now()) {
			access, err = provider(ctx)
			if err != nil {
				return nil, fmt.Errorf("refresh OSS credentials: %w", err)
			}
		}
		if err := sleepContext(ctx, time.Duration(attempt)*time.Second); err != nil {
			return nil, err
		}
	}

	completed := false
	defer func() {
		if completed {
			return
		}
		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p115MultipartAbortTimeout)
		defer cancel()
		if p115OSSAccessNeedsRefresh(access.token, time.Now()) {
			if refreshed, refreshErr := provider(abortCtx); refreshErr == nil {
				access = refreshed
			}
		}
		if abortErr := access.bucket.AbortMultipartUpload(imur, p115OSSRequestOptions(abortCtx, &access.token)...); abortErr != nil {
			abortErr = fmt.Errorf("abort multipart: %w", abortErr)
			if retErr == nil {
				retErr = abortErr
			} else {
				retErr = errors.Join(retErr, abortErr)
			}
		}
	}()

	parts := make([]oss.UploadPart, 0, len(chunks))
	for _, chunk := range chunks {
		part, err := uploadP115Part(ctx, provider, &access, imur, body, chunk, len(chunks))
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p115OSSAccessNeedsRefresh(access.token, time.Now()) {
		refreshed, err := provider(ctx)
		if err != nil {
			return nil, fmt.Errorf("refresh OSS credentials before completion: %w", err)
		}
		access = refreshed
	}

	callbackBody = nil
	if _, err := access.bucket.CompleteMultipartUpload(
		imur,
		parts,
		p115OSSCallbackOptions(ctx, params, &access.token, &callbackBody)...,
	); err != nil {
		return nil, fmt.Errorf("complete multipart: %w", err)
	}
	completed = true
	return callbackBody, nil
}

func uploadP115Part(
	ctx context.Context,
	provider p115OSSAccessProvider,
	access *p115OSSAccess,
	imur oss.InitiateMultipartUploadResult,
	body p115PreparedUploadBody,
	chunk p115MultipartChunk,
	totalParts int,
) (oss.UploadPart, error) {
	return uploadP115PartWithNoProgressTimeout(
		ctx,
		provider,
		access,
		imur,
		body,
		chunk,
		totalParts,
		p115MultipartPartNoProgressTimeout,
	)
}

func uploadP115PartWithNoProgressTimeout(
	ctx context.Context,
	provider p115OSSAccessProvider,
	access *p115OSSAccess,
	imur oss.InitiateMultipartUploadResult,
	body p115PreparedUploadBody,
	chunk p115MultipartChunk,
	totalParts int,
	noProgressTimeout time.Duration,
) (oss.UploadPart, error) {
	if noProgressTimeout <= 0 {
		return oss.UploadPart{}, fmt.Errorf("p115 multipart: invalid part no-progress timeout %s", noProgressTimeout)
	}
	var lastErr error
	for attempt := 1; attempt <= p115MultipartPartAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return oss.UploadPart{}, err
		}
		if p115OSSAccessNeedsRefresh(access.token, time.Now()) || (attempt > 1 && isP115OSSCredentialError(lastErr)) {
			refreshed, err := provider(ctx)
			if err != nil {
				return oss.UploadPart{}, fmt.Errorf("refresh OSS credentials: %w", err)
			}
			*access = refreshed
		}

		part, err := uploadP115PartAttempt(
			ctx,
			*access,
			imur,
			body,
			chunk,
			noProgressTimeout,
		)
		if err == nil && strings.TrimSpace(part.ETag) == "" {
			err = errors.New("OSS returned an empty part ETag")
		}
		if err == nil {
			return part, nil
		}
		lastErr = err
		if attempt >= p115MultipartPartAttempts || (!isRetryableP115UploadError(err) && !isP115OSSCredentialError(err)) {
			return oss.UploadPart{}, fmt.Errorf("upload part %d/%d after %d attempt(s): %w", chunk.number, totalParts, attempt, err)
		}
		retryDelay := time.Duration(attempt) * time.Second
		if errors.Is(err, errP115MultipartPartNoProgress) {
			log.Printf(
				"[p115] multipart part %d/%d attempt %d/%d made no progress for %s; canceled its connection and retrying the same multipart session in %s",
				chunk.number,
				totalParts,
				attempt,
				p115MultipartPartAttempts,
				noProgressTimeout,
				retryDelay,
			)
		}
		if err := sleepContext(ctx, retryDelay); err != nil {
			return oss.UploadPart{}, err
		}
	}
	return oss.UploadPart{}, fmt.Errorf("upload part %d/%d: %w", chunk.number, totalParts, lastErr)
}

func uploadP115PartAttempt(
	ctx context.Context,
	access p115OSSAccess,
	imur oss.InitiateMultipartUploadResult,
	body p115PreparedUploadBody,
	chunk p115MultipartChunk,
	noProgressTimeout time.Duration,
) (oss.UploadPart, error) {
	connection := &p115PartConnectionTracker{}
	watchdogCtx, reportProgress, stopWatchdog := p115PartNoProgressContext(ctx, noProgressTimeout, connection.close)
	attemptCtx := httptrace.WithClientTrace(watchdogCtx, &httptrace.ClientTrace{GotConn: connection.gotConn})
	section := io.NewSectionReader(body.readerAt, body.start+chunk.offset, chunk.size)
	reader := &p115PartProgressReader{reader: section, reportProgress: reportProgress}
	part, err := access.bucket.UploadPart(
		imur,
		&p115ReaderWithContext{ctx: attemptCtx, reader: reader, remaining: chunk.size},
		chunk.size,
		chunk.number,
		p115OSSRequestOptions(attemptCtx, &access.token)...,
	)
	cause := stopWatchdog()
	if err == nil {
		return part, nil
	}
	if parentErr := ctx.Err(); parentErr != nil {
		return oss.UploadPart{}, parentErr
	}
	if errors.Is(cause, errP115MultipartPartNoProgress) {
		return oss.UploadPart{}, fmt.Errorf("%w for %s", errP115MultipartPartNoProgress, noProgressTimeout)
	}
	return oss.UploadPart{}, err
}

type p115PartConnectionTracker struct {
	mu     sync.Mutex
	conn   net.Conn
	closed bool
}

func (t *p115PartConnectionTracker) gotConn(info httptrace.GotConnInfo) {
	if info.Conn == nil {
		return
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		_ = info.Conn.Close()
		return
	}
	t.conn = info.Conn
	t.mu.Unlock()
}

func (t *p115PartConnectionTracker) close() {
	t.mu.Lock()
	t.closed = true
	conn := t.conn
	t.conn = nil
	t.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

type p115PartProgressReader struct {
	reader         io.Reader
	reportProgress func()
}

func (r *p115PartProgressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.reportProgress()
	}
	return n, err
}

// p115PartNoProgressContext bounds one UploadPart attempt without imposing a
// total-duration limit. Each successful body read renews the timer. On expiry,
// the caller first closes the exact connection reported by net/http/httptrace,
// then this watchdog cancels only the attempt context. The parent upload and
// its OSS UploadID stay alive for the existing retry loop.
func p115PartNoProgressContext(
	parent context.Context,
	timeout time.Duration,
	closeConnection func(),
) (context.Context, func(), func() error) {
	attemptCtx, cancel := context.WithCancelCause(parent)
	progress := make(chan struct{}, 1)
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		for {
			select {
			case <-parent.Done():
				if closeConnection != nil {
					closeConnection()
				}
				return
			case <-stop:
				return
			case <-progress:
				resetP115PartNoProgressTimer(timer, timeout)
			case <-timer.C:
				// Progress queued at the timeout boundary wins over the timer so
				// scheduler jitter cannot cancel a connection that is still moving.
				select {
				case <-progress:
					timer.Reset(timeout)
					continue
				default:
				}
				if closeConnection != nil {
					closeConnection()
				}
				cancel(errP115MultipartPartNoProgress)
				return
			}
		}
	}()

	reportProgress := func() {
		select {
		case progress <- struct{}{}:
		default:
		}
	}

	var stopOnce sync.Once
	var cause error
	stopWatchdog := func() error {
		stopOnce.Do(func() {
			close(stop)
			<-done
			cause = context.Cause(attemptCtx)
			cancel(nil)
		})
		return cause
	}
	return attemptCtx, reportProgress, stopWatchdog
}

func resetP115PartNoProgressTimer(timer *time.Timer, timeout time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(timeout)
}

func (d *Driver) p115OSSAccessProvider(params *sdk.UploadOSSParams) p115OSSAccessProvider {
	return func(ctx context.Context) (p115OSSAccess, error) {
		token, err := d.p115OSSToken(ctx)
		if err != nil {
			return p115OSSAccess{}, err
		}
		client, err := newP115OSSClient(&token)
		if err != nil {
			return p115OSSAccess{}, err
		}
		bucket, err := client.Bucket(params.Bucket)
		if err != nil {
			return p115OSSAccess{}, err
		}
		return p115OSSAccess{bucket: bucket, token: token}, nil
	}
}

func (d *Driver) p115OSSToken(ctx context.Context) (sdk.UploadOSSTokenResp, error) {
	var result sdk.UploadOSSTokenResp
	requestCtx, cancel := context.WithTimeout(ctx, p115UploadControlTimeout)
	defer cancel()
	resp, err := d.client.Client.R().
		SetContext(requestCtx).
		SetResult(&result).
		ForceContentType("application/json;charset=UTF-8").
		Get(sdk.ApiUploadOSSToken)
	if err = sdk.CheckErr(err, &result, resp); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return sdk.UploadOSSTokenResp{}, ctxErr
		}
		return sdk.UploadOSSTokenResp{}, fmt.Errorf("get OSS token: %w", err)
	}
	if result.AccessKeyID == "" || result.AccessKeySecret == "" || result.SecurityToken == "" {
		return sdk.UploadOSSTokenResp{}, errors.New("get OSS token: incomplete credentials")
	}
	return result, nil
}

func newP115OSSClient(token *sdk.UploadOSSTokenResp, options ...oss.ClientOption) (*oss.Client, error) {
	if token == nil {
		return nil, errors.New("p115 OSS client: nil token")
	}
	clientOptions := []oss.ClientOption{
		oss.EnableMD5(true),
		oss.EnableCRC(true),
		oss.HTTPClient(scopedproxy.NewHTTPClient(0)),
	}
	clientOptions = append(clientOptions, options...)
	return oss.New(sdk.OSSEndpoint, token.AccessKeyID, token.AccessKeySecret, clientOptions...)
}

func p115OSSRequestOptions(ctx context.Context, token *sdk.UploadOSSTokenResp) []oss.Option {
	securityToken := ""
	if token != nil {
		securityToken = token.SecurityToken
	}
	return []oss.Option{
		oss.SetHeader(sdk.OssSecurityTokenHeaderName, securityToken),
		oss.UserAgentHeader(sdk.OSSUserAgent),
		oss.WithContext(ctx),
	}
}

func p115OSSCallbackOptions(
	ctx context.Context,
	params *sdk.UploadOSSParams,
	token *sdk.UploadOSSTokenResp,
	callbackBody *[]byte,
) []oss.Option {
	options := p115OSSRequestOptions(ctx, token)
	options = append(options,
		oss.Callback(base64.StdEncoding.EncodeToString([]byte(params.Callback.Callback))),
		oss.CallbackVar(base64.StdEncoding.EncodeToString([]byte(params.Callback.CallbackVar))),
		oss.CallbackResult(callbackBody),
	)
	return options
}

func p115OSSAccessNeedsRefresh(token sdk.UploadOSSTokenResp, now time.Time) bool {
	return !token.Expiration.IsZero() && !now.Add(p115OSSTokenRefreshBefore).Before(token.Expiration)
}

func isP115OSSCredentialError(err error) bool {
	if err == nil {
		return false
	}
	var serviceErr oss.ServiceError
	if errors.As(err, &serviceErr) {
		if serviceErr.StatusCode == http.StatusUnauthorized || serviceErr.StatusCode == http.StatusForbidden {
			return true
		}
		switch strings.ToLower(serviceErr.Code) {
		case "securitytokenexpired", "invalidsecuritytoken", "invalidaccesskeyid", "accessdenied":
			return true
		}
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "security token expired") ||
		strings.Contains(text, "securitytokenexpired") ||
		strings.Contains(text, "invalid security token")
}

func isRetryableP115UploadError(err error) bool {
	if errors.Is(err, errP115MultipartPartNoProgress) {
		return true
	}
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, errP115UploadResultUnavailable) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var serviceErr oss.ServiceError
	if errors.As(err, &serviceErr) {
		return serviceErr.StatusCode == http.StatusRequestTimeout ||
			serviceErr.StatusCode == http.StatusTooManyRequests ||
			serviceErr.StatusCode >= 500
	}
	var statusErr oss.UnexpectedStatusCodeError
	if errors.As(err, &statusErr) {
		status := statusErr.Got()
		return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "connection reset") ||
		strings.Contains(text, "connection refused") ||
		strings.Contains(text, "broken pipe") ||
		strings.Contains(text, "unexpected eof") ||
		strings.Contains(text, "i/o timeout") ||
		strings.Contains(text, "tls handshake timeout") ||
		strings.Contains(text, "temporary failure") ||
		strings.Contains(text, "too many requests") ||
		strings.Contains(text, "http 408") ||
		strings.Contains(text, "http 429") ||
		strings.Contains(text, "http 500") ||
		strings.Contains(text, "http 502") ||
		strings.Contains(text, "http 503") ||
		strings.Contains(text, "http 504")
}
