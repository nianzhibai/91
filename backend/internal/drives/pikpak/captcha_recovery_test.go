package pikpak

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-resty/resty/v2"
)

// writeErrorJSON 模拟 PikPak 在业务错误时返回 4xx + JSON body 的行为；
// 这是 resty 把 body 解到 SetError(&e) 的前提（2xx 只解 SetResult）。
func writeErrorJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(body))
}

// TestRefreshCaptchaTokenRecoversFrom4002 验证 refreshCaptchaToken 在
// 服务端返回 error_code=4002 时会清空缓存的 captcha_token 后自动重试一次：
//
//   - 第一次调用：body 里携带过期 token "expired-captcha"，服务端回 4002
//   - 内部检测到 4002 + captchaToken 非空 → 清空 d.captchaToken
//   - 第二次调用：body 里 captcha_token 为空字符串，服务端发回新 token
//
// 这覆盖 driver 重启后 Init() → refreshCaptchaTokenAtLogin 用持久化的旧
// captcha_token 调 /v1/shield/captcha/init 直接被拒的场景。
func TestRefreshCaptchaTokenRecoversFrom4002(t *testing.T) {
	var calls int32
	type bodyShape struct {
		CaptchaToken string `json:"captcha_token"`
	}
	var (
		firstBody  bodyShape
		secondBody bodyShape
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		switch n {
		case 1:
			_ = json.NewDecoder(r.Body).Decode(&firstBody)
			writeErrorJSON(w, `{
				"error_code": 4002,
				"error": "captcha_invalid",
				"error_description": "Code(4002) - captcha_token expired"
			}`)
		case 2:
			_ = json.NewDecoder(r.Body).Decode(&secondBody)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"captcha_token": "fresh-captcha",
				"expires_in": 300
			}`))
		default:
			t.Errorf("unexpected captcha init call #%d", n)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	d := newTestDriver(t, server)
	d.captchaToken = "expired-captcha"
	persisted := struct {
		access, refresh, captcha, deviceID string
	}{}
	d.onTokenUpdate = func(access, refresh, captcha, deviceID string) {
		persisted.access = access
		persisted.refresh = refresh
		persisted.captcha = captcha
		persisted.deviceID = deviceID
	}

	if err := d.refreshCaptchaTokenAtLogin(context.Background(), "GET:/drive/v1/files", "user-1"); err != nil {
		t.Fatalf("refreshCaptchaTokenAtLogin: %v", err)
	}

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("captcha init called %d times, want 2", got)
	}
	if firstBody.CaptchaToken != "expired-captcha" {
		t.Errorf("first body captcha_token = %q, want \"expired-captcha\"", firstBody.CaptchaToken)
	}
	if secondBody.CaptchaToken != "" {
		t.Errorf("second body captcha_token = %q, want empty (cleared after 4002)", secondBody.CaptchaToken)
	}
	if d.captchaToken != "fresh-captcha" {
		t.Errorf("d.captchaToken = %q, want \"fresh-captcha\"", d.captchaToken)
	}
	if persisted.captcha != "fresh-captcha" {
		t.Errorf("onTokenUpdate captcha = %q, want \"fresh-captcha\"", persisted.captcha)
	}
}

// TestRefreshCaptchaTokenRecoversFrom9 覆盖 PikPak 返回 error_code=9
// captcha_invalid 的路径。这个错误和 4002 一样表示当前 captcha_token 已被拒绝；
// 重试 captcha/init 前必须先清空旧 token，否则服务端会继续拒绝。
func TestRefreshCaptchaTokenRecoversFrom9(t *testing.T) {
	var calls int32
	type bodyShape struct {
		CaptchaToken string `json:"captcha_token"`
	}
	var (
		firstBody  bodyShape
		secondBody bodyShape
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		switch n {
		case 1:
			_ = json.NewDecoder(r.Body).Decode(&firstBody)
			writeErrorJSON(w, `{
				"error_code": 9,
				"error": "captcha_invalid",
				"error_description": "Verification code is invalid"
			}`)
		case 2:
			_ = json.NewDecoder(r.Body).Decode(&secondBody)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"captcha_token": "fresh-captcha",
				"expires_in": 300
			}`))
		default:
			t.Errorf("unexpected captcha init call #%d", n)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	d := newTestDriver(t, server)
	d.captchaToken = "expired-captcha"

	if err := d.refreshCaptchaTokenAtLogin(context.Background(), "GET:/drive/v1/files", "user-1"); err != nil {
		t.Fatalf("refreshCaptchaTokenAtLogin: %v", err)
	}

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("captcha init called %d times, want 2", got)
	}
	if firstBody.CaptchaToken != "expired-captcha" {
		t.Errorf("first body captcha_token = %q, want \"expired-captcha\"", firstBody.CaptchaToken)
	}
	if secondBody.CaptchaToken != "" {
		t.Errorf("second body captcha_token = %q, want empty (cleared after error_code=9)", secondBody.CaptchaToken)
	}
	if d.captchaToken != "fresh-captcha" {
		t.Errorf("d.captchaToken = %q, want \"fresh-captcha\"", d.captchaToken)
	}
}

