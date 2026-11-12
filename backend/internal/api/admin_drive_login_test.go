package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/nianzhibai/91/backend/internal/drivelogin"
)

func driveLoginRouter(admin *AdminServer) http.Handler {
	router := chi.NewRouter()
	router.Post("/admin/api/drive-logins", admin.handleStartDriveLogin)
	router.Get("/admin/api/drive-logins/{id}", admin.handleDriveLoginStatus)
	router.Delete("/admin/api/drive-logins/{id}", admin.handleCancelDriveLogin)
	return router
}

func serveDriveLogin(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rr
}

func startDriveLogin(t *testing.T, handler http.Handler, kind string) drivelogin.Session {
	t.Helper()
	rr := serveDriveLogin(handler, http.MethodPost, "/admin/api/drive-logins", `{"kind":"`+kind+`"}`)
	if rr.Code != http.StatusCreated || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("start %s: status=%d body=%s", kind, rr.Code, rr.Body.String())
	}
	var session drivelogin.Session
	if err := json.NewDecoder(rr.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	if session.ID == "" || session.Kind != kind || !strings.HasPrefix(session.QRImageDataURL, "data:image/png;base64,") ||
		session.ExpiresAt.IsZero() || session.IntervalMs < 1000 {
		t.Fatalf("session = %#v", session)
	}
	return session
}

