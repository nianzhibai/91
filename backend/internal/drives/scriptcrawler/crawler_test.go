package scriptcrawler

import (
	"context"
	"database/sql"
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

func serveScriptCrawlerMedia(t *testing.T, payload string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, payload)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/video.mp4"
}

func serveScriptCrawlerFiles(t *testing.T, dir string) string {
	t.Helper()
	server := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(server.Close)
	return server.URL
}

func TestCrawlerRunOnceDownloadsVideoAndSkipsExisting(t *testing.T) {
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
	if err := os.WriteFile(dummyScript, []byte("CRAWLER_NAME = 'Test'\nCRAWLER_PROTOCOL = 'crawler.v3'\n"), 0o755); err != nil {
		t.Fatalf("write dummy script: %v", err)
	}
	wrapper := filepath.Join(tmp, "helper-wrapper.sh")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestScriptCrawlerHelperProcess \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	t.Setenv("GO_WANT_SCRIPTCRAWLER_HELPER", "1")
	c := NewCrawler(CrawlerConfig{
		Driver:      drv,
		Catalog:     cat,
		CrawlerName: "Demo Crawler",
		PythonPath:  wrapper,
		FFprobePath: writeScriptCrawlerFFprobeStub(t, tmp, true),
		ScriptPath:  dummyScript,
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 1 || (res.Known+res.Duplicates) != 0 || res.Failed != 0 {
		t.Fatalf("result = new:%d skipped:%d failed:%d, want 1/0/0", res.NewVideos, (res.Known + res.Duplicates), res.Failed)
	}
	v, err := cat.GetVideo(ctx, importVideoID("demo", "abc-123"))
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if v.Title != "Imported From Helper" || v.FileID != mediaFileStem("abc-123")+".mp4" || v.Size == 0 {
		t.Fatalf("video = title:%q file:%q size:%d", v.Title, v.FileID, v.Size)
	}
	if !v.PublishedAt.Equal(v.CreatedAt) {
		t.Fatalf("crawler timestamps = published %s created %s, want identical backend import time", v.PublishedAt, v.CreatedAt)
	}
	if !hasString(v.Tags, "Demo Crawler") {
		t.Fatalf("video tags = %#v, want crawler name tag", v.Tags)
	}
	if _, err := os.Stat(filepath.Join(drv.VideosDir(), mediaFileStem("abc-123")+".mp4")); err != nil {
		t.Fatalf("video file not copied: %v", err)
	}

	res, err = c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.NewVideos != 0 || (res.Known+res.Duplicates) != 1 {
		t.Fatalf("second result = new:%d skipped:%d, want 0/1", res.NewVideos, (res.Known + res.Duplicates))
	}
	if res.Known != 1 {
		t.Fatalf("seen snapshot = %d, want 1", res.Known)
	}
}

func TestCrawlerRunOnceKeepsPreviewPendingForGlobalScheduling(t *testing.T) {
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
	if err := os.WriteFile(dummyScript, []byte("CRAWLER_NAME = 'Test'\nCRAWLER_PROTOCOL = 'crawler.v3'\n"), 0o755); err != nil {
		t.Fatalf("write dummy script: %v", err)
	}
	wrapper := filepath.Join(tmp, "helper-wrapper.sh")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestScriptCrawlerHelperProcess \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	t.Setenv("GO_WANT_SCRIPTCRAWLER_HELPER", "1")
	c := NewCrawler(CrawlerConfig{
		Driver:      drv,
		Catalog:     cat,
		PythonPath:  wrapper,
		FFprobePath: writeScriptCrawlerFFprobeStub(t, tmp, true),
		ScriptPath:  dummyScript,
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 1 || res.Failed != 0 {
		t.Fatalf("result = new:%d failed:%d, want 1/0", res.NewVideos, res.Failed)
	}
	v, err := cat.GetVideo(ctx, importVideoID("demo", "abc-123"))
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if v.PreviewStatus != "pending" {
		t.Fatalf("preview status = %q, want pending", v.PreviewStatus)
	}
	if v.FingerprintStatus != "ready" || v.SampledSHA256 == "" {
		t.Fatalf("fingerprint status=%q sampled=%q, want ready and sampled hash", v.FingerprintStatus, v.SampledSHA256)
	}
	pending, err := cat.ListVideosByPreviewStatus(ctx, "demo", "pending", 0)
	if err != nil {
		t.Fatalf("list pending previews: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending previews = %d, want 1", len(pending))
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
	if err := os.WriteFile(dummyScript, []byte("CRAWLER_NAME = 'Test'\nCRAWLER_PROTOCOL = 'crawler.v3'\n"), 0o755); err != nil {
		t.Fatalf("write dummy script: %v", err)
	}
	wrapper := filepath.Join(tmp, "helper-wrapper.sh")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestScriptCrawlerHelperProcess \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	t.Setenv("GO_WANT_SCRIPTCRAWLER_HELPER", "1")
	c := NewCrawler(CrawlerConfig{
		Driver:      drv,
		Catalog:     cat,
		PythonPath:  wrapper,
		FFprobePath: writeScriptCrawlerFFprobeStub(t, tmp, true),
		ScriptPath:  dummyScript,
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 1 || res.Known != 0 {
		t.Fatalf("result = new:%d seen:%d, want 1/0", res.NewVideos, res.Known)
	}
	videoID := importVideoID("demo", "abc-123")
	if _, err := cat.GetVideo(ctx, videoID); err != nil {
		t.Fatalf("get crawler video: %v", err)
	}

	res, err = c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.NewVideos != 0 || (res.Known+res.Duplicates) != 1 || res.Known != 1 {
		t.Fatalf("second result = new:%d skipped:%d seen:%d, want 0/1/1", res.NewVideos, (res.Known + res.Duplicates), res.Known)
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
	if err := os.WriteFile(dummyScript, []byte("CRAWLER_NAME = 'Test'\nCRAWLER_PROTOCOL = 'crawler.v3'\n"), 0o755); err != nil {
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
		Driver:      drv,
		Catalog:     cat,
		PythonPath:  wrapper,
		FFprobePath: writeScriptCrawlerFFprobeStub(t, tmp, true),
		ScriptPath:  dummyScript,

		WorkDir: scriptDir,
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 1 || (res.Known+res.Duplicates) != 0 || res.Failed != 0 {
		t.Fatalf("result = new:%d skipped:%d failed:%d, want 1/0/0", res.NewVideos, (res.Known + res.Duplicates), res.Failed)
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
		ID:   drv.ID(),
		Kind: Kind,
		Name: "Demo",
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
	dummyScript := filepath.Join(tmp, "helper-script")
	if err := os.WriteFile(dummyScript, []byte("CRAWLER_NAME = 'Test'\nCRAWLER_PROTOCOL = 'crawler.v3'\n"), 0o755); err != nil {
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
		Driver:      drv,
		Catalog:     cat,
		PythonPath:  wrapper,
		FFprobePath: writeScriptCrawlerFFprobeStub(t, tmp, true),
		ScriptPath:  dummyScript,

		HTTPClient: srv.Client(),
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
	if _, err := cat.EnsureTag(ctx, "保留标签", "user"); err != nil {
		t.Fatal(err)
	}
	deletedTag, err := cat.EnsureTag(ctx, "已删除标签", "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.SetManualVideoTags(ctx, v.ID, []string{"保留标签", "已删除标签"}); err != nil {
		t.Fatal(err)
	}
	crawlerTag, err := cat.EnsureCrawlerTag(ctx, "Demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.EnsureCrawlerTagForVideo(ctx, v.ID, crawlerTag.Label); err != nil {
		t.Fatal(err)
	}
	if err := cat.DeleteVideoWithTombstone(ctx, v.ID); err != nil {
		t.Fatalf("delete with tombstone: %v", err)
	}
	if err := cat.ReconcileVideoTags(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found, err := cat.LookupTagLabel(ctx, crawlerTag.Label); err != nil || !found {
		t.Fatalf("unreferenced crawler tag was removed: found=%v err=%v", found, err)
	}
	if _, err := cat.DeleteTag(ctx, deletedTag.ID); err != nil {
		t.Fatal(err)
	}
	if err := cat.RemoveDeletedVideo(ctx, v.ID); err != nil {
		t.Fatalf("remove deleted video: %v", err)
	}
	failRemote.Store(true)
	res, err = c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("restore run: %v", err)
	}
	if res.NewVideos != 0 || (res.Known+res.Duplicates) != 1 || res.Failed != 0 {
		t.Fatalf("restore crawl result = new:%d skipped:%d failed:%d, want 0/1/0", res.NewVideos, (res.Known + res.Duplicates), res.Failed)
	}
	if res.Known != 1 {
		t.Fatalf("restore crawl seen snapshot = %d, want pending source treated as seen", res.Known)
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
	if len(restored.Tags) != 2 || restored.Tags[0] != crawlerTag.Label || restored.Tags[1] != "保留标签" {
		t.Fatalf("restored tags = %v, want crawler source and surviving manual tag", restored.Tags)
	}
	if _, found, err := cat.LookupTagLabel(ctx, "已删除标签"); err != nil || found {
		t.Fatalf("crawler restore recreated a deleted tag: found=%v err=%v", found, err)
	}
	metadata, err := cat.ListVideoTagMetadata(ctx, []string{v.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := metadata[v.ID]["保留标签"].Source; got != "manual" {
		t.Fatalf("restored tag assignment source = %q, want manual", got)
	}
	if got := metadata[v.ID][crawlerTag.Label]; got.Source != "crawler" || got.Evidence != "爬虫:"+crawlerTag.Label {
		t.Fatalf("restored crawler tag assignment = %#v", got)
	}
	if deleted, err := cat.IsVideoDeleted(ctx, v.ID); err != nil || deleted {
		t.Fatalf("restored video tombstone remains: deleted=%v err=%v", deleted, err)
	}
}

func TestImporterRestoreRequestedVideosMatchesCurrentRulesForTaglessVideo(t *testing.T) {
	for _, manual := range []bool{false, true} {
		name := "automatic"
		if manual {
			name = "manual empty selection"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			cat, err := catalog.Open(filepath.Join(root, "catalog.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cat.Close() })
			drv := New(Config{ID: "tagless", RootDir: filepath.Join(root, "crawler")})
			if err := drv.Init(ctx); err != nil {
				t.Fatal(err)
			}
			if err := cat.UpsertDrive(ctx, &catalog.Drive{ID: drv.ID(), Kind: Kind, Name: "Crawler", RootID: "/"}); err != nil {
				t.Fatal(err)
			}
			media := []byte("retained media")
			if err := os.WriteFile(filepath.Join(drv.VideosDir(), "retained.mp4"), media, 0o600); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			video := &catalog.Video{
				ID: BuildVideoID(drv.ID(), "source-1"), DriveID: drv.ID(), FileID: "retained.mp4", FileName: "retained.mp4",
				Title: "travel clip", Size: int64(len(media)), PublishedAt: now, CreatedAt: now,
			}
			if err := cat.UpsertVideo(ctx, video); err != nil {
				t.Fatal(err)
			}
			if manual {
				if err := cat.SetManualVideoTags(ctx, video.ID, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := cat.DeleteVideoWithTombstone(ctx, video.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := cat.CreateTagAndClassify(ctx, "travel", "user"); err != nil {
				t.Fatal(err)
			}
			if err := cat.RemoveDeletedVideo(ctx, video.ID); err != nil {
				t.Fatal(err)
			}
			importer := &Importer{cfg: ImporterConfig{Driver: drv, Catalog: cat}}
			if count, err := importer.RestoreRequestedVideos(ctx); err != nil || count != 1 {
				t.Fatalf("restore retained source: count=%d err=%v", count, err)
			}
			saved, err := cat.GetVideo(ctx, video.ID)
			if err != nil {
				t.Fatal(err)
			}
			if manual {
				if len(saved.Tags) != 0 {
					t.Fatalf("restore filled manually cleared tags: %v", saved.Tags)
				}
			} else if len(saved.Tags) != 1 || saved.Tags[0] != "travel" {
				t.Fatalf("restore did not apply current rule: %v", saved.Tags)
			}
		})
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
	if err := os.WriteFile(dummyScript, []byte("CRAWLER_NAME = 'Test'\nCRAWLER_PROTOCOL = 'crawler.v3'\n"), 0o755); err != nil {
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
		Driver:      drv,
		Catalog:     cat,
		PythonPath:  wrapper,
		FFprobePath: writeScriptCrawlerFFprobeStub(t, tmp, true),
		ScriptPath:  dummyScript,
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 1 || (res.Known+res.Duplicates) != 1 || res.Failed != 0 || res.Checked != 2 {
		t.Fatalf("result = total:%d new:%d skipped:%d failed:%d, want 2/1/1/0", res.Checked, res.NewVideos, (res.Known + res.Duplicates), res.Failed)
	}
	if _, err := cat.GetVideo(ctx, importVideoID("demo", "dup-source")); err == nil {
		t.Fatal("duplicate candidate should not be imported")
	}
	if _, err := os.Stat(filepath.Join(drv.VideosDir(), mediaFileStem("dup-source")+".mp4")); !os.IsNotExist(err) {
		t.Fatalf("duplicate local file stat = %v, want removed", err)
	}
	v, err := cat.GetVideo(ctx, importVideoID("demo", "unique-source"))
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
	assertCrawlerDuplicateRecord(t, filepath.Join(tmp, "catalog.db"), importVideoID("demo", "dup-source"), "existing-canonical", "sampled_sha256", "skipped_import")
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
	mediaURL := serveScriptCrawlerFiles(t, outputDir)

	c := NewCrawler(CrawlerConfig{
		Driver:         drv,
		Catalog:        cat,
		FFprobePath:    writeScriptCrawlerFFprobeStub(t, tmp, true),
		CommonThumbDir: commonThumbDir,
	})
	imported, err := c.Import(ctx, ctx, Item{
		DiscoveryKey:    "detail:test",
		SourceID:        "near-source",
		Title:           "91 Test Similar Title 1215516 - source suffix",
		Author:          "helper",
		DurationSeconds: 257,
		Media:           MediaRef{Type: "url", URL: mediaURL + "/near-video.mp4"},
		Thumbnail:       &MediaRef{Type: "url", URL: mediaURL + "/near-thumb.jpg"},
	})
	if err != nil {
		t.Fatalf("process item: %v", err)
	}
	if imported != ImportDuplicate {
		t.Fatal("near duplicate imported, want skipped")
	}
	if _, err := cat.GetVideo(ctx, importVideoID("demo", "near-source")); err == nil {
		t.Fatal("near duplicate should not be inserted into catalog")
	}
	if _, err := os.Stat(filepath.Join(drv.VideosDir(), mediaFileStem("near-source")+".mp4")); !os.IsNotExist(err) {
		t.Fatalf("near duplicate video stat = %v, want removed", err)
	}
	if sourceThumb, err := drv.ThumbPath("near-source.jpg"); err != nil {
		t.Fatalf("source thumb path: %v", err)
	} else if _, err := os.Stat(sourceThumb); !os.IsNotExist(err) {
		t.Fatalf("source thumb stat = %v, want removed", err)
	}
	if _, err := os.Stat(mediaasset.ThumbnailPathInDir(commonThumbDir, importVideoID("demo", "near-source"))); !os.IsNotExist(err) {
		t.Fatalf("common thumb stat = %v, want removed", err)
	}
	seen, err := cat.ListCrawlerSourceIDs(ctx, Kind, "demo")
	if err != nil {
		t.Fatalf("list seen source ids: %v", err)
	}
	if !hasString(seen, "near-source") {
		t.Fatalf("seen ids = %#v, want near-source", seen)
	}
	assertCrawlerDuplicateRecord(t, filepath.Join(tmp, "catalog.db"), importVideoID("demo", "near-source"), canonicalID, "title_duration_thumbnail", "skipped_import")
}

func TestCrawlerProcessItemKeepsExistingNearDuplicateEvenWhenCandidateLarger(t *testing.T) {
	for _, location := range []string{"local", "uploaded"} {
		t.Run(location, func(t *testing.T) {
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
			existingDrive := drv.ID()
			if location == "uploaded" {
				existingDrive = "remote-drive"
			}
			existingFile := filepath.Join(drv.VideosDir(), "smaller.mp4")
			if location == "uploaded" {
				existingFile = filepath.Join(t.TempDir(), "remote-smaller.mp4")
			}
			if err := os.WriteFile(existingFile, []byte("small"), 0o600); err != nil {
				t.Fatal(err)
			}
			previewFile := filepath.Join(commonThumbDir, "existing-preview.mp4")
			if err := os.WriteFile(previewFile, []byte("preview"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := cat.UpsertVideo(ctx, &catalog.Video{
				ID:              smallerID,
				DriveID:         existingDrive,
				FileID:          "smaller.mp4",
				FileName:        "smaller.mp4",
				Title:           "91 Test Larger Candidate 1215516",
				DurationSeconds: 257,
				Size:            5,
				Ext:             "mp4",
				ThumbnailURL:    "/p/thumb/" + smallerID,
				Views:           7, Favorites: 3, PreviewStatus: "ready", PreviewLocal: "existing-preview.mp4",
				PublishedAt: now,
				CreatedAt:   now,
				UpdatedAt:   now,
			}); err != nil {
				t.Fatalf("seed smaller video: %v", err)
			}
			writeScriptCrawlerWebP(t, mediaasset.ThumbnailPathInDir(commonThumbDir, smallerID))
			if _, err := cat.SetVisitReaction(ctx, smallerID, "visit-1234567890123456", catalog.VideoReactionLike); err != nil {
				t.Fatal(err)
			}
			if err := cat.CreateVideoShare(ctx, "share", "share-token", smallerID, time.Now()); err != nil {
				t.Fatal(err)
			}
			before, err := cat.GetVideo(ctx, smallerID)
			if err != nil {
				t.Fatal(err)
			}

			outputDir := drv.OutputDir()
			mediaPath := filepath.Join(outputDir, "larger-video.mp4")
			if err := os.WriteFile(mediaPath, []byte("near-duplicate-larger-candidate-bytes"), 0o644); err != nil {
				t.Fatalf("write media: %v", err)
			}
			thumbPath := filepath.Join(outputDir, "larger-thumb.jpg")
			writeScriptCrawlerWebP(t, thumbPath)
			mediaURL := serveScriptCrawlerFiles(t, outputDir)

			c := NewCrawler(CrawlerConfig{
				Driver:         drv,
				Catalog:        cat,
				FFprobePath:    writeScriptCrawlerFFprobeStub(t, tmp, true),
				CommonThumbDir: commonThumbDir,
			})
			imported, err := c.Import(ctx, ctx, Item{
				DiscoveryKey:    "detail:test",
				SourceID:        "larger-source",
				Title:           "91 Test Larger Candidate 1215516 - source suffix",
				DurationSeconds: 257,
				Media:           MediaRef{Type: "url", URL: mediaURL + "/larger-video.mp4"},
				Thumbnail:       &MediaRef{Type: "url", URL: mediaURL + "/larger-thumb.jpg"},
			})
			if err != nil {
				t.Fatalf("process item: %v", err)
			}
			if imported != ImportDuplicate {
				t.Fatalf("outcome=%s, want duplicate", imported)
			}
			existing, err := cat.GetVideo(ctx, smallerID)
			if err != nil || existing.Size != 5 {
				t.Fatalf("existing modified: %+v %v", existing, err)
			}
			if deleted, err := cat.IsVideoDeleted(ctx, smallerID); err != nil || deleted {
				t.Fatalf("existing tombstoned: %v %v", deleted, err)
			}
			if _, err := cat.GetVideo(ctx, importVideoID("demo", "larger-source")); err == nil {
				t.Fatal("duplicate imported")
			}
			if _, err := os.Stat(mediaasset.ThumbnailPathInDir(commonThumbDir, smallerID)); err != nil {
				t.Fatal(err)
			}
			if existing.DriveID != before.DriveID || existing.FileID != before.FileID || existing.Likes != 1 || existing.Views != 7 || existing.Favorites != 3 || !existing.UpdatedAt.Equal(before.UpdatedAt) {
				t.Fatalf("existing associations changed: before=%+v after=%+v", before, existing)
			}
			for _, path := range []string{existingFile, previewFile, mediaasset.ThumbnailPathInDir(commonThumbDir, smallerID)} {
				if _, err := os.Stat(path); err != nil {
					t.Fatal("existing asset removed:", err)
				}
			}
			for _, path := range []string{filepath.Join(drv.VideosDir(), mediaFileStem("larger-source")+".mp4"), mediaasset.ThumbnailPathInDir(commonThumbDir, importVideoID("demo", "larger-source"))} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("candidate file retained: %s %v", path, err)
				}
			}
			db, err := sql.Open("sqlite", filepath.Join(tmp, "catalog.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var shareVideo string
			if err := db.QueryRow(`SELECT video_id FROM video_shares WHERE id='share'`).Scan(&shareVideo); err != nil || shareVideo != smallerID {
				t.Fatalf("share changed: %s %v", shareVideo, err)
			}
			known, err := cat.KnownCrawlerCandidates(ctx, drv.ID(), []catalog.CrawlerIdentity{{DiscoveryKey: "detail:test"}})
			if err != nil || !known["detail:test"] {
				t.Fatalf("missing discovery alias: %v %v", known, err)
			}
			var canonical string
			if err := db.QueryRow(`SELECT canonical_video_id FROM crawler_seen_sources WHERE drive_id='demo' AND source_id='larger-source' AND status='duplicate'`).Scan(&canonical); err != nil || canonical != smallerID {
				t.Fatalf("history: %s %v", canonical, err)
			}
			assertCrawlerDuplicateRecord(t, filepath.Join(tmp, "catalog.db"), importVideoID("demo", "larger-source"), smallerID, "title_duration_thumbnail", "skipped_import")
		})
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
	if err := os.WriteFile(dummyScript, []byte("CRAWLER_NAME = 'Test'\nCRAWLER_PROTOCOL = 'crawler.v3'\n"), 0o755); err != nil {
		t.Fatalf("write dummy script: %v", err)
	}
	wrapper := filepath.Join(tmp, "helper-wrapper.sh")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestScriptCrawlerHelperProcess \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	t.Setenv("GO_WANT_SCRIPTCRAWLER_HELPER", "1")
	c := NewCrawler(CrawlerConfig{
		Driver:      drv,
		Catalog:     cat,
		CrawlerName: "Demo Crawler",
		PythonPath:  wrapper,
		FFprobePath: writeScriptCrawlerFFprobeStub(t, tmp, false),
		ScriptPath:  dummyScript,
	})
	res, err := c.RunOnce(ctx, 1)
	if err == nil {
		t.Fatal("expected import failure to reach caller")
	}
	if res.NewVideos != 0 || (res.Known+res.Duplicates) != 0 || res.Failed != 1 || res.Checked != 1 {
		t.Fatalf("result = total:%d new:%d skipped:%d failed:%d, want 1/0/0/1", res.Checked, res.NewVideos, (res.Known + res.Duplicates), res.Failed)
	}
	if _, err := cat.GetVideo(ctx, importVideoID("demo", "abc-123")); err == nil {
		t.Fatal("invalid video should not be imported")
	}
	if _, err := os.Stat(filepath.Join(drv.VideosDir(), mediaFileStem("abc-123")+".mp4")); !os.IsNotExist(err) {
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
	if err := os.WriteFile(dummyScript, []byte("CRAWLER_NAME = 'Test'\nCRAWLER_PROTOCOL = 'crawler.v3'\n"), 0o755); err != nil {
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
		Driver:      drv,
		Catalog:     cat,
		CrawlerName: "Demo Crawler",
		PythonPath:  wrapper,
		FFmpegPath:  writeScriptCrawlerFFmpegStub(t, tmp),
		FFprobePath: writeScriptCrawlerFFprobeStub(t, tmp, true),
		ScriptPath:  dummyScript,
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 1 || (res.Known+res.Duplicates) != 0 || res.Failed != 0 {
		t.Fatalf("result = new:%d skipped:%d failed:%d, want 1/0/0", res.NewVideos, (res.Known + res.Duplicates), res.Failed)
	}
	v, err := cat.GetVideo(ctx, importVideoID("demo", "hls-source"))
	if err != nil {
		t.Fatalf("get hls video: %v", err)
	}
	if v.FileID != mediaFileStem("hls-source")+".mp4" || v.Size != int64(len("hls-video-bytes")) {
		t.Fatalf("video file=%q size=%d, want hls-source.mp4 size %d", v.FileID, v.Size, len("hls-video-bytes"))
	}
	data, err := os.ReadFile(filepath.Join(drv.VideosDir(), mediaFileStem("hls-source")+".mp4"))
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

func TestScriptCrawlerHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_SCRIPTCRAWLER_HELPER") != "1" {
		return
	}
	jobPath := ""
	for i, arg := range os.Args {
		if arg == "--job" && i+1 < len(os.Args) {
			jobPath = os.Args[i+1]
		}
	}
	data, err := os.ReadFile(jobPath)
	if err != nil {
		os.Exit(2)
	}
	var job Job
	if json.Unmarshal(data, &job) != nil {
		os.Exit(2)
	}
	if !filepath.IsAbs(jobPath) || !filepath.IsAbs(job.WorkDir) {
		os.Exit(2)
	}
	ids := []string{"abc-123"}
	if os.Getenv("GO_WANT_SCRIPTCRAWLER_DUP_UNIQUE") == "1" {
		ids = []string{"dup-source", "unique-source"}
	}
	if os.Getenv("GO_WANT_SCRIPTCRAWLER_HLS") == "1" {
		ids = []string{"hls-source"}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content := "helper-video"
		switch r.URL.Path {
		case "/dup-source.mp4":
			content = scriptCrawlerDuplicateBytes
		case "/unique-source.mp4":
			content = scriptCrawlerUniqueBytes
		}
		fmt.Fprint(w, content)
	}))
	defer server.Close()
	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for {
		var cmd struct {
			Type, RequestID string
			Candidate       Candidate
			Limit           int
		}
		var raw map[string]json.RawMessage
		if decoder.Decode(&raw) != nil {
			os.Exit(2)
		}
		_ = json.Unmarshal(raw["type"], &cmd.Type)
		_ = json.Unmarshal(raw["request_id"], &cmd.RequestID)
		_ = json.Unmarshal(raw["candidate"], &cmd.Candidate)
		_ = json.Unmarshal(raw["limit"], &cmd.Limit)
		switch cmd.Type {
		case "discover":
			candidates := []Candidate{}
			for _, id := range ids {
				candidates = append(candidates, Candidate{DiscoveryKey: "detail:" + id, SourceID: id, Locator: json.RawMessage(`{}`)})
			}
			_ = encoder.Encode(pageResponse{envelope: envelope{Type: "page", RequestID: cmd.RequestID}, Items: candidates})
		case "resolve":
			id := cmd.Candidate.SourceID
			title := "Imported From Helper"
			if id == "dup-source" {
				title = "Duplicate Candidate"
			}
			if id == "unique-source" {
				title = "Unique Candidate"
			}
			media := MediaRef{Type: "url", URL: server.URL + "/" + id + ".mp4"}
			if os.Getenv("GO_WANT_SCRIPTCRAWLER_SIMPLE") == "1" {
				media = MediaRef{Type: "url", URL: os.Getenv("GO_SCRIPTCRAWLER_MEDIA_URL")}
			}
			if id == "hls-source" {
				media = MediaRef{Type: "url", URL: "https://media.example.test/video.m3u8", Headers: map[string]string{"Referer": "https://example.test/"}}
			}
			_ = encoder.Encode(itemResponse{envelope: envelope{Type: "item", RequestID: cmd.RequestID}, Item: Item{DiscoveryKey: cmd.Candidate.DiscoveryKey, SourceID: id, Title: title, Author: "helper", Media: media}})
		case "stop":
			_ = encoder.Encode(envelope{Type: "stopped", RequestID: cmd.RequestID})
			os.Exit(0)
		default:
			os.Exit(2)
		}
	}
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