// TestRefreshCaptchaTokenDoesNotLoopOn4002WithEmptyToken 防止退化成无限重试：
// 如果调用方一开始 captchaToken 就是空，又遇上 4002，不应该再清空一次重试
// （清空后还是空，再发会拿到同样的错误），应该直接返回错误让上层处理。
func TestRefreshCaptchaTokenDoesNotLoopOn4002WithEmptyToken(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		writeErrorJSON(w, `{"error_code": 4002, "error": "captcha_invalid"}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	d := newTestDriver(t, server)
	d.captchaToken = "" // 起点就是空

	err := d.refreshCaptchaTokenAtLogin(context.Background(), "GET:/drive/v1/files", "user-1")
	if err == nil {
		t.Fatal("expected error from refreshCaptchaTokenAtLogin")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("captcha init called %d times, want 1 (no retry when token already empty)", got)
	}
}

func TestLoginRecoversFromRejectedPersistedCaptchaToken(t *testing.T) {
	for _, errorCode := range []int{4002, 9} {
		t.Run(fmt.Sprintf("error_code_%d", errorCode), func(t *testing.T) {
			var (
				signinCalls  int32
				captchaCalls int32
				signinTokens []string
				persisted    []string
			)

			mux := http.NewServeMux()
			mux.HandleFunc("/v1/auth/signin", func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&signinCalls, 1)
				var body struct {
					CaptchaToken string `json:"captcha_token"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				signinTokens = append(signinTokens, body.CaptchaToken)
				if len(signinTokens) == 1 {
					writeErrorJSON(w, fmt.Sprintf(`{
						"error_code": %d,
						"error": "captcha_invalid",
						"error_description": "captcha token rejected"
					}`, errorCode))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"access_token": "fresh-access",
					"refresh_token": "fresh-refresh",
					"sub": "user-1"
				}`))
			})
			mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&captchaCalls, 1)
				var body captchaTokenRequest
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body.CaptchaToken != "" {
					t.Errorf("captcha init token = %q, want empty after signin rejection", body.CaptchaToken)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"captcha_token":"fresh-login-captcha","expires_in":300}`))
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			d := newTestDriver(t, server)
			d.captchaToken = "persisted-stale-captcha"
			d.onTokenUpdate = func(_, _, captcha, _ string) {
				persisted = append(persisted, captcha)
			}

			if err := d.login(context.Background()); err != nil {
				t.Fatalf("login: %v", err)
			}

			if got := atomic.LoadInt32(&signinCalls); got != 2 {
				t.Fatalf("signin calls = %d, want 2", got)
			}
			if got := atomic.LoadInt32(&captchaCalls); got != 1 {
				t.Fatalf("captcha init calls = %d, want 1", got)
			}
			if len(signinTokens) != 2 || signinTokens[0] != "persisted-stale-captcha" || signinTokens[1] != "fresh-login-captcha" {
				t.Fatalf("signin captcha tokens = %#v", signinTokens)
			}
			if len(persisted) < 3 || persisted[0] != "" || persisted[1] != "fresh-login-captcha" {
				t.Fatalf("persisted captcha sequence = %#v, want clear then fresh token", persisted)
			}
		})
	}
}

func TestLoginStopsAfterSingleCaptchaRecoveryAttempt(t *testing.T) {
	var signinCalls, captchaCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/signin", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&signinCalls, 1)
		writeErrorJSON(w, `{"error_code":4002,"error":"captcha_invalid"}`)
	})
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&captchaCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"captcha_token":"fresh-login-captcha","expires_in":300}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	d := newTestDriver(t, server)
	d.captchaToken = "persisted-stale-captcha"

	err := d.login(context.Background())
	if err == nil || !IsCaptchaError(err) {
		t.Fatalf("login error = %v, want captcha error", err)
	}
	if got := atomic.LoadInt32(&signinCalls); got != 2 {
		t.Fatalf("signin calls = %d, want exactly 2", got)
	}
	if got := atomic.LoadInt32(&captchaCalls); got != 1 {
		t.Fatalf("captcha init calls = %d, want exactly 1", got)
	}
}

