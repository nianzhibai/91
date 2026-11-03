package googledrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/video-site/backend/internal/drives"
)

const testPlaybackUserAgent = "test-browser/1.0"

func TestPlaybackAuthorizationMatchesGoogleWebHash(t *testing.T) {
	want := "SAPISIDHASH 1700000000_d976d98190ef7f29cd004a2b30d2211db402e674"
	for _, cookie := range []string{"SAPISID=sapisid-test; SID=other", "__Secure-3PAPISID=sapisid-test"} {
		got, err := playbackAuthorization(cookie, time.Unix(1700000000, 0))
		if err != nil || got != want {
			t.Fatalf("authorization=%q err=%v", got, err)
		}
	}
	for _, cookie := range []string{"SID=other", "SAPISID=value\r\nX-Leak: value"} {
		if _, err := playbackAuthorization(cookie, time.Now()); err == nil {
			t.Fatal("accepted invalid web cookie")
		}
	}
}

func TestPlaybackSourcesUseTheConfiguredWebAccount(t *testing.T) {
	for _, index := range []string{"", "3"} {
		t.Run("account-"+index, func(t *testing.T) {
			want := index
			if want == "" {
				want = "0"
			}
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Goog-AuthUser") != want {
					http.Error(w, "wrong Google account", http.StatusForbidden)
					return
				}
				var out playbackResponse
				out.MediaStreamingData.FormatStreamingData.ProgressiveTranscodes = []playbackTranscode{testPlaybackTranscode(18, 640, 360)}
				_ = json.NewEncoder(w).Encode(out)
			}))
			defer api.Close()
			d := New(Config{PlaybackCookie: "SAPISID=test-cookie", PlaybackAuthUser: index, PlaybackAPIURL: api.URL})
			sources, err := d.PlaybackSourcesWithUserAgent(context.Background(), "file", testPlaybackUserAgent)
			if err != nil || len(sources) != 1 || sources[0].Status != "ready" {
				t.Fatalf("configured account could not resolve playback: %v", err)
			}
		})
	}
}

func TestPlaybackRejectsInvalidWebAccountBeforeSendingCredentials(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("invalid account configuration reached Google")
	}))
	defer api.Close()
	for _, index := range []string{"-1", "+3", "1.5", "account", "4294967296", "3\r\nX-Other: value"} {
		d := New(Config{PlaybackCookie: "SAPISID=test-cookie", PlaybackAuthUser: index, PlaybackAPIURL: api.URL})
		sources, err := d.PlaybackSourcesWithUserAgent(context.Background(), "file", testPlaybackUserAgent)
		if err != nil || len(sources) != 1 || sources[0].Status != "unavailable" || !strings.Contains(sources[0].Message, "账号索引") {
			t.Fatalf("invalid account did not disable only web playback: %+v %v", sources, err)
		}
	}
}

func TestInvalidPlaybackCookieOnlyDisablesCompatibility(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("invalid web cookie reached Google")
	}))
	defer api.Close()
	for _, cookie := range []string{"SID=web-secret", "SAPISID=web-secret\r\nX-Leak: web-secret"} {
		d := New(Config{AccessToken: "oauth-secret", PlaybackCookie: cookie, PlaybackAPIURL: api.URL})
		sources, err := d.PlaybackSourcesWithUserAgent(context.Background(), "file", testPlaybackUserAgent)
		if err != nil || len(sources) != 1 || sources[0].Status != "unavailable" || !strings.Contains(sources[0].Message, "Cookie") || strings.Contains(sources[0].Message, "web-secret") {
			t.Fatalf("invalid cookie did not disable only web playback: %+v %v", sources, err)
		}
		if d.tokenSnapshot().access != "oauth-secret" || d.linkCooldownError(time.Now()) != nil {
			t.Fatal("web cookie failure changed original-file authorization")
		}
	}
}

func testPlaybackTranscode(itag, width, height int) playbackTranscode {
	var transcode playbackTranscode
	transcode.Itag = itag
	transcode.URL = fmt.Sprintf("https://rr1.c.drive.google.com/videoplayback?expire=%d&sig=media-secret", time.Now().Add(time.Hour).Unix())
	transcode.Metadata.MimeType = "video/mp4"
	transcode.Metadata.Width, transcode.Metadata.Height = width, height
	transcode.Metadata.Bitrate = 2_000_000
	transcode.Metadata.VideoCodec = "avc1.64001F"
	transcode.Metadata.AudioCodec = "mp4a.40.2"
	return transcode
}

