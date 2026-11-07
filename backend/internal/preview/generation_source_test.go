package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

type hlsTestDrive struct {
	*previewFakeDrive
	resolve func(bool) (*drives.GenerationSource, error)
}

func (d *hlsTestDrive) GenerationSource(ctx context.Context, id string, refresh bool) (*drives.GenerationSource, error) {
	return d.resolve(refresh)
}

func TestGenerationInputSharesProbeRefreshBudgetWithGeneration(t *testing.T) {
	for _, status := range []int{403, 412, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Retry-After", "11")
				w.WriteHeader(status)
			}))
			defer server.Close()
			resolutions := 0
			drive := &hlsTestDrive{previewFakeDrive: &previewFakeDrive{kind: "quark"}}
			drive.resolve = func(refresh bool) (*drives.GenerationSource, error) {
				resolutions++
				if refresh != (resolutions == 2) {
					t.Errorf("refresh=%v resolutions=%d", refresh, resolutions)
				}
				return &drives.GenerationSource{Protocol: "hls", Link: &drives.StreamLink{URL: server.URL}, RefreshStatuses: []int{403, 412}}, nil
			}
			input, err := resolveGenerationInput(context.Background(), drive, "file")
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			operation := func(_ context.Context, link *drives.StreamLink) error {
				if link.URL == "https://video.example/clip.mp4" {
					return nil
				}
				response, err := server.Client().Get(link.URL)
				if err != nil {
					return err
				}
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
				// Demuxers can skip a rejected segment and report success.
				return nil
			}
			_, err = input.probe(context.Background(), func(ctx context.Context, link *drives.StreamLink) (float64, error) { return 8, operation(ctx, link) })
			if status == 429 {
				delay, ok := drives.RateLimitRetryAfter(err)
				if !ok || delay != 11*time.Second || drive.streamCalls != 0 || resolutions != 1 || requests.Load() != 1 {
					t.Fatalf("limit err=%v original=%d resolutions=%d requests=%d", err, drive.streamCalls, resolutions, requests.Load())
				}
			} else {
				if err != nil || drive.streamCalls != 1 || resolutions != 2 || requests.Load() != 2 {
					t.Fatalf("fallback err=%v original=%d resolutions=%d requests=%d", err, drive.streamCalls, resolutions, requests.Load())
				}
				if err := input.run(context.Background(), operation); err != nil {
					t.Fatal(err)
				}
				if resolutions != 2 || drive.streamCalls != 1 {
					t.Fatal("generation repeated probe recovery")
				}
			}
		})
	}
}

func TestGenerationInputCancellationNeverResolvesOriginal(t *testing.T) {
	drive := &generationPreviewDrive{previewFakeDrive: &previewFakeDrive{kind: "quark"}}
	ctx, cancel := context.WithCancel(context.Background())
	input, err := resolveGenerationInput(ctx, drive, "file")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	err = input.run(ctx, func(context.Context, *drives.StreamLink) error { cancel(); return context.Canceled })
	if !errors.Is(err, context.Canceled) || drive.streamCalls != 0 || len(drive.generationRefreshes) != 1 {
		t.Fatalf("err=%v originals=%d", err, drive.streamCalls)
	}
}

