package guangyapan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"

	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/scopedproxy"
)

const (
	guangYaMultipartTargetPartSize        int64 = 8 * 1024 * 1024
	guangYaMultipartTargetParts                 = 1000
	guangYaMultipartMaxParts                    = 10000
	guangYaMultipartConcurrency                 = 6
	guangYaMultipartPartAttempts                = 3
	guangYaMultipartPartNoProgressTimeout       = 60 * time.Second
	guangYaMultipartAbortTimeout                = 30 * time.Second

	guangYaMaxMultipartObjectSize int64 = int64(oss.MaxPartSize) * guangYaMultipartMaxParts
)

var errGuangYaMultipartPartNoProgress = errors.New("guangyapan multipart: part upload made no progress")

// guangYaOSSBucket is the complete OSS session surface used by this driver.
// Keeping the state machine behind a small interface makes retry, cancellation
// and abort behavior testable without a live GuangYaPan account.
type guangYaOSSBucket interface {
	PutObject(objectKey string, reader io.Reader, options ...oss.Option) error
	InitiateMultipartUpload(objectKey string, options ...oss.Option) (oss.InitiateMultipartUploadResult, error)
	UploadPart(imur oss.InitiateMultipartUploadResult, reader io.Reader, partSize int64, partNumber int, options ...oss.Option) (oss.UploadPart, error)
	CompleteMultipartUpload(imur oss.InitiateMultipartUploadResult, parts []oss.UploadPart, options ...oss.Option) (oss.CompleteMultipartUploadResult, error)
	AbortMultipartUpload(imur oss.InitiateMultipartUploadResult, options ...oss.Option) error
}

type guangYaPreparedUploadBody struct {
	readerAt io.ReaderAt
	start    int64
	cleanup  func()
}

type guangYaReadSeekAt interface {
	io.ReadSeeker
	io.ReaderAt
}