func pollDriveLogin(t *testing.T, handler http.Handler, id string) drivelogin.Result {
	t.Helper()
	rr := serveDriveLogin(handler, http.MethodGet, "/admin/api/drive-logins/"+id, "")
	if rr.Code != http.StatusOK || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("poll: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var result drivelogin.Result
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

// expectDriveLoginSuccess polls once, checks the credentials and confirms the
// finished login is forgotten.
func expectDriveLoginSuccess(t *testing.T, handler http.Handler, session drivelogin.Session, want map[string]string) {
	t.Helper()
	result := pollDriveLogin(t, handler, session.ID)
	if result.State != drivelogin.Success || !maps.Equal(result.Credentials, want) {
		t.Fatalf("result = %#v, want credentials %v", result, want)
	}
	if rr := serveDriveLogin(handler, http.MethodGet, "/admin/api/drive-logins/"+session.ID, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("finished login polled again: status=%d", rr.Code)
	}
}

type driveLoginTransport func(*http.Request) (*http.Response, error)

func (f driveLoginTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonUpstreamResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func TestQuarkDriveLoginReturnsCookie(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/cas/ajax/getTokenForQrcodeLogin":
			http.SetCookie(w, &http.Cookie{Name: "_UP_LOGIN", Value: "temporary", Path: "/", HttpOnly: true})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": 2000000,
				"data":   map[string]any{"members": map[string]string{"token": "quark-token"}},
			})
		case "/cas/ajax/getServiceTicketByQrcodeToken":
			if r.URL.Query().Get("token") != "quark-token" {
				t.Errorf("poll token = %q", r.URL.Query().Get("token"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": 2000000,
				"data":   map[string]any{"members": map[string]string{"service_ticket": "service-ticket"}},
			})
		case "/account/info":
			http.SetCookie(w, &http.Cookie{Name: "__pus", Value: "pus-value", Path: "/", HttpOnly: true})
			http.SetCookie(w, &http.Cookie{Name: "__puus", Value: "puus-value", Path: "/", HttpOnly: true})
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "code": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	handler := driveLoginRouter(&AdminServer{QuarkQRUOPBaseURL: upstream.URL, QuarkQRPanBaseURL: upstream.URL})
	session := startDriveLogin(t, handler, "quark")
	if !strings.HasPrefix(session.QRCodeURL, "https://su.quark.cn/4_eMHBJ?") {
		t.Fatalf("qr code url = %q", session.QRCodeURL)
	}
	expectDriveLoginSuccess(t, handler, session, map[string]string{"cookie": "__pus=pus-value; __puus=puus-value"})
}

func TestP115DriveLoginReturnsCookie(t *testing.T) {
	client := &http.Client{Transport: driveLoginTransport(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host + req.URL.Path {
		case "qrcodeapi.115.com/api/1.0/web/1.0/token":
			return jsonUpstreamResponse(req, `{"state":1,"code":0,"data":{"uid":"qr-uid","time":1784127027,"sign":"qr-sign","qrcode":"https://115.com/scan/dg-qr-uid"}}`), nil
		case "qrcodeapi.115.com/get/status/":
			if req.URL.Query().Get("uid") != "qr-uid" || req.URL.Query().Get("sign") != "qr-sign" {
				t.Errorf("status query = %s", req.URL.RawQuery)
			}
			return jsonUpstreamResponse(req, `{"state":1,"code":0,"data":{"status":2}}`), nil
		case "passportapi.115.com/app/1.0/web/1.0/login/qrcode":
			return jsonUpstreamResponse(req, `{"state":1,"code":0,"data":{"cookie":{"UID":"user-uid","CID":"cid","SEID":"seid","KID":"kid"}}}`), nil
		default:
			t.Errorf("unexpected upstream request: %s", req.URL)
			return nil, fmt.Errorf("unexpected request")
		}
	})}

	handler := driveLoginRouter(&AdminServer{P115QRHTTPClient: client})
	session := startDriveLogin(t, handler, "p115")
	if session.QRCodeURL != "https://115.com/scan/dg-qr-uid" {
		t.Fatalf("qr code url = %q", session.QRCodeURL)
	}
	expectDriveLoginSuccess(t, handler, session, map[string]string{"cookie": "UID=user-uid;CID=cid;SEID=seid;KID=kid"})
}

func p123LoginUpstream(t *testing.T, result map[string]any) *httptest.Server {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/user/qr-code/generate":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "message": "ok",
				"data": map[string]string{"uniID": "uni-1", "url": "https://www.123pan.com/wx-app-login.html"},
			})
		case "/api/user/qr-code/result":
			if r.URL.Query().Get("uniID") != "uni-1" {
				t.Errorf("uniID = %q", r.URL.Query().Get("uniID"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": result})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func TestP123DriveLoginReturnsAccessToken(t *testing.T) {
	upstream := p123LoginUpstream(t, map[string]any{"loginStatus": 3, "scanPlatform": 7, "token": "Bearer app-token"})
	handler := driveLoginRouter(&AdminServer{P123UserAPIBaseURL: upstream.URL + "/api"})
	session := startDriveLogin(t, handler, "p123")
	if session.IntervalMs != 1800 {
		t.Fatalf("interval = %d", session.IntervalMs)
	}
	expectDriveLoginSuccess(t, handler, session, map[string]string{"access_token": "app-token"})
}

func TestRejectedDriveLoginEnds(t *testing.T) {
	upstream := p123LoginUpstream(t, map[string]any{"loginStatus": 2})
	handler := driveLoginRouter(&AdminServer{P123UserAPIBaseURL: upstream.URL + "/api"})
	session := startDriveLogin(t, handler, "p123")
	if result := pollDriveLogin(t, handler, session.ID); result.State != drivelogin.Rejected || result.Credentials != nil {
		t.Fatalf("result = %#v", result)
	}
	if rr := serveDriveLogin(handler, http.MethodGet, "/admin/api/drive-logins/"+session.ID, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("rejected login polled again: status=%d", rr.Code)
	}
}

func TestYun139DriveLoginReturnsAuthorizationOnce(t *testing.T) {
	token := "one|two|three|" + fmt.Sprint(time.Now().Add(24*time.Hour).UnixMilli())
	calls := 0
	client := &http.Client{Transport: driveLoginTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" {
			t.Error("QR login did not use unauthenticated POST")
		}
		data, _ := json.Marshal(map[string]any{"code": "0", "data": map[string]string{"account": "13800000000", "token": token}})
		return jsonUpstreamResponse(r, string(data)), nil
	})}

	handler := driveLoginRouter(&AdminServer{Yun139QRHTTPClient: client})
	session := startDriveLogin(t, handler, "yun139")
	authorization := base64.StdEncoding.EncodeToString([]byte("pc:13800000000:" + token))
	expectDriveLoginSuccess(t, handler, session, map[string]string{"authorization": authorization})
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

