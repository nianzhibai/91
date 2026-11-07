package guangyapan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

type instantOnlyReader struct{}

func (instantOnlyReader) Read([]byte) (int, error) {
	return 0, errors.New("instant upload body must not be read")
}

func TestDriverRefreshListAndStream(t *testing.T) {
	var refreshed bool
	var listedRoot bool
	updates := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/token":
			refreshed = true
			writeTestJSON(w, map[string]any{
				"access_token":  "new-access",
				"refresh_token": "new-refresh",
			})
		case "/v1/user/me":
			if got := r.Header.Get("Authorization"); got != "Bearer new-access" {
				t.Fatalf("auth header = %q, want new access token", got)
			}
			writeTestJSON(w, map[string]any{"sub": "user-1"})
		case "/userres/v1/file/get_file_list":
			if got := r.Header.Get("Authorization"); got != "Bearer new-access" {
				t.Fatalf("api auth header = %q, want new access token", got)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode list body: %v", err)
			}
			if body["parentId"] != "" {
				t.Fatalf("parentId = %#v, want root empty string", body["parentId"])
			}
			listedRoot = true
			writeTestJSON(w, map[string]any{
				"code": 0,
				"msg":  "success",
				"data": map[string]any{
					"total": 2,
					"list": []map[string]any{
						{"fileId": "dir-1", "parentId": "", "fileName": "Movies", "resType": 2},
						{"fileId": "file-1", "parentId": "", "fileName": "clip.mp4", "fileSize": 123, "gcid": "0123456789abcdef0123456789abcdef01234567", "resType": 1, "utime": 1700000000},
					},
				},
			})
		case "/nd.bizuserres.s/v1/get_res_download_url":
			writeTestJSON(w, map[string]any{
				"code": 0,
				"msg":  "success",
				"data": map[string]any{"signedURL": "https://cdn.example.test/clip.mp4"},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	d := New(Config{
		ID:             "gy",
		RefreshToken:   "old-refresh",
		AccountBaseURL: srv.URL,
		APIBaseURL:     srv.URL,
		OnCredentialsUpdate: func(values map[string]string) {
			for k, v := range values {
				updates[k] = v
			}
		},
	})
	if err := d.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	if !refreshed {
		t.Fatal("refresh token endpoint was not called")
	}
	if updates["access_token"] != "new-access" || updates["refresh_token"] != "new-refresh" {
		t.Fatalf("updates = %#v, want refreshed tokens", updates)
	}

	entries, err := d.List(context.Background(), "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !listedRoot || len(entries) != 2 {
		t.Fatalf("listedRoot=%v entries=%#v", listedRoot, entries)
	}
	if !entries[0].IsDir || entries[1].ID != "file-1" || entries[1].Size != 123 || entries[1].Hash != "0123456789ABCDEF0123456789ABCDEF01234567" {
		t.Fatalf("entries = %#v", entries)
	}

	link, err := d.StreamURL(context.Background(), "file-1")
	if err != nil {
		t.Fatalf("stream url: %v", err)
	}
	if link.URL != "https://cdn.example.test/clip.mp4" {
		t.Fatalf("stream url = %q", link.URL)
	}
}

func TestDriverResolvesRootPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/user/me":
			writeTestJSON(w, map[string]any{"sub": "user-1"})
		case "/userres/v1/file/get_file_list":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode list body: %v", err)
			}
			parent, _ := body["parentId"].(string)
			switch parent {
			case "":
				writeTestJSON(w, listTestResponse([]map[string]any{
					{"fileId": "folder-a", "parentId": "", "fileName": "影视", "resType": 2},
				}))
			case "folder-a":
				writeTestJSON(w, listTestResponse([]map[string]any{
					{"fileId": "folder-b", "parentId": "folder-a", "fileName": "电影", "resType": 2},
				}))
			case "folder-b":
				writeTestJSON(w, listTestResponse([]map[string]any{
					{"fileId": "file-1", "parentId": "folder-b", "fileName": "movie.mp4", "fileSize": 456, "resType": 1},
				}))
			default:
				t.Fatalf("unexpected parent %q", parent)
			}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	d := New(Config{
		ID:             "gy",
		RootID:         "configured-root",
		RootPath:       "影视/电影",
		AccessToken:    "access",
		AccountBaseURL: srv.URL,
		APIBaseURL:     srv.URL,
	})
	if err := d.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	if d.RootID() != "folder-b" {
		t.Fatalf("root id = %q, want folder-b", d.RootID())
	}
	entries, err := d.List(context.Background(), "")
	if err != nil {
		t.Fatalf("list resolved root: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "file-1" {
		t.Fatalf("entries = %#v", entries)
	}
}

func TestDriverSendSMSCodeUpdatesVerificationState(t *testing.T) {
	updates := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/shield/captcha/init":
			writeTestJSON(w, map[string]any{"captcha_token": "captcha-1"})
		case "/v1/auth/verification":
			writeTestJSON(w, map[string]any{"verification_id": "verify-1"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	d := New(Config{
		ID:             "gy",
		PhoneNumber:    "13800000000",
		SendCode:       true,
		AccountBaseURL: srv.URL,
		APIBaseURL:     srv.URL,
		OnCredentialsUpdate: func(values map[string]string) {
			for k, v := range values {
				updates[k] = v
			}
		},
	})
	err := d.Init(context.Background())
	if err == nil || !strings.Contains(err.Error(), "验证码已发送") {
		t.Fatalf("init err = %v, want verification prompt", err)
	}
	if updates["captcha_token"] != "captcha-1" || updates["verification_id"] != "verify-1" || updates["send_code"] != "false" {
		t.Fatalf("updates = %#v, want sms state saved", updates)
	}
	if updates["device_id"] == "" {
		t.Fatalf("updates = %#v, want generated device id saved", updates)
	}
}

func TestListHTTP429ReturnsRateLimitError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/userres/v1/file/get_file_list" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		writeTestJSON(w, map[string]any{"code": 429, "msg": "操作频繁，请稍后重试"})
	}))
	defer srv.Close()

	d := New(Config{
		ID:             "gy",
		AccessToken:    "access",
		AccountBaseURL: srv.URL,
		APIBaseURL:     srv.URL,
	})
	_, err := d.List(context.Background(), "")
	if err == nil {
		t.Fatal("list succeeded, want rate limit error")
	}
	var rateLimit *drives.RateLimitError
	if !errors.As(err, &rateLimit) {
		t.Fatalf("error = %T %[1]v, want RateLimitError", err)
	}
	if rateLimit.RetryAfter != 2*time.Minute {
		t.Fatalf("retry after = %s, want 2m", rateLimit.RetryAfter)
	}
}

