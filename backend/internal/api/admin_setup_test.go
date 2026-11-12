package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nianzhibai/91/backend/internal/auth"
	"github.com/nianzhibai/91/backend/internal/catalog"
)

func TestSetupPersistsOnlyInDatabaseAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	const source = "# unchanged configuration\nserver:\n  listen: ':8080'\n"
	server, configPath := newConfigAPIForTest(t, source)
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	server.Catalog = cat
	server.Auth = &auth.Authenticator{Catalog: cat}
	assertSetupStatus(t, server, true)
	res := httptest.NewRecorder()
	server.handleSetup(res, httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"username":"owner","password":"secret123"}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("setup status=%d body=%s", res.Code, res.Body.String())
	}
	if len(res.Result().Cookies()) != 1 {
		t.Fatal("setup did not create a session")
	}
	user, err := cat.GetUserByUsername(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	valid, sessionUserID, err := cat.ValidateSession(ctx, res.Result().Cookies()[0].Value)
	if err != nil || !valid || sessionUserID != user.ID {
		t.Fatalf("session valid=%v user=%d err=%v", valid, sessionUserID, err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	cat, err = catalog.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	server.Catalog = cat
	server.Auth = &auth.Authenticator{Catalog: cat}
	assertSetupStatus(t, server, false)
	res = httptest.NewRecorder()
	server.handleLogin(res, httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"owner","password":"secret123"}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("restarted login status=%d body=%s", res.Code, res.Body.String())
	}
	res = httptest.NewRecorder()
	server.handleSetup(res, httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"username":"other","password":"different"}`)))
	if res.Code != http.StatusConflict {
		t.Fatalf("repeated setup status=%d", res.Code)
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != source {
		t.Fatal("setup or login changed config.yaml")
	}
	if err := cat.DeleteUser(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	assertSetupStatus(t, server, false)
	res = httptest.NewRecorder()
	server.handleSetup(res, httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"username":"other","password":"different"}`)))
	if res.Code != http.StatusConflict {
		t.Fatalf("setup after admin deletion status=%d", res.Code)
	}
}

func assertSetupStatus(t *testing.T, server *AdminServer, want bool) {
	t.Helper()
	res := httptest.NewRecorder()
	server.handleSetupStatus(res, httptest.NewRequest(http.MethodGet, "/api/auth/setup/status", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("setup status=%d body=%s", res.Code, res.Body.String())
	}
	var status struct {
		Required bool `json:"required"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Required != want {
		t.Fatalf("setup required=%v, want %v", status.Required, want)
	}
}

func TestSetupFailsClosedWhenDatabaseIsUnavailable(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	server := &AdminServer{Catalog: cat, Auth: &auth.Authenticator{Catalog: cat}}
	for _, handler := range []http.HandlerFunc{server.handleSetupStatus, server.handleSetup, server.handleLogin} {
		res := httptest.NewRecorder()
		handler(res, httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"username":"owner","password":"secret123"}`)))
		if res.Code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
		}
	}
}

func TestSetupRejectsInvalidPasswordWithoutInitializing(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	server := &AdminServer{Catalog: cat, Auth: &auth.Authenticator{Catalog: cat}}
	for _, password := range []string{"short", strings.Repeat("x", 73)} {
		res := httptest.NewRecorder()
		server.handleSetup(res, httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"username":"owner","password":"`+password+`"}`)))
		if res.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
		}
		assertSetupStatus(t, server, true)
	}
}

func TestConcurrentSetupRequestsReturnOneSuccess(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	server := &AdminServer{Catalog: cat, Auth: &auth.Authenticator{Catalog: cat}}
	responses := make([]*httptest.ResponseRecorder, 2)
	var workers sync.WaitGroup
	start := make(chan struct{})
	for i, username := range []string{"first", "second"} {
		responses[i] = httptest.NewRecorder()
		workers.Add(1)
		go func(res *httptest.ResponseRecorder, username string) {
			defer workers.Done()
			<-start
			server.handleSetup(res, httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"username":"`+username+`","password":"secret123"}`)))
		}(responses[i], username)
	}
	close(start)
	workers.Wait()
	statuses := map[int]int{}
	for _, res := range responses {
		statuses[res.Code]++
	}
	if statuses[http.StatusOK] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("setup statuses=%v", statuses)
	}
}
