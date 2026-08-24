package scriptcrawler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/fingerprint"
	"github.com/video-site/backend/internal/mediaasset"
)

const (
	scriptCrawlerDuplicateBytes = "duplicate-video-bytes"
	scriptCrawlerUniqueBytes    = "unique-video-bytes"
	scriptCrawlerWebPBase64     = "UklGRrIBAABXRUJQVlA4TKUBAAAvSsAYAA8w//M///MfeJAkbXvaSG7m8Q3GfYSBJekwQztm/IcZlgwnmWImn2BK7aFmBtnVir6q//8VOkFE/xm4baTIu8c48ArEo6+B3zFKYln3pqClSCKX0begFTAXFOLXHSyF8cCNcZEG4OywuA4KVVfJCiArU7GAgJI8+lJP/OKMT/fBAjevg1cYB7YVkFuWga2lyPi5I0HFy5YTpWIHg0RZpkniRVW9odHAKOwosWuOGdxIyn2OvaCDvhg/we6TwadPBPbqBV58MsLmMJ8yZnOWk8SRz4N+QoyPL+MnamzMvcE1rHNEr91F9GKZPVUcS9w7PhhH36suB9qPeYb/oLk6cuTiJ0wOK3m5h1cKjW6EVZCYMK7dxcKCBdgP9HkKr9gkAO2P8GKZGWVdIAatQa+1IDpt6qyorVwdy01xdW8Jkfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil9w4L2bvMycmjQcqXaMCO6BlY28i+FOLzbfI1vEqxAhotocAAA=="
)

func writeScriptCrawlerFFprobeStub(t *testing.T, dir string, ok bool) string {
	t.Helper()
	name := "ffprobe-ok.sh"
	body := "#!/bin/sh\necho video\nexit 0\n"
	if !ok {
		name = "ffprobe-fail.sh"
		body = "#!/bin/sh\necho 'moov atom not found' >&2\nexit 1\n"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write ffprobe stub: %v", err)
	}
	return path
}