func TestPlaybackSourcesUseWebCookieAndOnlyExposeMuxedMP4(t *testing.T) {
	low := testPlaybackTranscode(18, 360, 640)
	high := testPlaybackTranscode(22, 720, 1280)
	unsafe := testPlaybackTranscode(99, 1920, 1080)
	unsafe.URL = "https://attacker.example/videoplayback"
	audioOnly := testPlaybackTranscode(140, 0, 0)
	audioOnly.Metadata.VideoCodec = ""
	expired := testPlaybackTranscode(100, 1920, 1080)
	expired.URL = "https://rr1.c.drive.google.com/videoplayback?expire=1"
	var calls int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/media/file/playback" || r.URL.Query().Get("key") != drivePlaybackAPIKey {
			t.Errorf("unexpected playback request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Cookie") != "SAPISID=web-cookie; SID=web-session" || !strings.HasPrefix(r.Header.Get("Authorization"), "SAPISIDHASH ") || strings.Contains(r.Header.Get("Authorization"), "oauth-secret") {
			t.Error("playback did not use web authentication independently of OAuth")
		}
		if r.Header.Get("Origin") != drivePlaybackOrigin || r.Header.Get("Referer") != drivePlaybackOrigin+"/" || r.UserAgent() != testPlaybackUserAgent {
			t.Error("missing Drive web origin")
		}
		var response playbackResponse
		response.MediaStreamingData.FormatStreamingData.ProgressiveTranscodes = []playbackTranscode{low, high, high, unsafe, audioOnly, expired}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer api.Close()
	d := New(Config{AccessToken: "oauth-secret", PlaybackCookie: "SAPISID=web-cookie; SID=web-session", PlaybackAPIURL: api.URL + "/media"})
	sources, err := d.PlaybackSourcesWithUserAgent(context.Background(), "file", testPlaybackUserAgent)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(sources) != 2 || sources[0].ID != "cloud-22" || sources[1].ID != "cloud-18" {
		t.Fatalf("sources=%+v calls=%d", sources, calls)
	}
	for i, source := range sources {
		if source.Protocol != "file" || source.Kind != "transcoded" || source.Status != "ready" || source.FileRelay == nil || !source.Link.ClientRedirectSafe || len(source.Link.Headers) != 1 || source.Link.Headers.Get("User-Agent") != testPlaybackUserAgent {
			t.Fatalf("unsafe or unusable source %+v", source)
		}
		if !strings.Contains(source.MediaType, "avc1.64001F") || !strings.Contains(source.MediaType, "mp4a.40.2") {
			t.Fatalf("media type %s", source.MediaType)
		}
		if source.Link.Expires.Before(time.Now().Add(58 * time.Minute)) {
			t.Fatal("lost signed URL expiry")
		}
		wantLabel := []string{"720p", "360p"}[i]
		if source.Label != wantLabel || source.Resolution != wantLabel {
			t.Fatalf("portrait resolution=%q", source.Label)
		}
	}
}

func TestPlaybackWithoutWebCookieDoesNotUseOAuthOrNetwork(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("optional playback contacted upstream without a web cookie")
	}))
	defer api.Close()
	d := New(Config{AccessToken: "oauth-secret", PlaybackAPIURL: api.URL})
	sources, err := d.PlaybackSourcesWithUserAgent(context.Background(), "file", testPlaybackUserAgent)
	if err != nil || len(sources) != 1 || sources[0].Status != "unavailable" || sources[0].Message == "" {
		t.Fatalf("sources=%+v err=%v", sources, err)
	}
}

func TestPlaybackSourcesBindRedirectsToTheViewersUserAgent(t *testing.T) {
	var received []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = append(received, r.UserAgent())
		if r.Header.Get("Cookie") != "SAPISID=account-cookie" || strings.Contains(r.Header.Get("Authorization"), "site-secret") {
			t.Error("viewer credentials replaced Google account credentials")
		}
		var out playbackResponse
		out.MediaStreamingData.FormatStreamingData.ProgressiveTranscodes = []playbackTranscode{testPlaybackTranscode(22, 720, 1280)}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer api.Close()
	d := New(Config{PlaybackCookie: "SAPISID=account-cookie", PlaybackAPIURL: api.URL})
	for _, ua := range []string{"Chrome/viewer-a", "Safari/viewer-b"} {
		sources, err := d.PlaybackSourcesWithUserAgent(context.Background(), "file", ua)
		if err != nil || len(sources) != 1 {
			t.Fatalf("sources=%+v err=%v", sources, err)
		}
		link := sources[0].Link
		if !link.ClientRedirectSafe || len(link.Headers) != 1 || link.Headers.Get("User-Agent") != ua {
			t.Fatalf("redirect lost the viewer identity: %+v", link)
		}
	}
	if len(received) != 2 || received[0] != "Chrome/viewer-a" || received[1] != "Safari/viewer-b" {
		t.Fatalf("metadata UAs=%v", received)
	}
	sources, err := d.PlaybackSourcesWithUserAgent(context.Background(), "file", "")
	if err != nil || len(sources) != 1 || sources[0].Status != "unavailable" || sources[0].Link != nil || len(received) != 2 {
		t.Fatalf("missing UA should not invent a browser identity or contact Google: %+v %v", sources, err)
	}
}

