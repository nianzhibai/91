package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/auth"
	"github.com/nianzhibai/91/backend/internal/catalog"
)

func TestUnbanEndpointResetsLoginFailureCount(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	createSessionUser(t, cat)
	const ip = "203.0.113.30"
	for i := 0; i < 3; i++ {
		if _, err := cat.RecordLoginAttempt(ctx, ip, false, time.Now(), 30*time.Minute, 3); err != nil {
			t.Fatal(err)
		}
	}
	server := &AdminServer{Catalog: cat, Auth: &auth.Authenticator{Catalog: cat}}
	res := httptest.NewRecorder()
	server.handleUnbanIP(res, requestWithRouteParam(http.MethodDelete, "/admin/api/banned-ips/"+ip, "ip", ip, strings.NewReader("")))
	if res.Code != http.StatusOK {
		t.Fatalf("unban status=%d body=%s", res.Code, res.Body.String())
	}
	for i := 0; i < 3; i++ {
		res = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"missing-user","password":"wrong"}`))
		req.RemoteAddr = ip + ":12345"
		server.handleLogin(res, req)
		want := http.StatusBadRequest
		if i == 2 {
			want = http.StatusForbidden
		}
		if res.Code != want {
			t.Fatalf("attempt %d status=%d body=%s", i+1, res.Code, res.Body.String())
		}
	}
}
