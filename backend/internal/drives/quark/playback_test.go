package quark

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestPlaybackSourcesUseRealRenditionsAndCurrentCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/1/clouddrive/file/v2/play" || r.Method != "POST" || r.URL.Query().Get("pr") != "ucpro" || r.Header.Get("Cookie") == "" {
			t.Errorf("unexpected playback request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["fid"] != "file" || body["supports"] != "fmp4,m3u8" {
			t.Errorf("body = %v", body)
		}
		http.SetCookie(w, &http.Cookie{Name: "__puus", Value: "rotated"})
		_, _ = w.Write([]byte(`{"code":0,"data":{"video_list":[
   {"resolution":"low","trans_status":"transcoding"},
   {"resolution":"super","trans_status":"raw","video_info":{"url":"https://video-play-c-zb.drive.quark.cn/raw"}},
   {"resolution":"high","trans_status":"success","right":"svip","video_info":{"file_format":"m3u8","url":"https://video-play-h-zb.drive.quark.cn/high?token=secret","width":540,"height":960,"bitrate":1199.75,"codec":"h264"}}
  ]}}`))
	}))
	defer server.Close()
	d := testDriverForServer(server.URL, server.Client(), t.TempDir())
	sources, err := d.PlaybackSources(context.Background(), "file")
	if err != nil || len(sources) != 2 {
		t.Fatalf("sources=%+v err=%v", sources, err)
	}
	high, low := sources[0], sources[1]
	if high.ID != "cloud-high" || high.Status != "ready" || high.Width != 540 || high.Bitrate != 1200 || high.Relay == nil || high.Link.Headers.Get("Cookie") != "" || high.Link.ClientRedirectSafe {
		t.Fatalf("high=%+v", high)
	}
	if low.ID != "cloud-low" || low.Status != "processing" || low.Link != nil {
		t.Fatalf("low=%+v", low)
	}
	req, _ := http.NewRequest("GET", high.Link.URL, nil)
	high.Relay.Authorize(req)
	if !strings.Contains(req.Header.Get("Cookie"), "__puus=rotated") {
		t.Fatal("rotated cookie not used")
	}
	d.applyResponseCookies([]*http.Cookie{{Name: "__puus", Value: "newest"}})
	high.Relay.Authorize(req)
	if !strings.Contains(req.Header.Get("Cookie"), "__puus=newest") {
		t.Fatal("active source retained stale cookie")
	}
}

func TestOriginalPlaybackDoesNotChangeDownloadContract(t *testing.T) {
	var fail bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v2/play") {
			if fail {
				http.Error(w, "unavailable", 503)
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"video_list":[{"resolution":"super","trans_status":"raw","video_info":{"url":"https://video-play-c-zb-cf.pds.quark.cn/raw?Expires=4102444800","bitrate":3561.0}}]}}`))
			return
		}
		writeQuarkTestJSON(w, map[string]any{"code": 0, "data": []any{map[string]string{"download_url": "https://download.example/original"}}})
	}))
	defer server.Close()
	d := testDriverForServer(server.URL, server.Client(), t.TempDir())
	link, err := d.OriginalPlaybackURL(context.Background(), "f")
	if err != nil || !strings.Contains(link.URL, "/raw?") || link.Headers.Get("Cookie") == "" || link.URLSource != "quark-raw" {
		t.Fatalf("link=%+v err=%v", link, err)
	}
	download, err := d.StreamURL(context.Background(), "f")
	if err != nil || download.URL != "https://download.example/original" || download.URLSource != "quark-download" {
		t.Fatalf("download=%+v err=%v", download, err)
	}
	fail = true
	fallback, err := d.OriginalPlaybackURL(context.Background(), "f")
	if err != nil || fallback.URL != download.URL || fallback.URLSource != "quark-download" {
		t.Fatalf("fallback=%+v err=%v", fallback, err)
	}
}

func TestPlaybackRejectsUntrustedMediaAndStatHandlesDeletedFiles(t *testing.T) {
	d := New(Config{})
	for _, raw := range []string{"http://video-play-h-zb.drive.quark.cn/a", "https://video-play-h-zb.drive.quark.cn.evil.test/a", "https://secret@video-play-h-zb.drive.quark.cn/a", "https://127.0.0.1/a"} {
		if _, err := d.playbackLink(raw); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fid") == "gone" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"fid":"gone","status":0}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"fid":"file","pdir_fid":"parent","file_name":"A&amp;B.mp4","file":true,"status":1,"size":123}}`))
	}))
	defer server.Close()
	d = testDriverForServer(server.URL, server.Client(), t.TempDir())
	file, err := d.Stat(context.Background(), "file")
	if err != nil || file.Name != "A&B.mp4" || file.ParentID != "parent" || file.IsDir {
		t.Fatalf("file=%+v err=%v", file, err)
	}
	if _, err := d.Stat(context.Background(), "gone"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted file: %v", err)
	}
}