// prepareUploadBody establishes the invariant required by reliable multipart
// retries: every byte range can be read again without depending on the current
// position of the caller's stream. Local files are reused in place; genuinely
// streaming inputs are staged in the configured temp dir.
func (d *Driver) prepareUploadBody(ctx context.Context, source io.Reader, declaredSize int64) (guangYaPreparedUploadBody, error) {
	if source == nil {
		return guangYaPreparedUploadBody{}, errors.New("guangyapan upload: nil reader")
	}
	if err := validateGuangYaUploadSize(declaredSize); err != nil {
		return guangYaPreparedUploadBody{}, err
	}

	if reusable, ok := source.(guangYaReadSeekAt); ok {
		start, err := reusable.Seek(0, io.SeekCurrent)
		if err != nil {
			return guangYaPreparedUploadBody{}, fmt.Errorf("guangyapan upload: seek body: %w", err)
		}
		end, endErr := reusable.Seek(0, io.SeekEnd)
		_, rewindErr := reusable.Seek(start, io.SeekStart)
		if endErr != nil {
			return guangYaPreparedUploadBody{}, fmt.Errorf("guangyapan upload: determine body size: %w", endErr)
		}
		if rewindErr != nil {
			return guangYaPreparedUploadBody{}, fmt.Errorf("guangyapan upload: rewind body: %w", rewindErr)
		}
		if end < start || end-start != declaredSize {
			return guangYaPreparedUploadBody{}, fmt.Errorf("guangyapan upload: size mismatch: declared %d, available %d", declaredSize, maxInt64(end-start, 0))
		}
		return guangYaPreparedUploadBody{readerAt: reusable, start: start}, nil
	}

	tempDir := strings.TrimSpace(d.uploadTempDir)
	if tempDir != "" {
		if err := os.MkdirAll(tempDir, 0o755); err != nil {
			return guangYaPreparedUploadBody{}, fmt.Errorf("guangyapan upload: create temp dir: %w", err)
		}
	}
	tmp, err := os.CreateTemp(tempDir, "guangyapan-upload-*.bin")
	if err != nil {
		return guangYaPreparedUploadBody{}, fmt.Errorf("guangyapan upload: create temp file: %w", err)
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}

	limited := &io.LimitedReader{R: &contextReader{ctx: ctx, r: source}, N: declaredSize + 1}
	written, err := io.Copy(tmp, limited)
	if err != nil {
		cleanup()
		return guangYaPreparedUploadBody{}, fmt.Errorf("guangyapan upload: stage body: %w", err)
	}
	if written != declaredSize {
		cleanup()
		return guangYaPreparedUploadBody{}, fmt.Errorf("guangyapan upload: size mismatch: declared %d, copied %d", declaredSize, written)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return guangYaPreparedUploadBody{}, fmt.Errorf("guangyapan upload: sync temp file: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return guangYaPreparedUploadBody{}, fmt.Errorf("guangyapan upload: rewind temp file: %w", err)
	}
	return guangYaPreparedUploadBody{readerAt: tmp, cleanup: cleanup}, nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func validateGuangYaUploadSize(size int64) error {
	if size < 0 {
		return fmt.Errorf("guangyapan upload: invalid file size %d", size)
	}
	if size > guangYaMaxMultipartObjectSize {
		return fmt.Errorf("guangyapan upload: file size %d exceeds OSS multipart limit %d", size, guangYaMaxMultipartObjectSize)
	}
	return nil
}

type guangYaMultipartChunk struct {
	number int
	offset int64
	size   int64
}

type guangYaMultipartResult struct {
	index int
	part  oss.UploadPart
	err   error
}

// planGuangYaMultipart targets at most about 1000 requests for ordinary files,
// then grows up to OSS's protocol maximum of 10000 parts for multi-terabyte
// objects. This avoids the old fixed 4 MiB plan, which crossed the protocol
// limit at roughly 39 GiB.
func planGuangYaMultipart(size int64) ([]guangYaMultipartChunk, error) {
	if size <= 0 {
		return nil, fmt.Errorf("guangyapan multipart: invalid size %d", size)
	}
	if err := validateGuangYaUploadSize(size); err != nil {
		return nil, err
	}

	partSize := guangYaMultipartTargetPartSize
	if count := (size + partSize - 1) / partSize; count > guangYaMultipartTargetParts {
		partSize = (size + guangYaMultipartTargetParts - 1) / guangYaMultipartTargetParts
		const alignment int64 = 1024 * 1024
		partSize = ((partSize + alignment - 1) / alignment) * alignment
	}
	if partSize > int64(oss.MaxPartSize) {
		partSize = int64(oss.MaxPartSize)
	}
	partCount := (size + partSize - 1) / partSize
	if partCount > guangYaMultipartMaxParts {
		return nil, fmt.Errorf("guangyapan multipart: part count %d exceeds OSS limit %d", partCount, guangYaMultipartMaxParts)
	}

	chunks := make([]guangYaMultipartChunk, 0, int(partCount))
	for offset, number := int64(0), 1; offset < size; offset, number = offset+partSize, number+1 {
		chunkSize := partSize
		if remaining := size - offset; remaining < chunkSize {
			chunkSize = remaining
		}
		chunks = append(chunks, guangYaMultipartChunk{number: number, offset: offset, size: chunkSize})
	}
	return chunks, nil
}

func (d *Driver) openUploadBucket(token *uploadTokenData) (guangYaOSSBucket, error) {
	if token == nil {
		return nil, errors.New("guangyapan upload: nil upload token")
	}
	if d.newOSSBucket != nil {
		return d.newOSSBucket(*token)
	}
	client, err := oss.New(
		normalizeOSSEndpoint(token.EndPoint, token.BucketName),
		token.AccessKeyID,
		token.SecretAccessKey,
		oss.SecurityToken(token.SessionToken),
		oss.HTTPClient(scopedproxy.NewHTTPClient(0)),
	)
	if err != nil {
		return nil, fmt.Errorf("guangyapan upload: create oss client: %w", err)
	}
	bucket, err := client.Bucket(token.BucketName)
	if err != nil {
		return nil, fmt.Errorf("guangyapan upload: create oss bucket: %w", err)
	}
	return bucket, nil
}

func guangYaOSSOptions(ctx context.Context) []oss.Option {
	return []oss.Option{oss.WithContext(ctx)}
}

func putEmptyGuangYaObject(ctx context.Context, bucket guangYaOSSBucket, objectPath string) error {
	var lastErr error
	for attempt := 1; attempt <= guangYaMultipartPartAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = bucket.PutObject(objectPath, strings.NewReader(""), guangYaOSSOptions(ctx)...)
		if lastErr == nil {
			return nil
		}
		if attempt >= guangYaMultipartPartAttempts || !isRetryableGuangYaUploadError(lastErr) {
			break
		}
		if err := sleepGuangYaUpload(ctx, time.Duration(attempt)*500*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("guangyapan put empty object after %d attempt(s): %w", guangYaMultipartPartAttempts, lastErr)
}

func uploadGuangYaMultipart(ctx context.Context, bucket guangYaOSSBucket, objectPath string, body guangYaPreparedUploadBody, size int64) (retErr error) {
	chunks, err := planGuangYaMultipart(size)
	if err != nil {
		return err
	}
	if body.readerAt == nil {
		return errors.New("guangyapan multipart: upload body does not support random access")
	}

	// GuangYa issues a standard OSS multipart session. Do not opt into OSS's
	// sequential mode: independent part numbers are uploaded by the bounded
	// worker pool below and may finish out of order.
	upload, err := bucket.InitiateMultipartUpload(objectPath, guangYaOSSOptions(ctx)...)
	if err != nil {
		return fmt.Errorf("guangyapan multipart: initiate: %w", err)
	}
	completed := false
	defer func() {
		if completed {
			return
		}
		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), guangYaMultipartAbortTimeout)
		defer cancel()
		if abortErr := bucket.AbortMultipartUpload(upload, guangYaOSSOptions(abortCtx)...); abortErr != nil {
			abortErr = fmt.Errorf("guangyapan multipart: abort: %w", abortErr)
			if retErr == nil {
				retErr = abortErr
			} else {
				retErr = errors.Join(retErr, abortErr)
			}
		}
	}()

	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()

	jobs := make(chan guangYaMultipartChunk, len(chunks))
	results := make(chan guangYaMultipartResult, len(chunks))
	for _, chunk := range chunks {
		jobs <- chunk
	}
	close(jobs)

	workerCount := min(guangYaMultipartConcurrency, len(chunks))
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for {
				if workerCtx.Err() != nil {
					return
				}
				select {
				case <-workerCtx.Done():
					return
				case chunk, ok := <-jobs:
					if !ok {
						return
					}
					if workerCtx.Err() != nil {
						return
					}
					part, uploadErr := uploadGuangYaPart(workerCtx, bucket, upload, body, chunk, len(chunks))
					results <- guangYaMultipartResult{index: chunk.number - 1, part: part, err: uploadErr}
					if uploadErr != nil {
						return
					}
				}
			}
		}()
	}
	go func() {
		workers.Wait()
		close(results)
	}()

	parts := make([]oss.UploadPart, len(chunks))
	var firstErr error
	for result := range results {
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
				cancelWorkers()
			}
			continue
		}
		parts[result.index] = result.part
	}
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := bucket.CompleteMultipartUpload(upload, parts, guangYaOSSOptions(ctx)...); err != nil {
		return fmt.Errorf("guangyapan multipart: complete: %w", err)
	}
	completed = true
	return nil
}

