package yun139

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/nianzhibai/91/backend/internal/drives"
)

func TestUploadBatchesPartsAndConfirmsCompletion(t *testing.T) {
	data := strings.Repeat("abcde", 41)
	sum := sha256.Sum256([]byte(data))
	digest := hex.EncodeToString(sum[:])
	var batches []int
	var uploaded strings.Builder
	completed := false
	d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/upload/") {
			if r.Method != http.MethodPut || r.Header.Get("Authorization") != "" || r.Header.Get("Mcloud-Sign") != "" || r.Header.Get("Cookie") != "" {
				t.Error("upload received account credentials or wrong method")
			}
			part, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/upload/"))
			chunk, _ := io.ReadAll(r.Body)
			start := (part - 1) * 2
			if string(chunk) != data[start:min(start+2, len(data))] || r.ContentLength != int64(len(chunk)) {
				t.Errorf("incorrect bytes for part %d: %q", part, chunk)
			}
			uploaded.Write(chunk)
			w.WriteHeader(http.StatusOK)
			return
		}
		var body struct {
			ID        string       `json:"fileId"`
			UploadID  string       `json:"uploadId"`
			Hash      string       `json:"contentHash"`
			Algorithm string       `json:"contentHashAlgorithm"`
			Parent    string       `json:"parentFileId"`
			Name      string       `json:"name"`
			Parts     []uploadPart `json:"partInfos"`
			Account   struct {
				Name string `json:"account"`
			} `json:"commonAccountInfo"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/personal/file/create", "/personal/file/getUploadUrl":
			batches = append(batches, len(body.Parts))
			if r.URL.Path == "/personal/file/create" {
				if body.Parent != "target" || body.Name != "video.mp4" || body.Hash != digest || body.Algorithm != "SHA256" {
					t.Errorf("create = %+v", body)
				}
			} else if body.ID != "new-file" || body.UploadID != "upload-session" || body.Account.Name != "13800000000" {
				t.Errorf("getUploadUrl = %+v", body)
			}
			urls := make([]uploadURL, 0, len(body.Parts))
			// Return URLs in reverse order to exercise part-number matching.
			for i := len(body.Parts) - 1; i >= 0; i-- {
				part := body.Parts[i]
				if part.HashContext.Offset != int64((part.Number-1)*2) {
					t.Errorf("offset = %+v", part)
				}
				urls = append(urls, uploadURL{Number: part.Number, URL: "http://" + r.Host + fmt.Sprintf("/upload/%d", part.Number)})
			}
			reply(w, map[string]any{"fileId": "new-file", "fileName": "video.mp4", "uploadId": "upload-session", "partInfos": urls})
		case "/personal/file/complete":
			if uploaded.String() != data || body.ID != "new-file" || body.UploadID != "upload-session" || body.Hash != digest || body.Algorithm != "SHA256" {
				t.Error("completed before correct bytes/hash were uploaded")
			}
			completed = true
			reply(w, nil)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	d.partSize = 2
	got, err := d.Upload(context.Background(), "target", "video.mp4", strings.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if got != "new-file" || !completed || !reflect.DeepEqual(batches, []int{100, 3}) {
		t.Fatalf("file=%q completed=%v batches=%v", got, completed, batches)
	}
	entry, err := d.Stat(context.Background(), got)
	if err != nil || entry.Name != "video.mp4" || entry.Size != int64(len(data)) {
		t.Fatalf("uploaded metadata = %+v, %v", entry, err)
	}
	assertNoUploadSpool(t, d)
}

func TestUploadFastPathsPreserveExistingFiles(t *testing.T) {
	for _, field := range []string{"exist", "rapidUpload"} {
		t.Run(field, func(t *testing.T) {
			calls := 0
			d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/personal/file/create" {
					t.Errorf("unexpected write %s", r.URL.Path)
				}
				reply(w, map[string]any{"fileId": "existing", "fileName": "video (1).mp4", field: true})
			})
			got, err := d.Upload(context.Background(), "", "video.mp4", strings.NewReader("data"), 4)
			if err != nil || got != "existing" || calls != 1 {
				t.Fatalf("Upload = %q, %v; calls=%d", got, err, calls)
			}
			entry, _ := d.Stat(context.Background(), got)
			if entry.Name != "video (1).mp4" {
				t.Fatalf("lost provider filename: %+v", entry)
			}
			assertNoUploadSpool(t, d)
		})
	}
}

func TestUploadRetriesTheEntirePartAfterTransientFailure(t *testing.T) {
	attempts := 0
	d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/personal/file/create":
			reply(w, map[string]any{"fileId": "file", "uploadId": "session", "partInfos": []uploadURL{{Number: 1, URL: "http://" + r.Host + "/upload"}}})
		case "/upload":
			attempts++
			body, _ := io.ReadAll(r.Body)
			if string(body) != "complete part" {
				t.Errorf("retry did not rewind: %q", body)
			}
			if attempts == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
			} else {
				w.WriteHeader(http.StatusOK)
			}
		case "/personal/file/complete":
			if attempts != 2 {
				t.Error("completed before retry succeeded")
			}
			reply(w, nil)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	if _, err := d.Upload(context.Background(), "", "video.mp4", strings.NewReader("complete part"), 13); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d", attempts)
	}
	assertNoUploadSpool(t, d)
}

func TestUploadRejectsMissingPartsAndNeverCompletes(t *testing.T) {
	for _, mode := range []string{"missing", "duplicate", "wrong-number", "failed-put", "failed-complete"} {
		t.Run(mode, func(t *testing.T) {
			completeCalls := 0
			d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/personal/file/create":
					urls := []uploadURL{{Number: 1, URL: "http://" + r.Host + "/upload/1"}, {Number: 2, URL: "http://" + r.Host + "/upload/2"}}
					switch mode {
					case "missing":
						urls = urls[:1]
					case "duplicate":
						urls[1].Number = 1
					case "wrong-number":
						urls[1].Number = 3
					}
					reply(w, map[string]any{"fileId": "new-file", "uploadId": "session", "partInfos": urls})
				case "/upload/1", "/upload/2":
					if mode == "failed-put" {
						w.WriteHeader(400)
					} else {
						w.WriteHeader(200)
					}
				case "/personal/file/complete":
					completeCalls++
					http.Error(w, "unavailable", 503)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			})
			d.partSize = 2
			got, err := d.Upload(context.Background(), "", "video.mp4", strings.NewReader("abcd"), 4)
			if err == nil || got != "" {
				t.Fatalf("Upload = %q, %v", got, err)
			}
			if mode != "failed-complete" && completeCalls != 0 {
				t.Fatal("completed an incomplete upload")
			}
			if _, err := d.Stat(context.Background(), "new-file"); !errors.Is(err, drives.ErrNotSupported) {
				t.Fatal("published failed upload metadata")
			}
			assertNoUploadSpool(t, d)
		})
	}
}

func TestUploadValidatesInputAndCancellationBeforeRemoteWrite(t *testing.T) {
	calls := 0
	d := testDriver(t, func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "unexpected request", 500) })
	for _, size := range []int64{3, 5, -1} {
		if _, err := d.Upload(context.Background(), "", "video.mp4", strings.NewReader("data"), size); err == nil {
			t.Errorf("accepted size %d", size)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Upload(ctx, "", "video.mp4", strings.NewReader("data"), 4); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled upload = %v", err)
	}
	if calls != 0 {
		t.Fatalf("wrote remotely %d times for invalid input", calls)
	}
	assertNoUploadSpool(t, d)
}

func assertNoUploadSpool(t *testing.T, d *Driver) {
	t.Helper()
	entries, err := os.ReadDir(d.uploadTempDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("upload spool leaked: %v, %v", entries, err)
	}
}