func writeScriptCrawlerFFmpegStub(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "ffmpeg-hls.sh")
	body := `#!/bin/sh
if [ "$#" -eq 3 ] && [ "$1" = "-hide_banner" ] && [ "$2" = "-h" ] && [ "$3" = "demuxer=hls" ]; then
  if [ "${GO_SCRIPTCRAWLER_FFMPEG_HELP_FAIL:-}" = "1" ]; then
    echo "hls help unavailable" >&2
    exit 1
  fi
  if [ -n "${GO_SCRIPTCRAWLER_FFMPEG_HLS_HELP:-}" ]; then
    printf '%s\n' "$GO_SCRIPTCRAWLER_FFMPEG_HLS_HELP"
  else
    printf '%s\n' \
      '  -allowed_extensions <string> .D.........' \
      '  -allowed_segment_extensions <string> .D.........' \
      '  -extension_picky <boolean> .D.........'
  fi
  exit 0
fi
if [ -n "$GO_SCRIPTCRAWLER_FFMPEG_ARGS_FILE" ]; then printf '%s\n' "$@" > "$GO_SCRIPTCRAWLER_FFMPEG_ARGS_FILE"; fi
out=""
for arg do out="$arg"; done
printf 'hls-video-bytes' > "$out"
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write ffmpeg stub: %v", err)
	}
	return path
}

func writeScriptCrawlerJPEG(t *testing.T, path string, c color.RGBA) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 48, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 48; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create jpeg: %v", err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
}

func writeScriptCrawlerWebP(t *testing.T, path string) {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(scriptCrawlerWebPBase64)
	if err != nil {
		t.Fatalf("decode WebP fixture: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write WebP fixture: %v", err)
	}
}

func TestCrawlerPreviewDisabledUsesTaskConfigurationSnapshot(t *testing.T) {
	drv := New(Config{ID: "crawler-main", RootDir: t.TempDir()})
	crawler := NewCrawler(CrawlerConfig{
		Driver:         drv,
		DisablePreview: true,
		GetDriveConfig: func(context.Context, string) (*catalog.Drive, error) {
			return &catalog.Drive{ID: "crawler-main", TeaserEnabled: true}, nil
		},
	})
	if crawler.previewDisabled(context.Background()) {
		t.Fatal("previewDisabled used stale constructor setting instead of task snapshot")
	}
}

func TestCrawlerRunOnceImportsLocalFileAndSkipsExisting(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	drv := New(Config{ID: "demo", RootDir: filepath.Join(tmp, "crawler")})
	if err := drv.Init(ctx); err != nil {
		t.Fatalf("driver init: %v", err)
	}
	dummyScript := filepath.Join(tmp, "helper-script")
	if err := os.WriteFile(dummyScript, []byte("helper"), 0o755); err != nil {
		t.Fatalf("write dummy script: %v", err)
	}
	wrapper := filepath.Join(tmp, "helper-wrapper.sh")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestScriptCrawlerHelperProcess \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	t.Setenv("GO_WANT_SCRIPTCRAWLER_HELPER", "1")
	c := NewCrawler(CrawlerConfig{
		Driver:              drv,
		Catalog:             cat,
		CrawlerName:         "Demo Crawler",
		PythonPath:          wrapper,
		FFprobePath:         writeScriptCrawlerFFprobeStub(t, tmp, true),
		ScriptPath:          dummyScript,
		SkipProtocolRefresh: true,
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 1 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("result = new:%d skipped:%d failed:%d, want 1/0/0", res.NewVideos, res.Skipped, res.Failed)
	}
	v, err := cat.GetVideo(ctx, BuildVideoID("demo", "abc-123"))
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if v.Title != "Imported From Helper" || v.FileID != "abc-123.mp4" || v.Size == 0 {
		t.Fatalf("video = title:%q file:%q size:%d", v.Title, v.FileID, v.Size)
	}
	if !v.PublishedAt.Equal(v.CreatedAt) {
		t.Fatalf("crawler timestamps = published %s created %s, want identical backend import time", v.PublishedAt, v.CreatedAt)
	}
	if !hasString(v.Tags, "Demo Crawler") {
		t.Fatalf("video tags = %#v, want crawler name tag", v.Tags)
	}
	if _, err := os.Stat(filepath.Join(drv.VideosDir(), "abc-123.mp4")); err != nil {
		t.Fatalf("video file not copied: %v", err)
	}

	res, err = c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.NewVideos != 0 || res.Skipped != 1 {
		t.Fatalf("second result = new:%d skipped:%d, want 0/1", res.NewVideos, res.Skipped)
	}
	if res.SeenSnapshot != 1 {
		t.Fatalf("seen snapshot = %d, want 1", res.SeenSnapshot)
	}
}

func TestCrawlerRunOnceMarksPreviewDisabledWhenConfigured(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	drv := New(Config{ID: "demo", RootDir: filepath.Join(tmp, "crawler")})
	if err := drv.Init(ctx); err != nil {
		t.Fatalf("driver init: %v", err)
	}
	dummyScript := filepath.Join(tmp, "helper-script")
	if err := os.WriteFile(dummyScript, []byte("helper"), 0o755); err != nil {
		t.Fatalf("write dummy script: %v", err)
	}
	wrapper := filepath.Join(tmp, "helper-wrapper.sh")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestScriptCrawlerHelperProcess \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	t.Setenv("GO_WANT_SCRIPTCRAWLER_HELPER", "1")
	c := NewCrawler(CrawlerConfig{
		Driver:              drv,
		Catalog:             cat,
		PythonPath:          wrapper,
		FFprobePath:         writeScriptCrawlerFFprobeStub(t, tmp, true),
		ScriptPath:          dummyScript,
		SkipProtocolRefresh: true,
		DisablePreview:      true,
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 1 || res.Failed != 0 {
		t.Fatalf("result = new:%d failed:%d, want 1/0", res.NewVideos, res.Failed)
	}
	v, err := cat.GetVideo(ctx, BuildVideoID("demo", "abc-123"))
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if v.PreviewStatus != "disabled" {
		t.Fatalf("preview status = %q, want disabled", v.PreviewStatus)
	}
	if v.FingerprintStatus != "ready" || v.SampledSHA256 == "" {
		t.Fatalf("fingerprint status=%q sampled=%q, want ready and sampled hash", v.FingerprintStatus, v.SampledSHA256)
	}
	pending, err := cat.ListVideosByPreviewStatus(ctx, "demo", "pending", 0)
	if err != nil {
		t.Fatalf("list pending previews: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending previews = %d, want 0", len(pending))
	}
}

func TestCrawlerRunOnceUsesCurrentDrivePreviewSwitch(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	drv := New(Config{ID: "demo", RootDir: filepath.Join(tmp, "crawler")})
	if err := drv.Init(ctx); err != nil {
		t.Fatalf("driver init: %v", err)
	}
	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:            drv.ID(),
		Kind:          Kind,
		Name:          "Demo",
		RootID:        "/",
		Credentials:   map[string]string{"script_path": "/tmp/crawler.py"},
		TeaserEnabled: true,
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
	dummyScript := filepath.Join(tmp, "helper-script")
	if err := os.WriteFile(dummyScript, []byte("helper"), 0o755); err != nil {
		t.Fatalf("write dummy script: %v", err)
	}
	wrapper := filepath.Join(tmp, "helper-wrapper.sh")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestScriptCrawlerHelperProcess \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	t.Setenv("GO_WANT_SCRIPTCRAWLER_HELPER", "1")
	c := NewCrawler(CrawlerConfig{
		Driver:              drv,
		Catalog:             cat,
		PythonPath:          wrapper,
		FFprobePath:         writeScriptCrawlerFFprobeStub(t, tmp, true),
		ScriptPath:          dummyScript,
		SkipProtocolRefresh: true,
		DisablePreview:      true,
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 1 || res.Failed != 0 {
		t.Fatalf("result = new:%d failed:%d, want 1/0", res.NewVideos, res.Failed)
	}
	v, err := cat.GetVideo(ctx, BuildVideoID("demo", "abc-123"))
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if v.PreviewStatus != "pending" {
		t.Fatalf("preview status = %q, want pending from current drive switch", v.PreviewStatus)
	}
}

func TestCrawlerRunOnceUsesDefaultCrawlerNamespace(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	drv := New(Config{ID: "demo", RootDir: filepath.Join(tmp, "crawler")})
	if err := drv.Init(ctx); err != nil {
		t.Fatalf("driver init: %v", err)
	}
	dummyScript := filepath.Join(tmp, "helper-script")
	if err := os.WriteFile(dummyScript, []byte("helper"), 0o755); err != nil {
		t.Fatalf("write dummy script: %v", err)
	}
	wrapper := filepath.Join(tmp, "helper-wrapper.sh")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestScriptCrawlerHelperProcess \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	t.Setenv("GO_WANT_SCRIPTCRAWLER_HELPER", "1")
	c := NewCrawler(CrawlerConfig{
		Driver:              drv,
		Catalog:             cat,
		PythonPath:          wrapper,
		FFprobePath:         writeScriptCrawlerFFprobeStub(t, tmp, true),
		ScriptPath:          dummyScript,
		SkipProtocolRefresh: true,
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 1 || res.SeenSnapshot != 0 {
		t.Fatalf("result = new:%d seen:%d, want 1/0", res.NewVideos, res.SeenSnapshot)
	}
	videoID := BuildVideoID("demo", "abc-123")
	if _, err := cat.GetVideo(ctx, videoID); err != nil {
		t.Fatalf("get crawler video: %v", err)
	}

	res, err = c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.NewVideos != 0 || res.Skipped != 1 || res.SeenSnapshot != 1 {
		t.Fatalf("second result = new:%d skipped:%d seen:%d, want 0/1/1", res.NewVideos, res.Skipped, res.SeenSnapshot)
	}
}

func TestCrawlerRunOncePassesAbsoluteJobPathsWhenWorkDirDiffers(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	t.Chdir(tmp)
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	drv := New(Config{ID: "demo", RootDir: filepath.Join("data", "crawler")})
	if err := drv.Init(ctx); err != nil {
		t.Fatalf("driver init: %v", err)
	}
	scriptDir := filepath.Join(tmp, "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir script dir: %v", err)
	}
	dummyScript := filepath.Join(scriptDir, "helper-script")
	if err := os.WriteFile(dummyScript, []byte("helper"), 0o755); err != nil {
		t.Fatalf("write dummy script: %v", err)
	}
	wrapper := filepath.Join(tmp, "helper-wrapper.sh")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestScriptCrawlerHelperProcess \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	t.Setenv("GO_WANT_SCRIPTCRAWLER_HELPER", "1")
	t.Setenv("GO_WANT_SCRIPTCRAWLER_ASSERT_ABS", "1")
	c := NewCrawler(CrawlerConfig{
		Driver:              drv,
		Catalog:             cat,
		PythonPath:          wrapper,
		FFprobePath:         writeScriptCrawlerFFprobeStub(t, tmp, true),
		ScriptPath:          dummyScript,
		SkipProtocolRefresh: true,
		WorkDir:             scriptDir,
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 1 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("result = new:%d skipped:%d failed:%d, want 1/0/0", res.NewVideos, res.Skipped, res.Failed)
	}
	if !filepath.IsAbs(res.JobFile) || !filepath.IsAbs(res.SeenFile) {
		t.Fatalf("result paths should be absolute: job=%q seen=%q", res.JobFile, res.SeenFile)
	}
}

func TestCrawlerRunOnceImportsSimpleMediaURLWithoutSourceID(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/video.mp4" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("simple-video-bytes"))
	}))
	defer srv.Close()

	drv := New(Config{ID: "demo", RootDir: filepath.Join(tmp, "crawler")})
	if err := drv.Init(ctx); err != nil {
		t.Fatalf("driver init: %v", err)
	}
	dummyScript := filepath.Join(tmp, "helper-script")
	if err := os.WriteFile(dummyScript, []byte("helper"), 0o755); err != nil {
		t.Fatalf("write dummy script: %v", err)
	}
	wrapper := filepath.Join(tmp, "helper-wrapper.sh")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestScriptCrawlerHelperProcess \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	t.Setenv("GO_WANT_SCRIPTCRAWLER_HELPER", "1")
	t.Setenv("GO_WANT_SCRIPTCRAWLER_SIMPLE", "1")
	t.Setenv("GO_SCRIPTCRAWLER_MEDIA_URL", srv.URL+"/video.mp4?token=first")
	c := NewCrawler(CrawlerConfig{
		Driver:              drv,
		Catalog:             cat,
		PythonPath:          wrapper,
		FFprobePath:         writeScriptCrawlerFFprobeStub(t, tmp, true),
		ScriptPath:          dummyScript,
		SkipProtocolRefresh: true,
		HTTPClient:          srv.Client(),
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 1 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("result = new:%d skipped:%d failed:%d, want 1/0/0", res.NewVideos, res.Skipped, res.Failed)
	}
	videos, err := cat.ListVideosByDrive(ctx, "demo")
	if err != nil {
		t.Fatalf("list videos: %v", err)
	}
	if len(videos) != 1 {
		t.Fatalf("videos = %d, want 1", len(videos))
	}
	v := videos[0]
	if !strings.HasPrefix(v.ID, BuildVideoID("demo", "auto-")) {
		t.Fatalf("video id = %q, want generated auto source id", v.ID)
	}
	if v.Title != "Simple Protocol Video" || v.Ext != "mp4" || v.ThumbnailURL != "" || v.Size == 0 {
		t.Fatalf("video = title:%q ext:%q thumb:%q size:%d", v.Title, v.Ext, v.ThumbnailURL, v.Size)
	}
	if _, err := os.Stat(filepath.Join(drv.VideosDir(), v.FileID)); err != nil {
		t.Fatalf("video file not downloaded: %v", err)
	}

	t.Setenv("GO_SCRIPTCRAWLER_MEDIA_URL", srv.URL+"/video.mp4?token=second")
	res, err = c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.NewVideos != 0 || res.Skipped != 1 {
		t.Fatalf("second result = new:%d skipped:%d, want 0/1", res.NewVideos, res.Skipped)
	}
}

func TestCrawlerRunOnceSkipsThenRestoresRetainedLocalVideo(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	var requests atomic.Int32
	var failRemote atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/video.mp4" {
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		if failRemote.Load() {
			http.Error(w, "remote unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("restored-video-bytes"))
	}))
	defer srv.Close()

	drv := New(Config{ID: "demo", RootDir: filepath.Join(tmp, "crawler")})
	if err := drv.Init(ctx); err != nil {
		t.Fatalf("driver init: %v", err)
	}
	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:            drv.ID(),
		Kind:          Kind,
		Name:          "Demo",
		TeaserEnabled: true,
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
	dummyScript := filepath.Join(tmp, "helper-script")
	if err := os.WriteFile(dummyScript, []byte("helper"), 0o755); err != nil {
		t.Fatalf("write dummy script: %v", err)
	}
	wrapper := filepath.Join(tmp, "helper-wrapper.sh")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestScriptCrawlerHelperProcess \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	t.Setenv("GO_WANT_SCRIPTCRAWLER_HELPER", "1")
	t.Setenv("GO_WANT_SCRIPTCRAWLER_SIMPLE", "1")
	t.Setenv("GO_SCRIPTCRAWLER_MEDIA_URL", srv.URL+"/video.mp4?token=restore")
	c := NewCrawler(CrawlerConfig{
		Driver:              drv,
		Catalog:             cat,
		PythonPath:          wrapper,
		FFprobePath:         writeScriptCrawlerFFprobeStub(t, tmp, true),
		ScriptPath:          dummyScript,
		SkipProtocolRefresh: true,
		HTTPClient:          srv.Client(),
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 1 || res.Failed != 0 {
		t.Fatalf("result = new:%d failed:%d, want 1/0", res.NewVideos, res.Failed)
	}
	videos, err := cat.ListVideosByDrive(ctx, "demo")
	if err != nil {
		t.Fatalf("list videos: %v", err)
	}
	if len(videos) != 1 {
		t.Fatalf("videos = %d, want 1", len(videos))
	}
	v := videos[0]
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
	localPath := filepath.Join(drv.VideosDir(), v.FileID)
	data, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("read local video: %v", err)
	}
	if string(data) != "restored-video-bytes" {
		t.Fatalf("local data = %q", data)
	}

	// Simulate a tombstone written by an older backend that accepted a source
	// publication date. Restore must re-establish the backend-owned timestamp.
	v.PublishedAt = v.CreatedAt.Add(-365 * 24 * time.Hour)
	if err := cat.UpsertVideo(ctx, v); err != nil {
		t.Fatalf("seed legacy crawler timestamp: %v", err)
	}
	if err := cat.DeleteVideoWithTombstone(ctx, v.ID); err != nil {
		t.Fatalf("delete with tombstone: %v", err)
	}
	if err := cat.RemoveDeletedVideo(ctx, v.ID); err != nil {
		t.Fatalf("remove deleted video: %v", err)
	}
	failRemote.Store(true)
	res, err = c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("restore run: %v", err)
	}
	if res.NewVideos != 0 || res.Skipped != 1 || res.Failed != 0 {
		t.Fatalf("restore crawl result = new:%d skipped:%d failed:%d, want 0/1/0", res.NewVideos, res.Skipped, res.Failed)
	}
	if res.SeenSnapshot != 1 {
		t.Fatalf("restore crawl seen snapshot = %d, want pending source treated as seen", res.SeenSnapshot)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests after skipped restore candidate = %d, want 1", got)
	}
	if _, err := cat.GetVideo(ctx, v.ID); err == nil {
		t.Fatal("video restored during crawl, want restore only after pipeline completion")
	}
	restoredCount, err := c.RestoreRequestedVideos(ctx)
	if err != nil {
		t.Fatalf("scan retained videos: %v", err)
	}
	if restoredCount != 1 {
		t.Fatalf("restored count = %d, want 1", restoredCount)
	}
	restored, err := cat.GetVideo(ctx, v.ID)
	if err != nil {
		t.Fatalf("get restored video: %v", err)
	}
	if restored.FileID != v.FileID || restored.Size != int64(len("restored-video-bytes")) {
		t.Fatalf("restored video = file:%q size:%d, want %q/%d", restored.FileID, restored.Size, v.FileID, len("restored-video-bytes"))
	}
	if restored.Title != v.Title || restored.PreviewStatus != "pending" {
		t.Fatalf("restored metadata = title:%q preview:%q, want %q/pending", restored.Title, restored.PreviewStatus, v.Title)
	}
	if !restored.PublishedAt.Equal(restored.CreatedAt) {
		t.Fatalf("restored timestamps = published %s created %s, want identical import time", restored.PublishedAt, restored.CreatedAt)
	}
	if deleted, err := cat.IsVideoDeleted(ctx, v.ID); err != nil || deleted {
		t.Fatalf("restored video tombstone remains: deleted=%v err=%v", deleted, err)
	}
}

func TestCrawlerRunOnceSkipsFingerprintDuplicateAndContinues(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	drv := New(Config{ID: "demo", RootDir: filepath.Join(tmp, "crawler")})
	if err := drv.Init(ctx); err != nil {
		t.Fatalf("driver init: %v", err)
	}

	seedFile := "seed-canonical.mp4"
	if err := os.WriteFile(filepath.Join(drv.VideosDir(), seedFile), []byte(scriptCrawlerDuplicateBytes), 0o644); err != nil {
		t.Fatalf("write seed video: %v", err)
	}
	seed := &catalog.Video{
		ID:          "seed-for-hash",
		DriveID:     drv.ID(),
		FileID:      seedFile,
		Title:       "Seed",
		Size:        int64(len(scriptCrawlerDuplicateBytes)),
		PublishedAt: time.Now(),
	}
	sampled, err := fingerprint.Compute(ctx, drv, seed, fingerprint.Config{}, nil)
	if err != nil {
		t.Fatalf("compute seed fingerprint: %v", err)
	}
	_ = os.Remove(filepath.Join(drv.VideosDir(), seedFile))

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:                "existing-canonical",
		DriveID:           "other-drive",
		FileID:            "existing.mp4",
		FileName:          "existing.mp4",
		Title:             "Existing Canonical",
		Size:              int64(len(scriptCrawlerDuplicateBytes)),
		Ext:               "mp4",
		SampledSHA256:     sampled,
		FingerprintStatus: "ready",
		PublishedAt:       now,
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatalf("seed canonical video: %v", err)
	}

	dummyScript := filepath.Join(tmp, "helper-script")
	if err := os.WriteFile(dummyScript, []byte("helper"), 0o755); err != nil {
		t.Fatalf("write dummy script: %v", err)
	}
	wrapper := filepath.Join(tmp, "helper-wrapper.sh")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestScriptCrawlerHelperProcess \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	t.Setenv("GO_WANT_SCRIPTCRAWLER_HELPER", "1")
	t.Setenv("GO_WANT_SCRIPTCRAWLER_DUP_UNIQUE", "1")
	c := NewCrawler(CrawlerConfig{
		Driver:              drv,
		Catalog:             cat,
		PythonPath:          wrapper,
		FFprobePath:         writeScriptCrawlerFFprobeStub(t, tmp, true),
		ScriptPath:          dummyScript,
		SkipProtocolRefresh: true,
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 1 || res.Skipped != 1 || res.Failed != 0 || res.TotalEntries != 2 {
		t.Fatalf("result = total:%d new:%d skipped:%d failed:%d, want 2/1/1/0", res.TotalEntries, res.NewVideos, res.Skipped, res.Failed)
	}
	if res.CandidateBudget <= res.TargetNew {
		t.Fatalf("candidate budget = %d, target = %d; want expanded budget", res.CandidateBudget, res.TargetNew)
	}
	if _, err := cat.GetVideo(ctx, BuildVideoID("demo", "dup-source")); err == nil {
		t.Fatal("duplicate candidate should not be imported")
	}
	if _, err := os.Stat(filepath.Join(drv.VideosDir(), "dup-source.mp4")); !os.IsNotExist(err) {
		t.Fatalf("duplicate local file stat = %v, want removed", err)
	}
	v, err := cat.GetVideo(ctx, BuildVideoID("demo", "unique-source"))
	if err != nil {
		t.Fatalf("unique video should be imported: %v", err)
	}
	if v.SampledSHA256 == "" || v.FingerprintStatus != "ready" {
		t.Fatalf("unique fingerprint = %q status=%q, want ready sampled fingerprint", v.SampledSHA256, v.FingerprintStatus)
	}
	seen, err := cat.ListCrawlerSourceIDs(ctx, Kind, "demo")
	if err != nil {
		t.Fatalf("list seen source ids: %v", err)
	}
	seenSet := map[string]bool{}
	for _, id := range seen {
		seenSet[id] = true
	}
	if !seenSet["dup-source"] || !seenSet["unique-source"] {
		t.Fatalf("seen ids = %#v, want duplicate and imported source ids", seen)
	}
}

func TestCrawlerProcessItemSkipsNearDuplicateByTitleDurationAndThumbnail(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	drv := New(Config{ID: "demo", RootDir: filepath.Join(tmp, "crawler")})
	if err := drv.Init(ctx); err != nil {
		t.Fatalf("driver init: %v", err)
	}
	commonThumbDir := filepath.Join(tmp, "common-thumbs")
	if err := os.MkdirAll(commonThumbDir, 0o755); err != nil {
		t.Fatalf("mkdir common thumbs: %v", err)
	}

	now := time.Now()
	canonicalID := "existing-canonical"
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:              canonicalID,
		DriveID:         "other-drive",
		FileID:          "existing.mp4",
		FileName:        "existing.mp4",
		Title:           "91 Test Similar Title 1215516",
		DurationSeconds: 257,
		Size:            12345,
		Ext:             "mp4",
		ThumbnailURL:    "/p/thumb/" + canonicalID,
		PublishedAt:     now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}); err != nil {
		t.Fatalf("seed canonical video: %v", err)
	}
	writeScriptCrawlerJPEG(t, mediaasset.ThumbnailPathInDir(commonThumbDir, canonicalID), color.RGBA{R: 210, G: 40, B: 40, A: 255})

	outputDir := drv.OutputDir()
	mediaPath := filepath.Join(outputDir, "near-video.mp4")
	if err := os.WriteFile(mediaPath, []byte("near-duplicate-but-different-bytes"), 0o644); err != nil {
		t.Fatalf("write media: %v", err)
	}
	thumbPath := filepath.Join(outputDir, "near-thumb.jpg")
	writeScriptCrawlerJPEG(t, thumbPath, color.RGBA{R: 211, G: 41, B: 41, A: 255})

	c := NewCrawler(CrawlerConfig{
		Driver:         drv,
		Catalog:        cat,
		FFprobePath:    writeScriptCrawlerFFprobeStub(t, tmp, true),
		CommonThumbDir: commonThumbDir,
	})
	imported, err := c.processItem(ctx, Item{
		SourceID:        "near-source",
		Title:           "91 Test Similar Title 1215516 - source suffix",
		Author:          "helper",
		DurationSeconds: 257,
		Media:           MediaRef{LocalFile: mediaPath},
		Thumbnail:       MediaRef{LocalFile: thumbPath},
	})
	if err != nil {
		t.Fatalf("process item: %v", err)
	}
	if imported {
		t.Fatal("near duplicate imported, want skipped")
	}
	if _, err := cat.GetVideo(ctx, BuildVideoID("demo", "near-source")); err == nil {
		t.Fatal("near duplicate should not be inserted into catalog")
	}
	if _, err := os.Stat(filepath.Join(drv.VideosDir(), "near-source.mp4")); !os.IsNotExist(err) {
		t.Fatalf("near duplicate video stat = %v, want removed", err)
	}
	if sourceThumb, err := drv.ThumbPath("near-source.jpg"); err != nil {
		t.Fatalf("source thumb path: %v", err)
	} else if _, err := os.Stat(sourceThumb); !os.IsNotExist(err) {
		t.Fatalf("source thumb stat = %v, want removed", err)
	}
	if _, err := os.Stat(mediaasset.ThumbnailPathInDir(commonThumbDir, BuildVideoID("demo", "near-source"))); !os.IsNotExist(err) {
		t.Fatalf("common thumb stat = %v, want removed", err)
	}
	seen, err := cat.ListCrawlerSourceIDs(ctx, Kind, "demo")
	if err != nil {
		t.Fatalf("list seen source ids: %v", err)
	}
	if !hasString(seen, "near-source") {
		t.Fatalf("seen ids = %#v, want near-source", seen)
	}
}

func TestCrawlerProcessItemKeepsLargerNearDuplicate(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	drv := New(Config{ID: "demo", RootDir: filepath.Join(tmp, "crawler")})
	if err := drv.Init(ctx); err != nil {
		t.Fatalf("driver init: %v", err)
	}
	commonThumbDir := filepath.Join(tmp, "common-thumbs")
	if err := os.MkdirAll(commonThumbDir, 0o755); err != nil {
		t.Fatalf("mkdir common thumbs: %v", err)
	}

	now := time.Now()
	smallerID := "smaller-canonical"
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:              smallerID,
		DriveID:         "other-drive",
		FileID:          "smaller.mp4",
		FileName:        "smaller.mp4",
		Title:           "91 Test Larger Candidate 1215516",
		DurationSeconds: 257,
		Size:            5,
		Ext:             "mp4",
		ThumbnailURL:    "/p/thumb/" + smallerID,
		PublishedAt:     now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}); err != nil {
		t.Fatalf("seed smaller video: %v", err)
	}
	writeScriptCrawlerWebP(t, mediaasset.ThumbnailPathInDir(commonThumbDir, smallerID))

	outputDir := drv.OutputDir()
	mediaPath := filepath.Join(outputDir, "larger-video.mp4")
	if err := os.WriteFile(mediaPath, []byte("near-duplicate-larger-candidate-bytes"), 0o644); err != nil {
		t.Fatalf("write media: %v", err)
	}
	thumbPath := filepath.Join(outputDir, "larger-thumb.jpg")
	writeScriptCrawlerWebP(t, thumbPath)

	c := NewCrawler(CrawlerConfig{
		Driver:         drv,
		Catalog:        cat,
		FFprobePath:    writeScriptCrawlerFFprobeStub(t, tmp, true),
		CommonThumbDir: commonThumbDir,
	})
	imported, err := c.processItem(ctx, Item{
		SourceID:        "larger-source",
		Title:           "91 Test Larger Candidate 1215516 - source suffix",
		DurationSeconds: 257,
		Media:           MediaRef{LocalFile: mediaPath},
		Thumbnail:       MediaRef{LocalFile: thumbPath},
	})
	if err != nil {
		t.Fatalf("process item: %v", err)
	}
	if !imported {
		t.Fatal("larger near duplicate was skipped, want imported")
	}
	if _, err := cat.GetVideo(ctx, smallerID); err == nil {
		t.Fatal("smaller near duplicate should be deleted from catalog")
	}
	if deleted, err := cat.IsVideoDeleted(ctx, smallerID); err != nil || !deleted {
		t.Fatalf("smaller tombstone = %v, %v; want deleted tombstone", deleted, err)
	}
	larger, err := cat.GetVideo(ctx, BuildVideoID("demo", "larger-source"))
	if err != nil {
		t.Fatalf("larger video should be imported: %v", err)
	}
	if larger.Size <= 5 {
		t.Fatalf("larger size = %d, want > 5", larger.Size)
	}
	if larger.Author != "" {
		t.Fatalf("larger author = %q, want empty when crawler omits author", larger.Author)
	}
	normalizedThumbPath := mediaasset.ThumbnailPathInDir(commonThumbDir, larger.ID)
	normalizedThumb, err := os.Open(normalizedThumbPath)
	if err != nil {
		t.Fatalf("open normalized thumbnail: %v", err)
	}
	defer normalizedThumb.Close()
	if _, err := jpeg.Decode(normalizedThumb); err != nil {
		t.Fatalf("crawler thumbnail was not normalized to JPEG: %v", err)
	}
}

func TestCrawlerRunOnceRejectsInvalidDownloadedVideo(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	drv := New(Config{ID: "demo", RootDir: filepath.Join(tmp, "crawler")})
	if err := drv.Init(ctx); err != nil {
		t.Fatalf("driver init: %v", err)
	}
	dummyScript := filepath.Join(tmp, "helper-script")
	if err := os.WriteFile(dummyScript, []byte("helper"), 0o755); err != nil {
		t.Fatalf("write dummy script: %v", err)
	}
	wrapper := filepath.Join(tmp, "helper-wrapper.sh")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestScriptCrawlerHelperProcess \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	t.Setenv("GO_WANT_SCRIPTCRAWLER_HELPER", "1")
	c := NewCrawler(CrawlerConfig{
		Driver:              drv,
		Catalog:             cat,
		CrawlerName:         "Demo Crawler",
		PythonPath:          wrapper,
		FFprobePath:         writeScriptCrawlerFFprobeStub(t, tmp, false),
		ScriptPath:          dummyScript,
		SkipProtocolRefresh: true,
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 0 || res.Skipped != 0 || res.Failed != 1 || res.TotalEntries != 1 {
		t.Fatalf("result = total:%d new:%d skipped:%d failed:%d, want 1/0/0/1", res.TotalEntries, res.NewVideos, res.Skipped, res.Failed)
	}
	if _, err := cat.GetVideo(ctx, BuildVideoID("demo", "abc-123")); err == nil {
		t.Fatal("invalid video should not be imported")
	}
	if _, err := os.Stat(filepath.Join(drv.VideosDir(), "abc-123.mp4")); !os.IsNotExist(err) {
		t.Fatalf("invalid local video stat = %v, want removed", err)
	}
	seen, err := cat.ListCrawlerSourceIDs(ctx, Kind, "demo")
	if err != nil {
		t.Fatalf("list seen source ids: %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("seen ids = %#v, want none for invalid video", seen)
	}
}

func TestCrawlerRunOnceDownloadsHLSMediaURL(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	drv := New(Config{ID: "demo", RootDir: filepath.Join(tmp, "crawler")})
	if err := drv.Init(ctx); err != nil {
		t.Fatalf("driver init: %v", err)
	}
	dummyScript := filepath.Join(tmp, "helper-script")
	if err := os.WriteFile(dummyScript, []byte("helper"), 0o755); err != nil {
		t.Fatalf("write dummy script: %v", err)
	}
	wrapper := filepath.Join(tmp, "helper-wrapper.sh")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestScriptCrawlerHelperProcess \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	t.Setenv("GO_WANT_SCRIPTCRAWLER_HELPER", "1")
	t.Setenv("GO_WANT_SCRIPTCRAWLER_HLS", "1")
	ffmpegArgsFile := filepath.Join(tmp, "ffmpeg-args.txt")
	t.Setenv("GO_SCRIPTCRAWLER_FFMPEG_ARGS_FILE", ffmpegArgsFile)
	c := NewCrawler(CrawlerConfig{
		Driver:              drv,
		Catalog:             cat,
		CrawlerName:         "Demo Crawler",
		PythonPath:          wrapper,
		FFmpegPath:          writeScriptCrawlerFFmpegStub(t, tmp),
		FFprobePath:         writeScriptCrawlerFFprobeStub(t, tmp, true),
		ScriptPath:          dummyScript,
		SkipProtocolRefresh: true,
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 1 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("result = new:%d skipped:%d failed:%d, want 1/0/0", res.NewVideos, res.Skipped, res.Failed)
	}
	v, err := cat.GetVideo(ctx, BuildVideoID("demo", "hls-source"))
	if err != nil {
		t.Fatalf("get hls video: %v", err)
	}
	if v.FileID != "hls-source.mp4" || v.Size != int64(len("hls-video-bytes")) {
		t.Fatalf("video file=%q size=%d, want hls-source.mp4 size %d", v.FileID, v.Size, len("hls-video-bytes"))
	}
	data, err := os.ReadFile(filepath.Join(drv.VideosDir(), "hls-source.mp4"))
	if err != nil {
		t.Fatalf("read hls output: %v", err)
	}
	if string(data) != "hls-video-bytes" {
		t.Fatalf("hls output = %q", string(data))
	}
	argsData, err := os.ReadFile(ffmpegArgsFile)
	if err != nil {
		t.Fatalf("read ffmpeg args: %v", err)
	}
	argsText := "\n" + string(argsData) + "\n"
	for _, want := range []string{
		"\n-protocol_whitelist\nhttp,https,tcp,tls,crypto\n",
		"\n-allowed_extensions\nALL\n",
		"\n-allowed_segment_extensions\nALL\n",
		"\n-extension_picky\n0\n",
	} {
		if !strings.Contains(argsText, want) {
			t.Fatalf("ffmpeg args missing %q in:\n%s", strings.TrimSpace(want), string(argsData))
		}
	}
}

func TestPruneCrawlDirKeepsRecentHistory(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(name string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	var seenNames, jobNames []string
	for i := 0; i < crawlHistoryKeep+3; i++ {
		runID := fmt.Sprintf("20250101T00000%dZ", i)
		seenNames = append(seenNames, "seen-"+runID+".txt")
		jobNames = append(jobNames, "job-"+runID+".json")
		writeFile(seenNames[i])
		writeFile(jobNames[i])
	}
	freshPart := writeFile("seen-20250102T000000Z.txt.part")
	stalePart := writeFile("job-20240101T000000Z.json.part")
	old := time.Now().Add(-2 * crawlPartMaxAge)
	if err := os.Chtimes(stalePart, old, old); err != nil {
		t.Fatalf("age stale part: %v", err)
	}
	unrelated := writeFile("notes.txt")
	matchingDir := filepath.Join(dir, "seen-20250103T000000Z.txt")
	if err := os.Mkdir(matchingDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	pruneCrawlDir(dir)

	pruned := append(append([]string{}, seenNames[:3]...), jobNames[:3]...)
	for _, name := range pruned {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should be pruned, stat err=%v", name, err)
		}
	}
	kept := append(append([]string{}, seenNames[3:]...), jobNames[3:]...)
	for _, name := range kept {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s should survive: %v", name, err)
		}
	}
	if _, err := os.Stat(freshPart); err != nil {
		t.Fatalf("fresh .part should survive: %v", err)
	}
	if _, err := os.Stat(stalePart); !os.IsNotExist(err) {
		t.Fatalf("stale .part should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated file should survive: %v", err)
	}
	if _, err := os.Stat(matchingDir); err != nil {
		t.Fatalf("directory should survive: %v", err)
	}
}

func TestCrawlerRunOncePrunesCrawlHistory(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	drv := New(Config{ID: "demo", RootDir: filepath.Join(tmp, "crawler")})
	if err := drv.Init(ctx); err != nil {
		t.Fatalf("driver init: %v", err)
	}
	crawlDir := drv.CrawlDir()
	var staleSeen, staleJobs []string
	for i := 0; i < crawlHistoryKeep+2; i++ {
		runID := fmt.Sprintf("20250101T00000%dZ", i)
		staleSeen = append(staleSeen, "seen-"+runID+".txt")
		staleJobs = append(staleJobs, "job-"+runID+".json")
		for _, name := range []string{staleSeen[i], staleJobs[i]} {
			if err := os.WriteFile(filepath.Join(crawlDir, name), []byte("x"), 0o644); err != nil {
				t.Fatalf("seed %s: %v", name, err)
			}
		}
	}
	dummyScript := filepath.Join(tmp, "helper-script")
	if err := os.WriteFile(dummyScript, []byte("helper"), 0o755); err != nil {
		t.Fatalf("write dummy script: %v", err)
	}
	wrapper := filepath.Join(tmp, "helper-wrapper.sh")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestScriptCrawlerHelperProcess \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	t.Setenv("GO_WANT_SCRIPTCRAWLER_HELPER", "1")
	c := NewCrawler(CrawlerConfig{
		Driver:              drv,
		Catalog:             cat,
		CrawlerName:         "Demo Crawler",
		PythonPath:          wrapper,
		FFprobePath:         writeScriptCrawlerFFprobeStub(t, tmp, true),
		ScriptPath:          dummyScript,
		SkipProtocolRefresh: true,
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}

	for _, name := range []string{staleSeen[0], staleSeen[1], staleJobs[0], staleJobs[1]} {
		if _, err := os.Stat(filepath.Join(crawlDir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should be pruned, stat err=%v", name, err)
		}
	}
	for i := 2; i < len(staleSeen); i++ {
		for _, name := range []string{staleSeen[i], staleJobs[i]} {
			if _, err := os.Stat(filepath.Join(crawlDir, name)); err != nil {
				t.Fatalf("%s should survive: %v", name, err)
			}
		}
	}
	if _, err := os.Stat(res.SeenFile); err != nil {
		t.Fatalf("current run seen file: %v", err)
	}
	if _, err := os.Stat(res.JobFile); err != nil {
		t.Fatalf("current run job file: %v", err)
	}
	entries, err := os.ReadDir(crawlDir)
	if err != nil {
		t.Fatalf("read crawl dir: %v", err)
	}
	seenCount, jobCount := 0, 0
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "seen-") && strings.HasSuffix(name, ".txt") {
			seenCount++
		}
		if strings.HasPrefix(name, "job-") && strings.HasSuffix(name, ".json") {
			jobCount++
		}
	}
	if seenCount != crawlHistoryKeep+1 || jobCount != crawlHistoryKeep+1 {
		t.Fatalf("crawl dir = %d seen / %d job files, want %d each", seenCount, jobCount, crawlHistoryKeep+1)
	}
}

func TestScriptCrawlerHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_SCRIPTCRAWLER_HELPER") != "1" {
		return
	}
	args := os.Args
	jobPath := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--job" {
			jobPath = args[i+1]
			break
		}
	}
	if jobPath == "" {
		fmt.Fprintln(os.Stderr, "missing --job")
		os.Exit(2)
	}
	data, err := os.ReadFile(jobPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if os.Getenv("GO_WANT_SCRIPTCRAWLER_ASSERT_ABS") == "1" {
		if !filepath.IsAbs(jobPath) || !filepath.IsAbs(job.SeenSourceIDsFile) || !filepath.IsAbs(job.OutputDir) {
			fmt.Fprintf(os.Stderr, "expected absolute paths, got job=%q seen=%q output=%q\n", jobPath, job.SeenSourceIDsFile, job.OutputDir)
			os.Exit(2)
		}
	}
	if os.Getenv("GO_WANT_SCRIPTCRAWLER_SIMPLE") == "1" {
		event := map[string]any{
			"title":     "Simple Protocol Video",
			"media_url": os.Getenv("GO_SCRIPTCRAWLER_MEDIA_URL"),
		}
		_ = json.NewEncoder(os.Stdout).Encode(event)
		os.Exit(0)
	}
	if os.Getenv("GO_WANT_SCRIPTCRAWLER_HLS") == "1" {
		event := Event{
			Type: "item",
			Item: Item{
				SourceID: "hls-source",
				Title:    "HLS Protocol Video",
				Author:   "helper",
				Media: MediaRef{
					URL: "https://media.example.test/video.m3u8",
					Headers: map[string]string{
						"Referer": "https://example.test/",
					},
				},
			},
		}
		_ = json.NewEncoder(os.Stdout).Encode(event)
		os.Exit(0)
	}
	if os.Getenv("GO_WANT_SCRIPTCRAWLER_DUP_UNIQUE") == "1" {
		duplicateFile := filepath.Join(job.OutputDir, "duplicate.mp4")
		if err := os.WriteFile(duplicateFile, []byte(scriptCrawlerDuplicateBytes), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		uniqueFile := filepath.Join(job.OutputDir, "unique.mp4")
		if err := os.WriteFile(uniqueFile, []byte(scriptCrawlerUniqueBytes), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		for _, event := range []Event{
			{
				Type: "item",
				Item: Item{
					SourceID: "dup-source",
					Title:    "Duplicate Candidate",
					Author:   "helper",
					Media:    MediaRef{LocalFile: duplicateFile},
				},
			},
			{
				Type: "item",
				Item: Item{
					SourceID: "unique-source",
					Title:    "Unique Candidate",
					Author:   "helper",
					Media:    MediaRef{LocalFile: uniqueFile},
				},
			},
		} {
			_ = json.NewEncoder(os.Stdout).Encode(event)
		}
		os.Exit(0)
	}
	localFile := filepath.Join(job.OutputDir, "helper.mp4")
	if err := os.WriteFile(localFile, []byte("helper-video"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	// Include the retired field to verify rolling compatibility with scripts
	// deployed before crawler timestamps became backend-owned. It must decode
	// successfully but cannot affect the stored timestamp.
	event := map[string]any{
		"type":             "item",
		"source_id":        "abc-123",
		"title":            "Imported From Helper",
		"author":           "helper",
		"media_local_file": localFile,
		"published_at":     "2021-11-05",
	}
	_ = json.NewEncoder(os.Stdout).Encode(event)
	os.Exit(0)
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
