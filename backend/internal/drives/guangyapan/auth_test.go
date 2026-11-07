package guangyapan

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

func TestInitPreservesAccountFailureAndCredentials(t *testing.T) {
	tests := []struct {
		name        string
		refresh     bool
		status      int
		body        string
		wantKind    drives.ProviderErrorKind
		wantCalls   int32
		wantMessage string
	}{
		{"validation unavailable", false, 503, `{"error":"server_error","error_description":"account maintenance"}`, drives.ProviderErrorUnavailable, 3, "account maintenance"},
		{"invalid validation response", false, 200, `<html>bad gateway</html>`, drives.ProviderErrorOther, 1, "invalid response"},
		{"validation missing user", false, 200, `{}`, drives.ProviderErrorOther, 1, "empty user sub"},
		{"refresh rejected", true, 400, `{"error":"invalid_grant","error_code":1001,"error_description":"refresh credential revoked"}`, drives.ProviderErrorAuth, 1, "error_code=1001"},
		{"refresh rejection in successful HTTP response", true, 200, `{"error":"invalid_grant","error_description":"refresh credential revoked"}`, drives.ProviderErrorAuth, 1, "invalid_grant"},
		{"refresh unavailable", true, 503, `{"error":"server_error","error_description":"refresh_token service down"}`, drives.ProviderErrorUnavailable, 3, "refresh_token service down"},
		{"client configuration rejected", true, 401, `{"error":"invalid_client","error_description":"client cannot use refresh_token"}`, drives.ProviderErrorOther, 1, "invalid_client"},
		{"refresh missing access token", true, 200, `{"refresh_token":"incomplete-refresh"}`, drives.ProviderErrorOther, 1, "empty access_token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls, refreshCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth/token" {
					refreshCalls.Add(1)
				} else if r.URL.Path != "/v1/user/me" {
					t.Errorf("unexpected account operation: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				if tt.refresh && r.URL.Path == "/v1/user/me" {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"error":"invalid_token"}`)
					return
				}
				calls.Add(1)
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			var saved map[string]string
			d := New(Config{AccessToken: "old-access", RefreshToken: "old-refresh", AccountBaseURL: srv.URL,
				OnCredentialsUpdate: func(values map[string]string) { saved = values },
			})
			if tt.wantKind != drives.ProviderErrorAuth {
				// An infrastructure failure must not fall through to another
				// login method or send a new SMS verification code.
				d.phoneNumber, d.sendCode = "13800000000", true
			}
			d.apiRateInterval, d.authRetryDelay = 0, 0
			err := d.Init(context.Background())
			var provider *drives.ProviderError
			if !errors.As(err, &provider) || provider.Kind != tt.wantKind {
				t.Fatalf("Init() = %v, want kind %s", err, tt.wantKind)
			}
			if !strings.Contains(err.Error(), tt.wantMessage) || strings.Contains(err.Error(), "use QR login") {
				t.Fatalf("Init() lost account error: %v", err)
			}
			if calls.Load() != tt.wantCalls || (!tt.refresh && refreshCalls.Load() != 0) {
				t.Fatalf("calls=%d refreshes=%d, want calls=%d refresh=%v", calls.Load(), refreshCalls.Load(), tt.wantCalls, tt.refresh)
			}
			if saved["access_token"] != "old-access" || saved["refresh_token"] != "old-refresh" {
				t.Fatal("failed authentication overwrote persisted credentials")
			}
			access, refresh := d.tokenSnapshot()
			if refresh != "old-refresh" || (!tt.refresh && access != "old-access") {
				t.Fatal("transient validation or failed refresh discarded credentials")
			}
		})
	}
}

func TestInitRetriesValidationWithoutRefreshing(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/user/me" || r.Header.Get("Authorization") != "Bearer old-access" {
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		writeTestJSON(w, map[string]string{"sub": "user-1"})
	}))
	defer srv.Close()
	d := New(Config{AccessToken: "old-access", RefreshToken: "old-refresh", AccountBaseURL: srv.URL})
	d.apiRateInterval, d.authRetryDelay = 0, 0
	if err := d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("validation calls = %d, want 2", calls.Load())
	}
}

