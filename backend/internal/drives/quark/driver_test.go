package quark

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

func TestRequestRejectsHTTPAndLogicalErrors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		retryAfter string
		want       string
		wantRate   time.Duration
	}{
		{name: "html 500", status: http.StatusInternalServerError, body: "upstream exploded", want: "http=500"},
		{name: "logical error", status: http.StatusOK, body: `{"status":200,"code":32001,"message":"login expired"}`, want: "login expired"},
		{name: "rate limit", status: http.StatusTooManyRequests, body: `{"status":429,"code":429,"message":"slow down"}`, retryAfter: "7", want: "slow down", wantRate: 7 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			d := testDriverForServer(server.URL, server.Client(), t.TempDir())
			err := d.Init(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Init error = %v, want containing %q", err, tc.want)
			}
			if tc.wantRate > 0 {
				wait, ok := drives.RateLimitRetryAfter(err)
				if !ok || wait != tc.wantRate {
					t.Fatalf("rate limit = (%s, %v), want (%s, true)", wait, ok, tc.wantRate)
				}
			}
		})
	}
}

func TestRequestSerializesCookieRotationAndPersistsBothCookies(t *testing.T) {
	var active atomic.Int32
	var maxActive atomic.Int32
	var sequence atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maxActive.Load()
			if current <= old || maxActive.CompareAndSwap(old, current) {
				break
			}
		}
		n := sequence.Add(1)
		time.Sleep(5 * time.Millisecond)
		http.SetCookie(w, &http.Cookie{Name: "__pus", Value: "pus-" + strconv.Itoa(int(n))})
		http.SetCookie(w, &http.Cookie{Name: "__puus", Value: "puus-" + strconv.Itoa(int(n))})
		writeQuarkTestJSON(w, map[string]any{"status": 200, "code": 0})
	}))
	defer server.Close()

	var callbackMu sync.Mutex
	var callbacks []string
	d := testDriverForServer(server.URL, server.Client(), t.TempDir())
	d.cookieMu.Lock()
	d.cookie = "other=keep; __pus=old-pus; __puus=old-puus"
	d.cookieMu.Unlock()
	d.onCookieUpdate = func(cookie string) {
		callbackMu.Lock()
		callbacks = append(callbacks, cookie)
		callbackMu.Unlock()
	}

	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.Init(context.Background()); err != nil {
				t.Errorf("Init: %v", err)
			}
		}()
	}
	wg.Wait()
	if maxActive.Load() != 1 {
		t.Fatalf("max concurrent provider requests = %d, want 1", maxActive.Load())
	}
	gotCookie := d.cookieSnapshot()
	for _, want := range []string{"other=keep", "__pus=pus-6", "__puus=puus-6"} {
		if !strings.Contains(gotCookie, want) {
			t.Fatalf("cookie = %q, missing %q", gotCookie, want)
		}
	}
	callbackMu.Lock()
	defer callbackMu.Unlock()
	if len(callbacks) != 6 || callbacks[len(callbacks)-1] != gotCookie {
		t.Fatalf("callbacks = %#v; final cookie = %q", callbacks, gotCookie)
	}
}

func TestOriginalStreamRequiresBackendCookieRelay(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "__pus=initial; __puus=initial" {
			t.Errorf("Cookie = %q", r.Header.Get("Cookie"))
		}
		writeQuarkTestJSON(w, map[string]any{
			"status": 200, "code": 0,
			"data": []any{map[string]any{"download_url": "https://download.example/original.mp4"}},
		})
	}))
	defer server.Close()
	d := testDriverForServer(server.URL, server.Client(), t.TempDir())
	link, err := d.StreamURL(context.Background(), "file")
	if err != nil {
		t.Fatalf("StreamURL: %v", err)
	}
	if link.ClientRedirectSafe || link.Headers.Get("Cookie") != "__pus=initial; __puus=initial" {
		t.Fatalf("original link = %#v", link)
	}
}

