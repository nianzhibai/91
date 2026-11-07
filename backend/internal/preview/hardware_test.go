package preview

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

func hardwareFixture(t *testing.T) (*Hardware, *Generator, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls")
	t.Setenv("GPU_TEST_LOG", logPath)
	ffmpeg := filepath.Join(dir, "ffmpeg")
	writeExecutable(t, ffmpeg, `#!/bin/sh
printf '%s\n' "$*" >> "$GPU_TEST_LOG"
out=""
for arg in "$@"; do out="$arg"; done
case " $* " in
  *" -hwaccel "*)
    if [ "$GPU_TEST_FAIL" = "probe" ]; then
      case " $* " in
        *" -hwaccel rkmpp "*)
          case "$out" in *teaser-seg-*) printf "No such filter: scale_rkrga" >&2; exit 1 ;; esac ;;
      esac
    fi
    if [ "$GPU_TEST_PAUSE" = "yes" ] && [ ! -e "$GPU_TEST_MARKER" ]; then
      touch "$GPU_TEST_MARKER"
      while [ ! -e "$GPU_TEST_RELEASE" ]; do sleep 0.01; done
    fi
    case "$out" in
      *teaser-seg-1-*)
        if [ "$GPU_TEST_FAIL" = "segment" ]; then
          printf 'Failed to initialise VAAPI connection' >&2
          exit 1
        fi ;;
      *.jpg)
        if [ "$GPU_TEST_FAIL" = "thumbnail" ]; then
          printf 'Failed setup for format cuda: unsupported bit depth' >&2
          exit 1
        fi ;;
    esac ;;
esac
printf 'media-output' > "$out"
`)
	ffprobe := filepath.Join(dir, "ffprobe")
	writeExecutable(t, ffprobe, "#!/bin/sh\nprintf '%s' '{\"streams\":[{\"codec_type\":\"video\",\"duration\":\"3.0\"}],\"format\":{\"duration\":\"3.0\"}}'\n")
	cfg := Config{FFmpegPath: ffmpeg, FFprobePath: ffprobe, Width: 128, LocalDir: filepath.Join(dir, "previews")}
	hardware := NewHardware(cfg)
	cfg.Hardware = hardware
	return hardware, New(cfg), logPath
}

func TestHardwareProbeVerifiesBothAssetsAndCachesResults(t *testing.T) {
	h, _, logPath := hardwareFixture(t)
	if s := h.Status(); s.Enabled || s.Available || s.Checked {
		t.Fatalf("CPU default = %+v", s)
	}
	s := h.Check(context.Background())
	if !s.Checked || !s.Available || s.Enabled || s.Backend == "" {
		t.Fatalf("probe = %+v", s)
	}
	calls, _ := os.ReadFile(logPath)
	requiredOptions := []string{"testsrc2=", "-frames:v 1", "-hwaccel ", "teaser-seg-", "fade=t=in", "hwdownload"}
	if s.Backend != "rkmpp" {
		requiredOptions = append(requiredOptions, "hwupload")
	}
	for _, required := range requiredOptions {
		if !strings.Contains(string(calls), required) {
			t.Fatalf("probe did not exercise %s:\n%s", required, calls)
		}
	}
	h.Check(context.Background())
	h.SetEnabled(context.Background(), true)
	after, _ := os.ReadFile(logPath)
	if string(calls) != string(after) {
		t.Fatal("cached checks or enabling probed hardware again")
	}
	if !h.Status().Enabled {
		t.Fatal("GPU was not enabled")
	}
	h.Probe(context.Background())
	after, _ = os.ReadFile(logPath)
	if len(after) <= len(calls) {
		t.Fatal("explicit probe did not retry")
	}
}

func TestGPUThumbnailFailureRetriesCPUOnce(t *testing.T) {
	for _, kind := range []string{"cuda", "rkmpp"} {
		t.Run(kind, func(t *testing.T) {
			h, gen, logPath := hardwareFixture(t)
			h.checked, h.enabled = true, true
			h.backend = &hardwareBackend{kind: kind, device: "0"}
			t.Setenv("GPU_TEST_FAIL", "thumbnail")
			path, err := gen.GenerateThumbnail(context.Background(), &drives.StreamLink{URL: "source.mp4"}, "video", 100)
			if err != nil {
				t.Fatal(err)
			}
			if data, _ := os.ReadFile(path); string(data) != "media-output" {
				t.Fatal("CPU cover was not published")
			}
			calls, _ := os.ReadFile(logPath)
			lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
			if len(lines) != 2 || !strings.Contains(lines[0], "-hwaccel "+kind) || strings.Contains(lines[1], "-hwaccel") {
				t.Fatalf("expected one GPU attempt then CPU:\n%s", calls)
			}
			if h.Status().FallbackCount != 1 {
				t.Fatalf("fallbacks = %d", h.Status().FallbackCount)
			}
		})
	}
}

