package fingerprint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/generation"
)

func TestComputeLocalFilesWithSameContentMatch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	body := []byte("same video bytes")
	a := filepath.Join(dir, "a.mp4")
	b := filepath.Join(dir, "b.mp4")
	if err := os.WriteFile(a, body, 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(b, body, 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}

	sumA, err := Compute(ctx, &fakeDrive{paths: map[string]string{"a": a}}, &catalog.Video{ID: "a", FileID: "a", Size: int64(len(body))}, Config{}, nil)
	if err != nil {
		t.Fatalf("compute a: %v", err)
	}
	sumB, err := Compute(ctx, &fakeDrive{paths: map[string]string{"b": b}}, &catalog.Video{ID: "b", FileID: "b", Size: int64(len(body))}, Config{}, nil)
	if err != nil {
		t.Fatalf("compute b: %v", err)
	}
	if sumA == "" || sumA != sumB {
		t.Fatalf("fingerprints = %q / %q, want same non-empty", sumA, sumB)
	}
}

func TestComputeRemoteUsesRangeSamples(t *testing.T) {
	ctx := context.Background()
	data := make([]byte, 10*1024*1024)
	for i := range data {
		data[i] = byte(i % 251)
	}
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawRange := r.Header.Get("Range")
		ranges = append(ranges, rawRange)
		var start, end int
		if _, err := fmt.Sscanf(rawRange, "bytes=%d-%d", &start, &end); err != nil {
			t.Fatalf("bad range %q: %v", rawRange, err)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer srv.Close()

	drv := &fakeDrive{paths: map[string]string{"remote": srv.URL + "/video.mp4"}}
	sum, err := Compute(ctx, drv, &catalog.Video{ID: "remote", FileID: "remote", Size: int64(len(data))}, Config{}, srv.Client())
	if err != nil {
		t.Fatalf("compute remote: %v", err)
	}
	if !strings.HasPrefix(sum, dedupe.SampledFingerprintPrefix) {
		t.Fatalf("fingerprint = %q, want the current scheme prefix", sum)
	}
	// Head, middle and tail of a 10 MiB file, 512 KiB each.
	want := []string{
		"bytes=0-524287",
		"bytes=4980736-5505023",
		"bytes=9961472-10485759",
	}
	if fmt.Sprint(ranges) != fmt.Sprint(want) {
		t.Fatalf("ranges = %#v, want %#v", ranges, want)
	}
}

func TestComputeRemoteFollowsWebDAV302WithoutImplicitReferer(t *testing.T) {
	ctx := context.Background()
	data := []byte("0123456789")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Referer"); got != "" {
			http.Error(w, "signed download rejects Referer", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization leaked to redirect target: %q", got)
		}
		if got := r.Header.Get("Cookie"); got != "" {
			t.Errorf("Cookie leaked to redirect target: %q", got)
		}
		if got := r.Header.Get("Range"); got != "bytes=0-9" {
			t.Errorf("Range = %q, want bytes=0-9", got)
		}
		w.Header().Set("Content-Range", "bytes 0-9/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data)
	}))
	t.Cleanup(cdn.Close)

	openList := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Basic secret" {
			t.Errorf("OpenList Authorization = %q, want Basic secret", got)
		}
		http.Redirect(w, r, cdn.URL+"/signed/video.mp4", http.StatusFound)
	}))
	t.Cleanup(openList.Close)

	drv := &fakeDrive{
		paths: map[string]string{"remote": openList.URL + "/dav/video.mp4"},
		headers: http.Header{
			"Authorization": {"Basic secret"},
			"Cookie":        {"dav_session=secret"},
		},
	}
	sum, err := Compute(ctx, drv, &catalog.Video{
		ID:     "remote",
		FileID: "remote",
		Size:   int64(len(data)),
	}, Config{}, nil)
	if err != nil {
		t.Fatalf("compute redirected remote: %v", err)
	}
	if sum == "" {
		t.Fatal("fingerprint should not be empty")
	}
}

