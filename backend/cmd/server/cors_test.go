package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCORSMiddlewareAllowsWhitelistedOrigin 验证白名单中的 Origin 能拿到
// 完整的 Allow-Origin / Allow-Credentials 响应头。
func TestCORSMiddlewareAllowsWhitelistedOrigin(t *testing.T) {
	mw := corsMiddleware([]string{"https://video.example.com"})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/list", nil)
	req.Header.Set("Origin", "https://video.example.com")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://video.example.com" {
		t.Fatalf("Allow-Origin = %q, want exact match", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("Allow-Credentials = %q, want true", got)
	}
	if got := rr.Header().Values("Vary"); len(got) == 0 || got[0] != "Origin" {
		t.Fatalf("Vary header = %v, want Origin", got)
	}
}

// TestCORSMiddlewareRejectsUnknownOrigin 验证不在白名单里的 Origin 不会
// 收到任何 Allow-Origin 响应头——浏览器据此拒绝读响应，杜绝 C-1 反射攻击。
func TestCORSMiddlewareRejectsUnknownOrigin(t *testing.T) {
	mw := corsMiddleware([]string{"https://video.example.com"})
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/list", nil)
	req.Header.Set("Origin", "https://evil.com")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// 后端业务仍然会被调用（请求会到达），关键是 Allow-Origin 头不能出现，
	// 浏览器侧会拦截响应内容；服务器自身没有理由阻止请求到达 handler，
	// 因为接口本身的鉴权由 a.Required 在更深一层完成。
	if !called {
		t.Fatal("inner handler should still be called for non-CORS-protected endpoints")
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Allow-Origin = %q, want empty for evil.com", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("Allow-Credentials = %q, want empty for evil.com", got)
	}
}

// TestCORSMiddlewarePreflightRejectsUnknownOrigin 验证 OPTIONS 预检对未授权
// Origin 直接 403，避免被当作放行信号。
func TestCORSMiddlewarePreflightRejectsUnknownOrigin(t *testing.T) {
	mw := corsMiddleware([]string{"https://video.example.com"})
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodOptions, "/admin/api/drives", nil)
	req.Header.Set("Origin", "https://evil.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("preflight status = %d, want 403", rr.Code)
	}
	if called {
		t.Fatal("inner handler must not be called for rejected preflight")
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Allow-Origin = %q, want empty", got)
	}
}

// TestCORSMiddlewarePreflightAllowsWhitelistedOrigin 验证白名单 Origin 的
// 预检会拿到 204 + 完整 Allow-* 头。
func TestCORSMiddlewarePreflightAllowsWhitelistedOrigin(t *testing.T) {
	mw := corsMiddleware([]string{"https://video.example.com"})
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodOptions, "/admin/api/drives", nil)
	req.Header.Set("Origin", "https://video.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rr.Code)
	}
	if called {
		t.Fatal("preflight should short-circuit before inner handler")
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://video.example.com" {
		t.Fatalf("Allow-Origin = %q", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Fatal("Allow-Methods missing on accepted preflight")
	}
}

// TestCORSMiddlewareSameOriginPassesThrough 验证同源请求（无 Origin 头，
// 例如服务端直接发起或浏览器导航）不受影响，按正常流程走，不会被 CORS 拦下。
func TestCORSMiddlewareSameOriginPassesThrough(t *testing.T) {
	mw := corsMiddleware([]string{"https://video.example.com"})
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/list", nil)
	// 不设置 Origin
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Fatal("same-origin request should reach handler")
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Allow-Origin = %q, want empty for same-origin request", got)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

// TestCORSMiddlewareWildcardEntryIsIgnored 验证 "*" 不会被当作白名单成员
// 接受——避免管理员误把通配符塞进列表导致回归到反射 Origin 的危险状态。
func TestCORSMiddlewareWildcardEntryIsIgnored(t *testing.T) {
	mw := corsMiddleware([]string{"*", "  ", "https://video.example.com"})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/list", nil)
	req.Header.Set("Origin", "https://anywhere.example")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Allow-Origin = %q, '*' must not whitelist arbitrary origins", got)
	}
}

func TestCORSAllowsConfigConcurrencyAndMediaHeaders(t *testing.T) {
	handler := corsMiddleware([]string{"https://frontend.example.com"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"version"`)
		w.WriteHeader(http.StatusOK)
	}))
	request := httptest.NewRequest(http.MethodOptions, "/admin/api/config.yaml", nil)
	request.Header.Set("Origin", "https://frontend.example.com")
	request.Header.Set("Access-Control-Request-Method", "PUT")
	request.Header.Set("Access-Control-Request-Headers", "Content-Type, If-Match")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("preflight status=%d", response.Code)
	}
	for _, header := range []string{"If-Match", "Range"} {
		if !strings.Contains(response.Header().Get("Access-Control-Allow-Headers"), header) {
			t.Errorf("header %s is unavailable", header)
		}
	}
	for _, header := range []string{"ETag", "Content-Range", "Content-Disposition"} {
		if !strings.Contains(response.Header().Get("Access-Control-Expose-Headers"), header) {
			t.Errorf("header %s is not exposed", header)
		}
	}
}
