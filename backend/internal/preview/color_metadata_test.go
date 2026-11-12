package preview

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

func TestColorRepairPreservesValidMetadataAndTargetsInputStreams(t *testing.T) {
	streams := []sourceColorStream{
		{Index: 1, CodecName: "h264", ColorSpace: "reserved", ColorPrimaries: "bt2020", ColorTransfer: "smpte2084"},
		{Index: 4, CodecName: "hevc", ColorSpace: "bt2020nc", ColorPrimaries: "reserved0", ColorTransfer: "reserved"},
		{Index: 5, CodecName: "h264", ColorSpace: "unknown", ColorPrimaries: "unknown", ColorTransfer: "unknown"},
		{Index: 6, CodecName: "h264", ColorSpace: "bt709", ColorPrimaries: "bt709", ColorTransfer: "bt709"},
		{Index: 7, CodecName: "av1", ColorSpace: "reserved"},
	}
	want := []string{
		"-bsf:1", "h264_metadata=matrix_coefficients=2",
		"-bsf:4", "hevc_metadata=colour_primaries=2:transfer_characteristics=2",
	}
	if got := colorRepairInputOptions(streams); !reflect.DeepEqual(got, want) {
		t.Fatalf("repair options = %v, want %v", got, want)
	}
}

func TestColorRepairIsLazyAndReusedUntilSourceChanges(t *testing.T) {
	dir := t.TempDir()
	ffmpeg := filepath.Join(dir, "ffmpeg")
	ffprobe := filepath.Join(dir, "ffprobe")
	commands := filepath.Join(dir, "commands")
	probes := filepath.Join(dir, "probes")
	writeExecutable(t, ffmpeg, fmt.Sprintf(`#!/bin/sh
printf 'command\n' >> %q
seen_filter=0
for arg in "$@"; do
  case "$arg" in
    -bsf:1) seen_filter=1 ;;
    -i) if [ "$seen_filter" -eq 0 ]; then printf 'Invalid color space' >&2; exit 1; fi ;;
  esac
done
`, commands))
	writeExecutable(t, ffprobe, fmt.Sprintf(`#!/bin/sh
printf 'probe\n' >> %q
printf '%%s' '{"streams":[{"index":1,"codec_name":"h264","color_space":"reserved","color_primaries":"bt2020","color_transfer":"smpte2084"}]}'
`, probes))
	gen := New(Config{FFmpegPath: ffmpeg, FFprobePath: ffprobe})
	var repair sourceColorRepair
	for _, url := range []string{"first.mp4", "first.mp4", "refreshed.mp4"} {
		link := &drives.StreamLink{URL: url}
		if out, err := gen.runMediaCommand(context.Background(), link, link, nil, nil, &repair); err != nil {
			t.Fatalf("generate %s: %v: %s", url, err, out)
		}
	}
	if got := countMarkerLines(t, commands); got != 5 {
		t.Fatalf("ffmpeg calls = %d, want two repaired attempts and one cached attempt", got)
	}
	if got := countMarkerLines(t, probes); got != 2 {
		t.Fatalf("color probes = %d, want once per source URL", got)
	}
	writeExecutable(t, ffmpeg, "#!/bin/sh\nexit 0\n")
	link := &drives.StreamLink{URL: "healthy.mp4"}
	if _, err := gen.runMediaCommand(context.Background(), link, link, nil, nil, &repair); err != nil {
		t.Fatal(err)
	}
	if got := countMarkerLines(t, probes); got != 2 {
		t.Fatalf("healthy command added a probe: %d", got)
	}
}

func TestColorProbeErrorsPreserveGenerationRecovery(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		message string
	}{
		{"rate limit", http.StatusTooManyRequests, "Server returned 429 Too Many Requests"},
		{"expired link", http.StatusForbidden, "Server returned 403 Forbidden"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			ffmpeg, ffprobe := filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "ffprobe")
			commands, probes := filepath.Join(dir, "commands"), filepath.Join(dir, "probes")
			writeExecutable(t, ffmpeg, fmt.Sprintf("#!/bin/sh\nprintf 'command\\n' >> %q\nprintf 'Invalid color space' >&2\nexit 1\n", commands))
			writeExecutable(t, ffprobe, fmt.Sprintf("#!/bin/sh\nprintf 'probe\\n' >> %q\nprintf '%%s' %q >&2\nexit 1\n", probes, test.message))
			gen := New(Config{FFmpegPath: ffmpeg, FFprobePath: ffprobe, LocalDir: filepath.Join(dir, "output")})
			_, err := gen.Generate(context.Background(), &drives.StreamLink{URL: "source.mp4"}, 120)
			if !drives.ErrorMentionsHTTPStatus(err, test.status) {
				t.Fatalf("generation lost probe status %d: %v", test.status, err)
			}
			_, rateLimited := drives.RateLimitRetryAfter(err)
			if want := test.status == http.StatusTooManyRequests; rateLimited != want {
				t.Fatalf("rateLimited = %v, want %v: %v", rateLimited, want, err)
			}
			if want := test.status == http.StatusForbidden; directMediaLinkRefreshAllowed(err) != want {
				t.Fatalf("link refresh allowed = %v, want %v: %v", directMediaLinkRefreshAllowed(err), want, err)
			}
			if teaserSegmentFallbackAllowed(err) || thumbnailOffsetFallbackAllowed(err) {
				t.Fatalf("probe failure should use source recovery rather than timestamp retries: %v", err)
			}
			if got := countMarkerLines(t, commands); got != 1 {
				t.Fatalf("ffmpeg calls = %d, want one", got)
			}
			if got := countMarkerLines(t, probes); got != 1 {
				t.Fatalf("color probes = %d, want one", got)
			}
		})
	}
}

