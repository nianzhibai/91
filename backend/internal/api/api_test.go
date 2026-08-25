package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/text/encoding/simplifiedchinese"

	"github.com/video-site/backend/internal/auth"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/mediaasset"
	"github.com/video-site/backend/internal/proxy"
	"github.com/video-site/backend/internal/subtitles"
)

func TestVideoSourceUsesDirectStreamForAvi(t *testing.T) {
	v := &catalog.Video{
		ID:      "video-1",
		DriveID: "drive-1",
		FileID:  "file-1",
		Ext:     "avi",
	}

	got := videoSource(v)

	if got != "/p/stream/drive-1/file-1" {
		t.Fatalf("video source = %q, want direct stream route", got)
	}
}

func TestVideoSourceUsesDirectStreamForMkv(t *testing.T) {
	v := &catalog.Video{
		ID:      "video-1",
		DriveID: "drive-1",
		FileID:  "file-1",
		Ext:     "mkv",
	}

	got := videoSource(v)

	if got != "/p/stream/drive-1/file-1" {
		t.Fatalf("video source = %q, want direct stream route", got)
	}
}

func TestVideoSourceKeepsDirectStreamForMp4(t *testing.T) {
	v := &catalog.Video{
		ID:      "video-1",
		DriveID: "drive-1",
		FileID:  "file-1",
		Ext:     "mp4",
	}

	got := videoSource(v)

	if got != "/p/stream/drive-1/file-1" {
		t.Fatalf("video source = %q, want direct stream route", got)
	}
}

func TestPlaybackMediaTypeDescribesSelectedResource(t *testing.T) {
	tests := []struct {
		name  string
		video *catalog.Video
		want  string
	}{
		{
			name:  "mp4",
			video: &catalog.Video{Ext: "MP4"},
			want:  "video/mp4",
		},
		{
			name:  "m4v",
			video: &catalog.Video{Ext: ".m4v"},
			want:  "video/mp4",
		},
		{
			name: "ready transcode",
			video: &catalog.Video{
				Ext:              "mkv",
				TranscodeStatus:  "ready",
				TranscodedFileID: "transcoded.mp4",
			},
			want: "video/mp4",
		},
		{
			name:  "untranscoded mkv",
			video: &catalog.Video{Ext: "mkv"},
			want:  "",
		},
		{
			name: "missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := playbackMediaType(tt.video); got != tt.want {
				t.Fatalf("media type = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVideoURLsEscapePathSegments(t *testing.T) {
	updated := time.UnixMilli(1778863000123)
	v := &catalog.Video{
		ID:                 "wopan-drive-fid/with space",
		DriveID:            "drive-1",
		FileID:             "fid/with space",
		Title:              "Video",
		ThumbnailURL:       "/p/thumb/wopan-drive-fid/with space",
		ThumbnailUpdatedAt: updated,
		PreviewUpdatedAt:   updated,
		UpdatedAt:          updated,
	}

	dto := mapVideo(v)
	if dto.Href != "/video/wopan-drive-fid%2Fwith%20space" {
		t.Fatalf("href = %q, want escaped video id", dto.Href)
	}
	if dto.PreviewSrc != "/p/preview/wopan-drive-fid%2Fwith%20space?v=1778863000123" {
		t.Fatalf("preview = %q, want escaped video id", dto.PreviewSrc)
	}
	if dto.Thumbnail != "/p/thumb/wopan-drive-fid%2Fwith%20space?v=1778863000123" {
		t.Fatalf("thumbnail = %q, want escaped video id", dto.Thumbnail)
	}
	if got := videoSource(v); got != "/p/stream/drive-1/fid%2Fwith%20space" {
		t.Fatalf("video source = %q, want escaped file id", got)
	}
}

func TestVideoDTOOmitsRetiredQualityMetadata(t *testing.T) {
	encoded, err := json.Marshal(mapVideo(&catalog.Video{
		ID:    "video-1",
		Title: "Video",
	}))
	if err != nil {
		t.Fatalf("marshal video DTO: %v", err)
	}
	if bytes.Contains(encoded, []byte(`"quality"`)) {
		t.Fatalf("video DTO still contains retired quality metadata: %s", encoded)
	}
}

func TestThumbnailURLRewritesStoredLocalURLForUnsafeVideoID(t *testing.T) {
	got := thumbnailURL(&catalog.Video{
		ID:                 "wopan-drive-fid/with space",
		ThumbnailURL:       "/p/thumb/wopan-drive-fid/with space",
		ThumbnailUpdatedAt: time.UnixMilli(1778863000123),
	})

	if got != "/p/thumb/wopan-drive-fid%2Fwith%20space?v=1778863000123" {
		t.Fatalf("thumbnail URL = %q, want escaped local URL", got)
	}
}

func TestHandleStreamDecodesEscapedWildcardFileID(t *testing.T) {
	local := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(local, []byte("ok"), 0o644); err != nil {
		t.Fatalf("write local video: %v", err)
	}
	drv := &apiStreamFakeDrive{localPath: local}
	reg := proxy.NewRegistry()
	reg.Set("drive-1", drv)
	srv := &Server{Proxy: proxy.New(reg)}

	router := chi.NewRouter()
	router.Get("/p/stream/{driveID}/*", srv.handleStream)
	req := httptest.NewRequest(http.MethodGet, "/p/stream/drive-1/fid%2Fwith%20space", nil)
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if drv.fileID != "fid/with space" {
		t.Fatalf("fileID = %q, want decoded original", drv.fileID)
	}
}

func TestHandleVideoSubtitlesUsesAnonymousClient(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	now := time.Now()
	const contentHash = "0123456789abcdef0123456789abcdef01234567"
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:              "video-1",
		DriveID:         "unmounted-drive",
		FileID:          "file-1",
		FileName:        "movie HND-970.mp4",
		ContentHash:     contentHash,
		Title:           "Movie",
		DurationSeconds: 257,
		PublishedAt:     now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	client := &apiFakeSubtitleClient{subtitles: []subtitles.Subtitle{
		{Name: "简体中文", Ext: "srt", Language: "zh-CN", URL: "https://subtitle.example/movie.srt", SourceLabel: "inner"},
		{Name: "繁体中文", Ext: "ssa", Language: "zh-TW", URL: "https://subtitle.example/movie.ssa", SourceLabel: "online"},
		{Name: "PGS", Ext: "sup", Language: "ja", URL: "https://subtitle.example/movie.sup", SourceLabel: "online"},
		{Name: "empty-url", Ext: "srt", Language: "en", SourceLabel: "online"},
	}}
	// No proxy registry or GuangYaPan drive is mounted.
	srv := &Server{Catalog: cat, SubtitleClient: client}

	router := chi.NewRouter()
	router.Get("/api/video/{id}/subtitles", srv.handleVideoSubtitles)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/video/video-1/subtitles", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	request := client.subtitleReq
	if request.FileID != "file-1" || request.FileName != "movie HND-970.mp4" ||
		request.ContentHash != contentHash || request.DurationSeconds != 257 ||
		!reflect.DeepEqual(request.LookupNames, []string{"HND-970"}) {
		t.Fatalf("subtitle request = %#v, want catalog metadata and filename alias", request)
	}
	var got []SubtitleDTO
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode subtitles: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("subtitles = %#v, want only supported text subtitles", got)
	}
	if got[0].URL != "/p/subtitle/video-1/0" || got[0].Type != "srt" || got[0].Ext != "srt" || got[0].Label != "zh-CN · 简体中文 · SRT" {
		t.Fatalf("first subtitle dto = %#v", got[0])
	}
	if got[1].URL != "/p/subtitle/video-1/1" || got[1].Type != "ass" || got[1].Ext != "ssa" || got[1].Label != "zh-TW · 繁体中文 · SSA" {
		t.Fatalf("second subtitle dto = %#v", got[1])
	}
}

func TestLoadVideoSubtitlesCoversEverySourceWithoutMountedDrive(t *testing.T) {
	const sampledSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	client := &apiFakeSubtitleClient{subtitles: []subtitles.Subtitle{
		{ID: "sub-b", Name: "字幕 B", Ext: "srt", URL: "https://subtitle.example/b.srt"},
		{ID: "sub-a", Name: "字幕 A", Ext: "srt", URL: "https://subtitle.example/a.srt"},
		{ID: "sub-pgs", Name: "PGS", Ext: "sup", URL: "https://subtitle.example/a.sup"},
	}}
	srv := &Server{SubtitleClient: client}
	driveIDs := []string{"pikpak-drive", "p115-drive", "ordinary-drive", localUploadDriveID}
	for index, driveID := range driveIDs {
		video := &catalog.Video{
			ID:              fmt.Sprintf("video-%d", index),
			DriveID:         driveID,
			FileID:          fmt.Sprintf("file-%d", index),
			FileName:        "movie.mp4",
			ContentHash:     "provider-content-hash",
			SampledSHA256:   sampledSHA256,
			DurationSeconds: 257,
		}
		subs, err := srv.loadVideoSubtitles(context.Background(), video)
		if err != nil {
			t.Fatalf("loadVideoSubtitles(%s): %v", driveID, err)
		}
		if len(subs) != 2 || subs[0].ID != "sub-a" || subs[1].ID != "sub-b" {
			t.Fatalf("subtitles for %s = %#v, want stable supported results", driveID, subs)
		}
	}
	if client.subtitleCalls != len(driveIDs) {
		t.Fatalf("subtitle calls = %d, want one for every source", client.subtitleCalls)
	}
	if request := client.subtitleReq; request.ContentHash != "provider-content-hash" ||
		request.SampledSHA256 != sampledSHA256 || request.DurationSeconds != 257 {
		t.Fatalf("last subtitle request = %#v", request)
	}
}

func TestLoadVideoSubtitlesWithoutClientReturnsEmpty(t *testing.T) {
	subs, err := (&Server{}).loadVideoSubtitles(context.Background(), &catalog.Video{
		DriveID:  "unmounted-drive",
		FileID:   "foreign-file-id",
		FileName: "movie.mp4",
	})
	if err != nil {
		t.Fatalf("loadVideoSubtitles: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("subtitles = %#v, want empty without an injected client", subs)
	}
}

func TestLoadVideoSubtitlesConvertsClientErrorsToCachedEmptyResults(t *testing.T) {
	now := time.Unix(1000, 0)
	client := &apiFakeSubtitleClient{subtitleErr: errors.New("temporary upstream failure")}
	srv := &Server{SubtitleClient: client, subtitleCacheNow: func() time.Time { return now }}
	video := &catalog.Video{ID: "video-error", DriveID: "offline-drive", FileID: "file-error", FileName: "movie.mp4"}

	for range 2 {
		subs, err := srv.loadVideoSubtitles(context.Background(), video)
		if err != nil || len(subs) != 0 {
			t.Fatalf("client error result = %#v, %v; want empty without error", subs, err)
		}
	}
	if client.subtitleCalls != 1 {
		t.Fatalf("error subtitle calls = %d, want cached empty result", client.subtitleCalls)
	}
	now = now.Add(time.Minute + time.Second)
	_, _ = srv.loadVideoSubtitles(context.Background(), video)
	if client.subtitleCalls != 2 {
		t.Fatalf("expired error subtitle calls = %d, want 2", client.subtitleCalls)
	}
}

func TestLoadVideoSubtitlesCachesPositiveAndEmptyResults(t *testing.T) {
	now := time.Unix(1000, 0)
	positive := &apiFakeSubtitleClient{subtitles: []subtitles.Subtitle{{ID: "sub", Ext: "srt", URL: "https://subtitle.example/sub.srt"}}}
	srv := &Server{SubtitleClient: positive, subtitleCacheNow: func() time.Time { return now }}
	video := &catalog.Video{ID: "video-1", DriveID: "drive-1", FileID: "file-1", FileName: "movie.mp4", DurationSeconds: 100}

	for range 2 {
		if _, err := srv.loadVideoSubtitles(context.Background(), video); err != nil {
			t.Fatalf("loadVideoSubtitles: %v", err)
		}
	}
	if positive.subtitleCalls != 1 {
		t.Fatalf("positive subtitle calls = %d, want 1", positive.subtitleCalls)
	}
	now = now.Add(5*time.Minute + time.Second)
	_, _ = srv.loadVideoSubtitles(context.Background(), video)
	if positive.subtitleCalls != 2 {
		t.Fatalf("expired positive subtitle calls = %d, want 2", positive.subtitleCalls)
	}

	empty := &apiFakeSubtitleClient{}
	srv.SubtitleClient = empty
	emptyVideo := &catalog.Video{ID: "video-2", DriveID: localUploadDriveID, FileID: "file-2", FileName: "empty.mp4"}
	_, _ = srv.loadVideoSubtitles(context.Background(), emptyVideo)
	_, _ = srv.loadVideoSubtitles(context.Background(), emptyVideo)
	if empty.subtitleCalls != 1 {
		t.Fatalf("empty subtitle calls = %d, want 1", empty.subtitleCalls)
	}
	now = now.Add(time.Minute + time.Second)
	_, _ = srv.loadVideoSubtitles(context.Background(), emptyVideo)
	if empty.subtitleCalls != 2 {
		t.Fatalf("expired empty subtitle calls = %d, want 2", empty.subtitleCalls)
	}
}

func TestSubtitleLookupAliasesUsesFilenameThenVideoIDThenTitle(t *testing.T) {
	tests := []struct {
		video catalog.Video
		want  string
	}{
		{video: catalog.Video{FileName: "prefix DASS-984.mp4", ID: "video-HND-970", Title: "SSIS-001"}, want: "DASS-984"},
		{video: catalog.Video{FileName: "ordinary.mp4", ID: "crawler-HND-970", Title: "SSIS-001"}, want: "HND-970"},
		{video: catalog.Video{FileName: "ordinary.mp4", ID: "opaque", Title: "title SSIS-001"}, want: "SSIS-001"},
	}
	for _, tt := range tests {
		got := subtitleLookupAliases(&tt.video)
		if len(got) != 1 || got[0] != tt.want {
			t.Errorf("subtitleLookupAliases(%#v) = %#v, want %q", tt.video, got, tt.want)
		}
	}
}

func TestFilterSupportedSubtitlesDropsDurationMismatchThenOrdersByChineseConfidence(t *testing.T) {
	subs := []subtitles.Subtitle{
		{ID: "mismatch", Name: "中字", Ext: "srt", Language: "", URL: "https://sub.example/mismatch.srt", DurationSeconds: 900},
		{ID: "unknown", Name: "unknown", Ext: "srt", Language: "zh-CN", URL: "https://sub.example/unknown.srt"},
		{ID: "close", Name: "movie.chs", Ext: "srt", URL: "https://sub.example/close.srt", DurationSeconds: 1010},
		{ID: "exact-en", Name: "English", Ext: "srt", Language: "en", URL: "https://sub.example/en.srt", DurationSeconds: 1000},
		{ID: "exact-zh", Name: "Chinese", Ext: "srt", Language: "zh_TW", URL: "https://sub.example/zh.srt", DurationSeconds: 1000},
		{ID: "unsupported", Name: "PGS", Ext: "sup", URL: "https://sub.example/pgs.sup", DurationSeconds: 1000},
	}

	got := filterSupportedSubtitles(subs, 1000)
	want := []string{"exact-zh", "exact-en", "close", "unknown"}
	if len(got) != len(want) {
		t.Fatalf("subtitles = %#v, want %d supported items", got, len(want))
	}
	for index, id := range want {
		if got[index].ID != id {
			t.Fatalf("subtitle order = %#v, want %#v", subtitleIDs(got), want)
		}
	}
}

func TestFilterSupportedSubtitlesWithoutVideoDurationUsesLanguageConfidence(t *testing.T) {
	subs := []subtitles.Subtitle{
		{ID: "en", Name: "English", Ext: "srt", Language: "en", URL: "https://sub.example/en.srt"},
		{ID: "unknown", Name: "plain", Ext: "srt", URL: "https://sub.example/plain.srt"},
		{ID: "inferred", Name: "movie.zh-CN.srt", Ext: "srt", URL: "https://sub.example/inferred.srt"},
		{ID: "explicit", Name: "Chinese", Ext: "srt", Language: "zh", URL: "https://sub.example/explicit.srt"},
	}

	got := filterSupportedSubtitles(subs, 0)
	want := []string{"explicit", "inferred", "unknown", "en"}
	if !reflect.DeepEqual(subtitleIDs(got), want) {
		t.Fatalf("subtitle order = %#v, want %#v", subtitleIDs(got), want)
	}
}

func subtitleIDs(subs []subtitles.Subtitle) []string {
	out := make([]string, len(subs))
	for index, sub := range subs {
		out[index] = sub.ID
	}
	return out
}

func TestHandleSubtitleFileRefreshesExpiredSignedURL(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID: "video-refresh", DriveID: "drive-refresh", FileID: "file-refresh", FileName: "movie.mp4",
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/expired.srt" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("fresh subtitle"))
	}))
	defer upstream.Close()
	client := &apiFakeSubtitleClient{subtitleFunc: func(call int, _ subtitles.Request) ([]subtitles.Subtitle, error) {
		path := "/expired.srt"
		if call > 1 {
			path = "/fresh.srt"
		}
		return []subtitles.Subtitle{{ID: "stable", Ext: "srt", URL: upstream.URL + path}}, nil
	}}
	srv := &Server{Catalog: cat, SubtitleClient: client}
	if _, err := srv.loadVideoSubtitles(ctx, &catalog.Video{ID: "video-refresh", DriveID: "drive-refresh", FileID: "file-refresh", FileName: "movie.mp4"}); err != nil {
		t.Fatalf("prime subtitles: %v", err)
	}
	router := chi.NewRouter()
	router.Get("/p/subtitle/{id}/{index}", srv.handleSubtitleFile)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/p/subtitle/video-refresh/0", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "fresh subtitle" {
		t.Fatalf("status=%d body=%q, want refreshed subtitle", rr.Code, rr.Body.String())
	}
	if client.subtitleCalls != 2 {
		t.Fatalf("subtitle calls = %d, want prime plus one refresh", client.subtitleCalls)
	}
}