func TestListCode429ReturnsRateLimitError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/userres/v1/file/get_file_list" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		calls.Add(1)
		writeTestJSON(w, map[string]any{"code": 429, "msg": "操作频繁，请稍后再试"})
	}))
	defer srv.Close()

	d := New(Config{
		ID:             "gy",
		AccessToken:    "access",
		AccountBaseURL: srv.URL,
		APIBaseURL:     srv.URL,
	})
	_, err := d.List(context.Background(), "")
	if err == nil {
		t.Fatal("list succeeded, want rate limit error")
	}
	var rateLimit *drives.RateLimitError
	if !errors.As(err, &rateLimit) {
		t.Fatalf("error = %T %[1]v, want RateLimitError", err)
	}
	if rateLimit.RetryAfter < 9*time.Minute {
		t.Fatalf("retry after = %s, want default provider cooldown", rateLimit.RetryAfter)
	}
	if _, err := d.List(context.Background(), ""); err == nil {
		t.Fatal("follow-up list succeeded during provider cooldown")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 while cooldown is active", got)
	}
}

func TestListInvalidToken403DoesNotReturnRateLimitError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/userres/v1/file/get_file_list" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusForbidden)
		writeTestJSON(w, map[string]any{"code": 401, "msg": "invalid access token"})
	}))
	defer srv.Close()

	d := New(Config{
		ID:             "gy",
		AccessToken:    "access",
		AccountBaseURL: srv.URL,
		APIBaseURL:     srv.URL,
	})
	_, err := d.List(context.Background(), "")
	if err == nil {
		t.Fatal("list succeeded, want auth error")
	}
	var rateLimit *drives.RateLimitError
	if errors.As(err, &rateLimit) {
		t.Fatalf("error = %T %[1]v, want non-rate-limit error", err)
	}
}

func TestUploadAcceptsInstantUploadResponses(t *testing.T) {
	tests := []struct {
		name string
		code int
		msg  string
	}{
		{name: "numeric code", code: 156, msg: "provider-specific instant result"},
		{name: "instant message", code: 0, msg: "秒传成功"},
		{name: "english message", code: 0, msg: "Upload already completed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var tokenCalls atomic.Int32
			var taskCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/nd.bizuserres.s/v1/get_res_center_token":
					tokenCalls.Add(1)
					writeTestJSON(w, map[string]any{
						"code": tc.code,
						"msg":  tc.msg,
						"data": map[string]any{"taskId": "task-instant"},
					})
				case "/nd.bizuserres.s/v1/file/get_info_by_task_id":
					taskCalls.Add(1)
					writeTestJSON(w, map[string]any{
						"code": 0,
						"msg":  "success",
						"data": map[string]any{"fileId": "file-instant"},
					})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			d := New(Config{AccessToken: "access", APIBaseURL: srv.URL, AccountBaseURL: srv.URL})
			d.newOSSBucket = func(uploadTokenData) (guangYaOSSBucket, error) {
				t.Fatal("instant upload must not create an OSS bucket")
				return nil, errors.New("unexpected OSS bucket")
			}
			fileID, err := d.Upload(context.Background(), "parent", "clip.mp4", instantOnlyReader{}, 4)
			if err != nil {
				t.Fatalf("upload: %v", err)
			}
			if fileID != "file-instant" || tokenCalls.Load() != 1 || taskCalls.Load() != 1 {
				t.Fatalf("fileID=%q tokenCalls=%d taskCalls=%d", fileID, tokenCalls.Load(), taskCalls.Load())
			}
			entry, err := d.Stat(context.Background(), fileID)
			if err != nil || entry.Name != "clip.mp4" || entry.ParentID != "parent" {
				t.Fatalf("cached entry=%#v err=%v", entry, err)
			}
		})
	}
}