func TestHLSGenerationWithRealFFmpeg(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	for _, format := range []string{"encrypted-ts", "fmp4"} {
		t.Run(format, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			mediaDir := t.TempDir()
			args := []string{"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10", "-t", "6", "-an", "-c:v", "libx264", "-threads", "1", "-pix_fmt", "yuv420p", "-g", "10", "-sc_threshold", "0", "-hls_time", "1", "-hls_playlist_type", "vod", "-hls_segment_filename", filepath.Join(mediaDir, "segment-%d.bin")}
			if format == "fmp4" {
				args = append(args, "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4")
			} else {
				key := filepath.Join(mediaDir, "key")
				if err := os.WriteFile(key, []byte("0123456789abcdef"), 0600); err != nil {
					t.Fatal(err)
				}
				info := filepath.Join(mediaDir, "keyinfo")
				if err := os.WriteFile(info, []byte("key\n"+key+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "-hls_key_info_file", info)
			}
			args = append(args, filepath.Join(mediaDir, "index.m3u8"))
			if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
				t.Fatalf("create fixture: %v %s", err, out)
			}
			var cookie atomic.Value
			cookie.Store("session=first")
			var mediaRequests, expiredRequests atomic.Int32
			var rejectInitial atomic.Bool
			refreshes := 0
			files := http.FileServer(http.Dir(mediaDir))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mediaRequests.Add(1)
				if r.URL.Path == "/expired.m3u8" {
					expiredRequests.Add(1)
					w.WriteHeader(http.StatusPreconditionFailed)
					return
				}
				if r.Header.Get("Cookie") != cookie.Load().(string) {
					http.Error(w, "cookie required", 412)
					return
				}
				files.ServeHTTP(w, r)
			}))
			defer server.Close()
			root, _ := url.Parse(server.URL)
			drive := &hlsTestDrive{previewFakeDrive: &previewFakeDrive{kind: "quark", streamErr: errors.New("unexpected original fallback")}}
			drive.resolve = func(refresh bool) (*drives.GenerationSource, error) {
				path := "/index.m3u8"
				if rejectInitial.Load() && !refresh {
					path = "/expired.m3u8"
				}
				if refresh {
					refreshes++
				}
				return &drives.GenerationSource{Protocol: "hls", Link: &drives.StreamLink{URL: server.URL + path}, Relay: &drives.HLSRelay{AllowedHosts: []string{root.Host}, Authorize: func(req *http.Request) { req.Header.Set("Cookie", cookie.Load().(string)) }}, RefreshStatuses: []int{403, 412}}, nil
			}
			// Both subprocesses must reach the local relay despite ambient proxy settings.
			t.Setenv("http_proxy", "http://127.0.0.1:1")
			t.Setenv("https_proxy", "http://127.0.0.1:1")
			cat, video := seedPreviewTestVideo(t, "hls-real-"+format)
			gen := New(Config{FFmpegPath: ffmpeg, FFprobePath: ffprobe, LocalDir: t.TempDir(), Width: 160, FFmpegThreads: 1})
			thumb := NewThumbWorker(gen, cat, drive)
			if thumb.process(ctx, video) {
				t.Fatal("thumbnail entered cooldown")
			}
			current, err := cat.GetVideo(ctx, video.ID)
			if err != nil || current.ThumbnailURL == "" || current.DurationSeconds <= 0 {
				t.Fatalf("thumbnail/duration: %+v err=%v", current, err)
			}
			cookie.Store("session=rotated")
			rejectInitial.Store(true)
			worker := NewWorker(gen, cat, drive)
			if worker.process(ctx, current) {
				t.Fatal("preview entered cooldown")
			}
			current, err = cat.GetVideo(ctx, video.ID)
			if err != nil || current.PreviewStatus != "ready" || !strings.HasSuffix(current.PreviewLocal, ".mp4") {
				t.Fatalf("preview: %+v err=%v", current, err)
			}
			if expiredRequests.Load() != 1 || refreshes != 1 {
				t.Fatalf("expired requests=%d refreshes=%d", expiredRequests.Load(), refreshes)
			}
			if drive.streamCalls != 0 || mediaRequests.Load() < 3 {
				t.Fatalf("original=%d media=%d", drive.streamCalls, mediaRequests.Load())
			}
		})
	}
}

func TestGenerationInputStopsAfterAuthenticationFailure(t *testing.T) {
	drive := &generationPreviewDrive{previewFakeDrive: &previewFakeDrive{kind: "quark"}}
	input, err := resolveGenerationInput(context.Background(), drive, "file")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	calls := 0
	operation := func(context.Context, *drives.StreamLink) error {
		calls++
		return &drives.ProviderError{Kind: drives.ProviderErrorAuth, Err: errors.New("expired account")}
	}
	for range 2 {
		var failure *drives.ProviderError
		if err := input.run(context.Background(), operation); !errors.As(err, &failure) || failure.Kind != drives.ProviderErrorAuth {
			t.Fatalf("auth error=%v", err)
		}
	}
	if calls != 1 || drive.streamCalls != 0 {
		t.Fatal("authentication failure was retried")
	}
}

func TestGenerationInputKeepsRefreshedSourceWhenDurationIsUnknown(t *testing.T) {
	drive := &generationPreviewDrive{previewFakeDrive: &previewFakeDrive{kind: "p115"}}
	input, err := resolveGenerationInput(context.Background(), drive, "file")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	_, err = input.probe(context.Background(), func(_ context.Context, link *drives.StreamLink) (float64, error) {
		if strings.Contains(link.URL, "initial") {
			return 0, errors.New("HTTP status=403")
		}
		return 0, errDurationUnavailable
	})
	if !errors.Is(err, errDurationUnavailable) || drive.streamCalls != 0 || len(drive.generationRefreshes) != 2 {
		t.Fatalf("probe=%v original=%d refreshes=%v", err, drive.streamCalls, drive.generationRefreshes)
	}
	if err := input.run(context.Background(), func(_ context.Context, link *drives.StreamLink) error {
		if !strings.Contains(link.URL, "refreshed") {
			t.Errorf("source changed: %s", link.URL)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
