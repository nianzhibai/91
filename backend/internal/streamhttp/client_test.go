package streamhttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoopbackRelayKeepsCredentialsOutOfCrossOriginRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Errorf("credentials leaked to target: %#v", r.Header)
		}
		if r.Header.Get("Range") != "bytes=1-3" {
			t.Errorf("Range = %q", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Range", "bytes 1-3/5")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "bcd")
	}))
	t.Cleanup(target.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Cookie") != "session=secret" {
			t.Errorf("origin credentials = %#v", r.Header)
		}
		http.Redirect(w, r, target.URL+"/video", http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	localURL, cleanup, err := StartLoopbackRelay(ctx, origin.URL+"/source", http.Header{
		"Authorization": {"Bearer secret"},
		"Cookie":        {"session=secret"},
	})
	if err != nil {
		t.Fatalf("StartLoopbackRelay: %v", err)
	}
	defer cleanup()
	req, _ := http.NewRequest(http.MethodGet, localURL, nil)
	req.Header.Set("Range", "bytes=1-3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("relay request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || string(body) != "bcd" || resp.Header.Get("Content-Range") != "bytes 1-3/5" {
		t.Fatalf("response status=%d headers=%#v body=%q", resp.StatusCode, resp.Header, body)
	}
}

func TestHasSensitiveHeaders(t *testing.T) {
	if HasSensitiveHeaders(http.Header{"User-Agent": {"ordinary"}}) {
		t.Fatal("ordinary headers reported sensitive")
	}
	for _, key := range []string{"Authorization", "Proxy-Authorization", "Cookie"} {
		if !HasSensitiveHeaders(http.Header{key: {"secret"}}) {
			t.Fatalf("%s was not reported sensitive", key)
		}
	}
}

func TestClientCrossOriginRedirectDropsImplicitRefererAndCredentials(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Referer"); got != "" {
			t.Errorf("Referer = %q, want empty", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization leaked to redirect target: %q", got)
		}
		if got := r.Header.Get("Cookie"); got != "" {
			t.Errorf("Cookie leaked to redirect target: %q", got)
		}
		if got := r.Header.Get("Range"); got != "bytes=2-5" {
			t.Errorf("Range = %q, want bytes=2-5", got)
		}
		if got := r.Header.Get("User-Agent"); got != "91-webdav" {
			t.Errorf("User-Agent = %q, want 91-webdav", got)
		}
		w.Header().Set("Content-Range", "bytes 2-5/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "2345")
	}))
	t.Cleanup(cdn.Close)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Basic secret" {
			t.Errorf("origin Authorization = %q, want Basic secret", got)
		}
		http.Redirect(w, r, cdn.URL+"/video.mp4", http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	req, err := http.NewRequest(http.MethodGet, origin.URL+"/dav/video.mp4", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Basic secret")
	req.Header.Set("Cookie", "session=secret")
	req.Header.Set("Range", "bytes=2-5")
	req.Header.Set("User-Agent", "91-webdav")
	resp, err := NewClient(0).Do(req)
	if err != nil {
		t.Fatalf("redirected request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusPartialContent || string(body) != "2345" {
		t.Fatalf("response = status %d body %q", resp.StatusCode, body)
	}
}

func TestClientPreservesExplicitProviderRefererAcrossRedirect(t *testing.T) {
	const providerReferer = "https://provider.example/"
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Referer"); got != providerReferer {
			t.Errorf("Referer = %q, want %q", got, providerReferer)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(cdn.Close)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cdn.URL+"/file", http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	req, err := http.NewRequest(http.MethodGet, origin.URL+"/file", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Referer", providerReferer)
	resp, err := NewClient(0).Do(req)
	if err != nil {
		t.Fatalf("redirected request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}
