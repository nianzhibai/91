package yun139

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

func testAuthorization(expires time.Time) string {
	return base64.StdEncoding.EncodeToString([]byte("user:13800000000:a|b|c|" + strconv.FormatInt(expires.UnixMilli(), 10)))
}

func reply(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
}

func testDriver(t *testing.T, handler http.HandlerFunc) *Driver {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if r.Header.Get("Authorization") == "" || r.Header.Get("X-Yun-Api-Version") != "v1" {
				t.Error("missing signed API headers")
			}
		}
		if r.URL.Path == "/route" {
			var body struct {
				UserInfo struct {
					Account string `json:"accountName"`
				} `json:"userInfo"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.UserInfo.Account != "13800000000" {
				t.Errorf("route account = %q", body.UserInfo.Account)
			}
			reply(w, map[string]any{"routePolicyList": []map[string]string{{"modName": "family", "httpsUrl": "https://unused.example"}, {"modName": "personal", "httpsUrl": server.URL + "/personal/"}}})
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	d := New(Config{ID: "mobile", Authorization: " Basic " + testAuthorization(time.Now().Add(30*24*time.Hour)) + " ", UploadTempDir: t.TempDir()})
	d.routeURL = server.URL + "/route"
	if err := d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSignatureMatchesJavaScriptEncodeURIComponent(t *testing.T) {
	// Fixtures calculated with Node's encodeURIComponent and crypto MD5.
	for _, tc := range []struct{ body, want string }{
		{`{"parentFileId":"/","name":"移动网盘 !*'()~"}`, "7602460040A974FB59F7505AAAF324FB"},
		{`{}`, "01A4A9AC876DCD55C1CF2E3549AC5602"},
	} {
		if got := signature([]byte(tc.body), "2026-10-23 14:15:16", "a1B2c3D4e5F6g7H8"); got != tc.want {
			t.Errorf("signature(%s) = %s, want %s", tc.body, got, tc.want)
		}
	}
}

func TestListUsesDiscoveredHostAndCursor(t *testing.T) {
	calls := 0
	d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/personal/file/list" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		sign := strings.Split(r.Header.Get("Mcloud-Sign"), ",")
		if len(sign) != 3 || signature(body, sign[0], sign[1]) != sign[2] {
			t.Error("signature did not cover the transmitted body")
		}
		var params struct {
			Parent string `json:"parentFileId"`
			Page   struct {
				Cursor string `json:"pageCursor"`
				Size   int    `json:"pageSize"`
			} `json:"pageInfo"`
		}
		_ = json.Unmarshal(body, &params)
		if params.Parent != "/" || params.Page.Size != 100 {
			t.Errorf("unexpected list parameters: %+v", params)
		}
		calls++
		if calls == 1 {
			if params.Page.Cursor != "" {
				t.Error("first cursor should be empty")
			}
			reply(w, map[string]any{"items": []map[string]any{{"fileId": "dir", "name": "电影", "type": "folder", "updatedAt": "2026-10-23T12:34:56.123+08:00"}}, "nextPageCursor": "page-2"})
		} else {
			if params.Page.Cursor != "page-2" {
				t.Error("missing next cursor")
			}
			reply(w, map[string]any{"items": []map[string]any{{"fileId": "video", "name": "电影.mp4", "type": "file", "size": 123, "updatedAt": "bad-date", "thumbnailUrls": []map[string]string{{"url": "https://thumb.example/video"}}}}})
		}
	})
	entries, err := d.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind() != Kind || d.ID() != "mobile" || d.RootID() != "/" || calls != 2 || len(entries) != 2 {
		t.Fatalf("unexpected mount/list: entries=%+v calls=%d", entries, calls)
	}
	if !entries[0].IsDir || entries[0].ModTime.IsZero() || entries[1].IsDir || entries[1].Size != 123 || !entries[1].ModTime.IsZero() || entries[1].ThumbnailURL == "" {
		t.Fatalf("incorrect metadata: %+v", entries)
	}
	got, err := d.Stat(context.Background(), "video")
	if err != nil || !reflect.DeepEqual(*got, entries[1]) {
		t.Fatalf("Stat = %+v, %v", got, err)
	}
	if _, err := d.Stat(context.Background(), "unknown"); !errors.Is(err, drives.ErrNotSupported) {
		t.Fatalf("unknown stat = %v", err)
	}
}

func TestListRejectsIncompletePagination(t *testing.T) {
	for _, mode := range []string{"repeated", "failure"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if mode == "failure" && calls == 2 {
					http.Error(w, "unavailable", 503)
					return
				}
				reply(w, map[string]any{"items": []map[string]string{{"fileId": "file", "name": "video.mp4"}}, "nextPageCursor": "same"})
			})
			entries, err := d.List(context.Background(), "/")
			if err == nil || entries != nil || calls != 2 {
				t.Fatalf("List = %+v, %v; calls=%d", entries, err, calls)
			}
			if _, err := d.Stat(context.Background(), "file"); !errors.Is(err, drives.ErrNotSupported) {
				t.Fatal("cached an incomplete listing")
			}
		})
	}
}

func TestStreamURLSelectsCDNWithoutAccountHeaders(t *testing.T) {
	for _, cdn := range []bool{true, false} {
		t.Run(strconv.FormatBool(cdn), func(t *testing.T) {
			d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/personal/file/getDownloadUrl" {
					t.Errorf("path = %s", r.URL.Path)
				}
				reply(w, map[string]any{"url": "https://origin.example/file?token=signed", "cdnUrl": "https://cdn.example/file?token=signed", "cdnSwitch": cdn})
			})
			link, err := d.StreamURL(context.Background(), "file")
			if err != nil {
				t.Fatal(err)
			}
			wantHost := "https://origin.example/"
			if cdn {
				wantHost = "https://cdn.example/"
			}
			if !strings.HasPrefix(link.URL, wantHost) || len(link.Headers) != 0 || !link.ClientRedirectSafe || !link.Expires.After(time.Now()) {
				t.Fatalf("link = %+v", link)
			}
		})
	}
	d := testDriver(t, func(w http.ResponseWriter, r *http.Request) { reply(w, map[string]string{"url": "javascript:bad"}) })
	if _, err := d.StreamURL(context.Background(), "file"); err == nil {
		t.Fatal("accepted invalid download URL")
	}
}

func TestEnsureDirRenameAndTrash(t *testing.T) {
	var operations []string
	d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		operations = append(operations, r.URL.Path)
		switch r.URL.Path {
		case "/personal/file/list":
			if body["parentFileId"] == "/" {
				reply(w, map[string]any{"items": []map[string]string{{"fileId": "existing", "name": "视频", "type": "folder"}}})
			} else {
				reply(w, map[string]any{"items": []any{}})
			}
		case "/personal/file/create":
			if body["parentFileId"] != "existing" || body["name"] != "新建" || body["type"] != "folder" {
				t.Errorf("create body = %+v", body)
			}
			reply(w, map[string]string{"fileId": "created"})
		case "/personal/file/update":
			if body["fileId"] != "created" || body["name"] != "renamed" {
				t.Errorf("rename = %+v", body)
			}
			reply(w, nil)
		case "/personal/recyclebin/batchTrash":
			if !reflect.DeepEqual(body["fileIds"], []any{"created"}) {
				t.Errorf("trash = %+v", body)
			}
			reply(w, nil)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	ctx := context.Background()
	if _, err := d.EnsureDir(ctx, "must-not-create/../escape"); err == nil {
		t.Fatal("accepted parent traversal")
	}
	if len(operations) != 0 {
		t.Fatal("mutated directories before validating path")
	}
	got, err := d.EnsureDir(ctx, "/视频/新建")
	if err != nil || got != "created" {
		t.Fatalf("EnsureDir = %q, %v", got, err)
	}
	if err := d.Rename(ctx, got, "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove(ctx, "/"); err == nil {
		t.Fatal("allowed trashing root")
	}
}

func TestRefreshIsSerializedAndPersistsAuthorization(t *testing.T) {
	var refreshes, updates atomic.Int32
	renewed := testAuthorization(time.Now().Add(60 * 24 * time.Hour))
	_, _, renewedToken, _, _ := parseAuthorization(renewed)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		var body struct {
			Token   string `xml:"token"`
			Account string `xml:"account"`
			Client  string `xml:"clienttype"`
		}
		if err := xml.NewDecoder(r.Body).Decode(&body); err != nil || body.Account != "13800000000" || body.Client != "656" || body.Token == "" {
			t.Errorf("refresh body = %+v, %v", body, err)
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(w, "<root><return>0</return><token>%s</token></root>", renewedToken)
	}))
	defer server.Close()
	d := New(Config{Authorization: testAuthorization(time.Now().Add(24 * time.Hour)), OnAuthorizationUpdate: func(value string) {
		updates.Add(1)
		if value != renewed {
			t.Error("persisted incorrect authorization")
		}
	}})
	d.refreshURL = server.URL
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			got, account, err := d.ensureAuthorization(context.Background())
			if err != nil || got != renewed || account != "13800000000" {
				t.Errorf("ensureAuthorization = %q, %v", account, err)
			}
		})
	}
	wg.Wait()
	if refreshes.Load() != 1 || updates.Load() != 1 {
		t.Fatalf("refreshes=%d updates=%d", refreshes.Load(), updates.Load())
	}
}

func TestInvalidOrExpiredAuthorizationDoesNotReachProvider(t *testing.T) {
	for _, authorization := range []string{"", "not-base64", base64.StdEncoding.EncodeToString([]byte("bad:token")), testAuthorization(time.Now().Add(-time.Hour))} {
		d := New(Config{Authorization: authorization})
		err := d.Init(context.Background())
		var providerErr *drives.ProviderError
		if !errors.As(err, &providerErr) || providerErr.Kind != drives.ProviderErrorAuth {
			t.Fatalf("Init = %v", err)
		}
	}
}

func TestFailedRefreshDoesNotReplaceCredentials(t *testing.T) {
	for _, response := range []string{
		`<root><return>1</return></root>`,
		`<root><return>0</return><token>invalid</token></root>`,
		`not XML`,
	} {
		t.Run(response, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, response)
			}))
			defer server.Close()
			original := testAuthorization(time.Now().Add(time.Hour))
			updated := false
			d := New(Config{Authorization: original, OnAuthorizationUpdate: func(string) { updated = true }})
			d.refreshURL = server.URL
			if _, _, err := d.ensureAuthorization(context.Background()); err == nil {
				t.Fatal("accepted failed refresh")
			}
			if updated || d.authorization != original {
				t.Fatal("failed refresh replaced credentials")
			}
		})
	}
}

func TestProviderErrorsAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		kind   drives.ProviderErrorKind
	}{
		{401, "", drives.ProviderErrorAuth}, {503, "", drives.ProviderErrorUnavailable},
		{200, `{"success":false,"code":"401","message":"登录失效"}`, drives.ProviderErrorAuth},
		{200, `{"success":false,"code":"not-found","message":"文件不存在"}`, drives.ProviderErrorOther},
	} {
		t.Run(fmt.Sprint(tc.status, tc.kind), func(t *testing.T) {
			d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			_, err := d.List(context.Background(), "/")
			var providerErr *drives.ProviderError
			if !errors.As(err, &providerErr) || providerErr.Kind != tc.kind {
				t.Fatalf("List error = %v", err)
			}
		})
	}
	d := testDriver(t, func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "7"); w.WriteHeader(429) })
	_, err := d.List(context.Background(), "/")
	if wait, ok := drives.RateLimitRetryAfter(err); !ok || wait != 7*time.Second {
		t.Fatalf("rate limit = %s, %v", wait, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.List(ctx, "/"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled list = %v", err)
	}
}
