package quark

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nianzhibai/91/backend/internal/drives"
)

func TestGenerationUsesReadyHLSAndRefreshesCurrentCookie(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/file/v2/play") {
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
		}
		n := calls.Add(1)
		http.SetCookie(w, &http.Cookie{Name: "__puus", Value: fmt.Sprint(n)})
		fmt.Fprintf(w, `{"code":0,"data":{"video_list":[{"resolution":"4k","trans_status":"transcoding"},{"resolution":"high","trans_status":"success","video_info":{"file_format":"m3u8","url":"https://video-play-h-zb.drive.quark.cn/video?sign=%d"}},{"resolution":"low","trans_status":"success","video_info":{"file_format":"m3u8","url":"https://video-play-l-zb.drive.quark.cn/video"}}]}}`, n)
	}))
	defer server.Close()
	d := testDriverForServer(server.URL, server.Client(), t.TempDir())
	first, err := d.GenerationSource(context.Background(), "file", false)
	if err != nil || !strings.Contains(first.Link.URL, "sign=1") || first.Relay == nil || first.Link.Headers.Get("Cookie") != "" {
		t.Fatalf("source=%+v err=%v", first, err)
	}
	_, err = d.GenerationSource(context.Background(), "file", false)
	if err != nil || calls.Load() != 1 {
		t.Fatalf("cache calls=%d err=%v", calls.Load(), err)
	}
	fresh, err := d.GenerationSource(context.Background(), "file", true)
	if err != nil || fresh.Link.URL == first.Link.URL || calls.Load() != 2 {
		t.Fatalf("refresh=%+v err=%v", fresh, err)
	}
	req, _ := http.NewRequest("GET", first.Link.URL, nil)
	first.Relay.Authorize(req)
	if !strings.Contains(req.Header.Get("Cookie"), "__puus=2") {
		t.Fatal("cached source captured old cookie")
	}
	if len(fresh.RefreshStatuses) != 2 || fresh.RefreshStatuses[1] != 412 {
		t.Fatal("missing Quark rejection policy")
	}
}

func TestGenerationPendingFallsBackAndLimitRemainsAnError(t *testing.T) {
	for _, limited := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limited {
				w.Header().Set("Retry-After", "20")
				w.WriteHeader(429)
				return
			}
			fmt.Fprint(w, `{"code":0,"data":{"video_list":[{"resolution":"high","trans_status":"waiting"}]}}`)
		}))
		d := testDriverForServer(server.URL, server.Client(), t.TempDir())
		_, err := d.GenerationSource(context.Background(), "file", false)
		if limited {
			if _, ok := drives.RateLimitRetryAfter(err); !ok {
				t.Fatalf("lost limit: %v", err)
			}
		} else if !errors.Is(err, drives.ErrGenerationStreamUnavailable) {
			t.Fatalf("pending: %v", err)
		}
		server.Close()
	}
}
