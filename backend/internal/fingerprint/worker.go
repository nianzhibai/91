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
	"sync"
	"syscall"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/streamhttp"
	"github.com/nianzhibai/91/backend/internal/tasklimit"
)

const (
	defaultSampleSizeBytes int64 = 512 * 1024
	defaultFullHashMaxSize int64 = 8 * 1024 * 1024
	defaultStallTimeout          = time.Minute
	defaultWorkerQueueSize       = 10000
)

type Config struct {
	Limiter         *tasklimit.Limiter
	SampleSizeBytes int64
	FullHashMaxSize int64
	// RateLimitCooldown is the pause when a throttling error gives no retry time.
	RateLimitCooldown time.Duration
	// StallTimeout ends a remote read that receives no data for this long.
	StallTimeout time.Duration
	HTTPClient   *http.Client
}

type Worker struct {
	Catalog         *catalog.Catalog
	Drive           drives.Drive
	Config          Config
	TaskGuard       func() func()
	OnStatusChanged func(bool)
	// OnContentReplaced receives a row whose recomputed fingerprint showed that
	// its source was replaced in place. Its generated assets were cleared and
	// need to be scheduled again.
	OnContentReplaced func(*catalog.Video)
	// Cooldown is shared by all workers of the drive. The application injects
	// it before Run starts; standalone workers get their own.
	Cooldown *tasklimit.Cooldown

	ch       chan *catalog.Video
	queue    videoQueue
	activity taskActivity
	http     *http.Client
}

type TaskStatus struct {
	State         string
	CurrentTitle  string
	QueueLength   int
	CooldownUntil time.Time
}

func NewWorker(cat *catalog.Catalog, drv drives.Drive, cfg Config) *Worker {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = streamhttp.NewClient(0)
	}
	if cfg.SampleSizeBytes <= 0 {
		cfg.SampleSizeBytes = defaultSampleSizeBytes
	}
	if cfg.FullHashMaxSize <= 0 {
		cfg.FullHashMaxSize = defaultFullHashMaxSize
	}
	if cfg.RateLimitCooldown <= 0 {
		cfg.RateLimitCooldown = drives.DefaultCooldown
	}
	return &Worker{
		Catalog:  cat,
		Drive:    drv,
		Config:   cfg,
		Cooldown: &tasklimit.Cooldown{},
		ch:       make(chan *catalog.Video, defaultWorkerQueueSize),
		http:     hc,
	}
}

func (w *Worker) Enqueue(v *catalog.Video) bool {
	defer w.notifyStatus(false)
	if v == nil {
		return false
	}
	if !w.queue.reserve(v.ID) {
		return true
	}
	select {
	case w.ch <- v:
		return true
	default:
		w.queue.release(v.ID)
		return false
	}
}

func (w *Worker) EnqueueBlocking(ctx context.Context, v *catalog.Video) bool {
	defer w.notifyStatus(false)
	if v == nil {
		return false
	}
	if !w.queue.reserve(v.ID) {
		return true
	}
	select {
	case w.ch <- v:
		return true
	case <-ctx.Done():
		w.queue.release(v.ID)
		return false
	}
}

func (w *Worker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case v := <-w.ch:
			w.processQueued(ctx, v)
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
}

func (w *Worker) Status() TaskStatus {
	if w == nil {
		return TaskStatus{State: "idle"}
	}
	currentID, currentTitle := w.activity.current()
	status := TaskStatus{
		State:        "idle",
		CurrentTitle: currentTitle,
		QueueLength:  w.queue.lengthExcluding(currentID),
	}
	if until, ok := w.Cooldown.Until(time.Now()); ok {
		status.State = "cooling"
		status.CooldownUntil = until
		return status
	}
	if currentID != "" {
		status.State = "generating"
		return status
	}
	if status.QueueLength > 0 {
		status.State = "queued"
	}
	return status
}

// WaitIdle blocks until the fingerprint queue is empty and no item is being processed.
func (w *Worker) WaitIdle(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if w.queue.lengthExcluding("") == 0 {
		return nil
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if w.queue.lengthExcluding("") == 0 {
				return nil
			}
		}
	}
}