func TestUploadRapidUploadReturnsValidatedIdentity(t *testing.T) {
	payload := []byte("rapid-upload-body")
	tempDir := t.TempDir()
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/1/clouddrive/file/upload/pre":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if int64(body["size"].(float64)) != int64(len(payload)) || body["file_name"] != "movie.mp4" {
				t.Errorf("preflight body = %#v", body)
			}
			writeQuarkTestJSON(w, map[string]any{"status": 200, "code": 0, "data": map[string]any{"task_id": "task-1", "fid": "pre-fid"}, "metadata": map[string]any{"part_size": 4}})
		case "/1/clouddrive/file/update/hash":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			wantMD5 := md5.Sum(payload)
			wantSHA1 := sha1.Sum(payload)
			if body["md5"] != hex.EncodeToString(wantMD5[:]) || body["sha1"] != hex.EncodeToString(wantSHA1[:]) {
				t.Errorf("hash body = %#v", body)
			}
			writeQuarkTestJSON(w, map[string]any{"status": 200, "code": 0, "data": map[string]any{"finish": true, "fid": "rapid-fid"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	d := testDriverForServer(server.URL, server.Client(), tempDir)
	result, err := d.UploadAndReportHash(context.Background(), "parent", "movie.mp4", bytes.NewBuffer(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("UploadAndReportHash: %v", err)
	}
	wantSHA1 := sha1.Sum(payload)
	if result.FileID != "rapid-fid" || result.Hash != hex.EncodeToString(wantSHA1[:]) || result.Size != int64(len(payload)) {
		t.Fatalf("result = %#v", result)
	}
	if len(paths) != 2 {
		t.Fatalf("request paths = %#v", paths)
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("staged temp files = %#v, err=%v", entries, err)
	}
}

func TestUploadMultipartRetriesPartCommitsAndFinishes(t *testing.T) {
	payload := []byte("ABCDEFG")
	var partMu sync.Mutex
	partAttempts := map[string]int{}
	parts := map[string][]byte{}
	var commitSeen bool
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/1/clouddrive/") {
			switch r.URL.Path {
			case "/1/clouddrive/file/upload/pre":
				writeQuarkTestJSON(w, map[string]any{
					"status": 200, "code": 0,
					"data": map[string]any{
						"task_id": "task-1", "upload_id": "upload-1", "obj_key": "object",
						"upload_url": server.URL, "bucket": "127", "auth_info": "auth-info",
						"callback": map[string]any{"callbackUrl": "https://callback.example", "callbackBody": "x=1"},
					},
					"metadata": map[string]any{"part_size": 4},
				})
			case "/1/clouddrive/file/update/hash":
				writeQuarkTestJSON(w, map[string]any{"status": 200, "code": 0, "data": map[string]any{"finish": false}})
			case "/1/clouddrive/file/upload/auth":
				writeQuarkTestJSON(w, map[string]any{"status": 200, "code": 0, "data": map[string]any{"auth_key": "signed-auth"}})
			case "/1/clouddrive/file/upload/finish":
				writeQuarkTestJSON(w, map[string]any{"status": 200, "code": 0, "data": map[string]any{"finish": true, "fid": "final-fid"}})
			default:
				http.NotFound(w, r)
			}
			return
		}
		if r.URL.Path != "/object" || r.URL.Query().Get("uploadId") != "upload-1" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "signed-auth" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Method == http.MethodPut {
			part := r.URL.Query().Get("partNumber")
			body, _ := io.ReadAll(r.Body)
			partMu.Lock()
			partAttempts[part]++
			attempt := partAttempts[part]
			partMu.Unlock()
			if part == "1" && attempt == 1 {
				http.Error(w, "retry me", http.StatusServiceUnavailable)
				return
			}
			partMu.Lock()
			parts[part] = body
			partMu.Unlock()
			w.Header().Set("ETag", `"etag-`+part+`"`)
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			digest := md5.Sum(body)
			if r.Header.Get("Content-MD5") != base64.StdEncoding.EncodeToString(digest[:]) {
				t.Errorf("commit Content-MD5 does not match body")
			}
			if !bytes.Contains(body, []byte(`<PartNumber>1</PartNumber>`)) || !bytes.Contains(body, []byte(`<PartNumber>2</PartNumber>`)) {
				t.Errorf("commit body = %q", body)
			}
			commitSeen = true
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}))
	defer server.Close()
	d := testDriverForServer(server.URL, server.Client(), t.TempDir())
	result, err := d.UploadAndReportHash(context.Background(), "parent", "movie.mp4", bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("UploadAndReportHash: %v", err)
	}
	if result.FileID != "final-fid" || !commitSeen {
		t.Fatalf("result = %#v, commitSeen=%v", result, commitSeen)
	}
	partMu.Lock()
	defer partMu.Unlock()
	if partAttempts["1"] != 2 || partAttempts["2"] != 1 || string(parts["1"])+string(parts["2"]) != string(payload) {
		t.Fatalf("attempts=%#v parts=%q/%q", partAttempts, parts["1"], parts["2"])
	}
}

func TestUploadRejectsMismatchedSizeBeforeRemoteMutation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	d := testDriverForServer(server.URL, server.Client(), t.TempDir())
	_, err := d.Upload(context.Background(), "parent", "movie.mp4", strings.NewReader("short"), 20)
	if err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("Upload error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("remote calls = %d, want 0", calls.Load())
	}
}

func TestUploadClientDoesNotFollowAuthorizationRedirect(t *testing.T) {
	targetCalled := atomic.Bool{}
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetCalled.Store(true)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()
	req, _ := http.NewRequest(http.MethodPut, origin.URL, strings.NewReader("x"))
	req.Header.Set("Authorization", "secret")
	res, err := newQuarkUploadHTTPClient(origin.Client()).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusFound || targetCalled.Load() {
		t.Fatalf("status=%d targetCalled=%v", res.StatusCode, targetCalled.Load())
	}
}

func TestOSSObjectURLUpgradesProviderHTTPURL(t *testing.T) {
	pre := uploadPreResp{}
	pre.Data.UploadURL = "http://oss-cn.example.com"
	pre.Data.Bucket = "bucket-name"
	pre.Data.ObjKey = "path/movie.mp4"
	pre.Data.UploadID = "upload-id"

	u, err := quarkOSSObjectURL(pre)
	if err != nil {
		t.Fatalf("quarkOSSObjectURL: %v", err)
	}
	if got, want := u.String(), "https://bucket-name.oss-cn.example.com/path/movie.mp4"; got != want {
		t.Fatalf("object URL = %q, want %q", got, want)
	}
}

func testDriverForServer(baseURL string, client *http.Client, tempDir string) *Driver {
	d := New(Config{ID: "quark-test", Cookie: "__pus=initial; __puus=initial", UploadTempDir: tempDir})
	d.apiBase = strings.TrimRight(baseURL, "/") + "/1/clouddrive"
	d.playbackAPI = d.apiBase
	if client != nil {
		d.client.SetTransport(client.Transport)
		d.uploadClient = newQuarkUploadHTTPClient(client)
	}
	return d
}

func writeQuarkTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func TestFileToEntryDecodesQuarkEscapedName(t *testing.T) {
	entry := fileToEntry(&file{Fid: "fid", FileName: "A&amp;B.mp4", File: true}, "parent")
	if entry.Name != "A&B.mp4" {
		t.Fatalf("entry name = %q", entry.Name)
	}
}
