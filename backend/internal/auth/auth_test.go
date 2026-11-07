package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

func TestLoginBanLastsUntilStartupReset(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Unix(1_700_000_000, 0)
	createTestAdmin(t, cat)
	authr := &Authenticator{
		Catalog: cat,
		Now:     func() time.Time { return now },
	}

	for i := 0; i < loginFailThreshold-1; i++ {
		ok, err := login(authr, httptest.NewRecorder(), loginRequest("203.0.113.10"), "admin", "wrong")
		if err != nil {
			t.Fatalf("failure %d returned error: %v", i+1, err)
		}
		if ok {
			t.Fatalf("failure %d returned ok", i+1)
		}
	}

	ok, err := login(authr, httptest.NewRecorder(), loginRequest("203.0.113.10"), "admin", "wrong")
	if ok {
		t.Fatal("third failed login returned ok")
	}
	if !errors.Is(err, ErrLoginIPBanned) {
		t.Fatalf("third failed login error = %v, want ErrLoginIPBanned", err)
	}

	banned, err := cat.IsLoginIPBanned(loginRequest("203.0.113.10").Context(), "203.0.113.10")
	if err != nil {
		t.Fatalf("query ban: %v", err)
	}
	if !banned {
		t.Fatal("ip was not persisted as banned")
	}

	now = now.Add(loginFailWindow * 2)
	reloaded := &Authenticator{Catalog: cat, Now: func() time.Time { return now }}
	ok, err = login(reloaded, httptest.NewRecorder(), loginRequest("203.0.113.10"), "admin", "secret")
	if ok {
		t.Fatal("banned ip logged in without a startup reset")
	}
	if !errors.Is(err, ErrLoginIPBanned) {
		t.Fatalf("banned ip error = %v, want ErrLoginIPBanned", err)
	}
	if err := cat.ResetLoginProtection(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted := &Authenticator{Catalog: cat, Now: func() time.Time { return now }}
	if ok, err := login(restarted, httptest.NewRecorder(), loginRequest("203.0.113.10"), "admin", "wrong"); err != nil || ok {
		t.Fatalf("first failure after restart ok=%v err=%v", ok, err)
	}
	if ok, err := login(restarted, httptest.NewRecorder(), loginRequest("203.0.113.10"), "admin", "secret"); err != nil || !ok {
		t.Fatalf("login after restart ok=%v err=%v", ok, err)
	}
}

func TestAuthenticatorsShareFailureCounts(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	for i := 0; i < loginFailThreshold; i++ {
		a := &Authenticator{Catalog: cat}
		role, err := a.UserLogin(httptest.NewRecorder(), loginRequest("203.0.113.22"), "missing-user", "wrong")
		if role != "" {
			t.Fatal("missing user logged in")
		}
		if i < loginFailThreshold-1 && err != nil {
			t.Fatal(err)
		}
		if i == loginFailThreshold-1 && !errors.Is(err, ErrLoginIPBanned) {
			t.Fatalf("shared failures did not ban IP: %v", err)
		}
	}
}

func TestSuccessfulLoginClearsFailedLoginWindow(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	createTestAdmin(t, cat)
	authr := &Authenticator{
		Catalog: cat,
	}

	for i := 0; i < loginFailThreshold-1; i++ {
		if ok, err := login(authr, httptest.NewRecorder(), loginRequest("203.0.113.11"), "admin", "wrong"); err != nil || ok {
			t.Fatalf("failed login %d ok=%v err=%v", i+1, ok, err)
		}
	}
	if ok, err := login(authr, httptest.NewRecorder(), loginRequest("203.0.113.11"), "admin", "secret"); err != nil || !ok {
		t.Fatalf("successful login after failures ok=%v err=%v", ok, err)
	}
	if ok, err := login(authr, httptest.NewRecorder(), loginRequest("203.0.113.11"), "admin", "wrong"); err != nil || ok {
		t.Fatalf("failure after successful login ok=%v err=%v", ok, err)
	}
}

func TestLoginCreatesSevenDaySession(t *testing.T) {
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

	createTestAdmin(t, cat)
	authr := &Authenticator{
		Catalog: cat,
	}

	before := time.Now()
	rr := httptest.NewRecorder()
	ok, err := login(authr, rr, loginRequest("203.0.113.12"), "admin", "secret")
	after := time.Now()
	if err != nil || !ok {
		t.Fatalf("login ok=%v err=%v", ok, err)
	}

	cookie := responseCookie(t, rr, sessionCookie)
	minExpires := before.Add(sessionTTL - time.Second)
	maxExpires := after.Add(sessionTTL + time.Second)
	if cookie.Expires.Before(minExpires) || cookie.Expires.After(maxExpires) {
		t.Fatalf("cookie expires at %s, want around %s", cookie.Expires, before.Add(sessionTTL))
	}

	session, found, err := cat.GetSession(ctx, cookie.Value)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if !found {
		t.Fatal("session was not persisted")
	}
	if session.ExpiresAt.Before(minExpires) || session.ExpiresAt.After(maxExpires) {
		t.Fatalf("db session expires at %s, want around %s", session.ExpiresAt, before.Add(sessionTTL))
	}
}

func TestRequiredRenewsSessionWhenLessThanHalfRemaining(t *testing.T) {
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

	now := time.Now().Truncate(time.Millisecond)
	token := "renew-token"
	if err := cat.CreateSessionUntil(ctx, token, now.Add(sessionRenewBefore-time.Minute), createTestAdmin(t, cat)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	authr := &Authenticator{
		Catalog: cat,
		Now:     func() time.Time { return now },
	}

	req := httptest.NewRequest(http.MethodGet, "/api/home", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	res := httptest.NewRecorder()
	authr.Required(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", res.Code)
	}
	expectedExpires := now.Add(sessionTTL)
	cookie := responseCookie(t, res, sessionCookie)
	if absDuration(cookie.Expires.Sub(expectedExpires)) > time.Second {
		t.Fatalf("renewed cookie expires at %s, want %s", cookie.Expires, expectedExpires)
	}
	session, found, err := cat.GetSession(ctx, token)
	if err != nil || !found {
		t.Fatalf("get renewed session found=%v err=%v", found, err)
	}
	if !session.ExpiresAt.Equal(expectedExpires) {
		t.Fatalf("renewed db session expires at %s, want %s", session.ExpiresAt, expectedExpires)
	}
}

func TestRequiredDoesNotRenewSessionWhenMoreThanHalfRemaining(t *testing.T) {
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

	now := time.Now().Truncate(time.Millisecond)
	token := "fresh-token"
	expiresAt := now.Add(sessionRenewBefore + time.Minute)
	if err := cat.CreateSessionUntil(ctx, token, expiresAt, createTestAdmin(t, cat)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	authr := &Authenticator{
		Catalog: cat,
		Now:     func() time.Time { return now },
	}

	req := httptest.NewRequest(http.MethodGet, "/api/home", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	res := httptest.NewRecorder()
	authr.Required(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", res.Code)
	}
	if cookies := res.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("unexpected renewal cookies: %#v", cookies)
	}
	session, found, err := cat.GetSession(ctx, token)
	if err != nil || !found {
		t.Fatalf("get session found=%v err=%v", found, err)
	}
	if !session.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("db session expires at %s, want unchanged %s", session.ExpiresAt, expiresAt)
	}
}

func TestRequiredProvidesOpaqueSessionIdentity(t *testing.T) {
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

	token := "identity-token"
	if err := cat.CreateSession(ctx, token, time.Hour, createTestAdmin(t, cat)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	authr := &Authenticator{Catalog: cat}

	var identity string
	req := httptest.NewRequest(http.MethodGet, "/api/home", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	res := httptest.NewRecorder()
	authr.Required(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ok bool
		identity, ok = SessionIdentityFromContext(r.Context())
		if !ok {
			t.Error("authenticated request is missing its session identity")
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", res.Code)
	}
	if identity == "" {
		t.Fatal("session identity is empty")
	}
	if identity == token || strings.Contains(identity, token) {
		t.Fatalf("session identity exposed the raw token: %q", identity)
	}
}

func TestRequiredRejectsBannedUserSession(t *testing.T) {
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
	hash, err := HashPassword("secret123")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	userID, err := cat.CreateUser(ctx, "viewer", hash, "user")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	authr := &Authenticator{Catalog: cat}
	rr := httptest.NewRecorder()
	role, err := authr.UserLogin(rr, loginRequest("203.0.113.30"), "viewer", "secret123")
	if err != nil || role != "user" {
		t.Fatalf("login role=%q err=%v", role, err)
	}
	if err := cat.SetUserBanned(ctx, userID, true); err != nil {
		t.Fatalf("ban user: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/home", nil)
	req.AddCookie(rr.Result().Cookies()[0])
	res := httptest.NewRecorder()
	authr.Required(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.Code)
	}
}

func TestRequiredRejectsDeletedUserSession(t *testing.T) {
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
	hash, err := HashPassword("secret123")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	userID, err := cat.CreateUser(ctx, "viewer", hash, "user")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	authr := &Authenticator{Catalog: cat}
	rr := httptest.NewRecorder()
	if role, err := authr.UserLogin(rr, loginRequest("203.0.113.31"), "viewer", "secret123"); err != nil || role != "user" {
		t.Fatalf("login role=%q err=%v", role, err)
	}
	if err := cat.DeleteUser(ctx, userID); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/home", nil)
	req.AddCookie(rr.Result().Cookies()[0])
	res := httptest.NewRecorder()
	authr.Required(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.Code)
	}
}

func TestAuthMiddlewareReturnsServiceUnavailableWhenCatalogFails(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := cat.Close(); err != nil {
		t.Fatalf("close catalog: %v", err)
	}
	authr := &Authenticator{Catalog: cat}

	tests := []struct {
		name string
		wrap func(http.Handler) http.Handler
	}{
		{name: "required", wrap: authr.Required},
		{name: "admin required", wrap: authr.AdminRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodGet, "/api/home", nil)
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid-looking-token"})
			res := httptest.NewRecorder()

			tt.wrap(next).ServeHTTP(res, req)

			if called {
				t.Fatal("protected handler was called")
			}
			if res.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body = %s", res.Code, res.Body.String())
			}
			if strings.Contains(res.Body.String(), "database") || strings.Contains(res.Body.String(), "interrupted") {
				t.Fatalf("response exposed the catalog error: %q", res.Body.String())
			}
		})
	}
}

func TestUserLoginRejectsMissingUser(t *testing.T) {
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
	hash, err := HashPassword("secret123")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := cat.CreateUser(ctx, "viewer", hash, "user"); err != nil {
		t.Fatalf("create user: %v", err)
	}

	authr := &Authenticator{Catalog: cat}
	role, err := authr.UserLogin(httptest.NewRecorder(), loginRequest("203.0.113.32"), "legacy-admin", "legacy-secret")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if role != "" {
		t.Fatalf("role = %q, want failed login", role)
	}
}

func TestCheckCurrentPasswordUsesDatabaseAdminSession(t *testing.T) {
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
	hash, err := HashPassword("database-secret")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := cat.CreateUser(ctx, "database-admin", hash, "admin"); err != nil {
		t.Fatalf("create admin: %v", err)
	}

	authr := &Authenticator{
		Catalog: cat,
	}
	loginResponse := httptest.NewRecorder()
	role, err := authr.UserLogin(
		loginResponse,
		loginRequest("203.0.113.33"),
		"database-admin",
		"database-secret",
	)
	if err != nil || role != "admin" {
		t.Fatalf("login role=%q err=%v", role, err)
	}
	request := httptest.NewRequest(http.MethodPost, "/admin/api/backups/example/restore", nil)
	request.AddCookie(responseCookie(t, loginResponse, sessionCookie))

	ok, err := authr.CheckCurrentPassword(request, "database-secret")
	if err != nil || !ok {
		t.Fatalf("database password check ok=%v err=%v", ok, err)
	}
	ok, err = authr.CheckCurrentPassword(request, "different-config-secret")
	if err != nil {
		t.Fatalf("wrong password check: %v", err)
	}
	if ok {
		t.Fatal("database admin was incorrectly verified with the config password")
	}
}

func TestLegacyAdminSessionsAreRejected(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	if err := cat.CreateSession(context.Background(), "legacy-token", time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	authr := &Authenticator{Catalog: cat}
	req := httptest.NewRequest(http.MethodGet, "/admin/api/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "legacy-token"})
	if ok, _, err := authr.ValidateRequest(httptest.NewRecorder(), req); err != nil || ok {
		t.Fatalf("legacy session accepted: ok=%v err=%v", ok, err)
	}
	if ok, err := authr.CheckCurrentPassword(req, "legacy-secret"); err != nil || ok {
		t.Fatalf("legacy password accepted: ok=%v err=%v", ok, err)
	}
	for _, middleware := range []func(http.Handler) http.Handler{authr.Required, authr.AdminRequired} {
		res := httptest.NewRecorder()
		middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("legacy session reached authenticated handler")
		})).ServeHTTP(res, req)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d", res.Code)
		}
	}
}

