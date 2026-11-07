package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/nianzhibai/91/backend/internal/auth"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives/yun139"
)

func TestMobilePersonalCloudConfiguration(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	admin := &AdminServer{Catalog: cat}
	for _, body := range []string{
		`{"id":"mobile","kind":"yun139","name":"移动个人云","credentials":{"authorization":"old-auth"}}`,
		`{"id":"mobile","kind":"yun139","name":"重命名个人云"}`,
		`{"id":"mobile","kind":"yun139","name":"重命名个人云","credentials":{"authorization":"new-auth"}}`,
	} {
		res := httptest.NewRecorder()
		admin.handleUpsertDrive(res, httptest.NewRequest(http.MethodPost, "/admin/api/drives", strings.NewReader(body)))
		if res.Code != http.StatusOK {
			t.Fatalf("save: status=%d body=%s", res.Code, res.Body.String())
		}
		if !strings.Contains(res.Body.String(), `"canUpload":true`) {
			t.Fatalf("upload capability missing: %s", res.Body.String())
		}
		if strings.Contains(res.Body.String(), "old-auth") || strings.Contains(res.Body.String(), "new-auth") {
			t.Fatal("save response leaked authorization")
		}
		got, err := cat.GetDrive(context.Background(), "mobile")
		if err != nil {
			t.Fatal(err)
		}
		if got.RootID != "/" || got.ScanRootID != "/" {
			t.Fatalf("roots = %q, %q", got.RootID, got.ScanRootID)
		}
		want := "old-auth"
		if strings.Contains(body, "new-auth") {
			want = "new-auth"
		}
		if got.Credentials["authorization"] != want {
			t.Fatal("credentials not preserved or updated")
		}
	}
	if driveKindLabel("yun139") != "移动网盘" {
		t.Fatal("missing public source label")
	}
}

type yun139QRTransport func(*http.Request) (*http.Response, error)

func (f yun139QRTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMobileQRHandlersShareSessionAndReturnAuthorization(t *testing.T) {
	token := "one|two|three|" + fmt.Sprint(time.Now().Add(24*time.Hour).UnixMilli())
	calls := 0
	admin := &AdminServer{Yun139QRHTTPClient: &http.Client{Transport: yun139QRTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" {
			t.Error("QR login did not use unauthenticated POST")
		}
		data, _ := json.Marshal(map[string]any{"code": "0", "data": map[string]string{"account": "13800000000", "token": token}})
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
	})}}
	start := httptest.NewRecorder()
	admin.handleYun139QRStart(start, httptest.NewRequest(http.MethodPost, "/admin/api/drives/yun139/qr", nil))
	if start.Code != http.StatusOK || start.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("start = %d %s", start.Code, start.Body.String())
	}
	var session yun139.QRCodeSession
	if err := json.NewDecoder(start.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	if session.SessionID == "" || !strings.HasPrefix(session.QRImageDataURL, "data:image/png;base64,") || strings.Contains(start.Body.String(), "authorization") {
		t.Fatal("incomplete or unsafe session response")
	}
	for range 2 {
		status := httptest.NewRecorder()
		admin.handleYun139QRStatus(status, httptest.NewRequest(http.MethodPost, "/admin/api/drives/yun139/qr/status", strings.NewReader(`{"sessionId":"`+session.SessionID+`"}`)))
		if status.Code != http.StatusOK || status.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("status = %d %s", status.Code, status.Body.String())
		}
		var result yun139.QRCodeStatus
		if err := json.NewDecoder(status.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		decoded, err := base64.StdEncoding.DecodeString(result.Authorization)
		if err != nil || result.State != "success" || string(decoded) != "pc:13800000000:"+token {
			t.Fatal("status response did not return usable authorization")
		}
	}
	if calls != 1 {
		t.Fatal("successful session was exchanged repeatedly")
	}
	unknown := httptest.NewRecorder()
	(&AdminServer{}).handleYun139QRStatus(unknown, httptest.NewRequest(http.MethodPost, "/admin/api/drives/yun139/qr/status", strings.NewReader(`{"sessionId":"not-a-session"}`)))
	if unknown.Code != http.StatusOK || !strings.Contains(unknown.Body.String(), `"state":"expired"`) {
		t.Fatalf("unknown session = %d %s", unknown.Code, unknown.Body.String())
	}
}

func TestMobileQRStatusRejectsMalformedBodies(t *testing.T) {
	for _, body := range []string{"", `{}`, `null`, `{"sessionId":" "}`, `{"sessionId":"id","extra":true}`, `{"sessionId":"id"}{}`, `{"sessionId":42}`, `{"sessionId":"` + strings.Repeat("x", 2048) + `"}`} {
		rr := httptest.NewRecorder()
		(&AdminServer{}).handleYun139QRStatus(rr, httptest.NewRequest(http.MethodPost, "/admin/api/drives/yun139/qr/status", strings.NewReader(body)))
		if rr.Code != http.StatusBadRequest || rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("malformed body: status=%d", rr.Code)
		}
	}
}

func TestMobileQRRoutesRequireAdministrator(t *testing.T) {
	router := chi.NewRouter()
	(&AdminServer{Auth: &auth.Authenticator{}}).Register(router)
	for _, path := range []string{"/admin/api/drives/yun139/qr", "/admin/api/drives/yun139/qr/status"} {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"sessionId":"id"}`)))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s = %d", path, rr.Code)
		}
	}
}