func TestGPUMidPreviewFailureRestartsWholeAssetWithCPU(t *testing.T) {
	for _, kind := range []string{"vaapi", "rkmpp"} {
		t.Run(kind, func(t *testing.T) {
			h, gen, logPath := hardwareFixture(t)
			h.checked, h.enabled = true, true
			h.backend = &hardwareBackend{kind: kind, device: "/dev/dri/renderD128"}
			t.Setenv("GPU_TEST_FAIL", "segment")
			path, err := gen.Generate(context.Background(), &drives.StreamLink{URL: "source.mp4"}, 300)
			if err != nil {
				t.Fatal(err)
			}
			calls, _ := os.ReadFile(logPath)
			var gpuSegments, cpuSegments int
			for _, line := range strings.Split(string(calls), "\n") {
				if !strings.Contains(line, "-vf") {
					continue
				}
				if strings.Contains(line, "-hwaccel") {
					gpuSegments++
				} else {
					cpuSegments++
					if !strings.Contains(line, "-c:v libx264") {
						t.Fatalf("CPU attempt retained hardware encoder: %s", line)
					}
				}
			}
			if gpuSegments != 2 || cpuSegments != 4 || h.Status().FallbackCount != 1 {
				t.Fatalf("GPU=%d CPU=%d status=%+v\n%s", gpuSegments, cpuSegments, h.Status(), calls)
			}
			entries, _ := os.ReadDir(gen.cfg.LocalDir)
			if len(entries) != 1 || filepath.Join(gen.cfg.LocalDir, entries[0].Name()) != path {
				t.Fatalf("partial GPU/CPU segments were not cleaned up: %v", entries)
			}
		})
	}
}

func TestDisablingGPUOnlyAffectsSubsequentAssets(t *testing.T) {
	h, gen, logPath := hardwareFixture(t)
	h.checked, h.enabled = true, true
	h.backend = &hardwareBackend{kind: "cuda", device: "0"}
	marker, release := logPath+".started", logPath+".release"
	t.Setenv("GPU_TEST_PAUSE", "yes")
	t.Setenv("GPU_TEST_MARKER", marker)
	t.Setenv("GPU_TEST_RELEASE", release)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := gen.Generate(ctx, &drives.StreamLink{URL: "source.mp4"}, 300)
		done <- err
	}()
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("preview did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.SetEnabled(ctx, false)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := gen.GenerateThumbnail(ctx, &drives.StreamLink{URL: "source.mp4"}, "next", 100); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(logPath)
	if strings.Count(string(calls), "-hwaccel cuda") != 4 {
		t.Fatalf("in-flight preview changed encoder after disable:\n%s", calls)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if strings.Contains(lines[len(lines)-1], "-hwaccel") {
		t.Fatal("subsequent cover still used GPU")
	}
}

func TestGPUFallbackDoesNotRetryNetworkErrorsOrCancellation(t *testing.T) {
	for _, message := range []string{
		"ffmpeg segment: HTTP 429 Too Many Requests (cuda)",
		"ffmpeg segment: Server returned 403 Forbidden (vaapi)",
		"ffmpeg segment: Connection timed out (nvenc)",
		"ffmpeg segment: Invalid data found when processing input",
	} {
		if hardwareFailure(&hardwareProcessingError{err: errors.New(message)}) {
			t.Errorf("unrelated failure classified as GPU: %s", message)
		}
	}
	if !hardwareFailure(&hardwareProcessingError{err: errors.New("Cannot allocate memory")}) {
		t.Fatal("opaque driver failure did not allow CPU fallback")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gen := &Generator{hardware: &hardwareBackend{kind: "cuda"}}
	if gen.fallbackToCPU(ctx, errors.New("cuda device failed")) {
		t.Fatal("cancellation started a CPU retry")
	}
}

func TestUnavailableGPUKeepsCPUGenerationWorking(t *testing.T) {
	h, gen, _ := hardwareFixture(t)
	h.cfg.FFmpegPath = filepath.Join(t.TempDir(), "missing-ffmpeg")
	h.SetEnabled(context.Background(), true)
	s := h.Status()
	if !s.Enabled || !s.Checked || s.Available || s.Reason == "" {
		t.Fatalf("unavailable state = %+v", s)
	}
	if _, err := gen.GenerateThumbnail(context.Background(), &drives.StreamLink{URL: "source.mp4"}, "cpu", 100); err != nil {
		t.Fatalf("unavailable GPU prevented CPU fallback: %v", err)
	}
}

// Run on a GPU-equipped host to validate the exact production filter chains.
func TestHardwareRealFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	h := NewHardware(Config{})
	status := h.Probe(context.Background())
	if !status.Available {
		if os.Getenv("VIDEO_TEST_GPU") == "1" {
			t.Fatalf("GPU required: %s", status.Reason)
		}
		t.Skipf("no usable GPU: %s", status.Reason)
	}
	t.Logf("verified GPU cover and preview generation: %s %s (%s)", status.Backend, status.Device, status.Name)
}

func TestCanceledProbePreservesLastDetection(t *testing.T) {
	h, _, _ := hardwareFixture(t)
	h.checked = true
	h.backend = &hardwareBackend{kind: "cuda", device: "0"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s := h.Probe(ctx); s.Available || s.Reason == "" {
		t.Fatalf("canceled result = %+v", s)
	}
	if s := h.Status(); !s.Available || s.Backend != "cuda" {
		t.Fatalf("cancellation replaced prior detection: %+v", s)
	}
}