func uploadGuangYaPart(
	ctx context.Context,
	bucket guangYaOSSBucket,
	upload oss.InitiateMultipartUploadResult,
	body guangYaPreparedUploadBody,
	chunk guangYaMultipartChunk,
	totalParts int,
) (oss.UploadPart, error) {
	return uploadGuangYaPartWithNoProgressTimeout(
		ctx,
		bucket,
		upload,
		body,
		chunk,
		totalParts,
		guangYaMultipartPartNoProgressTimeout,
	)
}

func uploadGuangYaPartWithNoProgressTimeout(
	ctx context.Context,
	bucket guangYaOSSBucket,
	upload oss.InitiateMultipartUploadResult,
	body guangYaPreparedUploadBody,
	chunk guangYaMultipartChunk,
	totalParts int,
	noProgressTimeout time.Duration,
) (oss.UploadPart, error) {
	if noProgressTimeout <= 0 {
		return oss.UploadPart{}, fmt.Errorf("guangyapan multipart: invalid part no-progress timeout %s", noProgressTimeout)
	}
	var lastErr error
	attemptsMade := 0
	for attempt := 1; attempt <= guangYaMultipartPartAttempts; attempt++ {
		attemptsMade = attempt
		if err := ctx.Err(); err != nil {
			return oss.UploadPart{}, err
		}
		part, err := uploadGuangYaPartAttempt(
			ctx,
			bucket,
			upload,
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
		if attempt >= guangYaMultipartPartAttempts || !isRetryableGuangYaUploadError(err) {
			break
		}
		retryDelay := time.Duration(attempt) * 500 * time.Millisecond
		log.Printf(
			"[guangyapan] multipart part %d/%d attempt %d/%d failed; retrying in %s: %v",
			chunk.number,
			totalParts,
			attempt,
			guangYaMultipartPartAttempts,
			retryDelay,
			err,
		)
		if err := sleepGuangYaUpload(ctx, retryDelay); err != nil {
			return oss.UploadPart{}, err
		}
	}
	return oss.UploadPart{}, fmt.Errorf(
		"guangyapan multipart: upload part %d/%d after %d attempt(s): %w",
		chunk.number,
		totalParts,
		attemptsMade,
		lastErr,
	)
}

func uploadGuangYaPartAttempt(
	ctx context.Context,
	bucket guangYaOSSBucket,
	upload oss.InitiateMultipartUploadResult,
	body guangYaPreparedUploadBody,
	chunk guangYaMultipartChunk,
	noProgressTimeout time.Duration,
) (oss.UploadPart, error) {
	attemptCtx, reportProgress, stopWatchdog := guangYaPartNoProgressContext(ctx, noProgressTimeout)
	section := io.NewSectionReader(body.readerAt, body.start+chunk.offset, chunk.size)
	reader := &guangYaPartProgressReader{reader: section, reportProgress: reportProgress}
	part, err := bucket.UploadPart(
		upload,
		&contextReader{ctx: attemptCtx, r: reader},
		chunk.size,
		chunk.number,
		guangYaOSSOptions(attemptCtx)...,
	)
	cause := stopWatchdog()
	if err == nil {
		return part, nil
	}
	if parentErr := ctx.Err(); parentErr != nil {
		return oss.UploadPart{}, parentErr
	}
	if errors.Is(cause, errGuangYaMultipartPartNoProgress) {
		return oss.UploadPart{}, fmt.Errorf("%w for %s", errGuangYaMultipartPartNoProgress, noProgressTimeout)
	}
	return oss.UploadPart{}, err
}

type guangYaPartProgressReader struct {
	reader         io.Reader
	reportProgress func()
}

func (r *guangYaPartProgressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.reportProgress()
	}
	return n, err
}