func TestHandleSubtitleFileProxiesSelectedSubtitle(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "video-1",
		DriveID:     "drive-1",
		FileID:      "file-1",
		FileName:    "movie.mp4",
		Title:       "Movie",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	const subtitleText = "1\n00:00:00,000 --> 00:00:01,000\n中文字幕\n"
	subtitleBytes, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(subtitleText))
	if err != nil {
		t.Fatalf("encode GB18030 subtitle: %v", err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/movie.srt" {
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(subtitleBytes)
	}))
	defer upstream.Close()

	client := &apiFakeSubtitleClient{subtitles: []subtitles.Subtitle{
		{Name: "简体中文", Ext: "srt", Language: "zh-CN", URL: upstream.URL + "/movie.srt", SourceLabel: "inner"},
	}}
	srv := &Server{Catalog: cat, SubtitleClient: client}

	router := chi.NewRouter()
	router.Get("/p/subtitle/{id}/{index}", srv.handleSubtitleFile)
	req := httptest.NewRequest(http.MethodGet, "/p/subtitle/video-1/0", nil)
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain", got)
	}
	if got := rr.Body.String(); got != subtitleText {
		t.Fatalf("body = %q", got)
	}
}

func TestVideoSourceUsesLocalUploadRoute(t *testing.T) {
	v := &catalog.Video{
		ID:      "video-1",
		DriveID: localUploadDriveID,
		FileID:  "upload-1.mp4",
		Ext:     "mp4",
	}

	got := videoSource(v)

	if got != "/p/upload/video-1" {
		t.Fatalf("video source = %q, want local upload route", got)
	}
}

func TestPreviewURLIncludesPreviewRevision(t *testing.T) {
	got := previewURL(&catalog.Video{
		ID:               "video-1",
		PreviewUpdatedAt: time.UnixMilli(1778863000123),
	})

	if got != "/p/preview/video-1?v=1778863000123" {
		t.Fatalf("preview URL = %q, want versioned URL", got)
	}
}

func TestPreviewURLFallsBackWithoutPreviewRevision(t *testing.T) {
	got := previewURL(&catalog.Video{ID: "video-1"})

	if got != "/p/preview/video-1" {
		t.Fatalf("preview URL = %q, want unversioned URL", got)
	}
}

func TestPublicWriteRoutesRequireAdminRole(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	hash, err := auth.HashPassword("secret123")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	userID, err := cat.CreateUser(ctx, "viewer", hash, "user")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := cat.CreateSession(ctx, "viewer-token", time.Hour, userID); err != nil {
		t.Fatalf("create session: %v", err)
	}

	router := chi.NewRouter()
	(&Server{Catalog: cat}).RegisterRoutes(router, &auth.Authenticator{Catalog: cat})
	req := httptest.NewRequest(http.MethodPut, "/api/video/video-1/tags", strings.NewReader(`{"tags":[]}`))
	req.AddCookie(&http.Cookie{Name: "vs_admin", Value: "viewer-token"})
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rr.Code, rr.Body.String())
	}
}

