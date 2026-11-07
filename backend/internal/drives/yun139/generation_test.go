package yun139

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nianzhibai/91/backend/internal/drives"
)

func TestGenerationHLSCacheAndRefresh(t *testing.T) {
	var calls atomic.Int32
	d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/personal/videoPreview/getPreviewInfo" {
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
		}
		n := calls.Add(1)
		reply(w, map[string]any{"fileId": "file", "previewInfo": map[string]string{"status": "Finished", "url": "https://hls.example/video.m3u8?sign=" + strings.Repeat("x", int(n))}})
	})
	first, err := d.GenerationSource(context.Background(), "file", false)
	if err != nil || first.Protocol != "hls" || first.Relay == nil || len(first.Link.Headers) != 0 {
		t.Fatalf("source=%+v err=%v", first, err)
	}
	_, err = d.GenerationSource(context.Background(), "file", false)
	if err != nil || calls.Load() != 1 {
		t.Fatalf("cache: calls=%d err=%v", calls.Load(), err)
	}
	fresh, err := d.GenerationSource(context.Background(), "file", true)
	if err != nil || fresh.Link.URL == first.Link.URL || calls.Load() != 2 {
		t.Fatalf("refresh: %+v %v", fresh, err)
	}
}

func TestGenerationPendingDoesNotPollAndAuthErrorsSurvive(t *testing.T) {
	for _, status := range []int{200, 401, 429} {
		var calls atomic.Int32
		d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if status != 200 {
				w.WriteHeader(status)
				return
			}
			reply(w, map[string]any{"fileId": "file", "previewInfo": map[string]string{"status": "Running"}})
		})
		for range 2 {
			_, err := d.GenerationSource(context.Background(), "file", false)
			switch status {
			case 200:
				if !errors.Is(err, drives.ErrGenerationStreamUnavailable) {
					t.Fatalf("pending=%v", err)
				}
			case 401:
				var failure *drives.ProviderError
				if !errors.As(err, &failure) || failure.Kind != drives.ProviderErrorAuth {
					t.Fatalf("auth=%v", err)
				}
			case 429:
				if _, ok := drives.RateLimitRetryAfter(err); !ok {
					t.Fatalf("limit=%v", err)
				}
			}
		}
		expected := int32(2)
		if status == 200 {
			expected = 1
		}
		if calls.Load() != expected {
			t.Fatalf("status=%d calls=%d", status, calls.Load())
		}
	}
}