func TestColorRepairRetriesProbeAfterFailureForSameSource(t *testing.T) {
	dir := t.TempDir()
	ffmpeg, ffprobe := filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "ffprobe")
	commands, probes := filepath.Join(dir, "commands"), filepath.Join(dir, "probes")
	writeExecutable(t, ffmpeg, fmt.Sprintf(`#!/bin/sh
printf 'command\n' >> %q
for arg in "$@"; do
  if [ "$arg" = '-bsf:0' ]; then exit 0; fi
done
printf 'Invalid color space' >&2
exit 1
`, commands))
	writeExecutable(t, ffprobe, fmt.Sprintf("#!/bin/sh\nprintf 'probe\\n' >> %q\nprintf 'Server returned 429 Too Many Requests' >&2\nexit 1\n", probes))
	gen := New(Config{FFmpegPath: ffmpeg, FFprobePath: ffprobe})
	link := &drives.StreamLink{URL: "source.mp4"}
	var repair sourceColorRepair
	_, err := gen.runMediaCommand(context.Background(), link, link, nil, nil, &repair)
	if _, ok := drives.RateLimitRetryAfter(err); !ok {
		t.Fatalf("probe rate limit was lost: %v", err)
	}
	writeExecutable(t, ffprobe, fmt.Sprintf("#!/bin/sh\nprintf 'probe\\n' >> %q\nprintf '%%s' '{\"streams\":[{\"index\":0,\"codec_name\":\"h264\",\"color_space\":\"reserved\"}]}'\n", probes))
	for i := 0; i < 2; i++ {
		if out, err := gen.runMediaCommand(context.Background(), link, link, nil, nil, &repair); err != nil {
			t.Fatalf("retry %d: %v: %s", i, err, out)
		}
	}
	if got := countMarkerLines(t, probes); got != 2 {
		t.Fatalf("color probes = %d, want failed probe and successful retry", got)
	}
	if got := countMarkerLines(t, commands); got != 4 {
		t.Fatalf("ffmpeg calls = %d, want initial failure, repaired retry, and cached attempt", got)
	}
}

func TestUnrecoverableColorErrorUsesBoundedPreviewRetries(t *testing.T) {
	for _, test := range []struct {
		name, probeJSON string
		wantCalls       int
	}{
		{"unsupported codec", `{"streams":[{"index":0,"codec_name":"av1","color_space":"reserved"}]}`, 9},
		{"valid metadata", `{"streams":[{"index":0,"codec_name":"h264","color_space":"bt709"}]}`, 9},
		{"repair still fails", `{"streams":[{"index":0,"codec_name":"h264","color_space":"reserved"}]}`, 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			ffmpeg, ffprobe := filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "ffprobe")
			commands, probes := filepath.Join(dir, "commands"), filepath.Join(dir, "probes")
			writeExecutable(t, ffmpeg, fmt.Sprintf("#!/bin/sh\nprintf 'command\\n' >> %q\nprintf 'Invalid color space' >&2\nexit 1\n", commands))
			writeExecutable(t, ffprobe, fmt.Sprintf("#!/bin/sh\nprintf 'probe\\n' >> %q\nprintf '%%s' '%s'\n", probes, test.probeJSON))
			gen := New(Config{FFmpegPath: ffmpeg, FFprobePath: ffprobe, LocalDir: filepath.Join(dir, "output")})
			if _, err := gen.Generate(context.Background(), &drives.StreamLink{URL: "source.mp4"}, 120); err == nil || !strings.Contains(err.Error(), "Invalid color space") {
				t.Fatalf("generation error = %v", err)
			}
			if got := countMarkerLines(t, commands); got != test.wantCalls {
				t.Fatalf("ffmpeg calls = %d, want %d", got, test.wantCalls)
			}
			if got := countMarkerLines(t, probes); got != 1 {
				t.Fatalf("color probes = %d, want one", got)
			}
			entries, err := os.ReadDir(gen.cfg.LocalDir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed task left temporary segments: %v, %v", entries, err)
			}
		})
	}
}

