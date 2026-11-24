package fingerprint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/streamhttp"
)

// The sampling scheme. A fingerprint hashes the file size and three samples:
// the head holds the container header, the middle holds media data, and the
// tail holds an index written at the end of the file (an MP4 moov box) or
// shows a download that stopped early. A file no larger than the three
// samples together is hashed whole.
//
// Changing any of this changes every fingerprint. Bump hashDomain and
// dedupe.SampledFingerprintPrefix together, so the catalog discards the
// fingerprints of the previous scheme instead of comparing them.
const (
	sampleSize  int64 = 512 * 1024
	sampleCount int64 = 3
	hashDomain        = "video-site-sampled-sha256-v2"
)

const defaultStallTimeout = time.Minute

// errSizeChanged reports that the source no longer has the size the catalog
// recorded. A later scan records the new size and requests a new fingerprint.
var errSizeChanged = errors.New("source size differs from the catalog")

type byteRange struct {
	start  int64
	length int64
}

func (r byteRange) end() int64 { return r.start + r.length - 1 }

func sampleRanges(size int64) []byteRange {
	if size <= sampleCount*sampleSize {
		return []byteRange{{start: 0, length: size}}
	}
	tail := size - sampleSize
	return []byteRange{
		{start: 0, length: sampleSize},
		{start: tail / 2, length: sampleSize},
		{start: tail, length: sampleSize},
	}
}

// Compute fingerprints v's source. It takes no generation slot: a sample is a
// few MiB of Range reads, whether the fingerprint step, an import or a
// blacklist check asks for it.
func Compute(ctx context.Context, drv drives.Drive, v *catalog.Video, cfg Config, hc *http.Client) (string, error) {
	if drv == nil {
		return "", errors.New("fingerprint: nil drive")
	}
	if v == nil {
		return "", errors.New("fingerprint: nil video")
	}
	if v.Size <= 0 {
		return "", errors.New("fingerprint: video size is empty")
	}
	if hc == nil {
		hc = streamhttp.NewClient(0)
	}
	stall := cfg.StallTimeout
	if stall <= 0 {
		stall = defaultStallTimeout
	}
	link, err := streamLink(ctx, drv, v.FileID)
	if err != nil {
		return "", err
	}
	source := &sampleSource{hc: hc, link: link, size: v.Size, stall: stall}
	ranges := sampleRanges(v.Size)
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\nsize=%d\nsamples=%d\n", hashDomain, v.Size, len(ranges))
	for _, r := range ranges {
		data, err := source.read(ctx, r)
		if isP115ForbiddenRangeError(drv, err) {
			// A 115 signed CDN URL can be rejected before its advertised expiry.
			// Refresh it once and retry the same byte range.
			if source.link, err = streamLink(ctx, drv, v.FileID); err == nil {
				data, err = source.read(ctx, r)
			}
			if isP115ForbiddenRangeError(drv, err) {
				// A second rejection is throttling, so the drive cools down rather
				// than failing the video or requesting new links again.
				err = &drives.RateLimitError{Provider: "p115", Err: err}
			}
		}
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "offset=%d length=%d\n", r.start, r.length)
		_, _ = h.Write(data)
		_, _ = h.Write([]byte("\n"))
	}
	return dedupe.SampledFingerprintPrefix + hex.EncodeToString(h.Sum(nil)), nil
}

func streamLink(ctx context.Context, drv drives.Drive, fileID string) (*drives.StreamLink, error) {
	link, err := drv.StreamURL(ctx, fileID)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: stream url: %w", err)
	}
	if link == nil || strings.TrimSpace(link.URL) == "" {
		return nil, errors.New("fingerprint: empty stream url")
	}
	return link, nil
}

// sampleSource reads byte ranges of one source and verifies that the source
// still has the size the catalog recorded.
type sampleSource struct {
	hc    *http.Client
	link  *drives.StreamLink
	size  int64
	stall time.Duration
}

func (s *sampleSource) read(ctx context.Context, r byteRange) ([]byte, error) {
	u, err := url.Parse(s.link.URL)
	var data []byte
	switch {
	case err == nil && (u.Scheme == "http" || u.Scheme == "https"):
		data, err = s.readHTTP(ctx, r)
	case err == nil && u.Scheme == "file":
		data, err = s.readLocal(u.Path, r)
	default:
		data, err = s.readLocal(s.link.URL, r)
	}
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != r.length {
		return nil, fmt.Errorf("fingerprint: short sample at %d: got %d want %d", r.start, len(data), r.length)
	}
	return data, nil
}