func wopanLoginUpstream(t *testing.T, result map[string]any) *httptest.Server {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		meta := map[string]string{"code": "0000", "message": "ok"}
		switch r.URL.Path {
		case "/QRCode/generate":
			_ = json.NewEncoder(w).Encode(map[string]any{"meta": meta, "result": map[string]string{"uuid": "uuid-1", "image": "iVBORw0KGgo="}})
		case "/QRCode/query":
			if r.URL.Query().Get("uuid") != "uuid-1" {
				t.Errorf("uuid = %q", r.URL.Query().Get("uuid"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"meta": meta, "result": result})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func TestWopanDriveLoginReturnsTokens(t *testing.T) {
	upstream := wopanLoginUpstream(t, map[string]any{"state": 3, "token": "access-1", "refreshToken": "refresh-1"})
	handler := driveLoginRouter(&AdminServer{WopanQRAPIBaseURL: upstream.URL + "/QRCode"})
	session := startDriveLogin(t, handler, "wopan")
	if session.QRImageDataURL != "data:image/png;base64,iVBORw0KGgo=" || session.IntervalMs != 1200 {
		t.Fatalf("session = %#v", session)
	}
	// The family ID is absent upstream, so it is omitted rather than sent empty.
	expectDriveLoginSuccess(t, handler, session, map[string]string{"access_token": "access-1", "refresh_token": "refresh-1"})
}

func TestWopanDriveLoginReportsScanAndExpiry(t *testing.T) {
	state := 2
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		meta := map[string]string{"code": "0000", "message": "ok"}
		if r.URL.Path == "/QRCode/generate" {
			_ = json.NewEncoder(w).Encode(map[string]any{"meta": meta, "result": map[string]string{"uuid": "uuid-1", "image": "iVBORw0KGgo="}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"meta": meta, "result": map[string]any{"state": state}})
	}))
	t.Cleanup(upstream.Close)

	handler := driveLoginRouter(&AdminServer{WopanQRAPIBaseURL: upstream.URL + "/QRCode"})
	session := startDriveLogin(t, handler, "wopan")
	if result := pollDriveLogin(t, handler, session.ID); result.State != drivelogin.Scanned || result.Message == "" {
		t.Fatalf("scanned result = %#v", result)
	}
	state = 4
	if result := pollDriveLogin(t, handler, session.ID); result.State != drivelogin.Expired {
		t.Fatalf("expired result = %#v", result)
	}
}

func TestGuangYaPanDriveLoginHonoursSlowDownAndReturnsTokens(t *testing.T) {
	polls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/device/code":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":               "device-1",
				"verification_uri_complete": "https://account.guangyapan.example/device?code=abc",
				"interval":                  5,
				"expires_in":                300,
			})
		case "/v1/auth/token":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["device_code"] != "device-1" {
				t.Errorf("device_code = %#v", body["device_code"])
			}
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "slow_down"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-1", "refresh_token": "refresh-1", "token_type": "Bearer"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	handler := driveLoginRouter(&AdminServer{GuangYaPanAccountBaseURL: upstream.URL})
	session := startDriveLogin(t, handler, "guangyapan")
	if session.QRCodeURL != "https://account.guangyapan.example/device?code=abc" || session.IntervalMs != 5000 {
		t.Fatalf("session = %#v", session)
	}
	if result := pollDriveLogin(t, handler, session.ID); result.State != drivelogin.Waiting || result.IntervalMs != 10000 {
		t.Fatalf("slow down result = %#v", result)
	}
	expectDriveLoginSuccess(t, handler, session, map[string]string{"access_token": "access-1", "refresh_token": "refresh-1"})
}

func TestDriveLoginRejectsInvalidRequestsAndUnknownSessions(t *testing.T) {
	upstreamCalled := false
	handler := driveLoginRouter(&AdminServer{P115QRHTTPClient: &http.Client{Transport: driveLoginTransport(func(r *http.Request) (*http.Response, error) {
		upstreamCalled = true
		return nil, fmt.Errorf("unexpected request")
	})}})
	for _, body := range []string{"", `null`, `{}`, `{"kind":"webdav"}`, `{"kind":"p115","extra":true}`, `{"kind":"p115"}{}`, `{"kind":42}`, `{"kind":"` + strings.Repeat("x", 2048) + `"}`} {
		rr := serveDriveLogin(handler, http.MethodPost, "/admin/api/drive-logins", body)
		if rr.Code != http.StatusBadRequest || rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("body %q: status=%d", body, rr.Code)
		}
	}
	if upstreamCalled {
		t.Fatal("invalid request reached the provider")
	}
	if rr := serveDriveLogin(handler, http.MethodGet, "/admin/api/drive-logins/unknown", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown session: status=%d", rr.Code)
	}
}

func TestCanceledDriveLoginIsForgotten(t *testing.T) {
	upstream := wopanLoginUpstream(t, map[string]any{"state": 1})
	handler := driveLoginRouter(&AdminServer{WopanQRAPIBaseURL: upstream.URL + "/QRCode"})
	session := startDriveLogin(t, handler, "wopan")
	if result := pollDriveLogin(t, handler, session.ID); result.State != drivelogin.Waiting {
		t.Fatalf("result = %#v", result)
	}
	if rr := serveDriveLogin(handler, http.MethodDelete, "/admin/api/drive-logins/"+session.ID, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("cancel: status=%d", rr.Code)
	}
	if rr := serveDriveLogin(handler, http.MethodGet, "/admin/api/drive-logins/"+session.ID, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("canceled session: status=%d", rr.Code)
	}
}
