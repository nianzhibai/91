package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/video-site/backend/internal/drives"
)

func TestReplaceFilePreservesHardLinkedBackupSnapshot(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "thumbnail.jpg")
	snapshot := filepath.Join(root, "snapshot-thumbnail.jpg")
	replacement := filepath.Join(root, "replacement.jpg")
	if err := os.WriteFile(target, []byte("old-thumbnail"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, snapshot); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := os.WriteFile(replacement, []byte("new-thumbnail"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(replacement, target); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "new-thumbnail" {
		t.Fatalf("replacement body = %q err=%v", body, err)
	}
	if body, err := os.ReadFile(snapshot); err != nil || string(body) != "old-thumbnail" {
		t.Fatalf("hard-linked snapshot body = %q err=%v", body, err)
	}
}

func TestNewDefaultsToThreeSecondTeaserSegments(t *testing.T) {
	gen := New(Config{})
	if gen.cfg.DurationSeconds != 3 {
		t.Fatalf("DurationSeconds = %d, want 3", gen.cfg.DurationSeconds)
	}
}

func TestMediumVideoPreviewPlanUsesFourThreeSecondSegments(t *testing.T) {
	plan := buildTeaserPlan(Config{DurationSeconds: 3, Segments: 3}, 300)
	if len(plan.starts) != 4 {
		t.Fatalf("segments = %d, want 4", len(plan.starts))
	}
	if plan.eachSec != 3 {
		t.Fatalf("eachSec = %.2f, want 3", plan.eachSec)
	}
	want := []float64{15, 95, 175, 255}
	for i := range want {
		if math.Abs(plan.starts[i]-want[i]) > 0.001 {
			t.Fatalf("start[%d] = %.2f, want %.2f", i, plan.starts[i], want[i])
		}
	}
}

func TestLongVideoPreviewPlanUsesFourThreeSecondSegments(t *testing.T) {
	plan := buildTeaserPlan(Config{DurationSeconds: 15, Segments: 3}, 601)
	if len(plan.starts) != 4 {
		t.Fatalf("segments = %d, want 4", len(plan.starts))
	}
	if plan.eachSec != 3 {
		t.Fatalf("eachSec = %.2f, want 3", plan.eachSec)
	}
	want := []float64{120.2, 240.4, 360.6, 480.8}
	for i := range want {
		if math.Abs(plan.starts[i]-want[i]) > 0.001 {
			t.Fatalf("start[%d] = %.2f, want %.2f", i, plan.starts[i], want[i])
		}
	}
}

func TestShortVideoPreviewPlanUsesUpToThreeThreeSecondSegments(t *testing.T) {
	plan := buildTeaserPlan(Config{DurationSeconds: 15, Segments: 3}, 20)
	if len(plan.starts) != 3 {
		t.Fatalf("segments = %d, want 3", len(plan.starts))
	}
	if plan.eachSec != 3 {
		t.Fatalf("eachSec = %.2f, want 3", plan.eachSec)
	}
	want := []float64{2, 9.5, 17}
	for i := range want {
		if math.Abs(plan.starts[i]-want[i]) > 0.001 {
			t.Fatalf("start[%d] = %.2f, want %.2f", i, plan.starts[i], want[i])
		}
	}
}

func TestShortVideoPreviewPlanDropsSegmentsThatDoNotFit(t *testing.T) {
	plan := buildTeaserPlan(Config{DurationSeconds: 15, Segments: 3}, 8)
	if len(plan.starts) != 2 {
		t.Fatalf("segments = %d, want 2", len(plan.starts))
	}
	if plan.eachSec != 3 {
		t.Fatalf("eachSec = %.2f, want 3", plan.eachSec)
	}
	want := []float64{0.8, 5}
	for i := range want {
		if math.Abs(plan.starts[i]-want[i]) > 0.001 {
			t.Fatalf("start[%d] = %.2f, want %.2f", i, plan.starts[i], want[i])
		}
	}
}

func TestTinyVideoPreviewPlanUsesWholeVideoAsSingleSegment(t *testing.T) {
	plan := buildTeaserPlan(Config{DurationSeconds: 15, Segments: 3}, 2.5)
	if len(plan.starts) != 1 {
		t.Fatalf("segments = %d, want 1", len(plan.starts))
	}
	if plan.eachSec != 2.5 {
		t.Fatalf("eachSec = %.2f, want 2.5", plan.eachSec)
	}
	if plan.starts[0] != 0 {
		t.Fatalf("start[0] = %.2f, want 0", plan.starts[0])
	}
}

func TestProbeIgnoresStderrWarnings(t *testing.T) {
	dir := t.TempDir()
	ffprobePath := filepath.Join(dir, "ffprobe")
	script := "#!/bin/sh\nprintf '%s\\n' 'h264 warning' >&2\nprintf '%s\\n' '364.800000'\n"
	if err := os.WriteFile(ffprobePath, []byte(script), 0o755); err != nil {
		t.Fatalf("write ffprobe stub: %v", err)
	}

	gen := New(Config{FFprobePath: ffprobePath})
	got, err := gen.Probe(context.Background(), &drives.StreamLink{URL: filepath.Join(dir, "video.mp4")})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got != 364.8 {
		t.Fatalf("duration = %v, want 364.8", got)
	}
}

func TestTeaserCandidateStartsKeepPrimaryAndAddFallbacks(t *testing.T) {
	primary := []float64{10.2, 64.65, 119.1, 173.55}
	got := teaserCandidateStarts(204, primary, 3)
	if len(got) <= len(primary) {
		t.Fatalf("candidate starts = %#v, want fallback starts after primary", got)
	}
	for i, want := range primary {
		if math.Abs(got[i]-want) > 0.001 {
			t.Fatalf("candidate[%d] = %.2f, want primary %.2f first", i, got[i], want)
		}
	}
}

func TestTeaserSegmentFallbackAllowedForBadSegmentOutput(t *testing.T) {
	for _, err := range []error{
		errors.New("generated teaser has no video stream"),
		errors.New("ffmpeg segment: signal: killed, stderr: "),
		errors.New("ffmpeg segment produced empty file, stderr: "),
	} {
		if !teaserSegmentFallbackAllowed(err) {
			t.Fatalf("teaserSegmentFallbackAllowed(%v) = false, want true", err)
		}
	}
	if teaserSegmentFallbackAllowed(errors.New("server returned 403 forbidden")) {
		t.Fatal("403 errors should not trigger teaser segment fallback")
	}
}

func TestTeaserSegmentFallbackRequiresPlannedSegmentCount(t *testing.T) {
	err := errors.New("only generated 2/4 teaser segments: generated teaser has no video stream")
	if !strings.Contains(err.Error(), "2/4") {
		t.Fatalf("error = %v, want generated/planned segment count", err)
	}
}

func TestShortVideoRequiresOnlyOneUsableTeaserSegment(t *testing.T) {
	if got := requiredTeaserSegments(12, 3, false); got != 1 {
		t.Fatalf("required segments = %d, want 1 for short video", got)
	}
	if got := requiredTeaserSegments(29.999, 3, true); got != 1 {
		t.Fatalf("required segments = %d, want 1 below 30 seconds", got)
	}
}

func TestMediumAndLongVideosRequireFullPlanBeforeExplicitDegradation(t *testing.T) {
	if got := requiredTeaserSegments(30, 4, false); got != 4 {
		t.Fatalf("required segments = %d, want full four-segment plan", got)
	}
	if got := requiredTeaserSegments(30, 4, true); got != 2 {
		t.Fatalf("required segments = %d, want two at 30 seconds", got)
	}
	if got := requiredTeaserSegments(204, 4, true); got != 2 {
		t.Fatalf("required segments = %d, want two for longer video", got)
	}
	if got := requiredTeaserSegments(204, 2, true); got != 2 {
		t.Fatalf("required segments = %d, want full two-segment plan", got)
	}
}

func TestThumbnailOffsetsPreferMiddleFrame(t *testing.T) {
	tests := []struct {
		name     string
		duration float64
		want     []float64
	}{
		{name: "unknown duration", duration: 0, want: []float64{5, 1, 0}},
		{name: "long video", duration: 2804.9, want: []float64{1402.45, 5, 1, 0}},
		{name: "short video", duration: 8.9, want: []float64{4.45, 5, 1, 0}},
		{name: "middle equals fallback", duration: 10, want: []float64{5, 1, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := thumbnailOffsets(tt.duration)
			if len(got) != len(tt.want) {
				t.Fatalf("offsets = %#v, want %#v", got, tt.want)
			}
			for i := range tt.want {
				if math.Abs(got[i]-tt.want[i]) > 0.001 {
					t.Fatalf("offset[%d] = %.2f, want %.2f", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestThumbnailVideoFilterUsesFullRangeJPEGPixelFormat(t *testing.T) {
	got := thumbnailVideoFilter(480)
	if !strings.Contains(got, "scale=480:-2:out_range=pc") {
		t.Fatalf("thumbnail filter = %q, want full-range scale output", got)
	}
	if !strings.Contains(got, "format=yuvj420p") {
		t.Fatalf("thumbnail filter = %q, want JPEG-friendly pixel format", got)
	}
}

func TestTeaserSegmentVideoFilterResetsTimestampsBeforeConcat(t *testing.T) {
	got := teaserSegmentVideoFilter(480, true, 3)
	if !strings.Contains(got, "setpts=PTS-STARTPTS") {
		t.Fatalf("teaser filter = %q, want timestamp reset", got)
	}
	if !strings.Contains(got, "fade=t=in") || !strings.Contains(got, "fade=t=out:st=2.80") {
		t.Fatalf("teaser filter = %q, want segment fades", got)
	}
}

func TestValidateGeneratedTeaserRejectsNonZeroStartTime(t *testing.T) {
	dir := t.TempDir()
	ffprobe := filepath.Join(dir, "ffprobe")
	probeScript := "#!/bin/sh\n" +
		`printf '%s' '{"streams":[{"codec_type":"video","duration":"3.0"}],"format":{"start_time":"5.366016","duration":"8.366016"}}'` + "\n"
	if err := os.WriteFile(ffprobe, []byte(probeScript), 0o755); err != nil {
		t.Fatalf("write ffprobe stub: %v", err)
	}
	path := filepath.Join(dir, "teaser.mp4")
	if err := os.WriteFile(path, []byte("not-empty"), 0o644); err != nil {
		t.Fatalf("write teaser: %v", err)
	}
	gen := New(Config{FFprobePath: ffprobe})
	if err := gen.validateGeneratedTeaser(context.Background(), path); err == nil || !strings.Contains(err.Error(), "starts too late") {
		t.Fatalf("validate error = %v, want late-start rejection", err)
	}
}

func TestThumbnailOffsetFallbackAllowedForEmptyOutputAndTimeouts(t *testing.T) {
	for _, err := range []error{
		errors.New("ffmpeg thumb produced empty file, stderr: "),
		errors.New("ffmpeg thumb: signal: killed, stderr: "),
		context.DeadlineExceeded,
	} {
		if !thumbnailOffsetFallbackAllowed(err) {
			t.Fatalf("thumbnailOffsetFallbackAllowed(%v) = false, want true", err)
		}
	}
	if thumbnailOffsetFallbackAllowed(errors.New("server returned 403 forbidden")) {
		t.Fatal("403 errors should not trigger thumbnail offset fallback")
	}
}

func TestFFmpeg429OutputBecomesRateLimitError(t *testing.T) {
	err := ffmpegCommandError("ffmpeg", errors.New("exit status 8"), []byte("Server returned 429 Too Many Requests"))
	var rateLimit *drives.RateLimitError
	if !errors.As(err, &rateLimit) {
		t.Fatalf("error = %T %[1]v, want RateLimitError", err)
	}
	if rateLimit.RetryAfter != 0 {
		t.Fatalf("retry after = %v, want none from ffmpeg stderr", rateLimit.RetryAfter)
	}
}

func TestFFmpegCommandErrorRedactsSignedURLs(t *testing.T) {
	err := ffmpegCommandError("ffmpeg", errors.New("exit status 8"), []byte("Error opening input file https://download.example/file.mp4?tempauth=secret."))
	got := err.Error()
	if strings.Contains(got, "tempauth=secret") {
		t.Fatalf("error leaked signed URL: %s", got)
	}
	if !strings.Contains(got, "https://<redacted>.") {
		t.Fatalf("error = %q, want redacted URL with punctuation preserved", got)
	}
}

func TestFFmpegHTTPInputOptionsUsesDedicatedUserAgent(t *testing.T) {
	link := &drives.StreamLink{
		URL: "https://download.example/video.mp4",
		Headers: http.Header{
			"User-Agent": {"Mozilla/5.0 115Browser/27.0.5.7"},
			"Cookie":     {"UID=redacted"},
		},
	}

	args := ffmpegHTTPInputOptions(link)
	joined := strings.Join(args, "\n")
	if !strings.Contains(joined, "-user_agent\nMozilla/5.0 115Browser/27.0.5.7") {
		t.Fatalf("args = %#v, want dedicated ffmpeg user_agent option", args)
	}
	if strings.Contains(joined, "User-Agent:") {
		t.Fatalf("args = %#v, user agent should not be duplicated in raw headers", args)
	}
	if strings.Contains(joined, "UID=redacted") || strings.Contains(joined, "Cookie:") {
		t.Fatalf("args = %#v, secret cookie must never be passed to ffmpeg", args)
	}
}

func TestShouldProxy115FFmpegLinks(t *testing.T) {
	if !shouldProxyFFmpegLink(&drives.StreamLink{URL: "https://cdnfhnfile.115cdn.net/file.mp4"}) {
		t.Fatal("115 CDN link should use local ffmpeg proxy")
	}
	if !shouldProxyFFmpegLink(&drives.StreamLink{
		URL:                  "https://webdav.example/dav/file.mp4",
		PassThroughRedirects: true,
	}) {
		t.Fatal("redirect-passthrough link should use local ffmpeg proxy")
	}
	if shouldProxyFFmpegLink(&drives.StreamLink{URL: "https://download.example/file.mp4"}) {
		t.Fatal("generic link should not use local ffmpeg proxy")
	}
	if !shouldProxyFFmpegLink(&drives.StreamLink{
		URL: "https://download.example/protected.mp4", Headers: http.Header{"Cookie": {"secret=1"}},
	}) {
		t.Fatal("credential-bearing link should use local ffmpeg proxy")
	}
}

func TestPrepareFFmpegLinkDoesNotLeakWebDAVCredentialsToRedirectTarget(t *testing.T) {
	originRequests := make(chan http.Header, 1)
	targetRequests := make(chan http.Header, 1)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests <- r.Header.Clone()
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 2-5/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "2345")
	}))
	t.Cleanup(target.Close)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originRequests <- r.Header.Clone()
		w.Header().Set("Location", target.URL+"/video.mp4")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	link := &drives.StreamLink{
		URL: origin.URL + "/dav/video.mp4",
		Headers: http.Header{
			"Authorization":       {"Basic webdav-secret"},
			"Cookie":              {"session=webdav-secret"},
			"Proxy-Authorization": {"Basic proxy-secret"},
			"User-Agent":          {"video-site-webdav"},
		},
		PassThroughRedirects: true,
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	proxied, cleanup, err := prepareFFmpegLink(ctx, link)
	if err != nil {
		t.Fatalf("prepare ffmpeg link: %v", err)
	}
	t.Cleanup(cleanup)
	if proxied.URL == link.URL {
		t.Fatal("ffmpeg link was not replaced with a loopback proxy URL")
	}
	if proxied.Headers != nil {
		t.Fatalf("proxied headers = %#v, want nil", proxied.Headers)
	}
	if proxied.PassThroughRedirects {
		t.Fatal("loopback ffmpeg link should not expose upstream redirect semantics")
	}

	req, err := http.NewRequest(http.MethodGet, proxied.URL, nil)
	if err != nil {
		t.Fatalf("new loopback request: %v", err)
	}
	req.Header.Set("Range", "bytes=2-5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request loopback proxy: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read loopback response: %v", err)
	}
	if resp.StatusCode != http.StatusPartialContent || string(body) != "2345" {
		t.Fatalf("response = status %d body %q", resp.StatusCode, body)
	}

	originHeaders := <-originRequests
	targetHeaders := <-targetRequests
	if got := originHeaders.Get("Authorization"); got != "Basic webdav-secret" {
		t.Fatalf("origin Authorization = %q, want WebDAV credentials", got)
	}
	if got := originHeaders.Get("Cookie"); got != "session=webdav-secret" {
		t.Fatalf("origin Cookie = %q, want WebDAV cookie", got)
	}
	if got := originHeaders.Get("Range"); got != "bytes=2-5" {
		t.Fatalf("origin Range = %q, want bytes=2-5", got)
	}
	for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Referer"} {
		if got := targetHeaders.Get(name); got != "" {
			t.Fatalf("%s leaked to redirect target: %q", name, got)
		}
	}
	if got := targetHeaders.Get("Range"); got != "bytes=2-5" {
		t.Fatalf("target Range = %q, want bytes=2-5", got)
	}
	if got := targetHeaders.Get("User-Agent"); got != "video-site-webdav" {
		t.Fatalf("target User-Agent = %q, want video-site-webdav", got)
	}
}

// newTeaserStubGenerator builds a Generator whose ffmpeg produces usable output
// for the first okSegments segment invocations and leaves the output file empty
// afterwards, which is the "damaged source" shape the degraded teaser path is
// meant to handle.
func newTeaserStubGenerator(t *testing.T, okSegments int) *Generator {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "segment-count")
	ffmpeg := filepath.Join(dir, "ffmpeg")
	script := fmt.Sprintf(`#!/bin/sh
out=""
for arg in "$@"; do out="$arg"; done
case " $* " in
  *" -f concat "*)
    printf 'concat-output' > "$out"
    exit 0
    ;;
esac
count=$(cat %[1]q 2>/dev/null || echo 0)
count=$((count + 1))
printf '%%s' "$count" > %[1]q
if [ "$count" -le %[2]d ]; then
  printf 'segment-output' > "$out"
fi
exit 0
`, counter, okSegments)
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatalf("write ffmpeg stub: %v", err)
	}

	ffprobe := filepath.Join(dir, "ffprobe")
	probeScript := "#!/bin/sh\n" +
		`printf '%s' '{"streams":[{"codec_type":"video","duration":"3.0"}],"format":{"duration":"3.0"}}'` + "\n"
	if err := os.WriteFile(ffprobe, []byte(probeScript), 0o755); err != nil {
		t.Fatalf("write ffprobe stub: %v", err)
	}

	return New(Config{
		FFmpegPath:  ffmpeg,
		FFprobePath: ffprobe,
		Width:       480,
		LocalDir:    filepath.Join(dir, "preview"),
	})
}

func TestGenerateSequentialAcceptsDegradedTeaserAndLogsIt(t *testing.T) {
	gen := newTeaserStubGenerator(t, 2)
	link := &drives.StreamLink{URL: filepath.Join(t.TempDir(), "video.mp4")}

	var logs strings.Builder
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	path, err := gen.generateSequential(context.Background(), 204, func(int) (*drives.StreamLink, error) {
		return link, nil
	})
	if err != nil {
		t.Fatalf("generate sequential: %v", err)
	}
	if path == "" {
		t.Fatal("expected a teaser path for the degraded result")
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	if !strings.Contains(logs.String(), "degraded teaser accepted segments=2/4") {
		t.Fatalf("logs = %q, want an explicit degraded-teaser record", logs.String())
	}
}

func TestGenerateSequentialRejectsTeaserBelowDegradedFloor(t *testing.T) {
	gen := newTeaserStubGenerator(t, 1)
	link := &drives.StreamLink{URL: filepath.Join(t.TempDir(), "video.mp4")}

	_, err := gen.generateSequential(context.Background(), 204, func(int) (*drives.StreamLink, error) {
		return link, nil
	})
	if err == nil {
		t.Fatal("expected an error: one usable segment is below the degraded floor")
	}
	if !strings.Contains(err.Error(), "only generated 1/4 teaser segments") {
		t.Fatalf("error = %v, want the generated/planned segment count", err)
	}
}

func TestGenerateSequentialKeepsFullPlanWhenEverySegmentWorks(t *testing.T) {
	gen := newTeaserStubGenerator(t, 4)
	link := &drives.StreamLink{URL: filepath.Join(t.TempDir(), "video.mp4")}

	var logs strings.Builder
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	path, err := gen.generateSequential(context.Background(), 204, func(int) (*drives.StreamLink, error) {
		return link, nil
	})
	if err != nil {
		t.Fatalf("generate sequential: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	if strings.Contains(logs.String(), "degraded teaser") {
		t.Fatalf("logs = %q, want no degradation for a healthy source", logs.String())
	}
}
