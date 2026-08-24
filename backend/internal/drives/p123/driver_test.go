package p123

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/video-site/backend/internal/drives"
)

func TestStreamURLResolvesDownloadInfoRedirect(t *testing.T) {
	ctx := context.Background()
	var downloadReferer string
	var download *httptest.Server
	download = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/resolve":
			downloadReferer = r.Header.Get("Referer")
			http.Redirect(w, r, download.URL+"/cdn/video.mp4", http.StatusFound)
		case "/cdn/video.mp4":
			t.Fatalf("driver followed redirect unexpectedly")
		default:
			http.NotFound(w, r)
		}
	}))
	defer download.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/user/sign_in":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 200,
				"data": map[string]string{"token": "token-1"},
			})
		case "/b/api/user/info":
			if got := r.Header.Get("Authorization"); got != "Bearer token-1" {
				t.Fatalf("Authorization = %q, want bearer token", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{}})
		case "/b/api/file/list/new":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"Next":  "-1",
					"Total": 1,
					"InfoList": []map[string]any{
						{
							"FileName":  "video.mp4",
							"Size":      1234,
							"UpdateAt":  "2026-01-02 03:04:05",
							"FileId":    100,
							"Type":      0,
							"Etag":      "ABCDEF",
							"S3KeyFlag": "flag-1",
						},
					},
				},
			})
		case "/b/api/file/download_info":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode download_info body: %v", err)
			}
			if got := body["fileName"]; got != "video.mp4" {
				t.Fatalf("fileName = %#v, want cached file metadata", got)
			}
			if got := body["etag"]; got != "ABCDEF" {
				t.Fatalf("etag = %#v, want cached etag", got)
			}
			entryURL := download.URL + "/entry?params=" + base64.StdEncoding.EncodeToString([]byte(download.URL+"/resolve"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]string{"DownloadUrl": entryURL},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	var savedToken string
	d := New(Config{
		ID:              "123-main",
		Username:        "user@example.com",
		Password:        "secret",
		MainAPIBaseURL:  api.URL + "/b/api",
		LoginAPIBaseURL: api.URL + "/api",
		OnTokenUpdate: func(access string) {
			savedToken = access
		},
	})
	if err := d.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if savedToken != "token-1" {
		t.Fatalf("saved token = %q, want token-1", savedToken)
	}
	if _, err := d.List(ctx, d.RootID()); err != nil {
		t.Fatalf("List() error = %v", err)
	}

	link, err := d.StreamURL(ctx, "100")
	if err != nil {
		t.Fatalf("StreamURL() error = %v", err)
	}
	if got := link.URL; got != download.URL+"/cdn/video.mp4" {
		t.Fatalf("URL = %q, want final CDN URL", got)
	}
	if got := link.Headers.Get("Referer"); !strings.HasPrefix(got, download.URL) {
		t.Fatalf("Referer = %q, want original download host", got)
	}
	if downloadReferer != defaultReferer {
		t.Fatalf("resolve Referer = %q, want %q", downloadReferer, defaultReferer)
	}
}

func TestConcurrentUnauthorizedRequestsShareOnePasswordLogin(t *testing.T) {
	var oldRequests atomic.Int32
	var logins atomic.Int32
	var persisted atomic.Int32
	var releaseOld sync.Once
	bothOldRequestsArrived := make(chan struct{})

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/user/sign_in":
			logins.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 200,
				"data": map[string]string{"token": "new-token"},
			})
		case "/b/api/user/info":
			if r.Header.Get("Authorization") == "Bearer old-token" {
				if oldRequests.Add(1) == 2 {
					releaseOld.Do(func() { close(bothOldRequestsArrived) })
				}
				<-bothOldRequestsArrived
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 401, "message": "expired"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	d := New(Config{
		ID:              "123-main",
		Username:        "user@example.com",
		Password:        "secret",
		AccessToken:     "old-token",
		MainAPIBaseURL:  api.URL + "/b/api",
		LoginAPIBaseURL: api.URL + "/api",
		OnTokenUpdate: func(string) {
			persisted.Add(1)
		},
	})

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := d.request(context.Background(), endpointUserInfo, http.MethodGet, nil, nil)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent request: %v", err)
		}
	}
	if got := logins.Load(); got != 1 {
		t.Fatalf("password logins = %d, want 1", got)
	}
	if got := persisted.Load(); got != 1 {
		t.Fatalf("token persistence callbacks = %d, want 1", got)
	}
	if got := d.currentToken(); got != "new-token" {
		t.Fatalf("access token = %q, want new-token", got)
	}
}