func TestLoginAccessProhibitedExplainsSupportedAlternatives(t *testing.T) {
	var signinCalls, captchaCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/signin", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&signinCalls, 1)
		writeErrorJSON(w, `{
			"error_code":4126,
			"error":"invalid_grant",
			"error_description":"AccessProhibited"
		}`)
	})
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&captchaCalls, 1)
		t.Fatal("4126 must not trigger captcha refresh")
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	d := newTestDriver(t, server)
	d.captchaToken = "fresh-captcha"

	err := d.login(context.Background())
	if err == nil {
		t.Fatal("login succeeded, want AccessProhibited error")
	}
	for _, want := range []string{"4126", "refresh_token", "WebDAV"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("login error = %q, want %q guidance", err, want)
		}
	}
	if got := atomic.LoadInt32(&signinCalls); got != 1 {
		t.Fatalf("signin calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&captchaCalls); got != 0 {
		t.Fatalf("captcha init calls = %d, want 0", got)
	}
}

func TestInitWithRefreshTokenDoesNotSendPersistedCaptchaToken(t *testing.T) {
	var captchaCalls int32
	var captchaBody struct {
		CaptchaToken string `json:"captcha_token"`
	}
	var persisted struct {
		access, refresh, captcha string
		calls                    int
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token": "fresh-access",
			"refresh_token": "fresh-refresh",
			"sub": "user-1"
		}`))
	})
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&captchaCalls, 1)
		_ = json.NewDecoder(r.Body).Decode(&captchaBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"captcha_token": "fresh-captcha",
			"expires_in": 300
		}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	d := newTestDriver(t, server)
	d.captchaToken = "persisted-stale-captcha"
	d.onTokenUpdate = func(access, refresh, captcha, deviceID string) {
		persisted.access = access
		persisted.refresh = refresh
		persisted.captcha = captcha
		persisted.calls++
	}

	if err := d.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if got := atomic.LoadInt32(&captchaCalls); got != 1 {
		t.Fatalf("captcha init calls = %d, want 1", got)
	}
	if captchaBody.CaptchaToken != "" {
		t.Errorf("captcha init body captcha_token = %q, want empty", captchaBody.CaptchaToken)
	}
	if d.captchaToken != "fresh-captcha" {
		t.Errorf("d.captchaToken = %q, want \"fresh-captcha\"", d.captchaToken)
	}
	if persisted.access != "fresh-access" || persisted.refresh != "fresh-refresh" || persisted.captcha != "fresh-captcha" {
		t.Errorf("persisted tokens = (%q, %q, %q), want fresh values", persisted.access, persisted.refresh, persisted.captcha)
	}
	if persisted.calls < 2 {
		t.Errorf("persist callback calls = %d, want at least 2 (clear stale + persist fresh)", persisted.calls)
	}
}

func TestInitFallsBackToLoginWhenRefreshReturnsCaptchaInvalid(t *testing.T) {
	var (
		tokenCalls   int32
		captchaCalls int32
		signinCalls  int32
	)
	var signinBody struct {
		CaptchaToken string `json:"captcha_token"`
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tokenCalls, 1)
		writeErrorJSON(w, `{
			"error_code": 4002,
			"error": "captcha_invalid",
			"error_description": "Code(4002) - captcha_token expired"
		}`)
	})
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&captchaCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			_, _ = w.Write([]byte(`{
				"captcha_token": "login-captcha",
				"expires_in": 300
			}`))
		case 2:
			_, _ = w.Write([]byte(`{
				"captcha_token": "files-captcha",
				"expires_in": 300
			}`))
		default:
			t.Errorf("unexpected captcha init call #%d", n)
		}
	})
	mux.HandleFunc("/v1/auth/signin", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&signinCalls, 1)
		_ = json.NewDecoder(r.Body).Decode(&signinBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token": "login-access",
			"refresh_token": "login-refresh",
			"sub": "user-1"
		}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	d := newTestDriver(t, server)
	d.captchaToken = "persisted-stale-captcha"

	if err := d.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if got := atomic.LoadInt32(&tokenCalls); got != 1 {
		t.Fatalf("token refresh calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&signinCalls); got != 1 {
		t.Fatalf("signin calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&captchaCalls); got != 2 {
		t.Fatalf("captcha init calls = %d, want 2 (login + post-login files action)", got)
	}
	if signinBody.CaptchaToken != "login-captcha" {
		t.Errorf("signin captcha_token = %q, want \"login-captcha\"", signinBody.CaptchaToken)
	}
	if d.accessToken != "login-access" || d.refreshToken != "login-refresh" || d.captchaToken != "files-captcha" {
		t.Errorf("driver tokens = (%q, %q, %q), want login/files tokens", d.accessToken, d.refreshToken, d.captchaToken)
	}
}

func TestConcurrentExpiredAccessTokensShareOneRefresh(t *testing.T) {
	var oldRequests atomic.Int32
	var refreshes atomic.Int32
	var persisted atomic.Int32
	var releaseOld sync.Once
	bothOldRequestsArrived := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token", func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token": "new-access",
			"refresh_token": "new-refresh",
			"sub": "user-1"
		}`))
	})
	mux.HandleFunc("/drive/v1/files/file-a", func(w http.ResponseWriter, r *http.Request) {
		handleConcurrentPikPakStat(w, r, &oldRequests, &releaseOld, bothOldRequestsArrived)
	})
	mux.HandleFunc("/drive/v1/files/file-b", func(w http.ResponseWriter, r *http.Request) {
		handleConcurrentPikPakStat(w, r, &oldRequests, &releaseOld, bothOldRequestsArrived)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	d := newTestDriver(t, server)
	d.onTokenUpdate = func(_, _, _, _ string) { persisted.Add(1) }

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, fileID := range []string{"file-a", "file-b"} {
		wg.Add(1)
		go func(fileID string) {
			defer wg.Done()
			_, err := d.Stat(context.Background(), fileID)
			errs <- err
		}(fileID)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent stat: %v", err)
		}
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("token refreshes = %d, want 1", got)
	}
	if got := persisted.Load(); got != 1 {
		t.Fatalf("token persistence callbacks = %d, want 1", got)
	}
	auth := d.authSnapshot()
	if auth.accessToken != "new-access" || auth.refreshToken != "new-refresh" {
		t.Fatalf("auth = %#v, want refreshed token pair", auth)
	}
}