// guangYaPartNoProgressContext bounds one UploadPart attempt without imposing
// a total-duration limit. Each successful body read renews the timer. When the
// HTTP transport stops consuming the body (for example because a proxy's
// outbound TCP flow is stuck), canceling the request closes that connection so
// the existing part retry loop can replay the same byte range on a fresh one.
func guangYaPartNoProgressContext(
	parent context.Context,
	timeout time.Duration,
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
				return
			case <-stop:
				return
			case <-progress:
				resetGuangYaPartNoProgressTimer(timer, timeout)
			case <-timer.C:
				// A progress notification queued at the timeout boundary wins over
				// the timer so an active upload is not canceled by scheduler jitter.
				select {
				case <-progress:
					timer.Reset(timeout)
					continue
				default:
				}
				cancel(errGuangYaMultipartPartNoProgress)
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

func resetGuangYaPartNoProgressTimer(timer *time.Timer, timeout time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(timeout)
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

func sleepGuangYaUpload(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isRetryableGuangYaUploadError(err error) bool {
	if errors.Is(err, errGuangYaMultipartPartNoProgress) {
		return true
	}
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if status, ok := guangYaOSSStatus(err); ok {
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
		strings.Contains(text, "service unavailable")
}

func guangYaOSSStatus(err error) (int, bool) {
	var serviceErr oss.ServiceError
	if errors.As(err, &serviceErr) {
		return serviceErr.StatusCode, serviceErr.StatusCode > 0
	}
	var statusErr oss.UnexpectedStatusCodeError
	if errors.As(err, &statusErr) {
		return statusErr.Got(), true
	}
	return 0, false
}

func (d *Driver) classifyUploadError(err error) error {
	if err == nil {
		return nil
	}
	status, ok := guangYaOSSStatus(err)
	if !ok || (status != http.StatusTooManyRequests && status < 500) {
		return err
	}
	wait := d.setAPICooldown(0)
	return &drives.RateLimitError{
		Provider:   Kind,
		RetryAfter: wait,
		Err:        fmt.Errorf("guangyapan OSS rate limited: status=%d: %w", status, err),
	}
}

func (d *Driver) acquireUpload(ctx context.Context) (func(), error) {
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