func TestPlaybackErrorsAreClassifiedWithoutLeakingWebCredentials(t *testing.T) {
	for _, tt := range []struct {
		name       string
		status     int
		body, kind string
	}{
		{"auth", 401, `{"error":{"message":"web-secret"}}`, "auth"},
		{"forbidden", 403, `{"error":{"message":"web-secret","details":[{"reason":"USER_PERMISSION_DENIED"}]}}`, "permission"},
		{"quota", 403, `{"error":{"code":403,"errors":[{"reason":"downloadQuotaExceeded"}]}}`, "rate"},
		{"throttled", 429, "web-secret", "rate"},
		{"server", 503, "web-secret", "unavailable"},
		{"missing", 404, "web-secret", "missing"},
		{"malformed", 200, "web-secret", "invalid"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.kind == "rate" {
					w.Header().Set("Retry-After", "120")
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer api.Close()
			d := New(Config{PlaybackCookie: "SAPISID=web-secret", PlaybackAPIURL: api.URL})
			sources, err := d.PlaybackSourcesWithUserAgent(context.Background(), "file", testPlaybackUserAgent)
			if tt.kind == "auth" || tt.kind == "missing" || tt.kind == "permission" {
				if err != nil || len(sources) != 1 || sources[0].Status != "unavailable" {
					t.Fatalf("sources=%+v err=%v", sources, err)
				}
				if strings.Contains(sources[0].Message, "web-secret") {
					t.Fatal("web playback failure leaked credentials")
				}
				if tt.kind == "auth" && !strings.Contains(sources[0].Message, "更新兼容播放 Cookie") {
					t.Fatal("web login failure did not identify the optional playback credential")
				}
				if tt.kind == "permission" && (!strings.Contains(sources[0].Message, "账号索引") || strings.Contains(sources[0].Message, "Cookie") || strings.Contains(sources[0].Message, "web-secret")) {
					t.Fatal("file permissions were reported as an expired login or leaked credentials")
				}
				return
			}
			if err == nil || strings.Contains(err.Error(), "web-secret") {
				t.Fatalf("unexpected error %v", err)
			}
			if tt.kind == "rate" {
				if wait, limited := drives.RateLimitRetryAfter(err); !limited || wait != 2*time.Minute {
					t.Fatalf("rate error %v", err)
				}
				if d.linkCooldownError(time.Now()) != nil {
					t.Fatal("optional playback blocked original-file streaming")
				}
			} else if tt.kind != "invalid" {
				var provider *drives.ProviderError
				if !errors.As(err, &provider) || string(provider.Kind) != tt.kind {
					t.Fatalf("classification %v", err)
				}
			}
		})
	}
}

func TestPlaybackMetadataNeverFollowsCredentialRedirect(t *testing.T) {
	redirected := false
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer other.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 302) }))
	defer api.Close()
	d := New(Config{PlaybackCookie: "SAPISID=web-secret", PlaybackAPIURL: api.URL})
	if _, err := d.PlaybackSourcesWithUserAgent(context.Background(), "file", testPlaybackUserAgent); err == nil || redirected {
		t.Fatalf("redirected=%t err=%v", redirected, err)
	}
}

func TestPlaybackMediaURLRejectsUntrustedDestinations(t *testing.T) {
	for _, raw := range []string{
		"https://c.drive.google.com.evil.example/videoplayback", "https://rr.c.drive.google.com:444/videoplayback",
		"http://rr.c.drive.google.com/videoplayback", "https://secret@rr.c.drive.google.com/videoplayback",
		"https://rr.c.drive.google.com/videoplayback#secret", "https://rr.c.drive.google.com/other",
	} {
		u, _ := url.Parse(raw)
		if validPlaybackMediaURL(u) {
			t.Fatalf("accepted %s", raw)
		}
	}
}