func (w *Worker) processQueued(ctx context.Context, v *catalog.Video) {
	if v == nil {
		return
	}
	if w.Catalog == nil || w.Drive == nil || v.ID == "" {
		w.queue.release(v.ID)
		return
	}
	ctx = applog.WithFields(applog.NewTask(ctx, "fingerprint", w.Drive.ID()), applog.Fields{VideoID: v.ID, FileID: v.FileID})
	// A cooling drive waits before taking task admission or shared budget.
	if err := w.Cooldown.Wait(ctx); err != nil {
		w.queue.release(v.ID)
		return
	}
	if w.TaskGuard != nil {
		release := w.TaskGuard()
		if release == nil {
			w.queue.release(v.ID)
			return
		}
		defer release()
	}
	defer func() { w.queue.release(v.ID); w.notifyStatus(true) }()
	if err := ctx.Err(); err != nil {
		return
	}
	current, err := w.Catalog.GetVideo(ctx, v.ID)
	if err != nil {
		if ctx.Err() == nil {
			applog.Error(ctx, "Read queued video failed", err, applog.Fields{Stage: "lookup"})
		}
		return
	}
	if current.DriveID != w.Drive.ID() || current.SampledSHA256 != "" || current.FingerprintStatus == "ready" || current.Hidden {
		return
	}
	release, err := w.Config.Limiter.AcquireAfter(ctx, w.Cooldown)
	if err != nil {
		return
	}
	w.activity.start(current)
	w.notifyStatus(true)
	defer w.activity.done()
	sum, err := compute(ctx, w.Drive, current, w.Config, w.http)
	release()
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		if w.pauseForProviderError(ctx, err) {
			// The video stays pending, and the reconciler queues it again.
			return
		}
		applog.Error(ctx, "Fingerprint computation failed", err, applog.Fields{Stage: "compute"})
		_, _ = w.Catalog.UpdateVideoFingerprintForSource(ctx, current, "", "failed", err.Error())
		return
	}
	replaced, err := w.Catalog.UpdateVideoFingerprintForSource(ctx, current, sum, "ready", "")
	if err != nil {
		applog.Error(ctx, "Save fingerprint failed", err, applog.Fields{Stage: "save"})
		return
	}
	if !replaced {
		applog.Info(ctx, "Fingerprint ready", applog.Fields{})
		return
	}
	applog.Info(ctx, "Source replaced in place; regenerating its assets", applog.Fields{})
	if w.OnContentReplaced != nil {
		if updated, err := w.Catalog.GetVideo(ctx, current.ID); err == nil {
			w.OnContentReplaced(updated)
		}
	}
}

// pauseForProviderError pauses the drive's provider work and reports true when
// err says nothing about the file itself: the provider is throttling or briefly
// unavailable, or the read timed out or lost its connection.
func (w *Worker) pauseForProviderError(ctx context.Context, err error) bool {
	wait, limited := drives.RateLimitRetryAfter(err)
	if !limited && !drives.ShouldCooldown(w.Drive.Kind(), err) && !transientTransportError(err) {
		return false
	}
	if wait <= 0 {
		wait = w.Config.RateLimitCooldown
	}
	until := w.Cooldown.Pause(time.Now(), wait)
	w.notifyStatus(true)
	applog.Warn(ctx, "Fingerprint paused; drive cooling down until "+until.Format(time.RFC3339), err, applog.Fields{Stage: "compute"})
	return true
}

func transientTransportError(err error) bool {
	var timeout interface{ Timeout() bool }
	return (errors.As(err, &timeout) && timeout.Timeout()) || errors.Is(err, syscall.ECONNRESET)
}

func Compute(ctx context.Context, drv drives.Drive, v *catalog.Video, cfg Config, hc *http.Client) (string, error) {
	release, err := cfg.Limiter.Acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	return compute(ctx, drv, v, cfg, hc)
}

