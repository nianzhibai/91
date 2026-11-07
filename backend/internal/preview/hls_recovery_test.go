package preview

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

func TestHLSProbeStopsOnSegmentRateLimit(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	mediaDir := t.TempDir()
	args := []string{"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10", "-t", "6", "-an", "-c:v", "libx264", "-threads", "1", "-pix_fmt", "yuv420p", "-g", "10", "-sc_threshold", "0", "-hls_time", "1", "-hls_playlist_type", "vod", "-hls_segment_filename", filepath.Join(mediaDir, "segment-%d.ts"), filepath.Join(mediaDir, "index.m3u8")}
	if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("create fixture: %v %s", err, out)
	}
	var limited, afterLimit atomic.Int32
	files := http.FileServer(http.Dir(mediaDir))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limited.Load() > 0 {
			afterLimit.Add(1)
		}
		if r.URL.Path == "/segment-0.ts" {
			limited.Add(1)
			w.Header().Set("Retry-After", "17")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		files.ServeHTTP(w, r)
	}))
	defer server.Close()
	drive := &hlsTestDrive{previewFakeDrive: &previewFakeDrive{kind: "quark"}}
	drive.resolve = func(refresh bool) (*drives.GenerationSource, error) {
		if refresh {
			t.Error("rate limit must not refresh the source")
		}
		return &drives.GenerationSource{Protocol: "hls", Link: &drives.StreamLink{URL: server.URL + "/index.m3u8"}, RefreshStatuses: []int{403, 412}}, nil
	}
	input, err := resolveGenerationInput(ctx, drive, "file")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	gen := New(Config{FFmpegPath: ffmpeg, FFprobePath: ffprobe})
	_, err = input.probe(ctx, func(ctx context.Context, link *drives.StreamLink) (float64, error) {
		duration, err := gen.Probe(ctx, link)
		if delay, ok := drives.RateLimitRetryAfter(err); !ok || delay != 17*time.Second {
			t.Fatalf("command lost upstream rate limit: %v", err)
		}
		return duration, err
	})
	if delay, ok := drives.RateLimitRetryAfter(err); !ok || delay != 17*time.Second {
		t.Fatalf("task lost upstream rate limit: %v", err)
	}
	if limited.Load() != 1 || afterLimit.Load() != 0 || drive.streamCalls != 0 {
		t.Fatalf("limited=%d after limit=%d originals=%d", limited.Load(), afterLimit.Load(), drive.streamCalls)
	}
	if ctx.Err() != nil {
		t.Fatalf("source failure canceled the parent task: %v", ctx.Err())
	}
}

func TestHLSRefreshesBeforeTryingOtherTimestamps(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	// Count actual decoder invocations, not just requests blocked by the relay.
	dir := t.TempDir()
	logPath := filepath.Join(dir, "commands")
	t.Setenv("HLS_REAL_FFMPEG", ffmpeg)
	t.Setenv("HLS_COMMAND_LOG", logPath)
	wrapper := filepath.Join(dir, "ffmpeg")
	writeExecutable(t, wrapper, `#!/bin/sh
case " $* " in
  *" -h demuxer=hls "*) ;;
  *) printf 'decode\n' >> "$HLS_COMMAND_LOG" ;;
esac
exec "$HLS_REAL_FFMPEG" "$@"
`)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusPreconditionFailed)
	}))
	defer server.Close()
	originalErr := errors.New("original unavailable")
	drive := &hlsTestDrive{previewFakeDrive: &previewFakeDrive{kind: "quark", streamErr: originalErr}}
	resolutions := 0
	drive.resolve = func(refresh bool) (*drives.GenerationSource, error) {
		resolutions++
		if refresh != (resolutions == 2) {
			t.Errorf("refresh=%v resolutions=%d", refresh, resolutions)
		}
		return &drives.GenerationSource{Protocol: "hls", Link: &drives.StreamLink{URL: server.URL + "/index.m3u8"}, RefreshStatuses: []int{403, 412}}, nil
	}
	input, err := resolveGenerationInput(ctx, drive, "file")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	outputDir := t.TempDir()
	gen := New(Config{FFmpegPath: wrapper, LocalDir: outputDir})
	err = input.run(ctx, func(ctx context.Context, link *drives.StreamLink) error {
		_, err := gen.Generate(ctx, link, 120)
		if !drives.ErrorMentionsHTTPStatus(err, 412) || errors.Is(err, context.Canceled) {
			t.Fatalf("decoder lost upstream rejection: %v", err)
		}
		return err
	})
	if !errors.Is(err, originalErr) || resolutions != 2 || drive.streamCalls != 1 || requests.Load() != 2 {
		t.Fatalf("err=%v resolutions=%d originals=%d requests=%d", err, resolutions, drive.streamCalls, requests.Load())
	}
	commands, err := os.ReadFile(logPath)
	if err != nil || strings.Count(string(commands), "decode\n") != 2 {
		t.Fatalf("expected one decoder per source: %q, %v", commands, err)
	}
	entries, err := os.ReadDir(outputDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed segments left behind: %v, %v", entries, err)
	}
}

func TestHLSErrorCausePreventsHardwareFallback(t *testing.T) {
	hardware, gen, _ := hardwareFixture(t)
	hardware.checked, hardware.enabled = true, true
	hardware.backend = &hardwareBackend{kind: "cuda", device: "0"}
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("HLS media HTTP status=412")
	cancel(cause)
	_, err := gen.GenerateThumbnail(ctx, &drives.StreamLink{URL: "source.mp4"}, "video", 120)
	if !errors.Is(err, cause) || hardware.Status().FallbackCount != 0 {
		t.Fatalf("err=%v hardware fallbacks=%d", err, hardware.Status().FallbackCount)
	}
}
