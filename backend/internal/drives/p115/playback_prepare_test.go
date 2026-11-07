package p115

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nianzhibai/91/backend/internal/drives"
)

const playbackTestSHA1 = "0123456789ABCDEF0123456789ABCDEF01234567"

func pendingPlaybackInfo(pushed bool) string {
	return fmt.Sprintf(`{"state":true,"file_id":"file","file_status":0,"sha1":%q,"video_push_state":%t}`, playbackTestSHA1, pushed)
}

func newPreparingPlaybackDrive(t *testing.T, info func() string, submit func(*http.Request) (*http.Response, error)) *Driver {
	t.Helper()
	d := New(Config{ID: "115", Cookie: "UID=u; CID=c; SEID=s"})
	d.rememberPickCode("file", "pick")
	d.hlsClient = &http.Client{Transport: p115RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Cookie") != d.cookie || r.UserAgent() != p115HLSUserAgent || r.Referer() != p115HLSReferer {
			t.Errorf("missing provider authentication headers")
		}
		switch {
		case r.URL.Host == "webapi.115.com" && r.URL.Path == "/files/video":
			if r.Method != http.MethodGet || r.URL.Query().Get("pickcode") != "pick" {
				t.Errorf("query must address this video: %s %s", r.Method, r.URL)
			}
			return playbackJSONResponse(r, info()), nil
		case r.URL.Query().Get("ac") == "push":
			if r.Method != http.MethodPost || r.URL.Host != "115.com" || r.URL.Query().Get("ct") != "play" {
				t.Errorf("unexpected submission endpoint: %s %s", r.Method, r.URL)
			}
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.PostForm.Get("op") != "vip_push" || r.PostForm.Get("pickcode") != "pick" || r.PostForm.Get("sha1") != playbackTestSHA1 {
				t.Errorf("submission must only accelerate the requested file: %v", r.PostForm)
			}
			return submit(r)
		default:
			resp := playbackJSONResponse(r, "")
			resp.StatusCode = http.StatusForbidden
			return resp, nil
		}
	})}
	return d
}

func TestPendingPlaybackQueriesNeverSubmitAndPreparationIsDeduplicated(t *testing.T) {
	var submissions atomic.Int32
	var enqueues atomic.Int32
	d := newPreparingPlaybackDrive(t, func() string { return pendingPlaybackInfo(false) }, func(r *http.Request) (*http.Response, error) {
		submissions.Add(1)
		return playbackJSONResponse(r, `{"state":true,"msg_code":0}`), nil
	})
	base := d.hlsClient.Transport
	d.hlsClient.Transport = p115RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/files/video" && !r.URL.Query().Has("local") {
			enqueues.Add(1)
		}
		return base.RoundTrip(r)
	})
	ctx := context.Background()
	for range 2 {
		sources, err := d.PlaybackSources(ctx, "file")
		if err != nil || len(sources) != 1 || sources[0].Status != "processing" || sources[0].Link != nil {
			t.Fatalf("pending query: sources=%+v err=%v", sources, err)
		}
	}
	if submissions.Load() != 0 || enqueues.Load() != 0 {
		t.Fatal("read-only query submitted a job")
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			sources, err := d.PreparePlayback(ctx, "file")
			if err != nil || len(sources) != 1 || sources[0].Status != "processing" {
				t.Errorf("preparation: sources=%+v err=%v", sources, err)
			}
		})
	}
	wg.Wait()
	// A later query/retry must not reset the submission guard either.
	d.PlaybackSources(ctx, "file")
	d.PreparePlayback(ctx, "file")
	if submissions.Load() != 1 || enqueues.Load() != 1 {
		t.Fatalf("submissions=%d enqueues=%d, want one despite concurrent and later retries", submissions.Load(), enqueues.Load())
	}
}

func TestPreparationChecksProviderStateAfterRestart(t *testing.T) {
	d := newPreparingPlaybackDrive(t, func() string { return pendingPlaybackInfo(true) }, func(r *http.Request) (*http.Response, error) {
		t.Error("provider already accepted preparation")
		return playbackJSONResponse(r, `{"state":true}`), nil
	})
	sources, err := d.PreparePlayback(context.Background(), "file")
	if err != nil || len(sources) != 1 || sources[0].Status != "processing" {
		t.Fatalf("sources=%+v err=%v", sources, err)
	}
}

