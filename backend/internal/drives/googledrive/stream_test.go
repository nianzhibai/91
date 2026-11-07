package googledrive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

func TestConcurrentMediaUnauthorizedRequestsShareOneRefresh(t *testing.T) {
	var rejected, refreshes atomic.Int32
	ready := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			refreshes.Add(1)
			writeTestJSON(w, tokenResp{AccessToken: "new-access"})
			return
		}
		if r.Header.Get("Authorization") == "Bearer old-access" {
			if rejected.Add(1) == 2 {
				close(ready)
			}
			select {
			case <-ready:
			case <-r.Context().Done():
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer new-access" || r.Header.Get("Range") != "bytes=2-4" {
			t.Errorf("unexpected media headers: %v", r.Header)
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("234"))
	}))
	t.Cleanup(upstream.Close)
	driver := New(Config{
		ID: "google", AccessToken: "old-access", RefreshToken: "refresh",
		ClientID: "client", ClientSecret: "secret", OAuthURL: upstream.URL + "/token",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL+"/file?alt=media", nil)
			if err != nil {
				results <- err
				return
			}
			req.Header.Set("Range", "bytes=2-4")
			resp, err := driver.RequestStream(req)
			if err != nil {
				results <- err
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err == nil && (resp.StatusCode != http.StatusPartialContent || string(body) != "234") {
				err = fmt.Errorf("status=%d body=%q", resp.StatusCode, body)
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if refreshes.Load() != 1 || rejected.Load() != 2 {
		t.Fatalf("refreshes=%d rejected=%d, want 1/2", refreshes.Load(), rejected.Load())
	}
}

func TestMediaUnauthorizedRecoveryStopsAfterOneRefresh(t *testing.T) {
	for _, refreshLimited := range []bool{false, true} {
		t.Run(fmt.Sprintf("refresh limited=%t", refreshLimited), func(t *testing.T) {
			var media, refreshes atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					refreshes.Add(1)
					if refreshLimited {
						w.Header().Set("Retry-After", "120")
						writeTestJSONStatus(w, http.StatusTooManyRequests, tokenResp{Error: "rate_limit_exceeded"})
						return
					}
					writeTestJSON(w, tokenResp{AccessToken: "new-access"})
					return
				}
				media.Add(1)
				w.WriteHeader(http.StatusUnauthorized)
			}))
			t.Cleanup(upstream.Close)
			driver := New(Config{
				ID: "google", AccessToken: "old-access", RefreshToken: "refresh",
				ClientID: "client", ClientSecret: "secret", OAuthURL: upstream.URL + "/token",
			})
			req, err := http.NewRequest(http.MethodGet, upstream.URL+"/file?alt=media", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := driver.RequestStream(req)
			if resp != nil || err == nil {
				t.Fatalf("response=%v error=%v, want failure", resp, err)
			}
			if refreshLimited {
				if wait, limited := drives.RateLimitRetryAfter(err); !limited || wait != 2*time.Minute {
					t.Fatalf("error=%v, want rate limit with 2m retry", err)
				}
				if _, err := driver.RequestStream(req); err == nil {
					t.Fatal("request succeeded during refresh cooldown")
				}
			} else {
				var provider *drives.ProviderError
				if !errors.As(err, &provider) || provider.Kind != drives.ProviderErrorAuth {
					t.Fatalf("error=%v, want authentication failure", err)
				}
			}
			wantMedia := int32(2)
			if refreshLimited {
				wantMedia = 1
			}
			if refreshes.Load() != 1 || media.Load() != wantMedia {
				t.Fatalf("refreshes=%d media=%d, want 1/%d", refreshes.Load(), media.Load(), wantMedia)
			}
		})
	}
}

func TestMediaRequestCancellationStopsUpstream(t *testing.T) {
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	driver := New(Config{AccessToken: "access"})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL+"/file?alt=media", nil)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := driver.RequestStream(req)
		result <- err
	}()
	select {
	case <-started:
		cancel()
	case <-ctx.Done():
		t.Fatal("media request did not reach upstream")
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want canceled request", err)
	}
}

func TestMediaRedirectUsesCurrentCredentialsOnlyAtOriginalOrigin(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
			t.Errorf("credentials leaked to redirect: %v", r.Header)
		}
		if r.Header.Get("Range") != "bytes=2-4" {
			t.Errorf("Range = %q", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Range", "bytes */2")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	t.Cleanup(cdn.Close)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer current-access" {
			t.Errorf("upstream Authorization = %q", r.Header.Get("Authorization"))
		}
		http.Redirect(w, r, cdn.URL+"/video", http.StatusFound)
	}))
	t.Cleanup(upstream.Close)
	driver := New(Config{AccessToken: "current-access"})
	req, err := http.NewRequest(http.MethodGet, upstream.URL+"/file?alt=media", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer cached-access")
	req.Header.Set("Cookie", "account=secret")
	req.Header.Set("Range", "bytes=2-4")
	resp, err := driver.RequestStream(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable || resp.Header.Get("Content-Range") != "bytes */2" {
		t.Fatalf("range response=%v", resp)
	}
	if req.Header.Get("Authorization") != "Bearer cached-access" {
		t.Fatal("media request mutated cached credentials")
	}
}

func TestMediaErrorBodyIsBoundedAndClosed(t *testing.T) {
	body := &trackedMediaBody{Reader: strings.NewReader(strings.Repeat("x", 128*1024))}
	err := mediaResponseError(&http.Response{
		StatusCode: http.StatusForbidden, Header: make(http.Header), Body: body,
	})
	if err == nil || !body.closed || body.read != 64*1024 {
		t.Fatalf("error=%v closed=%t bytes read=%d", err, body.closed, body.read)
	}
}

type trackedMediaBody struct {
	io.Reader
	read   int
	closed bool
}

func (b *trackedMediaBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}

func (b *trackedMediaBody) Close() error {
	b.closed = true
	return nil
}