func TestInitUsesAccessTokenWithoutLogin(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/user/sign_in":
			t.Fatalf("driver should not password-login when access_token is configured")
		case "/b/api/user/info":
			if got := r.Header.Get("Authorization"); got != "Bearer token-1" {
				t.Fatalf("Authorization = %q, want bearer token", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	d := New(Config{
		ID:              "123-main",
		AccessToken:     "Bearer token-1",
		MainAPIBaseURL:  api.URL + "/b/api",
		LoginAPIBaseURL: api.URL + "/api",
	})
	if err := d.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
}

func TestDefaultAPIBaseMatchesCurrentWebAPIHost(t *testing.T) {
	d := New(Config{ID: "123-main"})
	if d.mainAPIBase != "https://api.123278.com/b/api" {
		t.Fatalf("main api base = %q", d.mainAPIBase)
	}
	if d.loginAPIBase != "https://api.123278.com/b/api" {
		t.Fatalf("login api base = %q", d.loginAPIBase)
	}
}

func TestLoginRiskErrorSuggestsQRCodeLogin(t *testing.T) {
	err := loginError("当前账号存在境外登录风险，请使用短信验证码或者微信进行登录。")
	if err == nil || !strings.Contains(err.Error(), "扫码登录") {
		t.Fatalf("loginError() = %v, want qr login guidance", err)
	}
}

func TestRequestCode429ReturnsRateLimitError(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "2")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    429,
			"message": "请求太频繁",
		})
	}))
	defer api.Close()

	d := New(Config{
		ID:             "123-main",
		AccessToken:    "token-1",
		MainAPIBaseURL: api.URL,
	})
	_, err := d.request(context.Background(), endpointFileList, http.MethodGet, nil, nil)
	var rateLimit *drives.RateLimitError
	if !errors.As(err, &rateLimit) {
		t.Fatalf("error = %T %[1]v, want RateLimitError", err)
	}
	if rateLimit.RetryAfter != 2*time.Second {
		t.Fatalf("RetryAfter = %s, want 2s", rateLimit.RetryAfter)
	}
}

func TestListCoolsDownAndRetriesRateLimit(t *testing.T) {
	var listCalls int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/file/list/new" {
			http.NotFound(w, r)
			return
		}
		listCalls++
		if listCalls == 1 {
			w.Header().Set("Retry-After", "1")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code":    429,
				"message": "请求太频繁",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"Next":  "-1",
				"Total": 1,
				"InfoList": []map[string]any{
					{
						"FileName":  "video.mp4",
						"Size":      1234,
						"UpdateAt":  "2026-01-02 03:04:05",
						"FileId":    100,
						"Type":      0,
						"Etag":      "ABCDEF",
						"S3KeyFlag": "flag-1",
					},
				},
			},
		})
	}))
	defer api.Close()

	d := New(Config{
		ID:             "123-main",
		AccessToken:    "token-1",
		MainAPIBaseURL: api.URL,
	})
	entries, err := d.List(context.Background(), d.RootID())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if listCalls != 2 {
		t.Fatalf("list calls = %d, want 2", listCalls)
	}
	if len(entries) != 1 || entries[0].ID != "100" {
		t.Fatalf("entries = %#v, want one file", entries)
	}
}

func TestResolveDownloadURL429ReturnsRateLimitError(t *testing.T) {
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
	}))
	defer download.Close()

	d := New(Config{ID: "123-main"})
	_, err := d.resolveDownloadURL(context.Background(), download.URL)
	var rateLimit *drives.RateLimitError
	if !errors.As(err, &rateLimit) {
		t.Fatalf("error = %T %[1]v, want RateLimitError", err)
	}
	if rateLimit.RetryAfter != 3*time.Second {
		t.Fatalf("RetryAfter = %s, want 3s", rateLimit.RetryAfter)
	}
}

func TestUploadAndReportHashUsesPresignedPUTAndComplete(t *testing.T) {
	ctx := context.Background()
	body := []byte("video bytes for 123 upload")
	wantMD5 := fmt.Sprintf("%x", md5.Sum(body))

	var putBody []byte
	upload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("upload method = %s, want PUT", r.Method)
		}
		if r.ContentLength != int64(len(body)) {
			t.Fatalf("ContentLength = %d, want %d", r.ContentLength, len(body))
		}
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upload body: %v", err)
		}
		putBody = got
		w.WriteHeader(http.StatusOK)
	}))
	defer upload.Close()

	var uploadRequest map[string]any
	var uploadURLRequest map[string]any
	var completeRequest map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/file/upload_request":
			if err := json.NewDecoder(r.Body).Decode(&uploadRequest); err != nil {
				t.Fatalf("decode upload_request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"FileId":      9001,
					"Bucket":      "bucket-1",
					"Key":         "key-1",
					"StorageNode": "node-1",
					"UploadId":    "upload-1",
				},
			})
		case "/file/s3_upload_object/auth":
			if err := json.NewDecoder(r.Body).Decode(&uploadURLRequest); err != nil {
				t.Fatalf("decode s3 auth: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"presignedUrls": map[string]string{
						"1": upload.URL + "/part-1",
					},
				},
			})
		case "/file/upload_complete/v2":
			if err := json.NewDecoder(r.Body).Decode(&completeRequest); err != nil {
				t.Fatalf("decode complete: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	d := New(Config{
		ID:             "123-main",
		AccessToken:    "token-1",
		MainAPIBaseURL: api.URL,
	})
	res, err := d.UploadAndReportHash(ctx, "parent-1", "video.mp4", bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("UploadAndReportHash() error = %v", err)
	}
	if res.FileID != "9001" {
		t.Fatalf("FileID = %q, want 9001", res.FileID)
	}
	if res.Hash != wantMD5 {
		t.Fatalf("Hash = %q, want %q", res.Hash, wantMD5)
	}
	if res.Size != int64(len(body)) {
		t.Fatalf("Size = %d, want %d", res.Size, len(body))
	}
	if !bytes.Equal(putBody, body) {
		t.Fatalf("PUT body = %q, want %q", putBody, body)
	}
	if uploadRequest["etag"] != wantMD5 {
		t.Fatalf("upload etag = %#v, want %q", uploadRequest["etag"], wantMD5)
	}
	if uploadRequest["fileName"] != "video.mp4" || uploadRequest["parentFileId"] != "parent-1" {
		t.Fatalf("upload request = %#v, want fileName and parentFileId", uploadRequest)
	}
	if uploadURLRequest["partNumberStart"].(float64) != 1 || uploadURLRequest["partNumberEnd"].(float64) != 2 {
		t.Fatalf("s3 auth request = %#v, want part range 1..2", uploadURLRequest)
	}
	if completeRequest["fileId"].(float64) != 9001 || completeRequest["fileSize"].(float64) != float64(len(body)) {
		t.Fatalf("complete request = %#v, want file id and size", completeRequest)
	}
	if completeRequest["isMultipart"].(bool) {
		t.Fatalf("complete isMultipart = true, want false")
	}
}