func handleConcurrentPikPakStat(
	w http.ResponseWriter,
	r *http.Request,
	oldRequests *atomic.Int32,
	releaseOld *sync.Once,
	bothOldRequestsArrived chan struct{},
) {
	if r.Header.Get("Authorization") == "Bearer test-access-token" {
		if oldRequests.Add(1) == 2 {
			releaseOld.Do(func() { close(bothOldRequestsArrived) })
		}
		<-bothOldRequestsArrived
		writeErrorJSON(w, `{
			"error_code": 4122,
			"error": "unauthenticated",
			"error_description": "access token expired"
		}`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":%q,"name":"clip.mp4","kind":"drive#file","size":"1"}`, strings.TrimPrefix(r.URL.Path, "/drive/v1/files/"))
}

// TestRequestOnceRecoversFrom4002OnAPICall 验证一个普通 API 调用收到 4002
// 时，requestOnce 会先清空 captchaToken、再走 captcha 刷新，最后用新 token
// 重试请求，最终成功返回。
//
// 用 /drive/v1/files 这个真实存在的端点做载体（List 内部会走它）。
func TestRequestOnceRecoversFrom4002OnAPICall(t *testing.T) {
	var (
		filesCalls   int32
		captchaCalls int32
	)
	type capturedFiles struct {
		captchaHeader string
	}
	var firstFiles, secondFiles capturedFiles

	mux := http.NewServeMux()
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&filesCalls, 1)
		switch n {
		case 1:
			firstFiles.captchaHeader = r.Header.Get("X-Captcha-Token")
			writeErrorJSON(w, `{
				"error_code": 4002,
				"error": "captcha_invalid",
				"error_description": "Code(4002) - captcha_token expired"
			}`)
		case 2:
			secondFiles.captchaHeader = r.Header.Get("X-Captcha-Token")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"files": [], "next_page_token": ""}`))
		default:
			t.Errorf("unexpected /drive/v1/files call #%d", n)
		}
	})
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&captchaCalls, 1)
		// 验证 4002 路径先把 captchaToken 清空了，所以 captcha init 的 body 里
		// 不会再带过期 token。
		var body struct {
			CaptchaToken string `json:"captcha_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.CaptchaToken != "" {
			t.Errorf("captcha init body captcha_token = %q, want empty (4002 path should clear cache)", body.CaptchaToken)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"captcha_token": "fresh-captcha", "expires_in": 300}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	d := newTestDriver(t, server)
	d.captchaToken = "expired-captcha"

	if _, err := d.List(context.Background(), "any-parent"); err != nil {
		t.Fatalf("List: %v", err)
	}

	if got := atomic.LoadInt32(&filesCalls); got != 2 {
		t.Fatalf("/drive/v1/files calls = %d, want 2 (initial + retry)", got)
	}
	if got := atomic.LoadInt32(&captchaCalls); got != 1 {
		t.Fatalf("captcha init calls = %d, want 1", got)
	}
	if firstFiles.captchaHeader != "expired-captcha" {
		t.Errorf("first request X-Captcha-Token = %q, want \"expired-captcha\"", firstFiles.captchaHeader)
	}
	if secondFiles.captchaHeader != "fresh-captcha" {
		t.Errorf("retry X-Captcha-Token = %q, want \"fresh-captcha\"", secondFiles.captchaHeader)
	}
	if d.captchaToken != "fresh-captcha" {
		t.Errorf("d.captchaToken after recovery = %q, want \"fresh-captcha\"", d.captchaToken)
	}
}