func TestComputeRemote429ReturnsRateLimit(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429}}`))
	}))
	defer srv.Close()

	drv := &fakeDrive{paths: map[string]string{"remote": srv.URL + "/video.mp4"}}
	_, err := Compute(ctx, drv, &catalog.Video{ID: "remote", FileID: "remote", Size: 1024 * 1024}, Config{}, srv.Client())
	if err == nil {
		t.Fatal("compute succeeded, want rate limit")
	}
	var rateLimit *drives.RateLimitError
	if !errors.As(err, &rateLimit) {
		t.Fatalf("error = %T %[1]v, want RateLimitError", err)
	}
	if rateLimit.RetryAfter != time.Minute {
		t.Fatalf("retry after = %s, want 1m", rateLimit.RetryAfter)
	}
}

func TestComputeP115RefreshesRejectedRangeLinkOnce(t *testing.T) {
	ctx := context.Background()
	data := []byte("0123456789")
	rejected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "signed url rejected", http.StatusForbidden)
	}))
	t.Cleanup(rejected.Close)
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=0-9" {
			t.Errorf("Range = %q, want bytes=0-9", got)
		}
		w.Header().Set("Content-Range", "bytes 0-9/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data)
	}))
	t.Cleanup(ready.Close)

	drv := &fakeDrive{kind: "p115", streamURLs: []string{
		rejected.URL + "/expired.mp4",
		ready.URL + "/refreshed.mp4",
	}}
	sum, err := Compute(ctx, drv, &catalog.Video{
		ID: "p115-refresh", FileID: "remote", Size: int64(len(data)),
	}, Config{}, ready.Client())
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if sum == "" {
		t.Fatal("fingerprint should not be empty")
	}
	if drv.streamCalls != 2 {
		t.Fatalf("StreamURL calls = %d, want initial link plus one refresh", drv.streamCalls)
	}
}

func TestComputeP115PersistentRangeForbiddenReturnsRateLimit(t *testing.T) {
	ctx := context.Background()
	rejected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "signed url rejected", http.StatusForbidden)
	}))
	t.Cleanup(rejected.Close)

	drv := &fakeDrive{kind: "p115", streamURLs: []string{rejected.URL, rejected.URL}}
	_, err := Compute(ctx, drv, &catalog.Video{
		ID: "p115-persistent-403", FileID: "remote", Size: 10,
	}, Config{}, rejected.Client())
	if err == nil {
		t.Fatal("Compute succeeded, want rate limit")
	}
	var rateLimit *drives.RateLimitError
	if !errors.As(err, &rateLimit) {
		t.Fatalf("error = %T %[1]v, want RateLimitError", err)
	}
	if rateLimit.Provider != "p115" || rateLimit.RetryAfter != 0 {
		t.Fatalf("rate limit = %#v, want p115 without a provider retry time", rateLimit)
	}
	if drv.streamCalls != 2 {
		t.Fatalf("StreamURL calls = %d, want exactly one refresh", drv.streamCalls)
	}
}

func TestJobP115PersistentRangeForbiddenKeepsFingerprintPending(t *testing.T) {
	ctx := context.Background()
	rejected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "signed url rejected", http.StatusForbidden)
	}))
	t.Cleanup(rejected.Close)

	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	video := &catalog.Video{
		ID:                "p115-worker-403",
		DriveID:           "fake",
		FileID:            "remote",
		Title:             "Clip",
		Size:              10,
		FingerprintStatus: "pending",
		PublishedAt:       time.Now(),
		CreatedAt:         time.Now(),
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	drv := &fakeDrive{kind: "p115", streamURLs: []string{rejected.URL, rejected.URL}}
	job := NewJob(cat, drv, Config{
		RateLimitCooldown: time.Millisecond,
		HTTPClient:        rejected.Client(),
	})
	if got := job.Run(ctx, video); got != generation.StepPaused {
		t.Fatalf("outcome = %v, want paused for a rejected signed link", got)
	}

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.FingerprintStatus != "pending" || got.FingerprintError != "" {
		t.Fatalf("fingerprint status/error = %q/%q, want pending with no permanent error", got.FingerprintStatus, got.FingerprintError)
	}
}

func TestJobResetsAssetsWhenVerificationFindsNewBytes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	drv := &fakeDrive{paths: map[string]string{"file": path}}
	video := &catalog.Video{ID: "clip", DriveID: "fake", FileID: "file", Title: "Clip", Size: 8, SourceModTime: "t1",
		ThumbnailURL: "/p/thumb/clip", PreviewStatus: "ready", PreviewLocal: "clip.mp4"}
	video.SampledSHA256, err = Compute(ctx, drv, video, Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatal(err)
	}
	// A scan saw a new timestamp; the same-size bytes were replaced.
	if err := cat.UpdateVideoMeta(ctx, video.ID, catalog.VideoMetaPatch{ExpectedSource: video, VerifyContent: true, SourceModTime: "t2"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replaced"), 0o644); err != nil {
		t.Fatal(err)
	}
	current, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := NewJob(cat, drv, Config{}).Run(ctx, current); got != generation.StepSettled {
		t.Fatalf("verification outcome = %v, want settled", got)
	}
	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil || got.PreviewStatus != "pending" || got.ThumbnailURL != "" || got.SampledSHA256 == video.SampledSHA256 {
		t.Fatalf("video = %+v, %v; want reset row with the new fingerprint", got, err)
	}
}

func TestJobProviderStatusCoolsDownOnlyForItsKind(t *testing.T) {
	ctx := context.Background()
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(unavailable.Close)

	for _, tc := range []struct {
		kind        string
		wantStatus  string
		wantCooling bool
	}{
		{kind: "pikpak", wantStatus: "pending", wantCooling: true},
		{kind: "webdav", wantStatus: "failed", wantCooling: false},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			cat := openTestCatalog(t)
			video := seedPendingVideo(t, cat, "status-"+tc.kind, 10)
			job := NewJob(cat, &fakeDrive{kind: tc.kind, paths: map[string]string{"remote": unavailable.URL}}, Config{
				HTTPClient: unavailable.Client(),
			})
			wantOutcome := generation.StepSettled
			if tc.wantCooling {
				wantOutcome = generation.StepPaused
			}
			if got := job.Run(ctx, video); got != wantOutcome {
				t.Fatalf("outcome = %v, want %v", got, wantOutcome)
			}

			got, err := cat.GetVideo(ctx, video.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.FingerprintStatus != tc.wantStatus {
				t.Fatalf("fingerprint status = %q, want %q", got.FingerprintStatus, tc.wantStatus)
			}
			if _, cooling := job.Cooldown.Until(time.Now()); cooling != tc.wantCooling {
				t.Fatalf("cooling = %v, want %v", cooling, tc.wantCooling)
			}
		})
	}
}

func TestComputeStalledRemoteReadTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-9/10")
		w.WriteHeader(http.StatusPartialContent)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	start := time.Now()
	_, err := Compute(context.Background(), &fakeDrive{paths: map[string]string{"remote": srv.URL}}, &catalog.Video{
		ID: "stalled", FileID: "remote", Size: 10,
	}, Config{StallTimeout: 50 * time.Millisecond}, srv.Client())
	if !errors.Is(err, errStalled) || !transientTransportError(err) {
		t.Fatalf("error = %v, want a transient stall", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("stalled read returned after %s", elapsed)
	}
}

func TestComputeSlowProgressingReadIsNotStalled(t *testing.T) {
	data := []byte("0123456789")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-9/10")
		w.WriteHeader(http.StatusPartialContent)
		for _, b := range data {
			// Each gap is under the stall timeout, but the whole read exceeds it.
			time.Sleep(20 * time.Millisecond)
			_, _ = w.Write([]byte{b})
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)

	sum, err := Compute(context.Background(), &fakeDrive{paths: map[string]string{"remote": srv.URL}}, &catalog.Video{
		ID: "slow", FileID: "remote", Size: int64(len(data)),
	}, Config{StallTimeout: 80 * time.Millisecond}, srv.Client())
	if err != nil || sum == "" {
		t.Fatalf("slow read = %q/%v, want a fingerprint", sum, err)
	}
}

func TestJobSkipsFingerprintedVideoWithoutReadingTheDrive(t *testing.T) {
	cat := openTestCatalog(t)
	video := seedPendingVideo(t, cat, "done", 10)
	if _, err := cat.UpdateVideoFingerprintForSource(context.Background(), video, "s2:abc", "ready", ""); err != nil {
		t.Fatal(err)
	}
	current, err := cat.GetVideo(context.Background(), video.ID)
	if err != nil {
		t.Fatal(err)
	}
	drv := &fakeDrive{}
	if got := NewJob(cat, drv, Config{}).Run(context.Background(), current); got != generation.StepSettled || drv.streamCalls != 0 {
		t.Fatalf("job read the drive for a fingerprinted video: outcome=%v calls=%d", got, drv.streamCalls)
	}
}

func openTestCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	return cat
}

func seedPendingVideo(t *testing.T, cat *catalog.Catalog, id string, size int64) *catalog.Video {
	t.Helper()
	video := &catalog.Video{
		ID: id, DriveID: "fake", FileID: "remote", Title: id, Size: size,
		FingerprintStatus: "pending", PublishedAt: time.Now(), CreatedAt: time.Now(),
	}
	if err := cat.UpsertVideo(context.Background(), video); err != nil {
		t.Fatal(err)
	}
	return video
}

func TestSampleRangesCoverHeadMiddleAndTail(t *testing.T) {
	whole := sampleCount * sampleSize
	if got := sampleRanges(whole); len(got) != 1 || got[0] != (byteRange{start: 0, length: whole}) {
		t.Fatalf("ranges for %d bytes = %+v, want the whole file", whole, got)
	}
	size := whole + 1
	got := sampleRanges(size)
	want := []byteRange{
		{start: 0, length: sampleSize},
		{start: (size - sampleSize) / 2, length: sampleSize},
		{start: size - sampleSize, length: sampleSize},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ranges for %d bytes = %+v, want %+v", size, got, want)
	}
	for i := 1; i < len(got); i++ {
		if got[i].start < got[i-1].start+got[i-1].length {
			t.Fatalf("samples overlap: %+v", got)
		}
	}
}

func TestComputeDetectsDifferentTail(t *testing.T) {
	dir := t.TempDir()
	data := make([]byte, 4*1024*1024)
	for i := range data {
		data[i] = byte(i % 251)
	}
	truncated := append([]byte(nil), data...)
	// A download that stopped early leaves the tail unwritten.
	clear(truncated[len(truncated)-1024:])
	sums := make([]string, 0, 2)
	for name, body := range map[string][]byte{"complete": data, "truncated": truncated} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		sum, err := Compute(context.Background(), &fakeDrive{paths: map[string]string{"f": path}}, &catalog.Video{FileID: "f", Size: int64(len(body))}, Config{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		sums = append(sums, sum)
	}
	if sums[0] == sums[1] {
		t.Fatal("files differing only in the tail got the same fingerprint")
	}
}

func TestComputeRejectsSourceWithAnotherSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(path, []byte("grown clip"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Compute(context.Background(), &fakeDrive{paths: map[string]string{"f": path}}, &catalog.Video{FileID: "f", Size: 4}, Config{}, nil)
	if !errors.Is(err, errSizeChanged) {
		t.Fatalf("local error = %v, want size changed", err)
	}

	data := make([]byte, 4*1024*1024)
	for _, tc := range []struct {
		name         string
		contentRange func(start, end int) string
		sizeChanged  bool
	}{
		{name: "other total", sizeChanged: true, contentRange: func(start, end int) string { return fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)+1) }},
		{name: "other range", contentRange: func(start, end int) string { return fmt.Sprintf("bytes %d-%d/%d", start+1, end+1, len(data)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var start, end int
				_, _ = fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
				w.Header().Set("Content-Range", tc.contentRange(start, end))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(data[start : end+1])
			}))
			defer srv.Close()
			_, err := Compute(context.Background(), &fakeDrive{paths: map[string]string{"f": srv.URL}}, &catalog.Video{FileID: "f", Size: int64(len(data))}, Config{}, srv.Client())
			if err == nil || errors.Is(err, errSizeChanged) != tc.sizeChanged {
				t.Fatalf("Compute error = %v, want rejection with sizeChanged=%v", err, tc.sizeChanged)
			}
		})
	}
}

type fakeDrive struct {
	kind        string
	paths       map[string]string
	streamURLs  []string
	streamCalls int
	headers     http.Header
}

func (d *fakeDrive) Kind() string {
	if d.kind != "" {
		return d.kind
	}
	return "fake"
}
func (d *fakeDrive) ID() string { return "fake" }
func (d *fakeDrive) Init(context.Context) error {
	return nil
}
func (d *fakeDrive) List(context.Context, string) ([]drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *fakeDrive) Stat(context.Context, string) (*drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *fakeDrive) StreamURL(_ context.Context, fileID string) (*drives.StreamLink, error) {
	d.streamCalls++
	streamURL := ""
	if len(d.streamURLs) > 0 {
		index := d.streamCalls - 1
		if index >= len(d.streamURLs) {
			index = len(d.streamURLs) - 1
		}
		streamURL = d.streamURLs[index]
	} else {
		streamURL = d.paths[fileID]
	}
	return &drives.StreamLink{
		URL:     streamURL,
		Headers: d.headers.Clone(),
		Expires: time.Now().Add(time.Minute),
	}, nil
}
func (d *fakeDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *fakeDrive) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *fakeDrive) RootID() string { return "root" }