func TestLoginUsesForwardedClientIPFromTrustedProxy(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	createTestAdmin(t, cat)
	authr := &Authenticator{
		Catalog: cat,
	}

	const forwardedIP = "203.0.113.12"
	for i := 0; i < loginFailThreshold; i++ {
		request := loginRequest("127.0.0.1")
		request.Header.Set("X-Forwarded-For", forwardedIP)
		ok, err := login(authr, httptest.NewRecorder(), request, "admin", "wrong")
		if ok {
			t.Fatalf("failed login %d returned ok", i+1)
		}
		if i < loginFailThreshold-1 && err != nil {
			t.Fatalf("failed login %d returned error: %v", i+1, err)
		}
		if i == loginFailThreshold-1 && !errors.Is(err, ErrLoginIPBanned) {
			t.Fatalf("final failed login error = %v, want ErrLoginIPBanned", err)
		}
	}

	banned, err := cat.IsLoginIPBanned(context.Background(), forwardedIP)
	if err != nil {
		t.Fatalf("query forwarded IP ban: %v", err)
	}
	if !banned {
		t.Fatal("forwarded client IP was not persisted as banned")
	}
	loopbackBanned, err := cat.IsLoginIPBanned(context.Background(), "127.0.0.1")
	if err != nil {
		t.Fatalf("query loopback ban: %v", err)
	}
	if loopbackBanned {
		t.Fatal("local proxy address was incorrectly persisted as banned")
	}
}

func loginRequest(ip string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/admin/api/login", strings.NewReader(`{}`))
	req.RemoteAddr = ip + ":12345"
	return req
}

func responseCookie(t *testing.T, rr *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range rr.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("response cookie %q not found", name)
	return nil
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func createTestAdmin(t *testing.T, cat *catalog.Catalog) int64 {
	t.Helper()
	hash, err := HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	id, err := cat.CreateUser(context.Background(), "admin", hash, "admin")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func login(a *Authenticator, w http.ResponseWriter, r *http.Request, username, password string) (bool, error) {
	role, err := a.UserLogin(w, r, username, password)
	return role == "admin", err
}

func TestUserLoginDoesNotAcceptDefaultCredentialsOnEmptyDatabase(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	a := &Authenticator{Catalog: cat}
	response := httptest.NewRecorder()
	role, err := a.UserLogin(response, loginRequest("203.0.113.50"), "admin", "admin123")
	if err != nil || role != "" || len(response.Result().Cookies()) != 0 {
		t.Fatalf("default login role=%q err=%v", role, err)
	}
}