func TestHandleVideoDetailDecodesEscapedVideoID(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "wopan-drive-fid/with space",
		DriveID:     "drive-1",
		FileID:      "fid/with space",
		Title:       "Video",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	router := chi.NewRouter()
	router.Get("/api/video/{id}", (&Server{Catalog: cat}).handleVideoDetail)
	req := httptest.NewRequest(http.MethodGet, "/api/video/wopan-drive-fid%2Fwith%20space", nil)
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got VideoDetailDTO
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != "wopan-drive-fid/with space" {
		t.Fatalf("id = %q, want original video id", got.ID)
	}
}

func TestThumbnailURLVersionsLocalGeneratedThumbnails(t *testing.T) {
	got := thumbnailURL(&catalog.Video{
		ID:                 "video-1",
		ThumbnailURL:       "/p/thumb/video-1",
		ThumbnailUpdatedAt: time.UnixMilli(1778863000123),
		UpdatedAt:          time.UnixMilli(1779999999999),
	})
	if got != "/p/thumb/video-1?v=1778863000123" {
		t.Fatalf("thumbnail URL = %q, want versioned local URL", got)
	}

	remote := "https://thumb.example/video-1.jpg"
	got = thumbnailURL(&catalog.Video{
		ID:                 "video-1",
		ThumbnailURL:       remote,
		ThumbnailUpdatedAt: time.UnixMilli(1778863000123),
	})
	if got != remote {
		t.Fatalf("remote thumbnail URL = %q, want unchanged %q", got, remote)
	}

	got = thumbnailURL(&catalog.Video{
		ID:                 "video-pending",
		ThumbnailUpdatedAt: time.UnixMilli(1778863000123),
	})
	if got != "" {
		t.Fatalf("pending thumbnail URL = %q, want no advertised asset", got)
	}
}

func TestShortsBackgroundPosterURLUsesLocalThumbnailVariant(t *testing.T) {
	if got := shortsBackgroundPosterURL("/p/thumb/video-1?v=1778863000123"); got !=
		"/p/thumb/video-1?v=1778863000123&variant=shorts-bg" {
		t.Fatalf("versioned shorts background URL = %q", got)
	}
	if got := shortsBackgroundPosterURL("/p/thumb/video-1"); got !=
		"/p/thumb/video-1?variant=shorts-bg" {
		t.Fatalf("unversioned shorts background URL = %q", got)
	}

	remote := "https://thumb.example/video-1.jpg"
	if got := shortsBackgroundPosterURL(remote); got != "" {
		t.Fatalf("remote shorts background URL = %q, want omitted", got)
	}
}

func TestHandleHomePrioritizesVideosWithReadyThumbnails(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	for i := 0; i < 20; i++ {
		id := "pending-video-" + strconv.Itoa(i)
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID:          id,
			DriveID:     "drive",
			FileID:      id,
			Title:       id,
			PublishedAt: now.Add(time.Duration(i) * time.Minute),
			CreatedAt:   now.Add(time.Duration(i) * time.Minute),
			UpdatedAt:   now.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("seed pending video %s: %v", id, err)
		}
	}
	for i := 0; i < homePageSize+2; i++ {
		id := "ready-video-" + strconv.Itoa(i)
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID:           id,
			DriveID:      "drive",
			FileID:       id,
			Title:        id,
			ThumbnailURL: "https://thumb.example/" + id + ".jpg",
			PublishedAt:  now.Add(-time.Duration(i+1) * time.Hour),
			CreatedAt:    now.Add(-time.Duration(i+1) * time.Hour),
			UpdatedAt:    now.Add(-time.Duration(i+1) * time.Hour),
		}); err != nil {
			t.Fatalf("seed ready video %s: %v", id, err)
		}
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/home", nil)
	(&Server{Catalog: cat}).handleHome(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got []VideoDTO
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != homePageSize {
		t.Fatalf("home items = %d, want %d", len(got), homePageSize)
	}
	for _, item := range got {
		if !strings.HasPrefix(item.ID, "ready-video-") {
			t.Fatalf("home returned %q without a ready thumbnail; items=%#v", item.ID, got)
		}
		if !strings.HasPrefix(item.Thumbnail, "https://thumb.example/") {
			t.Fatalf("thumbnail for %q = %q, want ready thumbnail URL", item.ID, item.Thumbnail)
		}
	}
}

func newHomeRecommendationTestRoute(t *testing.T, total, readyCount int) (*Server, http.Handler, string) {
	t.Helper()
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	for i := 0; i < total; i++ {
		id := "session-home-video-" + strconv.Itoa(i)
		video := &catalog.Video{
			ID:          id,
			DriveID:     "drive",
			FileID:      id,
			Title:       id,
			PublishedAt: now.Add(time.Duration(i) * time.Minute),
			CreatedAt:   now.Add(time.Duration(i) * time.Minute),
			UpdatedAt:   now.Add(time.Duration(i) * time.Minute),
		}
		if i < readyCount {
			video.ThumbnailURL = "https://thumb.example/" + id + ".jpg"
		}
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatalf("seed home video %s: %v", id, err)
		}
	}

	const token = "home-recommendation-session"
	if err := cat.CreateSession(ctx, token, time.Hour, 0); err != nil {
		t.Fatalf("create session: %v", err)
	}
	server := &Server{Catalog: cat}
	router := chi.NewRouter()
	server.RegisterRoutes(router, &auth.Authenticator{Catalog: cat})
	return server, router, token
}

func requestHomeRecommendationBatch(t *testing.T, handler http.Handler, token string, count int) []VideoDTO {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/home?count="+strconv.Itoa(count), nil)
	req.AddCookie(&http.Cookie{Name: "vs_admin", Value: token})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("count %d status = %d, body = %s", count, rr.Code, rr.Body.String())
	}
	var got []VideoDTO
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode count %d response: %v", count, err)
	}
	return got
}

func requestHomeLatestBatch(t *testing.T, handler http.Handler, token string, count int) []VideoDTO {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/home/latest?count="+strconv.Itoa(count), nil)
	req.AddCookie(&http.Cookie{Name: "vs_admin", Value: token})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("latest count %d status = %d, body = %s", count, rr.Code, rr.Body.String())
	}
	var got []VideoDTO
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode latest count %d response: %v", count, err)
	}
	return got
}

func assertUniqueHomeBatch(t *testing.T, got []VideoDTO) {
	t.Helper()
	seen := make(map[string]struct{}, len(got))
	for _, item := range got {
		if _, duplicate := seen[item.ID]; duplicate {
			t.Fatalf("home batch returned duplicate video %q; items=%#v", item.ID, got)
		}
		seen[item.ID] = struct{}{}
	}
}

func TestNextHomeSnapshotBatchStopsAfterUnavailableFreshRound(t *testing.T) {
	server, _, _ := newHomeRecommendationTestRoute(t, 0, 0)
	loadCalls := 0

	items, roundVideoIDs, roundCursor, err := server.nextHomeSnapshotBatch(
		context.Background(),
		[]string{"removed-from-current-round"},
		0,
		8,
		func() ([]string, error) {
			loadCalls++
			return []string{"removed-from-fresh-round"}, nil
		},
	)
	if err != nil {
		t.Fatalf("load unavailable snapshots: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %d, want 0", len(items))
	}
	if loadCalls != 1 {
		t.Fatalf("fresh round loads = %d, want 1", loadCalls)
	}
	if len(roundVideoIDs) != 1 || roundVideoIDs[0] != "removed-from-fresh-round" || roundCursor != 1 {
		t.Fatalf("fresh round state = %#v at %d, want exhausted unavailable round", roundVideoIDs, roundCursor)
	}
}

func TestHomeRouteCompletesWholeLibraryBeforeRepeating(t *testing.T) {
	// Twenty ready thumbnails are returned first; the final eight pending
	// thumbnails still belong to the same complete-library round.
	server, router, token := newHomeRecommendationTestRoute(t, 28, 20)
	seen := make(map[string]struct{}, 28)
	for _, count := range []int{8, 12, 8} {
		batch := requestHomeRecommendationBatch(t, router, token, count)
		if len(batch) != count {
			t.Fatalf("count %d returned %d items", count, len(batch))
		}
		assertUniqueHomeBatch(t, batch)
		for _, item := range batch {
			if _, duplicate := seen[item.ID]; duplicate {
				t.Fatalf("video %q repeated before the 28-video round completed", item.ID)
			}
			seen[item.ID] = struct{}{}
		}
	}
	if len(seen) != 28 {
		t.Fatalf("completed round contained %d unique videos, want 28", len(seen))
	}

	if len(server.homeRecommendationSessions) != 1 {
		t.Fatalf("server session records = %d, want 1", len(server.homeRecommendationSessions))
	}
	for _, session := range server.homeRecommendationSessions {
		if len(session.roundVideoIDs) != 28 || session.roundCursor != 28 {
			t.Fatalf("round state = %d/%d, want 28/28", session.roundCursor, len(session.roundVideoIDs))
		}
	}

	nextRound := requestHomeRecommendationBatch(t, router, token, 8)
	if len(nextRound) != 8 {
		t.Fatalf("next round returned %d items, want 8", len(nextRound))
	}
	assertUniqueHomeBatch(t, nextRound)
}

func TestHomeLatestRouteRotatesOneBoundedSnapshotWithMixedGridSizes(t *testing.T) {
	server, router, token := newHomeRecommendationTestRoute(t, 110, 110)
	seen := make(map[string]struct{}, 28)
	for _, count := range []int{8, 12, 8} {
		batch := requestHomeLatestBatch(t, router, token, count)
		if len(batch) != count {
			t.Fatalf("latest count %d returned %d items", count, len(batch))
		}
		assertUniqueHomeBatch(t, batch)
		for _, item := range batch {
			if _, duplicate := seen[item.ID]; duplicate {
				t.Fatalf("latest video %q repeated before the snapshot completed", item.ID)
			}
			seen[item.ID] = struct{}{}
		}
	}
	if len(seen) != 28 {
		t.Fatalf("latest mixed batches contained %d unique videos, want 28", len(seen))
	}

	if len(server.homeRecommendationSessions) != 1 {
		t.Fatalf("server session records = %d, want 1", len(server.homeRecommendationSessions))
	}
	for _, session := range server.homeRecommendationSessions {
		if len(session.latestVideoIDs) != homeLatestSnapshotSize {
			t.Fatalf("latest snapshot size = %d, want %d", len(session.latestVideoIDs), homeLatestSnapshotSize)
		}
		if session.latestCursor != 28 {
			t.Fatalf("latest cursor = %d, want 28", session.latestCursor)
		}
	}
}