func TestUploadAndReportHashReuseSkipsPUTAndComplete(t *testing.T) {
	body := []byte("reused body")
	var presignedCalled bool
	var completeCalled bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/file/upload_request":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"FileId": 7001,
					"Reuse":  true,
				},
			})
		case "/file/s3_upload_object/auth", "/file/s3_repare_upload_parts_batch":
			presignedCalled = true
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
		case "/file/upload_complete/v2":
			completeCalled = true
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	d := New(Config{
		ID:             "123-main",
		AccessToken:    "token-1",
		MainAPIBaseURL: api.URL,
	})
	res, err := d.UploadAndReportHash(context.Background(), "parent-1", "reused.mp4", bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("UploadAndReportHash() error = %v", err)
	}
	if res.FileID != "7001" {
		t.Fatalf("FileID = %q, want 7001", res.FileID)
	}
	if presignedCalled {
		t.Fatal("reuse upload should not request presigned URLs")
	}
	if completeCalled {
		t.Fatal("reuse upload should not call upload_complete")
	}
}

func TestUploadPresignedPUT429ReturnsRateLimitError(t *testing.T) {
	upload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "4")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
	}))
	defer upload.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/file/upload_request":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"FileId":      9001,
					"Bucket":      "bucket-1",
					"Key":         "key-1",
					"StorageNode": "node-1",
					"UploadId":    "upload-1",
				},
			})
		case "/file/s3_upload_object/auth":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"presignedUrls": map[string]string{"1": upload.URL},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	d := New(Config{
		ID:             "123-main",
		AccessToken:    "token-1",
		MainAPIBaseURL: api.URL,
	})
	_, err := d.UploadAndReportHash(context.Background(), "parent-1", "limited.mp4", strings.NewReader("limited"), int64(len("limited")))
	var rateLimit *drives.RateLimitError
	if !errors.As(err, &rateLimit) {
		t.Fatalf("error = %T %[1]v, want RateLimitError", err)
	}
	if rateLimit.RetryAfter != 4*time.Second {
		t.Fatalf("RetryAfter = %s, want 4s", rateLimit.RetryAfter)
	}
}

func TestBufferAndHashMD5UsesConfiguredTempDir(t *testing.T) {
	body := []byte("hello-123-upload-test")
	tempDir := filepath.Join(t.TempDir(), "upload-tmp")
	tmp, gotHex, n, err := bufferAndHashMD5(tempDir, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("bufferAndHashMD5 returned error: %v", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	if gotDir := filepath.Dir(tmp.Name()); gotDir != tempDir {
		t.Fatalf("tmp dir = %q, want %q", gotDir, tempDir)
	}
	want := md5.Sum(body)
	if gotHex != fmt.Sprintf("%x", want) {
		t.Fatalf("md5 = %s, want %x", gotHex, want)
	}
	if n != int64(len(body)) {
		t.Fatalf("written = %d, want %d", n, len(body))
	}
}

func TestRenameSendsExpectedBody(t *testing.T) {
	var renameRequest map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/file/rename" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&renameRequest); err != nil {
			t.Fatalf("decode rename: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{}})
	}))
	defer api.Close()

	d := New(Config{
		ID:             "123-main",
		AccessToken:    "token-1",
		MainAPIBaseURL: api.URL,
	})
	if err := d.Rename(context.Background(), "9001", "new name.mp4"); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	if renameRequest["driveId"].(float64) != 0 || renameRequest["fileId"] != "9001" || renameRequest["fileName"] != "new name.mp4" {
		t.Fatalf("rename request = %#v, want driveId/fileId/fileName", renameRequest)
	}
}
