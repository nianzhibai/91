package yun139

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/proxy"
)

func TestPlaybackUsesOfficialHLSWithoutFetchingMediaOrDownloadLinks(t *testing.T) {
	previewCalls, downloadCalls := 0, 0
	d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/personal/videoPreview/getPreviewInfo":
			previewCalls++
			if r.Method != http.MethodPost || body["fileId"] != "file" || body["category"] != "video" {
				t.Errorf("preview request = %s %+v", r.Method, body)
			}
			reply(w, map[string]any{
				"fileId": "file", "meta": map[string]string{"duration": "1800.985"},
				"previewInfo": map[string]string{"status": "Finished", "url": "https://hls.example/playlist.m3u8?sign=media-only"},
			})
		case "/personal/file/getDownloadUrl":
			downloadCalls++
			reply(w, map[string]string{"url": "https://original.example/video.ts"})
		default:
			t.Errorf("playback metadata unexpectedly fetched %s", r.URL.Path)
		}
	})
	links, err := d.PlaybackSources(context.Background(), "file")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].Link == nil || links[0].Link.URL != "https://hls.example/playlist.m3u8?sign=media-only" {
		t.Fatalf("missing HLS source: %+v", links)
	}
	if len(links[0].Link.Headers) != 0 || !links[0].Link.ClientRedirectSafe || !links[0].Link.Expires.After(time.Now()) || time.Until(links[0].Link.Expires) > 2*time.Minute {
		t.Fatalf("unsafe or unbounded browser link: %+v", links[0].Link)
	}
	if previewCalls != 1 || downloadCalls != 0 {
		t.Fatal("HLS resolution unnecessarily requested an original download")
	}
	original, err := d.StreamURL(context.Background(), "file")
	if err != nil || original.URL != "https://original.example/video.ts" || downloadCalls != 1 || previewCalls != 1 {
		t.Fatalf("original download changed: %v", err)
	}
}

func TestPlaybackReportsCloudAvailabilityWithoutResolvingOriginal(t *testing.T) {
	for _, status := range []string{"Pending", "Running", "Failed", "", "Finished"} {
		t.Run(status, func(t *testing.T) {
			downloadCalls := 0
			d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/personal/videoPreview/getPreviewInfo":
					url := "https://hls.example/not-ready.m3u8"
					if status == "Finished" {
						url = ""
					}
					reply(w, map[string]any{"fileId": "file", "previewInfo": map[string]string{"status": status, "url": url}})
				case "/personal/file/getDownloadUrl":
					downloadCalls++
					reply(w, map[string]string{"url": "https://original.example/video.mp4"})
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
				}
			})
			links, err := d.PlaybackSources(context.Background(), "file")
			wantStatus := "unavailable"
			if status == "Pending" || status == "Running" {
				wantStatus = "processing"
			}
			if err != nil || len(links) != 1 || links[0].Link != nil || links[0].Status != wantStatus || downloadCalls != 0 {
				t.Fatalf("fallback = %+v, %v", links, err)
			}
		})
	}
}

func TestPlaybackRejectsUnsafeOrMismatchedPreviewResponses(t *testing.T) {
	for _, tc := range []struct{ name, fileID, url string }{
		{"other file", "other", "https://hls.example/video.m3u8"},
		{"script URL", "file", "javascript:alert(1)"},
		{"URL credentials", "file", "https://account-secret@hls.example/video.m3u8"},
		{"fragment", "file", "https://hls.example/video.m3u8#invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/personal/videoPreview/getPreviewInfo" {
					t.Errorf("unexpected fallback %s", r.URL.Path)
				}
				reply(w, map[string]any{"fileId": tc.fileID, "previewInfo": map[string]string{"status": "Finished", "url": tc.url}})
			})
			links, err := d.PlaybackSources(context.Background(), "file")
			if err == nil || links != nil || strings.Contains(err.Error(), "account-secret") {
				t.Fatalf("unsafe preview accepted or leaked: %+v, %v", links, err)
			}
		})
	}
}

func TestPlaybackPreservesProviderErrorsAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		status int
		kind   drives.ProviderErrorKind
	}{
		{http.StatusUnauthorized, drives.ProviderErrorAuth},
		{http.StatusServiceUnavailable, drives.ProviderErrorUnavailable},
	} {
		d := testDriver(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status) })
		_, err := d.PlaybackSources(context.Background(), "file")
		var providerErr *drives.ProviderError
		if !errors.As(err, &providerErr) || providerErr.Kind != tc.kind {
			t.Fatalf("HTTP %d error = %v", tc.status, err)
		}
	}
	d := testDriver(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) })
	if _, err := d.PlaybackSources(context.Background(), "file"); err == nil {
		t.Fatal("lost rate limit")
	} else if _, ok := drives.RateLimitRetryAfter(err); !ok {
		t.Fatalf("rate limit classification lost: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.PlaybackSources(ctx, "file"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resolution = %v", err)
	}
	if _, err := d.PlaybackSources(context.Background(), " "); err == nil {
		t.Fatal("accepted empty file ID")
	}
}

func TestPlaybackUsesSharedCacheAndRefreshFetchesNewHLS(t *testing.T) {
	calls := 0
	d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		reply(w, map[string]any{"fileId": "file", "previewInfo": map[string]string{
			"status": "Finished", "url": "https://hls.example/playlist.m3u8?sign=" + strings.Repeat("x", calls),
		}})
	})
	registry := proxy.NewRegistry()
	registry.Set(d.ID(), d)
	resolver := proxy.New(registry)
	if !resolver.HasPlaybackProvider(d.ID()) {
		t.Fatal("Mobile Cloud is not registered as an HLS provider")
	}
	var originalURL string
	for range 2 {
		links, err := resolver.PlaybackSources(context.Background(), d.ID(), "file", "", false)
		if err != nil || links[0].Link == nil {
			t.Fatalf("resolve = %+v, %v", links, err)
		}
		originalURL = links[0].Link.URL
	}
	if calls != 1 {
		t.Fatalf("repeated preview requests bypassed shared cache: %d", calls)
	}
	links, err := resolver.PlaybackSources(context.Background(), d.ID(), "file", "", true)
	if err != nil || links[0].Link.URL == originalURL || calls != 2 {
		t.Fatalf("refresh reused expired source: calls=%d err=%v", calls, err)
	}
}