func TestHomeLatestRouteRefreshesSharedSnapshotAfterTTLWithoutRepeatingConsumedCards(t *testing.T) {
	server, router, token := newHomeRecommendationTestRoute(t, 20, 20)
	clock := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	server.homeRecommendationNow = func() time.Time { return clock }
	first := requestHomeLatestBatch(t, router, token, 8)
	firstIDs := make(map[string]struct{}, len(first))
	for _, item := range first {
		firstIDs[item.ID] = struct{}{}
	}

	now := time.Now().Add(24 * time.Hour)
	if err := server.Catalog.UpsertVideo(context.Background(), &catalog.Video{
		ID:           "new-latest-video",
		DriveID:      "drive",
		FileID:       "new-latest-video",
		Title:        "New latest video",
		ThumbnailURL: "https://thumb.example/new-latest-video.jpg",
		PublishedAt:  now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("seed new latest video: %v", err)
	}

	withinTTL := requestHomeLatestBatch(t, router, token, 8)
	if len(withinTTL) != 8 {
		t.Fatalf("cached latest batch returned %d items, want 8", len(withinTTL))
	}
	for _, item := range withinTTL {
		if item.ID == "new-latest-video" {
			t.Fatal("new video appeared before the shared latest snapshot TTL expired")
		}
	}

	clock = clock.Add(homeLatestSnapshotTTL)
	afterTTL := requestHomeLatestBatch(t, router, token, 4)
	if len(afterTTL) != 4 {
		t.Fatalf("refreshed latest batch returned %d items, want 4", len(afterTTL))
	}
	if afterTTL[0].ID != "new-latest-video" {
		t.Fatalf("first card after snapshot refresh = %q, want new latest video", afterTTL[0].ID)
	}
	for _, item := range append(withinTTL, afterTTL...) {
		if _, duplicate := firstIDs[item.ID]; duplicate {
			t.Fatalf("catalog refresh repeated consumed latest video %q", item.ID)
		}
	}
}

func TestHomeLatestRouteSerializesConcurrentRefreshesWithinOneSession(t *testing.T) {
	_, router, token := newHomeRecommendationTestRoute(t, 24, 24)
	type batchResult struct {
		items  []VideoDTO
		status int
		body   string
		err    error
	}
	start := make(chan struct{})
	results := make(chan batchResult, 2)
	for range 2 {
		go func() {
			<-start
			req := httptest.NewRequest(http.MethodGet, "/api/home/latest?count=12", nil)
			req.AddCookie(&http.Cookie{Name: "vs_admin", Value: token})
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			var items []VideoDTO
			err := json.NewDecoder(rr.Body).Decode(&items)
			results <- batchResult{items: items, status: rr.Code, body: rr.Body.String(), err: err}
		}()
	}
	close(start)

	seen := make(map[string]struct{}, 24)
	for range 2 {
		result := <-results
		if result.status != http.StatusOK || result.err != nil {
			t.Fatalf("concurrent latest response status=%d decode=%v body=%s", result.status, result.err, result.body)
		}
		if len(result.items) != 12 {
			t.Fatalf("concurrent latest batch returned %d items, want 12", len(result.items))
		}
		for _, item := range result.items {
			if _, duplicate := seen[item.ID]; duplicate {
				t.Fatalf("concurrent latest refreshes returned the same video %q", item.ID)
			}
			seen[item.ID] = struct{}{}
		}
	}
}

func TestHomeRouteSerializesConcurrentRefreshesWithinOneSession(t *testing.T) {
	_, router, token := newHomeRecommendationTestRoute(t, 24, 24)
	type batchResult struct {
		items  []VideoDTO
		status int
		body   string
		err    error
	}
	start := make(chan struct{})
	results := make(chan batchResult, 2)
	for range 2 {
		go func() {
			<-start
			req := httptest.NewRequest(http.MethodGet, "/api/home?count=12", nil)
			req.AddCookie(&http.Cookie{Name: "vs_admin", Value: token})
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			var items []VideoDTO
			err := json.NewDecoder(rr.Body).Decode(&items)
			results <- batchResult{items: items, status: rr.Code, body: rr.Body.String(), err: err}
		}()
	}
	close(start)

	seen := make(map[string]struct{}, 24)
	for range 2 {
		result := <-results
		if result.status != http.StatusOK || result.err != nil {
			t.Fatalf("concurrent response status=%d decode=%v body=%s", result.status, result.err, result.body)
		}
		if len(result.items) != 12 {
			t.Fatalf("concurrent batch returned %d items, want 12", len(result.items))
		}
		assertUniqueHomeBatch(t, result.items)
		for _, item := range result.items {
			if _, duplicate := seen[item.ID]; duplicate {
				t.Fatalf("concurrent refreshes returned the same video %q", item.ID)
			}
			seen[item.ID] = struct{}{}
		}
	}
	if len(seen) != 24 {
		t.Fatalf("concurrent refreshes returned %d unique videos, want 24", len(seen))
	}
}

func TestHomeRouteFinishesOldRoundBeforeFillingFromNextRound(t *testing.T) {
	server, router, token := newHomeRecommendationTestRoute(t, 14, 14)
	first := requestHomeRecommendationBatch(t, router, token, 12)
	second := requestHomeRecommendationBatch(t, router, token, 12)
	if len(first) != 12 || len(second) != 12 {
		t.Fatalf("batch sizes = %d and %d, want 12 and 12", len(first), len(second))
	}
	assertUniqueHomeBatch(t, first)
	assertUniqueHomeBatch(t, second)

	firstIDs := make(map[string]struct{}, len(first))
	for _, item := range first {
		firstIDs[item.ID] = struct{}{}
	}
	allIDs := make(map[string]struct{}, 14)
	for id := range firstIDs {
		allIDs[id] = struct{}{}
	}
	newAtBoundary := 0
	for index, item := range second {
		_, appearedInFirst := firstIDs[item.ID]
		if !appearedInFirst {
			newAtBoundary++
			if index >= 2 {
				t.Fatalf("old-round remainder appeared after the next round started: %#v", second)
			}
		}
		allIDs[item.ID] = struct{}{}
	}
	if newAtBoundary != 2 || len(allIDs) != 14 {
		t.Fatalf("boundary batch exposed %d unseen videos and %d total, want 2 and 14", newAtBoundary, len(allIDs))
	}
	for _, session := range server.homeRecommendationSessions {
		if len(session.roundVideoIDs) != 14 || session.roundCursor != 10 {
			t.Fatalf("new round state = %d/%d, want 10/14", session.roundCursor, len(session.roundVideoIDs))
		}
	}
}

func TestHomeRouteDoesNotDuplicateCardsWhenLibraryIsSmallerThanGrid(t *testing.T) {
	_, router, token := newHomeRecommendationTestRoute(t, 5, 3)
	first := requestHomeRecommendationBatch(t, router, token, homePageSize)
	second := requestHomeRecommendationBatch(t, router, token, homePageSize)
	if len(first) != 5 || len(second) != 5 {
		t.Fatalf("small-library batch sizes = %d and %d, want 5 and 5", len(first), len(second))
	}
	assertUniqueHomeBatch(t, first)
	assertUniqueHomeBatch(t, second)
}

func TestHandleHomeRejectsInvalidRecommendationCount(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/home?count=13", nil)
	(&Server{}).handleHome(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
}

func TestHandleListLatestPrefersReadyThumbnails(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	for i := 0; i < 20; i++ {
		id := "pending-latest-" + strconv.Itoa(i)
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID:          id,
			DriveID:     "drive",
			FileID:      id,
			Title:       id,
			PublishedAt: now.Add(time.Duration(i) * time.Minute),
			CreatedAt:   now.Add(time.Duration(i) * time.Minute),
			UpdatedAt:   now.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("seed pending video %s: %v", id, err)
		}
	}
	for i := 0; i < 12; i++ {
		id := "ready-latest-" + strconv.Itoa(i)
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID:           id,
			DriveID:      "drive",
			FileID:       id,
			Title:        id,
			ThumbnailURL: "https://thumb.example/" + id + ".jpg",
			PublishedAt:  now.Add(-time.Duration(i+1) * time.Hour),
			CreatedAt:    now.Add(-time.Duration(i+1) * time.Hour),
			UpdatedAt:    now.Add(-time.Duration(i+1) * time.Hour),
		}); err != nil {
			t.Fatalf("seed ready video %s: %v", id, err)
		}
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/list?page=1&size=12&sort=latest", nil)
	(&Server{Catalog: cat}).handleList(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var got struct {
		Items []VideoDTO `json:"items"`
		Total int        `json:"total"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Total != 32 {
		t.Fatalf("total = %d, want all matching videos included", got.Total)
	}
	if len(got.Items) != 12 {
		t.Fatalf("items = %d, want 12", len(got.Items))
	}
	for _, item := range got.Items {
		if !strings.HasPrefix(item.ID, "ready-latest-") {
			t.Fatalf("latest list returned %q before ready thumbnails; items=%#v", item.ID, got.Items)
		}
		if !strings.HasPrefix(item.Thumbnail, "https://thumb.example/") {
			t.Fatalf("thumbnail for %q = %q, want ready thumbnail URL", item.ID, item.Thumbnail)
		}
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/list?page=1&size=12&sort=latest&count=false", nil)
	(&Server{Catalog: cat}).handleList(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("count=false status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got = struct {
		Items []VideoDTO `json:"items"`
		Total int        `json:"total"`
	}{}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode count=false response: %v", err)
	}
	if got.Total != 0 {
		t.Fatalf("count=false total = %d, want 0", got.Total)
	}
	if len(got.Items) != 12 {
		t.Fatalf("count=false items = %d, want 12", len(got.Items))
	}
}

func TestHandleListIgnoresCategoryQueryAndDoesNotExposeCategory(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	for _, v := range []*catalog.Video{
		{
			ID:          "video-a",
			DriveID:     "drive",
			FileID:      "file-a",
			Title:       "A",
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
		{
			ID:          "video-b",
			DriveID:     "drive",
			FileID:      "file-b",
			Title:       "B",
			PublishedAt: now.Add(-time.Hour),
			CreatedAt:   now,
			UpdatedAt:   now,
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/list?page=1&size=24&cat=alpha", nil)
	(&Server{Catalog: cat}).handleList(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Total != 2 || len(got.Items) != 2 {
		t.Fatalf("response total/items = %d/%d, want 2/2", got.Total, len(got.Items))
	}
	for _, item := range got.Items {
		if _, ok := item["category"]; ok {
			t.Fatalf("list response exposed category: %#v", item)
		}
	}
}

func TestHandleUploadVideoSavesFileVideoTagsAndQueuesPreview(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	if _, err := cat.CreateTagAndClassify(ctx, "自定义上传", nil, "user"); err != nil {
		t.Fatalf("create managed upload tag: %v", err)
	}

	var queued *catalog.Video
	server := &Server{
		Catalog:  cat,
		LocalDir: t.TempDir(),
		OnVideoUploaded: func(v *catalog.Video) {
			queued = v
		},
	}
	req := multipartUploadRequest(t, map[string]string{
		"title": "用户上传标题",
		"tags":  "奶子,女大,人妻,后入,制服,美臀,口交,自定义上传",
	}, "clip.mp4", "video-bytes")
	rr := httptest.NewRecorder()

	server.handleUploadVideo(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var dto VideoDTO
	if err := json.NewDecoder(rr.Body).Decode(&dto); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if dto.ID == "" {
		t.Fatal("response video id is empty")
	}
	got, err := cat.GetVideo(ctx, dto.ID)
	if err != nil {
		t.Fatalf("get uploaded video: %v", err)
	}
	if got.DriveID != localUploadDriveID {
		t.Fatalf("drive id = %q, want %q", got.DriveID, localUploadDriveID)
	}
	if got.Title != "用户上传标题" {
		t.Fatalf("title = %q, want submitted title", got.Title)
	}
	if got.FileID != "用户上传标题.mp4" || got.FileName != got.FileID {
		t.Fatalf("file identity = id %q name %q, want title-based physical name", got.FileID, got.FileName)
	}
	if !sameStringSet(got.Tags, []string{"奶子", "女大", "人妻", "后入", "制服", "美臀", "口交", "自定义上传"}) {
		t.Fatalf("tags = %#v, want selected tags", got.Tags)
	}
	if got.PreviewStatus != "pending" {
		t.Fatalf("preview status = %q, want pending", got.PreviewStatus)
	}
	path := filepath.Join(server.localUploadDir(), got.FileID)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if string(data) != "video-bytes" {
		t.Fatalf("uploaded file content = %q, want original bytes", string(data))
	}
	if queued == nil || queued.ID != got.ID {
		t.Fatalf("queued video = %#v, want uploaded video", queued)
	}
}

func TestHandleUploadVideoDefaultsBlankTitleToOriginalFileName(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	server := &Server{Catalog: cat, LocalDir: t.TempDir()}
	req := multipartUploadRequest(t, map[string]string{"title": "  "}, "holiday.clip.final.mp4", "video-bytes")
	rr := httptest.NewRecorder()

	server.handleUploadVideo(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var dto VideoDTO
	if err := json.NewDecoder(rr.Body).Decode(&dto); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	got, err := cat.GetVideo(ctx, dto.ID)
	if err != nil {
		t.Fatalf("get uploaded video: %v", err)
	}
	if got.Title != "holiday.clip.final" {
		t.Fatalf("title = %q, want original file name without extension", got.Title)
	}
	if got.FileID != "holiday.clip.final.mp4" || got.FileName != got.FileID {
		t.Fatalf("file identity = id %q name %q", got.FileID, got.FileName)
	}
}

func TestHandleUploadVideoAddsStableSuffixOnFilenameCollision(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	server := &Server{Catalog: cat, LocalDir: t.TempDir()}
	for i := 0; i < 2; i++ {
		req := multipartUploadRequest(t, map[string]string{"title": "同名视频"}, "clip.mp4", fmt.Sprintf("video-%d", i))
		rr := httptest.NewRecorder()
		server.handleUploadVideo(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("upload %d status = %d body=%s", i, rr.Code, rr.Body.String())
		}
		if i == 1 {
			var dto VideoDTO
			if err := json.NewDecoder(rr.Body).Decode(&dto); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got, err := cat.GetVideo(ctx, dto.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if !strings.HasPrefix(got.FileID, "同名视频-") || !strings.HasSuffix(got.FileID, ".mp4") {
				t.Fatalf("collision file id = %q", got.FileID)
			}
			if got.Title != strings.TrimSuffix(got.FileID, ".mp4") {
				t.Fatalf("title = %q file = %q", got.Title, got.FileID)
			}
		}
	}
}

func TestHandleUploadVideoRejectsFilenameUnsafeTitle(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	server := &Server{Catalog: cat, LocalDir: t.TempDir()}
	req := multipartUploadRequest(t, map[string]string{"title": "bad/title"}, "clip.mp4", "video")
	rr := httptest.NewRecorder()
	server.handleUploadVideo(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleUploadVideoRejectsUnsupportedTag(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	server := &Server{Catalog: cat, LocalDir: t.TempDir()}
	req := multipartUploadRequest(t, map[string]string{"tags": "奶子,不存在"}, "clip.mp4", "video-bytes")
	rr := httptest.NewRecorder()

	server.handleUploadVideo(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
}

func TestHandleUploadVideoRejectsGeneratedTag(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	if _, err := cat.EnsureCrawlerTag(context.Background(), "爬虫来源"); err != nil {
		t.Fatalf("create crawler tag: %v", err)
	}

	server := &Server{Catalog: cat, LocalDir: t.TempDir()}
	req := multipartUploadRequest(t, map[string]string{"tags": "爬虫来源"}, "clip.mp4", "video-bytes")
	rr := httptest.NewRecorder()
	server.handleUploadVideo(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
}

func TestHandleUploadedVideoServesLocalUploadFile(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	root := t.TempDir()
	localDir := filepath.Join(root, "previews")
	uploadDir := filepath.Join(root, "uploads")
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		t.Fatalf("mkdir uploads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(uploadDir, "upload-1.mp4"), []byte("video-bytes"), 0o644); err != nil {
		t.Fatalf("write upload: %v", err)
	}
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "video-1",
		DriveID:     localUploadDriveID,
		FileID:      "upload-1.mp4",
		Title:       "Uploaded",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	server := &Server{Catalog: cat, LocalDir: localDir}
	req := requestWithRouteParam(http.MethodGet, "/p/upload/video-1", "videoID", "video-1", strings.NewReader(``))
	rr := httptest.NewRecorder()

	server.handleUploadedVideo(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "video-bytes" {
		t.Fatalf("body = %q, want uploaded bytes", rr.Body.String())
	}
}

func TestHandlePreviewIgnoresRemotePreviewFileIDAndServesLocalFile(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	localDir := t.TempDir()
	localPreview := filepath.Join(localDir, "video-1.mp4")
	if err := os.WriteFile(localPreview, []byte("local teaser"), 0o644); err != nil {
		t.Fatalf("write local preview: %v", err)
	}
	now := time.Now()
	previewRevision := time.UnixMilli(1778863000123)
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:               "video-1",
		DriveID:          "drive-1",
		FileID:           "file-1",
		Title:            "Video",
		PreviewStatus:    "ready",
		PreviewFileID:    "remote-preview-file",
		PreviewLocal:     localPreview,
		PreviewUpdatedAt: previewRevision,
		PublishedAt:      now,
		CreatedAt:        now,
		UpdatedAt:        now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	server := &Server{
		Catalog:  cat,
		LocalDir: localDir,
		Proxy:    proxy.New(proxy.NewRegistry()),
	}
	req := requestWithRouteParam(http.MethodGet, "/p/preview/video-1", "videoID", "video-1", strings.NewReader(``))
	rr := httptest.NewRecorder()

	server.handlePreview(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "local teaser" {
		t.Fatalf("body = %q, want local teaser bytes", rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	versionedReq := requestWithRouteParam(
		http.MethodGet,
		"/p/preview/video-1?v=1778863000123",
		"videoID",
		"video-1",
		strings.NewReader(``),
	)
	versionedRR := httptest.NewRecorder()
	server.handlePreview(versionedRR, versionedReq)
	if versionedRR.Code != http.StatusOK {
		t.Fatalf("versioned status = %d, body = %s", versionedRR.Code, versionedRR.Body.String())
	}
	if got := versionedRR.Header().Get("Cache-Control"); got != "private, max-age=31536000, immutable" {
		t.Fatalf("versioned Cache-Control = %q, want immutable private cache", got)
	}

	staleReq := requestWithRouteParam(
		http.MethodGet,
		"/p/preview/video-1?v=stale",
		"videoID",
		"video-1",
		strings.NewReader(``),
	)
	staleRR := httptest.NewRecorder()
	server.handlePreview(staleRR, staleReq)
	if got := staleRR.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("stale Cache-Control = %q, want no-store", got)
	}
}

func TestHandlePreviewServesRestoredAbsolutePathWithRelativeLocalDir(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	localDir := filepath.Join(t.TempDir(), "previews")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("mkdir previews: %v", err)
	}
	localPreview := filepath.Join(localDir, "video-1.mp4")
	if err := os.WriteFile(localPreview, []byte("restored teaser"), 0o644); err != nil {
		t.Fatalf("write local preview: %v", err)
	}
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	relativeLocalDir, err := filepath.Rel(workingDir, localDir)
	if err != nil {
		t.Fatalf("make local preview dir relative: %v", err)
	}
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:            "video-1",
		DriveID:       "drive-1",
		FileID:        "file-1",
		Title:         "Video",
		PreviewStatus: "ready",
		PreviewLocal:  localPreview,
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	server := &Server{
		Catalog:  cat,
		LocalDir: relativeLocalDir,
		Proxy:    proxy.New(proxy.NewRegistry()),
	}
	req := requestWithRouteParam(http.MethodGet, "/p/preview/video-1", "videoID", "video-1", strings.NewReader(``))
	rr := httptest.NewRecorder()

	server.handlePreview(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "restored teaser" {
		t.Fatalf("body = %q, want restored teaser bytes", rr.Body.String())
	}
}

func TestHandleThumbServesHashedPathForLongVideoID(t *testing.T) {
	localDir := t.TempDir()
	longID := "localstorage-" + strings.Repeat("x", 240)
	thumbPath := mediaasset.ThumbnailPath(localDir, longID)
	if err := os.MkdirAll(filepath.Dir(thumbPath), 0o755); err != nil {
		t.Fatalf("mkdir thumb dir: %v", err)
	}
	if err := os.WriteFile(thumbPath, []byte("thumb-bytes"), 0o644); err != nil {
		t.Fatalf("write thumb: %v", err)
	}

	server := &Server{
		LocalDir: localDir,
		Proxy:    proxy.New(proxy.NewRegistry()),
	}
	req := requestWithRouteParam(http.MethodGet, "/p/thumb/"+longID, "videoID", longID, strings.NewReader(``))
	rr := httptest.NewRecorder()

	server.handleThumb(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "thumb-bytes" {
		t.Fatalf("body = %q, want thumb bytes", rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "private, max-age=86400" {
		t.Fatalf("unversioned Cache-Control = %q", got)
	}

	versionedReq := requestWithRouteParam(
		http.MethodGet,
		"/p/thumb/"+longID+"?v=1778863000123",
		"videoID",
		longID,
		strings.NewReader(``),
	)
	versionedRR := httptest.NewRecorder()
	server.handleThumb(versionedRR, versionedReq)
	if versionedRR.Code != http.StatusOK {
		t.Fatalf("versioned status = %d, body = %s", versionedRR.Code, versionedRR.Body.String())
	}
	if got := versionedRR.Header().Get("Cache-Control"); got != "private, max-age=31536000, immutable" {
		t.Fatalf("versioned Cache-Control = %q", got)
	}
}

func TestHandleThumbServesSmallPreblurredShortsBackground(t *testing.T) {
	localDir := t.TempDir()
	thumbPath := mediaasset.ThumbnailPath(localDir, "video-1")
	if err := os.MkdirAll(filepath.Dir(thumbPath), 0o755); err != nil {
		t.Fatalf("mkdir thumb dir: %v", err)
	}
	source, err := os.Create(thumbPath)
	if err != nil {
		t.Fatalf("create thumb: %v", err)
	}
	frame := image.NewRGBA(image.Rect(0, 0, 480, 270))
	for y := 0; y < frame.Bounds().Dy(); y++ {
		for x := 0; x < frame.Bounds().Dx(); x++ {
			frame.SetRGBA(x, y, color.RGBA{
				R: uint8(x % 256),
				G: uint8(y % 256),
				B: uint8((x + y) % 256),
				A: 255,
			})
		}
	}
	if err := jpeg.Encode(source, frame, &jpeg.Options{Quality: 80}); err != nil {
		_ = source.Close()
		t.Fatalf("encode thumb: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("close thumb: %v", err)
	}

	server := &Server{
		LocalDir: localDir,
		Proxy:    proxy.New(proxy.NewRegistry()),
	}
	req := requestWithRouteParam(
		http.MethodGet,
		"/p/thumb/video-1?variant=shorts-bg",
		"videoID",
		"video-1",
		strings.NewReader(``),
	)
	rr := httptest.NewRecorder()

	server.handleThumb(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	generated, err := jpeg.Decode(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode generated background: %v", err)
	}
	if longest := max(generated.Bounds().Dx(), generated.Bounds().Dy()); longest > 96 {
		t.Fatalf("generated dimensions = %v, want longest side <= 96", generated.Bounds())
	}
	if _, err := os.Stat(mediaasset.ShortsBackgroundPath(localDir, "video-1")); err != nil {
		t.Fatalf("generated background cache: %v", err)
	}
}

func TestHandleTagsReturnsUnifiedTagPool(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "清纯女大后入",
		Tags:        []string{"后入", "女大"},
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if _, err := cat.CreateTagAndClassify(ctx, "清纯", nil, "user"); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if err := cat.SetManualVideoTags(ctx, "video-1", []string{"后入", "女大", "清纯"}); err != nil {
		t.Fatalf("set manual tags: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	rr := httptest.NewRecorder()
	(&Server{Catalog: cat}).handleTags(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Count int    `json:"count"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	labels := make([]string, 0, len(got))
	for _, tag := range got {
		labels = append(labels, tag.Label)
	}
	if !containsString(labels, "清纯") {
		t.Fatalf("labels = %#v, want user tag 清纯", labels)
	}
	if !containsString(labels, "后入") {
		t.Fatalf("labels = %#v, want builtin tag 后入", labels)
	}
	var qingchunCount int
	for _, tag := range got {
		if tag.Label == "清纯" {
			qingchunCount = tag.Count
		}
	}
	if qingchunCount != 1 {
		t.Fatalf("清纯 count = %d, want 1; tags = %#v", qingchunCount, got)
	}
}

func TestHandleUploadTagsReturnsManagedUserChoices(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	if _, err := cat.CreateTagAndClassify(ctx, "自定义上传", nil, "user"); err != nil {
		t.Fatalf("create user tag: %v", err)
	}
	if _, err := cat.EnsureCrawlerTag(ctx, "爬虫来源"); err != nil {
		t.Fatalf("create crawler tag: %v", err)
	}

	rr := httptest.NewRecorder()
	(&Server{Catalog: cat}).handleUploadTags(
		rr,
		httptest.NewRequest(http.MethodGet, "/api/upload/tags", nil),
	)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", rr.Header().Get("Cache-Control"))
	}
	var got []TagDTO
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	labels := make([]string, 0, len(got))
	for _, tag := range got {
		labels = append(labels, tag.Label)
	}
	if !containsString(labels, "奶子") || !containsString(labels, "自定义上传") {
		t.Fatalf("labels = %#v, want builtin and user tags", labels)
	}
	if containsString(labels, "AV") || containsString(labels, "爬虫来源") {
		t.Fatalf("labels = %#v, want no inferred or crawler tags", labels)
	}
}

func TestInvalidateTagCachePublishesCatalogChangesImmediately(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	server := &Server{Catalog: cat}

	request := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	recorder := httptest.NewRecorder()
	server.handleTags(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"label":"美臀"`) {
		t.Fatalf("initial tags status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if _, err := cat.SetBuiltinTagsEnabled(ctx, false); err != nil {
		t.Fatalf("disable builtin tags: %v", err)
	}

	server.InvalidateTagCache()
	recorder = httptest.NewRecorder()
	server.handleTags(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("refreshed tags status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), `"label":"美臀"`) {
		t.Fatalf("refreshed tags still contain builtins: %s", recorder.Body.String())
	}
}

func TestShortsRouteRejectsRemovedPostEndpoint(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	if err := cat.CreateSession(ctx, "shorts-route-token", time.Hour, 0); err != nil {
		t.Fatalf("create session: %v", err)
	}

	router := chi.NewRouter()
	(&Server{Catalog: cat}).RegisterRoutes(router, &auth.Authenticator{Catalog: cat})
	req := httptest.NewRequest(http.MethodPost, "/api/shorts/next", nil)
	req.AddCookie(&http.Cookie{Name: "vs_admin", Value: "shorts-route-token"})
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want %d; body = %s", rr.Code, http.StatusMethodNotAllowed, rr.Body.String())
	}
}

func TestHandleShortsNextUsesStableFeedTokenAndCursor(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	for index := 0; index < 7; index++ {
		id := "feed-" + strconv.Itoa(index)
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID:          id,
			DriveID:     "drive",
			FileID:      "file-" + id,
			Title:       id,
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	server := &Server{Catalog: cat}
	requestBatch := func(path string) shortsFeedResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		server.handleShortsNext(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
		var response shortsFeedResponse
		if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return response
	}

	first := requestBatch("/api/shorts/next?count=3")
	if first.FeedToken == "" {
		t.Fatal("new feed did not return a token")
	}
	if first.Total != 7 || len(first.Items) != 3 || first.NextCursor != 3 || first.RoundComplete {
		t.Fatalf("first response = %#v, want 3 of 7 with cursor 3", first)
	}
	for index, item := range first.Items {
		if item.FeedCursor != index+1 {
			t.Fatalf("item %d cursor = %d, want %d", index, item.FeedCursor, index+1)
		}
	}

	repeated := requestBatch("/api/shorts/next?feedToken=" + first.FeedToken + "&cursor=0&count=3")
	for index := range first.Items {
		if repeated.Items[index].ID != first.Items[index].ID || repeated.Items[index].FeedCursor != first.Items[index].FeedCursor {
			t.Fatalf("repeated cursor changed item %d: first=%#v repeated=%#v", index, first.Items[index], repeated.Items[index])
		}
	}

	seen := make(map[string]struct{}, first.Total)
	current := first
	for {
		for _, item := range current.Items {
			if _, duplicate := seen[item.ID]; duplicate {
				t.Fatalf("feed returned duplicate video %s", item.ID)
			}
			seen[item.ID] = struct{}{}
		}
		if current.RoundComplete {
			break
		}
		current = requestBatch(
			"/api/shorts/next?feedToken=" + first.FeedToken +
				"&cursor=" + strconv.Itoa(current.NextCursor) + "&count=3",
		)
	}
	if len(seen) != first.Total {
		t.Fatalf("feed returned %d unique videos, want %d", len(seen), first.Total)
	}
}

// 短视频页要用 sizeBytes/durationSeconds 算平均码率，把预加载门槛从固定
// 秒数换算成一份固定的字节预算。两者缺一不可，且元数据缺失时必须整个省略
// （omitempty），前端才能识别"码率未知"并退回原来的固定门槛。
func TestHandleShortsNextCarriesBitrateMetadata(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	seed := func(id string, size int64, duration int) {
		t.Helper()
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID:              id,
			DriveID:         "drive",
			FileID:          "file-" + id,
			Title:           id,
			Size:            size,
			DurationSeconds: duration,
			PublishedAt:     now,
			CreatedAt:       now,
			UpdatedAt:       now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("sized", 300<<20, 388)
	seed("unsized", 0, 0)
	seed("size-only", 100<<20, 0)
	seed("duration-only", 0, 120)
	seed("transcoded", 300<<20, 300)
	if err := cat.UpdateVideoTranscode(
		ctx,
		"transcoded",
		"ready",
		"",
		"file-transcoded-output",
		30<<20,
	); err != nil {
		t.Fatalf("mark transcoded video ready: %v", err)
	}

	server := &Server{Catalog: cat}
	req := httptest.NewRequest(http.MethodGet, "/api/shorts/next?count=5", nil)
	rr := httptest.NewRecorder()
	server.handleShortsNext(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	var response shortsFeedResponse
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := make(map[string]ShortsItemDTO, len(response.Items))
	for _, item := range response.Items {
		byID[item.ID] = item
	}

	sized, ok := byID["sized"]
	if !ok {
		t.Fatalf("feed did not return the sized video: %s", body)
	}
	if sized.SizeBytes != 300<<20 || sized.DurationSeconds != 388 {
		t.Fatalf("sized item = %d bytes / %ds, want 314572800 / 388", sized.SizeBytes, sized.DurationSeconds)
	}

	transcoded, ok := byID["transcoded"]
	if !ok {
		t.Fatalf("feed did not return the transcoded video: %s", body)
	}
	if transcoded.VideoSrc != "/p/stream/drive/file-transcoded-output" {
		t.Fatalf("transcoded video source = %q, want the transcoded asset", transcoded.VideoSrc)
	}
	if transcoded.SizeBytes != 30<<20 || transcoded.DurationSeconds != 300 {
		t.Fatalf(
			"transcoded item = %d bytes / %ds, want the 31457280-byte playback asset / 300s",
			transcoded.SizeBytes,
			transcoded.DurationSeconds,
		)
	}

	var rawResponse struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &rawResponse); err != nil {
		t.Fatalf("decode raw response: %v", err)
	}
	rawByID := make(map[string]map[string]json.RawMessage, len(rawResponse.Items))
	for _, item := range rawResponse.Items {
		var id string
		if err := json.Unmarshal(item["id"], &id); err != nil {
			t.Fatalf("decode raw item id: %v", err)
		}
		rawByID[id] = item
	}
	assertMetadataKeys := func(id string, want bool) {
		t.Helper()
		item, ok := rawByID[id]
		if !ok {
			t.Fatalf("feed did not return %s: %s", id, body)
		}
		_, hasSize := item["sizeBytes"]
		_, hasDuration := item["durationSeconds"]
		if hasSize != want || hasDuration != want {
			t.Fatalf(
				"%s metadata keys = size:%t duration:%t, want both %t: %s",
				id,
				hasSize,
				hasDuration,
				want,
				body,
			)
		}
	}
	assertMetadataKeys("sized", true)
	assertMetadataKeys("transcoded", true)
	for _, id := range []string{"unsized", "size-only", "duration-only"} {
		assertMetadataKeys(id, false)
	}
}

func TestPrewarmShortsStreamLinksUsesFirstTwoPlaybackTargetsAndUserAgent(t *testing.T) {
	registry := proxy.NewRegistry()
	drive := &apiPrewarmDrive{calls: make(chan apiPrewarmCall, 3)}
	registry.Set("drive", drive)
	server := &Server{Proxy: proxy.New(registry)}
	videos := []*catalog.Video{
		{
			ID:               "transcoded",
			DriveID:          "drive",
			FileID:           "original-1",
			TranscodeStatus:  "ready",
			TranscodedFileID: "transcoded-1",
		},
		{ID: "original", DriveID: "drive", FileID: "original-2"},
		{ID: "third", DriveID: "drive", FileID: "original-3"},
		{ID: "upload", DriveID: localUploadDriveID, FileID: "upload.mp4"},
	}

	server.prewarmShortsStreamLinks(
		videos,
		http.Header{"User-Agent": {"Shorts-Browser"}},
	)

	got := make([]apiPrewarmCall, 0, 2)
	for len(got) < 2 {
		select {
		case call := <-drive.calls:
			got = append(got, call)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for prewarm calls; got %#v", got)
		}
	}
	targets := map[string]bool{}
	for _, call := range got {
		targets[call.fileID] = true
	}
	if !targets["transcoded-1"] || !targets["original-2"] || len(targets) != 2 {
		t.Fatalf("prewarm calls = %#v, want the first two playback targets", got)
	}
	for _, call := range got {
		if call.userAgent != "Shorts-Browser" {
			t.Fatalf("prewarm UA = %q, want Shorts-Browser", call.userAgent)
		}
	}
	select {
	case extra := <-drive.calls:
		t.Fatalf("unexpected third prewarm call: %#v", extra)
	default:
	}
}

func TestHandleShortsNextSkipsVideosHiddenAfterFeedCreation(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	for index := 0; index < 3; index++ {
		id := "feed-visible-" + strconv.Itoa(index)
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID: id, DriveID: "drive", FileID: "file-" + id, Title: id,
			PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	server := &Server{Catalog: cat}
	firstReq := httptest.NewRequest(http.MethodGet, "/api/shorts/next?count=1", nil)
	firstRR := httptest.NewRecorder()
	server.handleShortsNext(firstRR, firstReq)
	var first shortsFeedResponse
	if firstRR.Code != http.StatusOK {
		t.Fatalf("create feed status = %d, body = %s", firstRR.Code, firstRR.Body.String())
	}
	if err := json.NewDecoder(firstRR.Body).Decode(&first); err != nil {
		t.Fatalf("decode first: %v", err)
	}

	feed := server.shortsFeeds[first.FeedToken]
	if feed == nil || len(feed.videoIDs) != 3 {
		t.Fatalf("stored feed = %#v, want 3 ids", feed)
	}
	hiddenID := feed.videoIDs[first.NextCursor]
	if err := cat.HideVideo(ctx, hiddenID); err != nil {
		t.Fatalf("hide %s: %v", hiddenID, err)
	}

	nextReq := httptest.NewRequest(
		http.MethodGet,
		"/api/shorts/next?feedToken="+first.FeedToken+"&cursor="+strconv.Itoa(first.NextCursor)+"&count=1",
		nil,
	)
	nextRR := httptest.NewRecorder()
	server.handleShortsNext(nextRR, nextReq)
	if nextRR.Code != http.StatusOK {
		t.Fatalf("next status = %d, body = %s", nextRR.Code, nextRR.Body.String())
	}
	var next shortsFeedResponse
	if err := json.NewDecoder(nextRR.Body).Decode(&next); err != nil {
		t.Fatalf("decode next: %v", err)
	}
	if len(next.Items) != 1 || next.Items[0].ID == hiddenID {
		t.Fatalf("next items = %#v, should skip hidden %s", next.Items, hiddenID)
	}
	if next.Items[0].FeedCursor <= first.NextCursor+1 {
		t.Fatalf("next cursor = %d, should advance past hidden entry", next.Items[0].FeedCursor)
	}
}

func TestHandleShortsNextRejectsExpiredFeedToken(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID: "feed-video", DriveID: "drive", FileID: "file", Title: "feed video",
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	clock := now
	server := &Server{Catalog: cat, shortsFeedNow: func() time.Time { return clock }}
	firstReq := httptest.NewRequest(http.MethodGet, "/api/shorts/next?count=1", nil)
	firstRR := httptest.NewRecorder()
	server.handleShortsNext(firstRR, firstReq)
	var first shortsFeedResponse
	if err := json.NewDecoder(firstRR.Body).Decode(&first); err != nil {
		t.Fatalf("decode first: %v", err)
	}

	clock = clock.Add(shortsFeedTTL)
	expiredReq := httptest.NewRequest(
		http.MethodGet,
		"/api/shorts/next?feedToken="+first.FeedToken+"&cursor="+strconv.Itoa(first.NextCursor)+"&count=1",
		nil,
	)
	expiredRR := httptest.NewRecorder()
	server.handleShortsNext(expiredRR, expiredReq)
	if expiredRR.Code != http.StatusGone {
		t.Fatalf("expired status = %d, want %d; body = %s", expiredRR.Code, http.StatusGone, expiredRR.Body.String())
	}
}

func TestShortsFeedStoreEvictsLeastRecentlyUsedSession(t *testing.T) {
	clock := time.Now()
	server := &Server{shortsFeedNow: func() time.Time { return clock }}
	for index := 0; index <= maxShortsFeedSessions; index++ {
		server.storeShortsFeed("feed-"+strconv.Itoa(index), []string{"video"})
		clock = clock.Add(time.Second)
	}

	if len(server.shortsFeeds) != maxShortsFeedSessions {
		t.Fatalf("stored feeds = %d, want bounded size %d", len(server.shortsFeeds), maxShortsFeedSessions)
	}
	if server.shortsFeeds["feed-0"] != nil {
		t.Fatal("oldest feed was not evicted")
	}
	if server.shortsFeeds["feed-"+strconv.Itoa(maxShortsFeedSessions)] == nil {
		t.Fatal("newest feed was unexpectedly evicted")
	}
}

func TestHandleShortsNextResumesFeedAfterServerRestart(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	for index := 0; index < 7; index++ {
		id := "restart-" + strconv.Itoa(index)
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID: id, DriveID: "drive", FileID: "file-" + id, Title: id,
			PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	requestBatch := func(server *Server, path string) shortsFeedResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		server.handleShortsNext(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
		var response shortsFeedResponse
		if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return response
	}

	serverA := &Server{Catalog: cat}
	first := requestBatch(serverA, "/api/shorts/next?count=3")
	if first.FeedToken == "" || len(first.Items) != 3 {
		t.Fatalf("first response = %#v, want 3 items with token", first)
	}

	// 模拟后端重启：同一个库、新的 Server（内存会话表为空）
	serverB := &Server{Catalog: cat}
	resumed := requestBatch(
		serverB,
		"/api/shorts/next?feedToken="+first.FeedToken+"&cursor="+strconv.Itoa(first.NextCursor)+"&count=4",
	)
	if len(resumed.Items) != 4 || !resumed.RoundComplete {
		t.Fatalf("resumed response = %#v, want final 4 items of the same round", resumed)
	}
	seen := make(map[string]struct{}, 7)
	for _, item := range append(append([]ShortsItemDTO{}, first.Items...), resumed.Items...) {
		if _, duplicate := seen[item.ID]; duplicate {
			t.Fatalf("resumed round repeated video %s", item.ID)
		}
		seen[item.ID] = struct{}{}
	}
	if len(seen) != 7 {
		t.Fatalf("round covered %d unique videos, want 7", len(seen))
	}

	// 重启后的快照顺序与重启前完全一致：重放首批得到相同条目
	replayed := requestBatch(serverB, "/api/shorts/next?feedToken="+first.FeedToken+"&cursor=0&count=3")
	for index := range first.Items {
		if replayed.Items[index].ID != first.Items[index].ID {
			t.Fatalf("replayed item %d = %s, want %s", index, replayed.Items[index].ID, first.Items[index].ID)
		}
	}
}

func TestHandleShortsNextExpiresPersistedFeedAfterRestart(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID: "persisted-video", DriveID: "drive", FileID: "file", Title: "persisted video",
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	clock := now
	serverA := &Server{Catalog: cat, shortsFeedNow: func() time.Time { return clock }}
	firstReq := httptest.NewRequest(http.MethodGet, "/api/shorts/next?count=1", nil)
	firstRR := httptest.NewRecorder()
	serverA.handleShortsNext(firstRR, firstReq)
	var first shortsFeedResponse
	if err := json.NewDecoder(firstRR.Body).Decode(&first); err != nil {
		t.Fatalf("decode first: %v", err)
	}

	// 重启后持久化快照依然要按 TTL 过期，不能变成永久令牌
	clock = clock.Add(shortsFeedTTL)
	serverB := &Server{Catalog: cat, shortsFeedNow: func() time.Time { return clock }}
	expiredReq := httptest.NewRequest(
		http.MethodGet,
		"/api/shorts/next?feedToken="+first.FeedToken+"&cursor="+strconv.Itoa(first.NextCursor)+"&count=1",
		nil,
	)
	expiredRR := httptest.NewRecorder()
	serverB.handleShortsNext(expiredRR, expiredReq)
	if expiredRR.Code != http.StatusGone {
		t.Fatalf("expired status = %d, want %d; body = %s", expiredRR.Code, http.StatusGone, expiredRR.Body.String())
	}
	if ids, _, err := cat.LoadShortsFeed(ctx, first.FeedToken); err != nil || ids != nil {
		t.Fatalf("expired persisted session = (%#v, %v), want pruned", ids, err)
	}
}

func TestHandleUpdateVideoTagsRejectsUnknownTags(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "普通标题",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	req := requestWithVideoID(http.MethodPut, "/api/video/video-1/tags", "video-1", strings.NewReader(`{"tags":["不存在"]}`))
	rr := httptest.NewRecorder()
	(&Server{Catalog: cat}).handleUpdateVideoTags(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
}

func TestHandleUpdateVideoTagsSavesExistingTags(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "清纯标题",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if _, err := cat.CreateTagAndClassify(ctx, "清纯", nil, "user"); err != nil {
		t.Fatalf("create tag: %v", err)
	}

	req := requestWithVideoID(http.MethodPut, "/api/video/video-1/tags", "video-1", strings.NewReader(`{"tags":["清纯"]}`))
	rr := httptest.NewRecorder()
	(&Server{Catalog: cat}).handleUpdateVideoTags(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got, err := cat.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if !sameStrings(got.Tags, []string{"清纯"}) {
		t.Fatalf("tags = %#v, want 清纯", got.Tags)
	}
}

func TestHandleVideoDetailIncludesDriveKindLabel(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	now := time.Now()
	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:        "drive-onedrive",
		Kind:      "onedrive",
		Name:      "Personal Drive",
		RootID:    "root",
		Status:    "ok",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "video-1",
		DriveID:     "drive-onedrive",
		FileID:      "file-1",
		Ext:         "mp4",
		Title:       "Video",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	req := requestWithVideoID(http.MethodGet, "/api/video/video-1", "video-1", strings.NewReader(``))
	rr := httptest.NewRecorder()
	(&Server{Catalog: cat}).handleVideoDetail(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got VideoDetailDTO
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SourceLabel != "OneDrive" {
		t.Fatalf("sourceLabel = %q, want OneDrive", got.SourceLabel)
	}
	if got.MediaType != "video/mp4" {
		t.Fatalf("mediaType = %q, want video/mp4", got.MediaType)
	}
}

func TestHandleVideoDetailRecommendationsPreferReadyThumbnails(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:           "current-video",
		DriveID:      "drive",
		FileID:       "current-video",
		Title:        "Current",
		Tags:         []string{"same-tag"},
		ThumbnailURL: "https://thumb.example/current-video.jpg",
		PublishedAt:  now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("seed current video: %v", err)
	}
	for i := 0; i < 20; i++ {
		id := "pending-related-" + strconv.Itoa(i)
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID:          id,
			DriveID:     "drive",
			FileID:      id,
			Title:       id,
			Tags:        []string{"same-tag"},
			PublishedAt: now.Add(time.Duration(i+1) * time.Minute),
			CreatedAt:   now.Add(time.Duration(i+1) * time.Minute),
			UpdatedAt:   now.Add(time.Duration(i+1) * time.Minute),
		}); err != nil {
			t.Fatalf("seed pending related video %s: %v", id, err)
		}
	}
	for i := 0; i < 8; i++ {
		id := "ready-related-" + strconv.Itoa(i)
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID:           id,
			DriveID:      "drive",
			FileID:       id,
			Title:        id,
			Tags:         []string{"same-tag"},
			ThumbnailURL: "https://thumb.example/" + id + ".jpg",
			PublishedAt:  now.Add(-time.Duration(i+1) * time.Hour),
			CreatedAt:    now.Add(-time.Duration(i+1) * time.Hour),
			UpdatedAt:    now.Add(-time.Duration(i+1) * time.Hour),
		}); err != nil {
			t.Fatalf("seed ready related video %s: %v", id, err)
		}
	}

	req := requestWithVideoID(http.MethodGet, "/api/video/current-video", "current-video", strings.NewReader(``))
	rr := httptest.NewRecorder()
	(&Server{Catalog: cat}).handleVideoDetail(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got VideoDetailDTO
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.RelatedVideos) != 6 {
		t.Fatalf("related videos = %d, want 6; items=%#v", len(got.RelatedVideos), got.RelatedVideos)
	}
	for _, item := range got.RelatedVideos {
		if !strings.HasPrefix(item.ID, "ready-related-") {
			t.Fatalf("related returned %q before ready thumbnails; items=%#v", item.ID, got.RelatedVideos)
		}
		if !strings.HasPrefix(item.Thumbnail, "https://thumb.example/") {
			t.Fatalf("thumbnail for %q = %q, want ready thumbnail URL", item.ID, item.Thumbnail)
		}
	}
}

func TestHandleHideVideoRemovesVideoFromPublicListAndDetail(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	for _, v := range []*catalog.Video{
		{
			ID:          "video-hidden",
			DriveID:     "drive",
			FileID:      "file-hidden",
			Title:       "Hide me",
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
		{
			ID:          "video-visible",
			DriveID:     "drive",
			FileID:      "file-visible",
			Title:       "Keep me",
			PublishedAt: now.Add(-time.Minute),
			CreatedAt:   now,
			UpdatedAt:   now,
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	server := &Server{Catalog: cat}
	hideReq := requestWithVideoID(http.MethodPost, "/api/video/video-hidden/hide", "video-hidden", strings.NewReader(``))
	hideRR := httptest.NewRecorder()
	server.handleHideVideo(hideRR, hideReq)

	if hideRR.Code != http.StatusOK {
		t.Fatalf("hide status = %d, body = %s", hideRR.Code, hideRR.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/list?page=1&size=24", nil)
	listRR := httptest.NewRecorder()
	server.handleList(listRR, listReq)

	if listRR.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listRR.Code, listRR.Body.String())
	}
	var listed struct {
		Items []VideoDTO `json:"items"`
		Total int        `json:"total"`
	}
	if err := json.NewDecoder(listRR.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if listed.Total != 1 || len(listed.Items) != 1 || listed.Items[0].ID != "video-visible" {
		t.Fatalf("listed = total:%d items:%#v, want only video-visible", listed.Total, listed.Items)
	}

	detailReq := requestWithVideoID(http.MethodGet, "/api/video/video-hidden", "video-hidden", strings.NewReader(``))
	detailRR := httptest.NewRecorder()
	server.handleVideoDetail(detailRR, detailReq)

	if detailRR.Code != http.StatusNotFound {
		t.Fatalf("detail status = %d, want 404; body = %s", detailRR.Code, detailRR.Body.String())
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, value := range a {
		seen[value]++
	}
	for _, value := range b {
		if seen[value] == 0 {
			return false
		}
		seen[value]--
	}
	return true
}

type apiStreamFakeDrive struct {
	localPath string
	fileID    string
}

func (d *apiStreamFakeDrive) Kind() string { return "fake" }
func (d *apiStreamFakeDrive) ID() string   { return "drive-1" }
func (d *apiStreamFakeDrive) Init(context.Context) error {
	return nil
}
func (d *apiStreamFakeDrive) List(context.Context, string) ([]drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *apiStreamFakeDrive) Stat(context.Context, string) (*drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *apiStreamFakeDrive) StreamURL(_ context.Context, fileID string) (*drives.StreamLink, error) {
	d.fileID = fileID
	return &drives.StreamLink{
		URL:     d.localPath,
		Expires: time.Now().Add(time.Minute),
	}, nil
}
func (d *apiStreamFakeDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *apiStreamFakeDrive) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *apiStreamFakeDrive) RootID() string { return "root" }

type apiPrewarmCall struct {
	fileID    string
	userAgent string
}

type apiPrewarmDrive struct {
	calls chan apiPrewarmCall
}

func (d *apiPrewarmDrive) Kind() string               { return "p115" }
func (d *apiPrewarmDrive) ID() string                 { return "drive" }
func (d *apiPrewarmDrive) Init(context.Context) error { return nil }
func (d *apiPrewarmDrive) List(context.Context, string) ([]drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *apiPrewarmDrive) Stat(context.Context, string) (*drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *apiPrewarmDrive) StreamURL(ctx context.Context, fileID string) (*drives.StreamLink, error) {
	return d.StreamURLWithHeader(ctx, fileID, nil)
}
func (d *apiPrewarmDrive) StreamURLWithHeader(_ context.Context, fileID string, header http.Header) (*drives.StreamLink, error) {
	d.calls <- apiPrewarmCall{fileID: fileID, userAgent: header.Get("User-Agent")}
	return &drives.StreamLink{
		URL:     "https://cdn.example/" + fileID,
		Expires: time.Now().Add(10 * time.Minute),
	}, nil
}
func (d *apiPrewarmDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *apiPrewarmDrive) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *apiPrewarmDrive) RootID() string { return "root" }

type apiFakeSubtitleClient struct {
	subtitles     []subtitles.Subtitle
	subtitleReq   subtitles.Request
	subtitleReqs  []subtitles.Request
	subtitleErr   error
	subtitleCalls int
	subtitleFunc  func(int, subtitles.Request) ([]subtitles.Subtitle, error)
}

func (c *apiFakeSubtitleClient) Subtitles(_ context.Context, req subtitles.Request) ([]subtitles.Subtitle, error) {
	c.subtitleReq = req
	c.subtitleReqs = append(c.subtitleReqs, req)
	c.subtitleCalls++
	if c.subtitleFunc != nil {
		return c.subtitleFunc(c.subtitleCalls, req)
	}
	if c.subtitleErr != nil {
		return nil, c.subtitleErr
	}
	return c.subtitles, nil
}

func requestWithVideoID(method, target, videoID string, body *strings.Reader) *http.Request {
	return requestWithRouteParam(method, target, "id", videoID, body)
}

func requestWithRouteParam(method, target, key, value string, body *strings.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, value)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	return req
}

func multipartUploadRequest(t *testing.T, fields map[string]string, fileName, fileContent string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatalf("write field %s: %v", key, err)
		}
	}
	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := part.Write([]byte(fileContent)); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}