func TestPreparationNeverSubmitsForUnconfirmedOrRejectedFiles(t *testing.T) {
	for _, tc := range []struct {
		name, info string
		wantError  bool
	}{
		{"expired login", `{"state":false,"errno":99,"error":"登录超时，请重新登录"}`, true},
		{"missing file", `{"state":false,"errno":70005}`, true},
		{"invalid JSON", `<html>upstream error</html>`, true},
		{"missing status", `{"state":true}`, false},
		{"wrong file", `{"state":true,"file_id":"other","file_status":0,"sha1":"` + playbackTestSHA1 + `"}`, false},
		{"invalid SHA1", `{"state":true,"file_id":"file","file_status":0,"sha1":"invalid"}`, false},
		{"already transcoded", `{"state":true,"file_id":"file","file_status":1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newPreparingPlaybackDrive(t, func() string { return tc.info }, func(r *http.Request) (*http.Response, error) {
				t.Error("unexpected transcode submission")
				return playbackJSONResponse(r, `{"state":true}`), nil
			})
			sources, err := d.PreparePlayback(context.Background(), "file")
			if (err != nil) != tc.wantError {
				t.Fatalf("sources=%+v err=%v", sources, err)
			}
			if !tc.wantError && (len(sources) != 1 || sources[0].Status != "unavailable") {
				t.Fatalf("unconfirmed status advertised as processing: %+v", sources)
			}
		})
	}
}

func TestAmbiguousSubmissionFailureIsNotRetried(t *testing.T) {
	var submissions atomic.Int32
	d := newPreparingPlaybackDrive(t, func() string { return pendingPlaybackInfo(false) }, func(*http.Request) (*http.Response, error) {
		submissions.Add(1)
		return nil, errors.New("connection lost after submitting")
	})
	for range 2 {
		if _, err := d.PreparePlayback(context.Background(), "file"); err == nil {
			t.Fatal("submission failure was hidden")
		}
	}
	if submissions.Load() != 1 {
		t.Fatal("ambiguous POST was submitted again")
	}
}

func TestPlaybackStatusThrottlePreventsPreparation(t *testing.T) {
	d := New(Config{ID: "115", Cookie: "UID=u"})
	d.rememberPickCode("file", "pick")
	d.hlsClient = &http.Client{Transport: p115RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Error("throttled query submitted preparation")
		}
		resp := playbackJSONResponse(r, "")
		resp.StatusCode = http.StatusTooManyRequests
		return resp, nil
	})}
	if _, err := d.PreparePlayback(context.Background(), "file"); err == nil {
		t.Fatal("missing throttle error")
	} else if _, limited := drives.RateLimitRetryAfter(err); !limited {
		t.Fatalf("throttle classification lost: %v", err)
	}
}

func TestConversionFinishingDuringStatusLookupRetriesHLSWithoutSubmitting(t *testing.T) {
	d := newPreparingPlaybackDrive(t, func() string {
		return `{"state":true,"file_id":"file","file_status":1}`
	}, func(r *http.Request) (*http.Response, error) {
		t.Error("completed file submitted another conversion")
		return playbackJSONResponse(r, `{"state":true}`), nil
	})
	base := d.hlsClient.Transport
	var masters atomic.Int32
	d.hlsClient.Transport = p115RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/video/m3u8/pick.m3u8" && masters.Add(1) > 1 {
			return playbackJSONResponse(r, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nhttps://cpats01.115.com/ready.m3u8\n"), nil
		}
		return base.RoundTrip(r)
	})
	sources, err := d.PlaybackSources(context.Background(), "file")
	if err != nil || len(sources) != 1 || sources[0].Status != "ready" || masters.Load() != 2 {
		t.Fatalf("sources=%+v err=%v masters=%d", sources, err, masters.Load())
	}
}