// Exercise an audio-first MP4 with reserved H.264 VUI. FFmpeg versions that
// reject it must repair the input before decode/filter negotiation; newer
// versions read the reserved values as unspecified and need no repair. Either
// way, generation must leave the source file untouched.
func TestGenerateMediaWithReservedColorMetadata(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	_, source := prepareColorMetadataMedia(t, ctx, ffmpeg, dir)
	gen := New(Config{FFmpegPath: ffmpeg, FFprobePath: ffprobe, Width: 160, LocalDir: filepath.Join(dir, "output")})
	link := &drives.StreamLink{URL: source}
	original := fileDigest(t, source)
	needsRepair := false
	if out, err := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-i", source, "-frames:v", "1", "-an", "-f", "null", "-").CombinedOutput(); err != nil {
		if !invalidColorMetadataText(string(out)) {
			t.Fatalf("source failed for a reason other than color metadata: %v: %s", err, out)
		}
		needsRepair = true
	} else {
		// This FFmpeg accepts the values. Exercise successful generation
		// there too, without requiring a repair-only failure mode.
		t.Log("FFmpeg accepts reserved metadata without repair")
	}
	if needsRepair {
		// A rejected source must be detected so generation repairs its input.
		if options, err := gen.probeColorRepair(ctx, link); err != nil || len(options) == 0 {
			t.Fatalf("rejected source has no color repair: options=%v err=%v", options, err)
		}
	}
	cover, err := gen.GenerateThumbnail(ctx, link, "video", 8)
	if err != nil {
		t.Fatalf("generate cover: %v", err)
	}
	if info, err := os.Stat(cover); err != nil || info.Size() == 0 {
		t.Fatalf("generated cover is missing or empty: %v", err)
	}
	preview, err := gen.Generate(ctx, link, 8)
	if err != nil {
		t.Fatalf("generate preview: %v", err)
	}
	if err := gen.validateGeneratedTeaser(ctx, preview); err != nil {
		t.Fatalf("validate preview: %v", err)
	}
	if fileDigest(t, source) != original {
		t.Fatal("generation changed the source file")
	}
}

func TestGenerateMediaWithChangingColorMetadata(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	valid, reserved := prepareColorMetadataMedia(t, ctx, ffmpeg, dir)
	var list strings.Builder
	for _, source := range []string{valid, valid, reserved, reserved} {
		fmt.Fprintf(&list, "file '%s'\n", escapeConcatPath(source))
	}
	listPath := filepath.Join(dir, "concat.txt")
	if err := os.WriteFile(listPath, []byte(list.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "changing.mp4")
	if out, err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "concat", "-safe", "0", "-i", listPath, "-map", "0", "-c", "copy", "-y", source).CombinedOutput(); err != nil {
		t.Fatalf("concatenate changing metadata: %v: %s", err, out)
	}
	gen := New(Config{FFmpegPath: ffmpeg, FFprobePath: ffprobe, Width: 160, LocalDir: filepath.Join(dir, "output")})
	link := &drives.StreamLink{URL: source}
	// The stream-level probe sees the valid opening SPS. Later segments still
	// need their own fallback when the second half's SPS is rejected.
	if options, err := gen.probeColorRepair(ctx, link); err != nil || len(options) != 0 {
		t.Fatalf("opening metadata should need no repair: options=%v err=%v", options, err)
	}
	cover, err := gen.GenerateThumbnail(ctx, link, "video", 32)
	if err != nil {
		t.Fatalf("generate cover with earlier-frame fallback: %v", err)
	}
	if info, err := os.Stat(cover); err != nil || info.Size() == 0 {
		t.Fatalf("generated cover is missing or empty: %v", err)
	}
	preview, err := gen.Generate(ctx, link, 32)
	if err != nil {
		t.Fatalf("generate preview with usable early segments: %v", err)
	}
	if err := gen.validateGeneratedTeaser(ctx, preview); err != nil {
		t.Fatalf("validate preview: %v", err)
	}
	if duration, err := gen.Probe(ctx, &drives.StreamLink{URL: preview}); err != nil || duration < 5.9 {
		t.Fatalf("preview did not retain both usable early segments: duration=%v err=%v", duration, err)
	}
}

func prepareColorMetadataMedia(t *testing.T, ctx context.Context, ffmpeg, dir string) (string, string) {
	t.Helper()
	valid, reserved := filepath.Join(dir, "valid.mp4"), filepath.Join(dir, "reserved.mp4")
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("prepare media: %v: %s", err, out)
		}
	}
	run("-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=mono",
		"-f", "lavfi", "-i", "testsrc2=s=160x90:r=24:d=8", "-t", "8", "-map", "0:a", "-map", "1:v",
		"-c:a", "aac", "-c:v", "libx264", "-threads", "1", "-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709", "-y", valid)
	run("-hide_banner", "-loglevel", "error", "-i", valid, "-map", "0", "-c", "copy",
		"-bsf:v", "h264_metadata=colour_primaries=0:matrix_coefficients=3", "-y", reserved)
	return valid, reserved
}

func fileDigest(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func countMarkerLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}
