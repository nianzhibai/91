package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/nianzhibai/91/backend/internal/drives/googledrive"
)

func TestGoogleDriveCachedMediaRefreshesExpiredToken(t *testing.T) {
	var expired atomic.Bool
	var metadata, media, refreshes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			refreshes.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"new-access"}`))
			return
		}
		if r.URL.Query().Get("alt") != "media" {
			metadata.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"file","name":"movie.mp4","size":"10"}`))
			return
		}
		media.Add(1)
		if got := r.Header.Get("Range"); got != "bytes=2-4" {
			t.Errorf("upstream Range = %q", got)
		}
		if expired.Load() && r.Header.Get("Authorization") != "Bearer new-access" {
			// Recovery must also work without a structured API error body.
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "3")
		w.Header().Set("Content-Range", "bytes 2-4/10")
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("234"))
	}))
	t.Cleanup(upstream.Close)

	driver := googledrive.New(googledrive.Config{
		ID: "google", AccessToken: "old-access", RefreshToken: "refresh",
		ClientID: "client", ClientSecret: "secret",
		APIBaseURL: upstream.URL + "/drive/v3", OAuthURL: upstream.URL + "/token",
	})
	registry := NewRegistry()
	registry.Set("google", driver)
	p := New(registry)
	if err := p.WarmStreamLink(context.Background(), "google", "file", nil); err != nil {
		t.Fatal(err)
	}
	var updates []proxyStatusUpdate
	p.SetStreamStatusReporter(func(driveID, status, lastError string) {
		updates = append(updates, proxyStatusUpdate{driveID, status, lastError})
	})

	for i := 0; i < 3; i++ {
		expired.Store(i > 0)
		request := httptest.NewRequest(http.MethodGet, "/p/stream/google/file", nil)
		request.Header.Set("Range", "bytes=2-4")
		response := httptest.NewRecorder()
		p.ServeStream(response, request, "google", "file")
		if response.Code != http.StatusPartialContent || response.Body.String() != "234" {
			t.Fatalf("request %d: status=%d body=%s", i, response.Code, response.Body.String())
		}
		if got := response.Header().Get("Content-Range"); got != "bytes 2-4/10" {
			t.Fatalf("Content-Range = %q", got)
		}
		if got := response.Header().Get("Authorization"); got != "" {
			t.Fatalf("browser received Authorization = %q", got)
		}
	}
	if metadata.Load() != 1 || media.Load() != 4 || refreshes.Load() != 1 {
		t.Fatalf("metadata=%d media=%d refreshes=%d, want 1/4/1", metadata.Load(), media.Load(), refreshes.Load())
	}
	if len(updates) != 1 || updates[0].status != "ok" {
		t.Fatalf("status updates = %+v, want only a successful result", updates)
	}
}

func TestGoogleDriveMediaErrorsRetainProviderClassification(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		retryAfter string
		code       string
		cooldown   bool
	}{
		{"download quota", http.StatusForbidden, `{"error":{"code":403,"errors":[{"reason":"downloadQuotaExceeded"}]}}`, "", "drive_rate_limited", true},
		{"retry header", http.StatusForbidden, `{}`, "120", "drive_rate_limited", true},
		{"plain rate limit", http.StatusTooManyRequests, "busy", "120", "drive_rate_limited", true},
		{"permissions", http.StatusForbidden, `{"error":{"code":403,"message":"Insufficient permissions"}}`, "", "drive_auth_failed", false},
		{"missing file", http.StatusNotFound, `{"error":{"code":404,"message":"File not found"}}`, "", "drive_source_not_found", false},
		{"unavailable", http.StatusServiceUnavailable, "temporarily unavailable", "", "drive_upstream_unavailable", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var media atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("alt") != "media" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"file"}`))
					return
				}
				media.Add(1)
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(upstream.Close)
			registry := NewRegistry()
			registry.Set("google", googledrive.New(googledrive.Config{
				ID: "google", AccessToken: "access", APIBaseURL: upstream.URL + "/drive/v3",
			}))
			p := New(registry)
			for i := 0; i < 2; i++ {
				response := httptest.NewRecorder()
				p.ServeStream(response, httptest.NewRequest(http.MethodGet, "/p/stream/google/file", nil), "google", "file")
				var payload streamErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if response.Code != http.StatusFailedDependency || payload.Code != tt.code {
					t.Fatalf("status=%d payload=%+v, want %s", response.Code, payload, tt.code)
				}
				if i == 0 && tt.retryAfter != "" && response.Header().Get("Retry-After") != tt.retryAfter {
					t.Fatalf("Retry-After = %q, want %q", response.Header().Get("Retry-After"), tt.retryAfter)
				}
				if i > 0 && tt.cooldown {
					seconds, err := strconv.Atoi(response.Header().Get("Retry-After"))
					if err != nil || seconds <= 0 || seconds > 300 {
						t.Fatalf("cooldown Retry-After = %q", response.Header().Get("Retry-After"))
					}
				}
			}
			wantCalls := int32(2)
			if tt.cooldown {
				wantCalls = 1
			}
			if media.Load() != wantCalls {
				t.Fatalf("media requests = %d, want %d", media.Load(), wantCalls)
			}
		})
	}
}