// compute runs after the caller has acquired the shared fingerprint budget.
func compute(ctx context.Context, drv drives.Drive, v *catalog.Video, cfg Config, hc *http.Client) (string, error) {
	if drv == nil {
		return "", errors.New("fingerprint: nil drive")
	}
	if v == nil {
		return "", errors.New("fingerprint: nil video")
	}
	if v.Size <= 0 {
		return "", errors.New("fingerprint: video size is empty")
	}
	if cfg.SampleSizeBytes <= 0 {
		cfg.SampleSizeBytes = defaultSampleSizeBytes
	}
	if cfg.FullHashMaxSize <= 0 {
		cfg.FullHashMaxSize = defaultFullHashMaxSize
	}
	if cfg.StallTimeout <= 0 {
		cfg.StallTimeout = defaultStallTimeout
	}
	if hc == nil {
		hc = streamhttp.NewClient(0)
	}
	link, err := drv.StreamURL(ctx, v.FileID)
	if err != nil {
		return "", fmt.Errorf("fingerprint: stream url: %w", err)
	}
	if link == nil || strings.TrimSpace(link.URL) == "" {
		return "", errors.New("fingerprint: empty stream url")
	}
	ranges := sampleRanges(v.Size, cfg.SampleSizeBytes, cfg.FullHashMaxSize)
	h := sha256.New()
	writeHashHeader(h, v.Size, ranges)
	for _, r := range ranges {
		data, err := readRange(ctx, hc, link, r, cfg.StallTimeout)
		if isP115ForbiddenRangeError(drv, err) {
			// A 115 signed CDN URL can be rejected before its advertised expiry.
			// Refresh it once and retry the same byte range.
			refreshed, refreshErr := drv.StreamURL(ctx, v.FileID)
			if refreshErr != nil {
				err = fmt.Errorf("fingerprint: refresh stream url after range rejection: %w", refreshErr)
			} else if refreshed == nil || strings.TrimSpace(refreshed.URL) == "" {
				err = errors.New("fingerprint: refreshed stream url is empty")
			} else {
				link = refreshed
				data, err = readRange(ctx, hc, link, r, cfg.StallTimeout)
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
		if int64(len(data)) != r.length {
			return "", fmt.Errorf("fingerprint: short sample at %d: got %d want %d", r.start, len(data), r.length)
		}
		_, _ = h.Write([]byte(fmt.Sprintf("offset=%d length=%d\n", r.start, r.length)))
		_, _ = h.Write(data)
		_, _ = h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type byteRange struct {
	start  int64
	length int64
}

type rangeResponseError struct {
	status int
	start  int64
	end    int64
}

func (e *rangeResponseError) Error() string {
	if e == nil {
		return "fingerprint: range request failed"
	}
	return fmt.Sprintf("fingerprint: range request got status=%d for bytes=%d-%d", e.status, e.start, e.end)
}

func isP115ForbiddenRangeError(drv drives.Drive, err error) bool {
	if drv == nil || !strings.EqualFold(drv.Kind(), "p115") || err == nil {
		return false
	}
	var responseErr *rangeResponseError
	return errors.As(err, &responseErr) && responseErr.status == http.StatusForbidden
}

func sampleRanges(size, sampleSize, fullHashMax int64) []byteRange {
	if size <= fullHashMax {
		return []byteRange{{start: 0, length: size}}
	}
	if sampleSize > size {
		sampleSize = size
	}
	maxStart := size - sampleSize
	percents := []int64{0, 20, 40, 60, 80}
	out := make([]byteRange, 0, len(percents))
	seen := make(map[int64]struct{}, len(percents))
	for _, pct := range percents {
		start := maxStart * pct / 100
		if _, ok := seen[start]; ok {
			continue
		}
		seen[start] = struct{}{}
		out = append(out, byteRange{start: start, length: sampleSize})
	}
	return out
}

func writeHashHeader(w io.Writer, size int64, ranges []byteRange) {
	// This hash domain is persisted in fingerprints; application renames must not change it.
	_, _ = fmt.Fprintf(w, "video-site-sampled-sha256-v1\nsize=%d\nsamples=%d\n", size, len(ranges))
}

func readRange(ctx context.Context, hc *http.Client, link *drives.StreamLink, r byteRange, stall time.Duration) ([]byte, error) {
	u, err := url.Parse(link.URL)
	if err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		return readHTTPRange(ctx, hc, link, r, stall)
	}
	path := link.URL
	if err == nil && u.Scheme == "file" {
		path = u.Path
	}
	return readLocalRange(path, r)
}

func readLocalRange(path string, r byteRange) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: open local stream: %w", err)
	}
	defer f.Close()
	buf := make([]byte, r.length)
	n, err := f.ReadAt(buf, r.start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("fingerprint: read local sample: %w", err)
	}
	if int64(n) != r.length {
		return nil, fmt.Errorf("fingerprint: read local sample at %d: got %d want %d", r.start, n, r.length)
	}
	return buf, nil
}

// errStalled marks a remote read that received no data within the stall
// timeout. It is a timeout, so the drive cools down and the video is retried.
var errStalled = stalledError{}

type stalledError struct{}

func (stalledError) Error() string { return "remote read stalled" }
func (stalledError) Timeout() bool { return true }

// readHTTPRange ends the request when the response stops making progress.
// The client has no overall timeout because samples can be large on slow
// links, and a connection that stops sending would otherwise hold the worker
// and its shared budget until shutdown.
func readHTTPRange(ctx context.Context, hc *http.Client, link *drives.StreamLink, r byteRange, stall time.Duration) ([]byte, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	watchdog := time.AfterFunc(stall, func() { cancel(errStalled) })
	defer watchdog.Stop()
	data, err := fetchHTTPRange(ctx, hc, link, r, func() { watchdog.Reset(stall) })
	if err != nil && errors.Is(context.Cause(ctx), errStalled) {
		return nil, fmt.Errorf("fingerprint: sample at %d received no data for %s: %w", r.start, stall, errStalled)
	}
	return data, err
}

func fetchHTTPRange(ctx context.Context, hc *http.Client, link *drives.StreamLink, r byteRange, progress func()) ([]byte, error) {
	end := r.start + r.length - 1
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.URL, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range link.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", r.start, end))
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: read remote sample: %w", err)
	}
	defer resp.Body.Close()
	progress()
	body := progressReader{r: resp.Body, progress: progress}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &drives.RateLimitError{
			Provider:   "fingerprint",
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Err:        fmt.Errorf("remote sample rate limited: status=%d", resp.StatusCode),
		}
	}
	if resp.StatusCode != http.StatusPartialContent {
		if resp.StatusCode == http.StatusOK && r.start == 0 {
			data, err := io.ReadAll(io.LimitReader(body, r.length+1))
			if err != nil {
				return nil, err
			}
			if int64(len(data)) == r.length {
				return data, nil
			}
		}
		return nil, &rangeResponseError{status: resp.StatusCode, start: r.start, end: end}
	}
	return io.ReadAll(io.LimitReader(body, r.length))
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

