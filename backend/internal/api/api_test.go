package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/proxy"
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

func TestPreviewURLIncludesUpdatedAtVersion(t *testing.T) {
	got := previewURL(&catalog.Video{
		ID:        "video-1",
		UpdatedAt: time.UnixMilli(1778863000123),
	})

	if got != "/p/preview/video-1?v=1778863000123" {
		t.Fatalf("preview URL = %q, want versioned URL", got)
	}
}

func TestPreviewURLFallsBackWithoutUpdatedAt(t *testing.T) {
	got := previewURL(&catalog.Video{ID: "video-1"})

	if got != "/p/preview/video-1" {
		t.Fatalf("preview URL = %q, want unversioned URL", got)
	}
}

func TestThumbnailURLVersionsLocalGeneratedThumbnails(t *testing.T) {
	got := thumbnailURL(&catalog.Video{
		ID:           "video-1",
		ThumbnailURL: "/p/thumb/video-1",
		UpdatedAt:    time.UnixMilli(1778863000123),
	})
	if got != "/p/thumb/video-1?v=1778863000123" {
		t.Fatalf("thumbnail URL = %q, want versioned local URL", got)
	}

	remote := "https://thumb.example/video-1.jpg"
	got = thumbnailURL(&catalog.Video{
		ID:           "video-1",
		ThumbnailURL: remote,
		UpdatedAt:    time.UnixMilli(1778863000123),
	})
	if got != remote {
		t.Fatalf("remote thumbnail URL = %q, want unchanged %q", got, remote)
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
		makeFrontendVisible(t, ctx, cat, id)
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
		makeFrontendVisible(t, ctx, cat, id)
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

func TestHandleHomeExcludesRecentlyShownVideos(t *testing.T) {
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
	for i := 0; i < homePageSize+4; i++ {
		id := "ready-video-" + strconv.Itoa(i)
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID:           id,
			DriveID:      "drive",
			FileID:       id,
			Title:        id,
			ThumbnailURL: "https://thumb.example/" + id + ".jpg",
			PublishedAt:  now.Add(time.Duration(i) * time.Minute),
			CreatedAt:    now.Add(time.Duration(i) * time.Minute),
			UpdatedAt:    now.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("seed ready video %s: %v", id, err)
		}
		makeFrontendVisible(t, ctx, cat, id)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/home?exclude=ready-video-0&exclude=ready-video-1", nil)
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
		if item.ID == "ready-video-0" || item.ID == "ready-video-1" {
			t.Fatalf("home returned excluded video %q; items=%#v", item.ID, got)
		}
		if !strings.HasPrefix(item.ID, "ready-video-") {
			t.Fatalf("home returned %q without a ready thumbnail; items=%#v", item.ID, got)
		}
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
		makeFrontendVisible(t, ctx, cat, id)
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
		makeFrontendVisible(t, ctx, cat, id)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/list?page=1&size=12&sort=latest", nil)
	(&Server{Catalog: cat}).handleList(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
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

func TestHandleListAndDetailRequireFrontendSelectionAllowedTagAndNotHidden(t *testing.T) {
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
		{ID: "visible", DriveID: "drive", FileID: "visible-file", Title: "Visible", PublishedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "unselected", DriveID: "drive", FileID: "unselected-file", Title: "Unselected", PublishedAt: now.Add(-time.Minute), CreatedAt: now, UpdatedAt: now},
		{ID: "no-access-tag", DriveID: "drive", FileID: "no-access-tag-file", Title: "No access tag", FrontendSelected: true, PublishedAt: now.Add(-2 * time.Minute), CreatedAt: now, UpdatedAt: now},
		{ID: "hidden", DriveID: "drive", FileID: "hidden-file", Title: "Hidden", Hidden: true, PublishedAt: now.Add(-3 * time.Minute), CreatedAt: now, UpdatedAt: now},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}
	makeFrontendVisible(t, ctx, cat, "visible", "public-tag")
	ensureAPITestTag(t, ctx, cat, "public-tag")
	if err := cat.SetManualVideoTags(ctx, "unselected", []string{"public-tag"}); err != nil {
		t.Fatalf("tag unselected video: %v", err)
	}
	publicTag := mustAPITestTagByLabel(t, ctx, cat, "public-tag")
	if err := cat.SetTagFrontendAccess(ctx, publicTag.ID, true); err != nil {
		t.Fatalf("set public tag frontend access: %v", err)
	}
	ensureAPITestTag(t, ctx, cat, "private-tag")
	if err := cat.SetManualVideoTags(ctx, "no-access-tag", []string{"private-tag"}); err != nil {
		t.Fatalf("tag no-access video: %v", err)
	}
	makeFrontendVisible(t, ctx, cat, "hidden", "public-tag")

	server := &Server{Catalog: cat}
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
	if listed.Total != 1 || len(listed.Items) != 1 || listed.Items[0].ID != "visible" {
		t.Fatalf("listed = total:%d items:%#v, want only visible", listed.Total, listed.Items)
	}

	for _, id := range []string{"visible", "unselected", "no-access-tag", "hidden"} {
		req := requestWithVideoID(http.MethodGet, "/api/video/"+id, id, strings.NewReader(``))
		rr := httptest.NewRecorder()
		server.handleVideoDetail(rr, req)
		if id == "visible" {
			if rr.Code != http.StatusOK {
				t.Fatalf("detail %s status = %d, want 200; body = %s", id, rr.Code, rr.Body.String())
			}
			continue
		}
		if rr.Code != http.StatusNotFound {
			t.Fatalf("detail %s status = %d, want 404; body = %s", id, rr.Code, rr.Body.String())
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
		"tags":  "奶子,口交,AV,女大",
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
	if !sameStringSet(got.Tags, []string{"奶子", "口交", "AV", "女大"}) {
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
	req := multipartUploadRequest(t, map[string]string{"tags": "奶子,后入"}, "clip.mp4", "video-bytes")
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
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:            "video-1",
		DriveID:       "drive-1",
		FileID:        "file-1",
		Title:         "Video",
		PreviewStatus: "ready",
		PreviewFileID: "remote-preview-file",
		PreviewLocal:  localPreview,
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
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
		Category:    "random-category",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if _, err := cat.CreateTagAndClassify(ctx, "清纯", nil, "user"); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	makeFrontendVisible(t, ctx, cat, "video-1", "清纯", "后入")

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
		t.Fatalf("labels = %#v, want system tag 后入", labels)
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

func TestHandleShortsNextUsesPreferredVideoLeastPopulatedTag(t *testing.T) {
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
		{ID: "current", DriveID: "drive", FileID: "f-current", Title: "current", Tags: []string{"common", "rare"}, PublishedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "common-1", DriveID: "drive", FileID: "f-common-1", Title: "common 1", Tags: []string{"common"}, PublishedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "common-2", DriveID: "drive", FileID: "f-common-2", Title: "common 2", Tags: []string{"common"}, PublishedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "rare-1", DriveID: "drive", FileID: "f-rare-1", Title: "rare 1", Tags: []string{"rare"}, PublishedAt: now, CreatedAt: now, UpdatedAt: now},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed %s: %v", v.ID, err)
		}
		makeFrontendVisible(t, ctx, cat, v.ID, v.Tags...)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/shorts/next", strings.NewReader(`{"seenIds":["current"],"count":3,"preferredFromVideoId":"current"}`))
	rr := httptest.NewRecorder()
	(&Server{Catalog: cat}).handleShortsNext(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Items         []ShortsItemDTO `json:"items"`
		Total         int             `json:"total"`
		RoundComplete bool            `json:"roundComplete"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := make([]string, 0, len(got.Items))
	for _, item := range got.Items {
		ids = append(ids, item.ID)
	}
	if got.Total != 4 {
		t.Fatalf("total = %d, want 4", got.Total)
	}
	if got.RoundComplete {
		t.Fatalf("roundComplete = true, want false with fallback-filled batch")
	}
	if !containsString(ids, "rare-1") {
		t.Fatalf("ids = %#v, want rare-1 from least populated tag", ids)
	}
	if containsString(ids, "current") {
		t.Fatalf("ids = %#v, should exclude current", ids)
	}
	if len(ids) != 3 {
		t.Fatalf("ids = %#v, want 3 items", ids)
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
		Title:       "Video",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	makeFrontendVisible(t, ctx, cat, "video-1")

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
	makeFrontendVisible(t, ctx, cat, "current-video", "same-tag")
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
		makeFrontendVisible(t, ctx, cat, id, "same-tag")
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
		makeFrontendVisible(t, ctx, cat, id, "same-tag")
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
		makeFrontendVisible(t, ctx, cat, v.ID)
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

func makeFrontendVisible(t *testing.T, ctx context.Context, cat *catalog.Catalog, videoID string, labels ...string) {
	t.Helper()
	if len(labels) == 0 {
		labels = []string{"AV"}
	}
	for _, label := range labels {
		ensureAPITestTag(t, ctx, cat, label)
	}
	if err := cat.SetManualVideoTags(ctx, videoID, labels); err != nil {
		t.Fatalf("attach frontend tags to %s: %v", videoID, err)
	}
	for _, label := range labels {
		tag := mustAPITestTagByLabel(t, ctx, cat, label)
		if err := cat.SetTagFrontendAccess(ctx, tag.ID, true); err != nil {
			t.Fatalf("set frontend access for tag %s: %v", label, err)
		}
	}
	if err := cat.SetVideoFrontendSelected(ctx, videoID, true); err != nil {
		t.Fatalf("select %s for frontend: %v", videoID, err)
	}
}

func ensureAPITestTag(t *testing.T, ctx context.Context, cat *catalog.Catalog, label string) {
	t.Helper()
	if _, ok := findAPITestTagByLabel(t, ctx, cat, label); ok {
		return
	}
	if _, err := cat.CreateTagAndClassify(ctx, label, nil, "user"); err != nil {
		t.Fatalf("create tag %s: %v", label, err)
	}
}

func mustAPITestTagByLabel(t *testing.T, ctx context.Context, cat *catalog.Catalog, label string) catalog.Tag {
	t.Helper()
	if tag, ok := findAPITestTagByLabel(t, ctx, cat, label); ok {
		return tag
	}
	t.Fatalf("tag %q not found", label)
	return catalog.Tag{}
}

func findAPITestTagByLabel(t *testing.T, ctx context.Context, cat *catalog.Catalog, label string) (catalog.Tag, bool) {
	t.Helper()
	tags, err := cat.ListTags(ctx)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	for _, tag := range tags {
		if tag.Label == label {
			return tag, true
		}
	}
	return catalog.Tag{}, false
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