func (s *sampleSource) readLocal(path string, r byteRange) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: open local stream: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("fingerprint: stat local stream: %w", err)
	}
	if info.Size() != s.size {
		return nil, fmt.Errorf("fingerprint: local file is %d bytes, catalog recorded %d: %w", info.Size(), s.size, errSizeChanged)
	}
	buf := make([]byte, r.length)
	n, err := f.ReadAt(buf, r.start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("fingerprint: read local sample: %w", err)
	}
	return buf[:n], nil
}

// errStalled marks a remote read that received no data within the stall
// timeout. It is a timeout, so the drive cools down and the video is retried.
var errStalled = stalledError{}

type stalledError struct{}

func (stalledError) Error() string { return "remote read stalled" }
func (stalledError) Timeout() bool { return true }

// readHTTP ends the request when the response stops making progress. The
// client has no overall timeout because samples can be slow on poor links,
// and a connection that stops sending would otherwise hold the worker and
// its shared budget until shutdown.
func (s *sampleSource) readHTTP(ctx context.Context, r byteRange) ([]byte, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	watchdog := time.AfterFunc(s.stall, func() { cancel(errStalled) })
	defer watchdog.Stop()
	data, err := s.fetchHTTP(ctx, r, func() { watchdog.Reset(s.stall) })
	if err != nil && errors.Is(context.Cause(ctx), errStalled) {
		return nil, fmt.Errorf("fingerprint: sample at %d received no data for %s: %w", r.start, s.stall, errStalled)
	}
	return data, err
}

func (s *sampleSource) fetchHTTP(ctx context.Context, r byteRange, progress func()) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.link.URL, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range s.link.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", r.start, r.end()))
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: read remote sample: %w", err)
	}
	defer resp.Body.Close()
	progress()
	body := progressReader{r: resp.Body, progress: progress}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &drives.RateLimitError{
			Provider:   "fingerprint",
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Err:        fmt.Errorf("remote sample rate limited: status=%d", resp.StatusCode),
		}
	case resp.StatusCode == http.StatusPartialContent:
		if err := s.checkContentRange(resp.Header.Get("Content-Range"), r); err != nil {
			return nil, err
		}
		return io.ReadAll(io.LimitReader(body, r.length))
	case resp.StatusCode == http.StatusOK && r.start == 0 && r.length == s.size:
		// A server that ignores Range still serves a whole-file sample. Reading
		// one byte past the recorded size detects a file that grew.
		data, err := io.ReadAll(io.LimitReader(body, r.length+1))
		if err == nil && int64(len(data)) != s.size {
			err = fmt.Errorf("fingerprint: remote file is not %d bytes: %w", s.size, errSizeChanged)
		}
		return data, err
	}
	return nil, &rangeResponseError{status: resp.StatusCode, start: r.start, end: r.end()}
}

// checkContentRange verifies a partial response. A missing header is
// accepted because the sample length is checked separately; a header that
// names another range or another file size is not.
func (s *sampleSource) checkContentRange(header string, r byteRange) error {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil
	}
	var start, end int64
	var total string
	if _, err := fmt.Sscanf(header, "bytes %d-%d/%s", &start, &end, &total); err != nil {
		return fmt.Errorf("fingerprint: invalid Content-Range %q", header)
	}
	if start != r.start || end != r.end() {
		return fmt.Errorf("fingerprint: Content-Range %q does not match bytes=%d-%d", header, r.start, r.end())
	}
	if total == "*" {
		return nil
	}
	size, err := strconv.ParseInt(total, 10, 64)
	if err != nil {
		return fmt.Errorf("fingerprint: invalid Content-Range %q", header)
	}
	if size != s.size {
		return fmt.Errorf("fingerprint: remote file is %d bytes, catalog recorded %d: %w", size, s.size, errSizeChanged)
	}
	return nil
}

type progressReader struct {
	r        io.Reader
	progress func()
}

func (p progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.progress()
	}
	return n, err
}

type rangeResponseError struct {
	status int
	start  int64
	end    int64
}

func (e *rangeResponseError) Error() string {
	return fmt.Sprintf("fingerprint: range request got status=%d for bytes=%d-%d", e.status, e.start, e.end)
}

func isP115ForbiddenRangeError(drv drives.Drive, err error) bool {
	if drv == nil || !strings.EqualFold(drv.Kind(), "p115") || err == nil {
		return false
	}
	var responseErr *rangeResponseError
	return errors.As(err, &responseErr) && responseErr.status == http.StatusForbidden
}

func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}
