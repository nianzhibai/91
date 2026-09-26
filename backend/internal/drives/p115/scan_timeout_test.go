package p115

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/video-site/backend/internal/scanner"
)

func TestScannerRetries115DirectoryTimeouts(t *testing.T) {
	for _, tt := range []struct {
		name       string
		timeouts   int
		cancelAt   int
		wantCalls  int
		wantFailed bool
	}{
		{name: "recovers after one timeout", timeouts: 1, wantCalls: 2},
		{name: "recovers after two timeouts", timeouts: 2, wantCalls: 3},
		{name: "exhausts retries", timeouts: 3, wantCalls: 3, wantFailed: true},
		{name: "canceled during retry", timeouts: 3, cancelAt: 2, wantCalls: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			driver := newP115ListTestDriver(p115RoundTripFunc(func(request *http.Request) (*http.Response, error) {
				var body string
				switch dirID := request.URL.Query().Get("cid"); dirID {
				case "0":
					body = `{"state":true,"cid":"0","count":2,"offset":0,"data":[{"cid":"slow-dir","pid":"0","n":"Slow"},{"fid":"healthy-file","cid":"0","n":"healthy.mp4","s":"12"}]}`
				case "slow-dir":
					calls++
					if calls == tt.cancelAt {
						cancel()
					}
					if calls <= tt.timeouts {
						<-request.Context().Done()
						return nil, request.Context().Err()
					}
					body = `{"state":true,"cid":"slow-dir","count":1,"offset":0,"data":[{"fid":"recovered-file","cid":"slow-dir","n":"recovered.mp4","s":"12"}]}`
				default:
					t.Fatalf("unexpected directory %q", dirID)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    request,
				}, nil
			}))
			driver.listTimeout = 20 * time.Millisecond
			scan := scanner.New(nil, driver, []string{".mp4"}, nil, nil)
			var waits []time.Duration
			scan.RetryWait = func(ctx context.Context, delay time.Duration) error {
				waits = append(waits, delay)
				return ctx.Err()
			}
			snapshot, stats, err := scan.Discover(ctx, "")
			if calls != tt.wantCalls {
				t.Fatalf("directory requests = %d, want %d", calls, tt.wantCalls)
			}
			if tt.cancelAt != 0 {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("discover error = %v, want context.Canceled", err)
				}
				if len(snapshot.Issues) != 0 || len(snapshot.FailedDirIDs) != 0 {
					t.Fatal("scan cancellation must not become a directory failure")
				}
				return
			}
			if len(waits) != tt.wantCalls-1 {
				t.Fatalf("backoff waits = %d, want %d", len(waits), tt.wantCalls-1)
			}
			for i, delay := range waits {
				base := time.Second
				if i == 1 {
					base = 3 * time.Second
				}
				if delay < base || delay >= base+base/4 {
					t.Fatalf("retry %d delay = %s", i+1, delay)
				}
			}
			if err != nil {
				t.Fatalf("discover: %v", err)
			}
			if _, found := snapshot.SeenFileIDs["healthy-file"]; !found {
				t.Fatal("scan skipped the healthy sibling")
			}
			if _, failed := snapshot.FailedDirIDs["slow-dir"]; failed != tt.wantFailed {
				t.Fatalf("failed directory = %v, want %v", failed, tt.wantFailed)
			}
			if tt.wantFailed {
				if len(snapshot.Issues) != 1 || stats.Errors != 1 || !errors.Is(snapshot.Issues[0].Err, context.DeadlineExceeded) {
					t.Fatalf("issues = %#v, stats = %#v, want one timeout after exhausting retries", snapshot.Issues, stats)
				}
				if _, enumerated := snapshot.EnumeratedDirIDs["slow-dir"]; enumerated {
					t.Fatal("failed directory must remain protected from missing-file cleanup")
				}
			} else {
				if _, found := snapshot.SeenFileIDs["recovered-file"]; !found {
					t.Fatal("successful retry did not discover the directory's video")
				}
				if !snapshot.Complete() || stats.Errors != 0 {
					t.Fatal("recovered timeout must not mark the scan as partially complete")
				}
			}
		})
	}
}

func TestScannerUses115StartingDirectoryName(t *testing.T) {
	metadataCalls := 0
	driver := newP115ListTestDriver(p115RoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/files/get_info" {
			metadataCalls++
			if got := request.URL.Query().Get("file_id"); got != "library" {
				t.Fatalf("metadata file_id = %q", got)
			}
			return playbackJSONResponse(request, `{"state":true,"data":[{"cid":"library","pid":"0","n":"旅行"}]}`), nil
		}
		if got := request.URL.Query().Get("cid"); got != "library" {
			t.Fatalf("list cid = %q", got)
		}
		return playbackJSONResponse(request, `{"state":true,"cid":"library","count":1,"offset":0,"data":[{"fid":"clip","cid":"library","n":"clip.mp4","s":"12"}]}`), nil
	}))
	snapshot, _, err := scanner.New(nil, driver, []string{".mp4"}, nil, nil).Discover(context.Background(), "library")
	if err != nil || len(snapshot.Files) != 1 {
		t.Fatalf("discover: snapshot=%#v, err=%v", snapshot, err)
	}
	file := snapshot.Files[0]
	if metadataCalls != 1 || file.DirName != "旅行" || len(file.AncestorDirNames) != 1 || file.AncestorDirNames[0] != "旅行" {
		t.Fatalf("metadata calls=%d, file=%#v", metadataCalls, file)
	}
}