func TestInitRefreshRetriesPersistRotationBeforeValidation(t *testing.T) {
	for _, failValidation := range []bool{false, true} {
		t.Run(map[bool]string{false: "recovered", true: "validation unavailable after rotation"}[failValidation], func(t *testing.T) {
			var refreshCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth/token" {
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["refresh_token"] != "old-refresh" {
						t.Error("unexpected refresh credential")
					}
					if refreshCalls.Add(1) == 1 {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					writeTestJSON(w, map[string]string{"access_token": "new-access", "refresh_token": "new-refresh"})
					return
				}
				if r.Header.Get("Authorization") == "Bearer old-access" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if failValidation {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				writeTestJSON(w, map[string]string{"sub": "user-1"})
			}))
			defer srv.Close()
			var saved map[string]string
			d := New(Config{AccessToken: "old-access", RefreshToken: "old-refresh", AccountBaseURL: srv.URL,
				OnCredentialsUpdate: func(values map[string]string) { saved = values },
			})
			d.apiRateInterval, d.authRetryDelay = 0, 0
			err := d.Init(context.Background())
			if (err != nil) != failValidation {
				t.Fatalf("Init() = %v", err)
			}
			if failValidation && !strings.Contains(err.Error(), "after refresh") {
				t.Fatalf("missing post-refresh validation context: %v", err)
			}
			if refreshCalls.Load() != 2 || saved["access_token"] != "new-access" || saved["refresh_token"] != "new-refresh" {
				t.Fatal("refresh must retry once and persist the rotated credentials even if validation fails")
			}
		})
	}
}

type accountRoundTripper func(*http.Request) (*http.Response, error)

func (f accountRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAccountTransportRetrySafety(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		failure   error
		wantCalls int
	}{
		{"validation timeout", http.MethodGet, context.DeadlineExceeded, 2},
		{"validation connection reset", http.MethodGet, syscall.ECONNRESET, 2},
		{"refresh dial failure", http.MethodPost, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, 2},
		{"refresh response lost", http.MethodPost, io.ErrUnexpectedEOF, 1},
		{"refresh response timed out", http.MethodPost, context.DeadlineExceeded, 1},
		{"canceled request", http.MethodGet, context.Canceled, 1},
		{"permanent DNS failure", http.MethodGet, &net.DNSError{IsNotFound: true, Err: "no such host"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := New(Config{})
			d.apiRateInterval, d.authRetryDelay = 0, 0
			calls := 0
			d.accountClient.SetTransport(accountRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return nil, tt.failure
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"sub":"user-1"}`)), Request: r}, nil
			}))
			var out userMeResp
			err := d.requestAccount(context.Background(), "test", tt.method, "/v1/test", "", nil, &out)
			if calls != tt.wantCalls || (err == nil) != (tt.wantCalls == 2) {
				t.Fatalf("calls=%d err=%v, want calls=%d", calls, err, tt.wantCalls)
			}
			if err != nil && !errors.Is(err, tt.failure) {
				t.Fatalf("underlying transport error lost: %v", err)
			}
		})
	}
}

func TestAccountRetryCancellation(t *testing.T) {
	d := New(Config{AccessToken: "old-access", RefreshToken: "old-refresh"})
	d.authRetryDelay = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	requested := make(chan struct{})
	d.accountClient.SetTransport(accountRoundTripper(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(requested)
		}
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
	}))
	done := make(chan error, 1)
	go func() { done <- d.Init(ctx) }()
	select {
	case <-requested:
	case <-time.After(time.Second):
		t.Fatal("request never started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
			t.Fatalf("err=%v calls=%d", err, calls.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop retry backoff")
	}
}

func TestAccountHonorsRateLimitAndLongRetryAfter(t *testing.T) {
	for _, status := range []int{429, 503} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":"temporarily_unavailable"}`)
		}))
		d := New(Config{RefreshToken: "old-refresh", AccountBaseURL: srv.URL})
		d.authRetryDelay = 0
		err := d.Init(context.Background())
		srv.Close()
		if err == nil || calls.Load() != 1 {
			t.Fatalf("status=%d err=%v calls=%d", status, err, calls.Load())
		}
		wait, limited := drives.RateLimitRetryAfter(err)
		if (status == 429) != limited || (limited && wait != 2*time.Minute) {
			t.Fatalf("status=%d rate limited=%v wait=%s", status, limited, wait)
		}
	}
}

func TestAccountErrorsDoNotExposeCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		writeTestJSON(w, map[string]string{
			"error": "invalid_grant", "error_description": "revoked secret-refresh; replacement secret-new-access",
			"access_token": "secret-new-access", "refresh_token": "secret-new-refresh",
		})
	}))
	defer srv.Close()
	d := New(Config{RefreshToken: "secret-refresh", AccountBaseURL: srv.URL})
	err := d.Init(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("missing diagnostic details: %v", err)
	}
	for _, secret := range []string{"secret-refresh", "secret-new-access", "secret-new-refresh"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("error exposes a credential")
		}
	}
}
