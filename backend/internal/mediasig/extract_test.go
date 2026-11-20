package mediasig

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testSourceDuration = 60 * time.Second

func requireFFmpeg(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	return path
}

func runTestFFmpeg(t *testing.T, ffmpeg string, args ...string) {
	t.Helper()
	args = append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}, args...)
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// fractalSource renders a zooming fractal whose picture keeps changing.
func fractalSource(t *testing.T, ffmpeg, path string, startX float64) {
	t.Helper()
	source := fmt.Sprintf("mandelbrot=size=320x240:rate=10:maxiter=200:start_x=%g:end_pts=60", startX)
	runTestFFmpeg(t, ffmpeg, "-f", "lavfi", "-i", source, "-t", "60",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "20", path)
}

func testWindows() []Window {
	windows := make([]Window, 0, len(testFractions))
	for _, fraction := range testFractions {
		windows = append(windows, Window{Start: time.Duration(fraction * float64(testSourceDuration)), Duration: 3 * time.Second})
	}
	return windows
}

func fullFileClips(path string, windows []Window) []Clip {
	clips := make([]Clip, 0, len(windows))
	for _, window := range windows {
		clips = append(clips, Clip{Path: path, Offset: window.Start, Window: window})
	}
	return clips
}

func extractFile(t *testing.T, ffmpeg, path string) *Signature {
	t.Helper()
	signature, err := Extract(context.Background(), ffmpeg, testSourceDuration, fullFileClips(path, testWindows()))
	if err != nil {
		t.Fatalf("Extract %s: %v", filepath.Base(path), err)
	}
	return signature
}

func TestExtractMatchesLetterboxedReencodeAndRejectsOtherVideo(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	fractalSource(t, ffmpeg, source, -0.743643887037151)
	copy := filepath.Join(dir, "copy.mp4")
	runTestFFmpeg(t, ffmpeg, "-i", source,
		"-vf", "scale=320:180,pad=320:240:0:30:black,drawbox=x=8:y=38:w=48:h=10:color=white@0.5:t=fill",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "36", copy)
	other := filepath.Join(dir, "other.mp4")
	fractalSource(t, ffmpeg, other, -1.25)

	sourceSignature := extractFile(t, ffmpeg, source)
	if got := len(sourceSignature.Frames); got != 4*6 {
		t.Fatalf("frames = %d, want six per window", got)
	}
	for i, frame := range sourceSignature.Frames {
		want := testWindows()[frame.Window].Start + time.Duration(i%6)*SampleInterval
		if frame.At != want {
			t.Fatalf("frame %d at %v, want %v", i, frame.At, want)
		}
	}

	got := Compare(sourceSignature, extractFile(t, ffmpeg, copy))
	t.Logf("copy: %+v", got)
	if got.Verdict != VerdictStrong {
		t.Fatalf("re-encoded copy = %+v, want strong", got)
	}
	if got := Compare(sourceSignature, extractFile(t, ffmpeg, other)); got.Verdict != VerdictNone {
		t.Fatalf("other video = %+v, want none", got)
	}
}

func TestFromTeaserMapsSegmentsToSourceTime(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	fractalSource(t, ffmpeg, source, -0.743643887037151)

	// Cut a teaser the way the generator does: faded 3-second segments.
	windows := testWindows()
	args := []string{}
	filters := []string{}
	for i, window := range windows {
		args = append(args, "-ss", seconds(window.Start), "-t", "3", "-i", source)
		filters = append(filters, fmt.Sprintf("[%d:v]setpts=PTS-STARTPTS,fade=t=in:st=0:d=0.2,fade=t=out:st=2.8:d=0.2[v%d]", i, i))
	}
	concat := ""
	for i := range windows {
		concat += fmt.Sprintf("[v%d]", i)
	}
	filters = append(filters, concat+fmt.Sprintf("concat=n=%d:v=1:a=0,scale=240:-2[out]", len(windows)))
	teaser := filepath.Join(dir, "teaser.mp4")
	args = append(args, "-filter_complex", strings.Join(filters, ";"), "-map", "[out]",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "28", teaser)
	runTestFFmpeg(t, ffmpeg, args...)

	signature, err := FromTeaser(context.Background(), ffmpeg, teaser, testSourceDuration, windows)
	if err != nil {
		t.Fatalf("FromTeaser: %v", err)
	}
	for _, frame := range signature.Frames {
		offset := frame.At - windows[frame.Window].Start
		if offset < teaserEdgeGuard || offset > 3*time.Second-teaserEdgeGuard {
			t.Fatalf("frame %+v lies in a fade", frame)
		}
	}
	if got := Compare(extractFile(t, ffmpeg, source), signature); got.Verdict != VerdictStrong {
		t.Fatalf("teaser signature = %+v, want strong", got)
	}
}