func parseRetryAfter(raw string) time.Duration {
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

type taskActivity struct {
	mu           sync.Mutex
	currentID    string
	currentTitle string
}

func (a *taskActivity) start(v *catalog.Video) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if v == nil {
		a.currentID = ""
		a.currentTitle = ""
		return
	}
	a.currentID = v.ID
	a.currentTitle = v.Title
}

func (a *taskActivity) done() {
	a.mu.Lock()
	a.currentID = ""
	a.currentTitle = ""
	a.mu.Unlock()
}

func (a *taskActivity) current() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.currentID, a.currentTitle
}

type videoQueue struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

func (q *videoQueue) reserve(id string) bool {
	if id == "" {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ids == nil {
		q.ids = make(map[string]struct{})
	}
	if _, ok := q.ids[id]; ok {
		return false
	}
	q.ids[id] = struct{}{}
	return true
}

func (q *videoQueue) release(id string) {
	if id == "" {
		return
	}
	q.mu.Lock()
	delete(q.ids, id)
	q.mu.Unlock()
}

func (q *videoQueue) lengthExcluding(currentID string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.ids)
	if currentID != "" {
		if _, ok := q.ids[currentID]; ok {
			n--
		}
	}
	if n < 0 {
		return 0
	}
	return n
}

func (w *Worker) notifyStatus(immediate bool) {
	if w.OnStatusChanged != nil {
		w.OnStatusChanged(immediate)
	}
}
