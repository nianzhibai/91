package onedrive

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/scopedproxy"
)

func TestUploadTrafficUsesScopedProxyWithoutSessionAuthorization(t *testing.T) {
	var small, chunk atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() {
			t.Error("request did not use configured proxy")
		}
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Host == "graph.example.test" {
			small.Add(1)
			if r.Header.Get("Authorization") != "Bearer secret" {
				t.Error("Graph upload lost authorization")
			}
		} else {
			chunk.Add(1)
			if r.Header.Get("Authorization") != "" {
				t.Error("Graph token leaked to upload session")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"remote"}`)
	}))
	defer proxy.Close()
	d := New(Config{AccessToken: "secret", APIBaseURL: "http://graph.example.test"})
	ctx, err := scopedproxy.WithURL(context.Background(), proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Upload(ctx, "parent", "small.mp4", strings.NewReader("test"), 4); err != nil {
		t.Fatal(err)
	}
	if _, err = d.putUploadSessionChunkWithRetry(ctx, "http://upload.example.test/session", 0, 4, []byte("test")); err != nil {
		t.Fatal(err)
	}
	if small.Load() != 1 || chunk.Load() != 1 {
		t.Fatalf("small=%d chunk=%d", small.Load(), chunk.Load())
	}
}

func TestUploadRequestsHaveIndependentTimeoutsAndHonorCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer srv.Close()
	d := New(Config{APIBaseURL: srv.URL})
	d.client.SetTimeout(time.Millisecond)
	d.uploadClient.SetTimeout(50 * time.Millisecond)
	_, _, err := d.putUploadSessionChunk(context.Background(), srv.URL, 0, 4, []byte("test"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error=%v", err)
	}
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := d.Upload(ctx, "parent", "small.mp4", strings.NewReader("test"), 4); result <- err }()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error=%v", err)
	}
}

func TestUploadRetryReconcilesAlreadyAcceptedChunk(t *testing.T) {
	var puts, gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			connection.Close() // The server stored the bytes, but its response was lost.
			return
		}
		gets.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"nextExpectedRanges":["4-"]}`)
	}))
	defer srv.Close()
	d := New(Config{})
	item, err := d.putUploadSessionChunkWithRetry(context.Background(), srv.URL, 0, 8, []byte("test"))
	if err != nil || item != nil || puts.Load() != 1 || gets.Load() != 1 {
		t.Fatalf("item=%+v err=%v puts=%d gets=%d", item, err, puts.Load(), gets.Load())
	}
}

func TestUploadRateLimitStopsChunkRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	d := New(Config{})
	_, err := d.putUploadSessionChunkWithRetry(context.Background(), srv.URL, 0, 4, []byte("test"))
	if _, limited := drives.RateLimitRetryAfter(err); !limited || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}
