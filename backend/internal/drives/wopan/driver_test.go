package wopan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	sdk "github.com/OpenListTeam/wopan-sdk-go"
	"github.com/nianzhibai/91/backend/internal/drives"
)

type sdkRoundTripFunc func(*http.Request) (*http.Response, error)

func (f sdkRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestSDKRequestsUseBrowserUserAgent(t *testing.T) {
	responses := map[string]string{
		sdk.KeyAppQueryUser:     `{"userId":"test-user"}`,
		sdk.KeyClassifyRule:     `{"fileTypes":{}}`,
		sdk.KeyGetZoneInfo:      `{"url":"https://upload.example.invalid"}`,
		sdk.KeyQueryAllFiles:    `{"files":[{"id":"file-id","fid":"file-fid","name":"clip.mp4","type":1,"size":123}]}`,
		sdk.KeyGetDownloadUrlV2: `{"list":[{"fid":"file-fid","downloadUrl":"https://download.example.invalid/clip.mp4"}]}`,
	}
	var calls []string
	// The SDK fixes its API host. Intercept HTTPS at the default transport so
	// this exercises the driver's complete Init path without real requests.
	previousTransport := http.DefaultTransport
	transport := previousTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = false
	transport.RegisterProtocol("https", sdkRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		if !strings.HasPrefix(r.UserAgent(), "Mozilla/5.0 ") {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Status:     "403 Forbidden",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("request blocked")),
				Request:    r,
			}, nil
		}
		var request struct {
			Header struct {
				Key string `json:"key"`
			} `json:"header"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		data, ok := responses[request.Header.Key]
		if !ok {
			return nil, fmt.Errorf("unexpected SDK request %q", request.Header.Key)
		}
		calls = append(calls, request.Header.Key)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"STATUS":"200","RSP":{"RSP_CODE":"0000","DATA":` + data + `}}`)),
			Request:    r,
		}, nil
	}))
	http.DefaultTransport = transport
	t.Cleanup(func() {
		http.DefaultTransport = previousTransport
		transport.CloseIdleConnections()
	})

	driver := New(Config{ID: "wopan-test", AccessToken: "0123456789abcdef"})
	ctx := context.Background()
	if err := driver.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	entries, err := driver.List(ctx, "0")
	if err != nil || len(entries) != 1 || entries[0].ID != "file-fid" {
		t.Fatalf("List() = %+v, error = %v", entries, err)
	}
	link, err := driver.StreamURL(ctx, entries[0].ID)
	if err != nil || link.URL != "https://download.example.invalid/clip.mp4" {
		t.Fatalf("StreamURL() = %+v, error = %v", link, err)
	}
	wantCalls := []string{sdk.KeyAppQueryUser, sdk.KeyClassifyRule, sdk.KeyGetZoneInfo, sdk.KeyQueryAllFiles, sdk.KeyGetDownloadUrlV2}
	if !slices.Equal(calls, wantCalls) {
		t.Fatalf("SDK requests = %v, want %v", calls, wantCalls)
	}
}

func TestFileToEntryUsesDirectoryIDAndFileFID(t *testing.T) {
	dir := fileToEntry(&sdk.File{
		Id:   "dir-object-id",
		Fid:  "0",
		Type: 0,
		Name: "collection",
	}, "root")
	if !dir.IsDir {
		t.Fatal("directory entry IsDir = false")
	}
	if dir.ID != "dir-object-id" {
		t.Fatalf("directory id = %q, want object id", dir.ID)
	}

	file := fileToEntry(&sdk.File{
		Id:   "file-object-id",
		Fid:  "fid/with/slash",
		Type: 1,
		Name: "clip.mp4",
		Size: 123,
	}, "dir-object-id")
	if file.IsDir {
		t.Fatal("file entry IsDir = true")
	}
	if file.ID != "fid/with/slash" {
		t.Fatalf("file id = %q, want fid for download", file.ID)
	}
}

func TestDeleteFileIDFromWopanFileUsesObjectIDForFID(t *testing.T) {
	got, ok := deleteFileIDFromWopanFile(&sdk.File{
		Id:   "file-object-id",
		Fid:  "fid/with/slash",
		Type: 1,
		Name: "clip.mp4",
		Size: 123,
	}, drives.SourceFile{
		FileID: "fid/with/slash",
		Name:   "clip.mp4",
		Size:   123,
	})
	if !ok {
		t.Fatal("delete file id not resolved")
	}
	if got != "file-object-id" {
		t.Fatalf("delete file id = %q, want object id", got)
	}
}

func TestDeleteFileIDFromWopanFileAcceptsObjectID(t *testing.T) {
	got, ok := deleteFileIDFromWopanFile(&sdk.File{
		Id:   "file-object-id",
		Fid:  "fid-1",
		Type: 1,
		Name: "clip.mp4",
		Size: 123,
	}, drives.SourceFile{
		FileID: "file-object-id",
		Name:   "clip.mp4",
		Size:   123,
	})
	if !ok {
		t.Fatal("delete file id not resolved")
	}
	if got != "file-object-id" {
		t.Fatalf("delete file id = %q, want object id", got)
	}
}

func TestDeleteFileIDFromWopanFileRejectsIDMismatch(t *testing.T) {
	if _, ok := deleteFileIDFromWopanFile(&sdk.File{
		Id:   "file-object-id",
		Fid:  "fid-1",
		Type: 1,
		Name: "clip.mp4",
		Size: 123,
	}, drives.SourceFile{
		FileID: "other-fid",
		Name:   "clip.mp4",
		Size:   123,
	}); ok {
		t.Fatal("delete file id resolved despite id mismatch")
	}
}

func TestWopanRequestErrorWrapsRateLimit(t *testing.T) {
	err := wopanRequestError("list", errors.New("request failed with status: 429 Too Many Requests"))
	var rateLimit *drives.RateLimitError
	if !errors.As(err, &rateLimit) {
		t.Fatalf("error = %T %[1]v, want RateLimitError", err)
	}
	if rateLimit.Provider != "wopan" {
		t.Fatalf("provider = %q, want wopan", rateLimit.Provider)
	}
}

func TestWopanRequestErrorLeavesNormalErrors(t *testing.T) {
	err := wopanRequestError("download url", errors.New("invalid access token"))
	var rateLimit *drives.RateLimitError
	if errors.As(err, &rateLimit) {
		t.Fatalf("error = %T %[1]v, want non-rate-limit error", err)
	}
}