// TestRequestOnceRecoversFrom9OnAPICall 验证普通 API 调用收到 error_code=9
// 时，会先清空旧 captchaToken，再刷新 captcha 并重试原请求。
func TestRequestOnceRecoversFrom9OnAPICall(t *testing.T) {
	var (
		filesCalls   int32
		captchaCalls int32
	)
	type capturedFiles struct {
		captchaHeader string
	}
	var firstFiles, secondFiles capturedFiles

	mux := http.NewServeMux()
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&filesCalls, 1)
		switch n {
		case 1:
			firstFiles.captchaHeader = r.Header.Get("X-Captcha-Token")
			writeErrorJSON(w, `{
				"error_code": 9,
				"error": "captcha_invalid",
				"error_description": "Verification code is invalid"
			}`)
		case 2:
			secondFiles.captchaHeader = r.Header.Get("X-Captcha-Token")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"files": [], "next_page_token": ""}`))
		default:
			t.Errorf("unexpected /drive/v1/files call #%d", n)
		}
	})
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&captchaCalls, 1)
		var body struct {
			CaptchaToken string `json:"captcha_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.CaptchaToken != "" {
			t.Errorf("captcha init body captcha_token = %q, want empty (error_code=9 path should clear cache)", body.CaptchaToken)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"captcha_token": "fresh-captcha", "expires_in": 300}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	d := newTestDriver(t, server)
	d.captchaToken = "expired-captcha"

	if _, err := d.List(context.Background(), "any-parent"); err != nil {
		t.Fatalf("List: %v", err)
	}

	if got := atomic.LoadInt32(&filesCalls); got != 2 {
		t.Fatalf("/drive/v1/files calls = %d, want 2 (initial + retry)", got)
	}
	if got := atomic.LoadInt32(&captchaCalls); got != 1 {
		t.Fatalf("captcha init calls = %d, want 1", got)
	}
	if firstFiles.captchaHeader != "expired-captcha" {
		t.Errorf("first request X-Captcha-Token = %q, want \"expired-captcha\"", firstFiles.captchaHeader)
	}
	if secondFiles.captchaHeader != "fresh-captcha" {
		t.Errorf("retry X-Captcha-Token = %q, want \"fresh-captcha\"", secondFiles.captchaHeader)
	}
	if d.captchaToken != "fresh-captcha" {
		t.Errorf("d.captchaToken after recovery = %q, want \"fresh-captcha\"", d.captchaToken)
	}
}

// TestRequestOnceDoesNotRetryTwiceOn4002 验证 4002 恢复路径只重试一次；
// 如果重试请求依然失败（哪怕是再来一个 4002），也不会再次进入恢复逻辑，
// 而是把错误返回出去，避免无限循环。
func TestRequestOnceDoesNotRetryTwiceOn4002(t *testing.T) {
	var filesCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&filesCalls, 1)
		writeErrorJSON(w, `{"error_code": 4002, "error": "captcha_invalid"}`)
	})
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"captcha_token": "fresh-captcha", "expires_in": 300}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	d := newTestDriver(t, server)
	d.captchaToken = "expired-captcha"
	// 用一个独立 client，避免被前面 test 修改的 transport 残留影响
	d.client = resty.New().SetHeader("Accept", "application/json")
	d.client.SetTransport(&rewritingTransport{
		base:   http.DefaultTransport,
		target: server.Listener.Addr().String(),
	})

	_, err := d.List(context.Background(), "any-parent")
	if err == nil {
		t.Fatal("expected error when retry also fails with 4002")
	}
	if got := atomic.LoadInt32(&filesCalls); got != 2 {
		t.Fatalf("/drive/v1/files calls = %d, want 2 (one retry only)", got)
	}
}