func TestUploadRunsReliableMultipartSession(t *testing.T) {
	bucket := newFakeGuangYaOSSBucket()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/nd.bizuserres.s/v1/get_res_center_token":
			writeTestJSON(w, map[string]any{
				"code": 0,
				"msg":  "success",
				"data": map[string]any{
					"taskId":          "task-upload",
					"objectPath":      "objects/clip",
					"bucketName":      "bucket",
					"endPoint":        "https://oss.example.test",
					"accessKeyID":     "key",
					"secretAccessKey": "secret",
				},
			})
		case "/nd.bizuserres.s/v1/file/get_info_by_task_id":
			writeTestJSON(w, map[string]any{
				"code": 0,
				"msg":  "success",
				"data": map[string]any{"fileId": "file-upload"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	d := New(Config{AccessToken: "access", APIBaseURL: srv.URL, AccountBaseURL: srv.URL})
	d.newOSSBucket = func(uploadTokenData) (guangYaOSSBucket, error) { return bucket, nil }
	fileID, err := d.Upload(context.Background(), "parent", "clip.mp4", bytes.NewReader([]byte("payload")), 7)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if fileID != "file-upload" || bucket.completes != 1 || bucket.aborts != 0 || bucket.partCalls[1] != 1 {
		t.Fatalf("fileID=%q completes=%d aborts=%d partCalls=%v", fileID, bucket.completes, bucket.aborts, bucket.partCalls)
	}
}

func TestConcurrentUnauthorizedRequestsRefreshOnce(t *testing.T) {
	var oldRequests atomic.Int32
	var refreshCalls atomic.Int32
	releaseOld := make(chan struct{})
	var releaseOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/token":
			refreshCalls.Add(1)
			// Some providers rotate refresh credentials while returning the same
			// access-token value. The generation guard must still deduplicate.
			writeTestJSON(w, map[string]any{"access_token": "old-access", "refresh_token": "new-refresh"})
		case "/userres/v1/file/get_file_list", "/nd.bizuserres.s/v1/get_res_download_url":
			if r.Header.Get("Authorization") == "Bearer old-access" && refreshCalls.Load() == 0 {
				if oldRequests.Add(1) == 2 {
					releaseOnce.Do(func() { close(releaseOld) })
				}
				select {
				case <-releaseOld:
				case <-time.After(2 * time.Second):
				}
				w.WriteHeader(http.StatusUnauthorized)
				writeTestJSON(w, map[string]any{"code": 401, "msg": "expired"})
				return
			}
			if r.Header.Get("Authorization") != "Bearer old-access" {
				w.WriteHeader(http.StatusUnauthorized)
				writeTestJSON(w, map[string]any{"code": 401, "msg": "wrong token"})
				return
			}
			if r.URL.Path == "/userres/v1/file/get_file_list" {
				writeTestJSON(w, listTestResponse(nil))
			} else {
				writeTestJSON(w, map[string]any{
					"code": 0,
					"msg":  "success",
					"data": map[string]any{"signedURL": "https://cdn.example.test/video.mp4"},
				})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	d := New(Config{
		AccessToken:    "old-access",
		RefreshToken:   "old-refresh",
		AccountBaseURL: srv.URL,
		APIBaseURL:     srv.URL,
	})
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		_, err := d.List(context.Background(), "")
		errs <- err
	}()
	go func() {
		<-start
		_, err := d.StreamURL(context.Background(), "file-1")
		errs <- err
	}()
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent request: %v", err)
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	if got := oldRequests.Load(); got != 2 {
		t.Fatalf("old-token requests = %d, want 2", got)
	}
}

func listTestResponse(items []map[string]any) map[string]any {
	return map[string]any{
		"code": 0,
		"msg":  "success",
		"data": map[string]any{
			"total": len(items),
			"list":  items,
		},
	}
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(err)
	}
}
