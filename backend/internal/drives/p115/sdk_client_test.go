package p115

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/video-site/backend/internal/drives"
)

func TestSDKOperationsKeepConcurrentRequestsIndependent(t *testing.T) {
	type operationContextKey struct{}
	const operationCount = 5
	arrived := make(chan struct{}, operationCount)
	release := make(chan struct{})
	d := newP115ListTestDriver(p115RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		// Hold each operation's first request until every operation reaches the
		// transport. This also catches fixes that serialize network requests.
		select {
		case <-release:
		default:
			arrived <- struct{}{}
			<-release
		}
		if cookie, err := r.Cookie("UID"); err != nil || cookie.Value != "test-user" {
			t.Errorf("request lost configured cookie: %v, %v", cookie, err)
		}
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		body := `{"state":true}`
		endpoint := r.URL.Scheme + "://" + r.URL.Host + r.URL.Path
		switch endpoint {
		case sdk.ApiFileInfo:
			id := r.URL.Query().Get("file_id")
			if r.Context().Value(operationContextKey{}) != id {
				t.Errorf("file request inherited another operation's context")
			}
			if id == "" || r.Method != http.MethodGet || len(r.PostForm) != 0 {
				t.Errorf("invalid file request: %s %s %v", r.Method, r.URL, r.PostForm)
			}
			// Only stream requests need a pick code; generation stops before HLS.
			pickCode := ""
			if strings.HasPrefix(id, "stream-") {
				pickCode = "pick-" + id
			}
			body = fmt.Sprintf(`{"state":true,"data":[{"fid":%q,"n":%q,"pc":%q}]}`, id, id+".mp4", pickCode)
		case sdk.ApiFileRename:
			id := r.PostForm.Get("fid")
			if r.Context().Value(operationContextKey{}) != id {
				t.Errorf("rename request inherited another operation's context")
			}
			if !strings.HasPrefix(id, "rename-") || r.PostForm.Get("file_name") != id+".mp4" || r.Method != http.MethodPost {
				t.Errorf("invalid rename request: %s %v", r.Method, r.PostForm)
			}
		case sdk.ApiFileDelete:
			if r.Context().Value(operationContextKey{}) != r.PostForm.Get("fid[0]") {
				t.Errorf("remove request inherited another operation's context")
			}
			if !strings.HasPrefix(r.PostForm.Get("fid[0]"), "remove-") || r.Method != http.MethodPost {
				t.Errorf("invalid remove request: %s %v", r.Method, r.PostForm)
			}
		case sdk.ApiDownloadGetUrl:
			if r.Context().Value(operationContextKey{}) != r.UserAgent() {
				t.Errorf("download request inherited another operation's context")
			}
			if !strings.HasPrefix(r.UserAgent(), "stream-") || r.PostForm.Get("data") == "" || r.Method != http.MethodPost {
				t.Errorf("invalid download request: method=%s ua=%q", r.Method, r.UserAgent())
			}
			// A business rejection exercises the SDK download request without
			// needing to construct an encrypted provider response.
			body = fmt.Sprintf(`{"state":false,"error":%q}`, "rejected-"+r.UserAgent())
		default:
			t.Errorf("unexpected endpoint: %s", endpoint)
		}
		if endpoint != sdk.ApiDownloadGetUrl && r.UserAgent() != "p115-list-test" {
			t.Errorf("request inherited another operation's UA: %q", r.UserAgent())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	}))
	d.client.ImportCredential(&sdk.Credential{UID: "test-user", CID: "test-cid", SEID: "test-seid"})
	ctx := context.Background()
	operations := []struct {
		name string
		run  func(context.Context, string) error
	}{
		{"stat", func(ctx context.Context, id string) error {
			entry, err := d.Stat(ctx, id)
			if err == nil && (entry.ID != id || entry.Name != id+".mp4") {
				return fmt.Errorf("wrong file result: %+v", entry)
			}
			return err
		}},
		{"rename", func(ctx context.Context, id string) error { return d.Rename(ctx, id, id+".mp4") }},
		{"remove", func(ctx context.Context, id string) error { return d.Remove(ctx, id) }},
		{"stream", func(ctx context.Context, id string) error {
			_, err := d.StreamURLWithHeader(ctx, id, http.Header{"User-Agent": {id}})
			if err == nil || !strings.Contains(err.Error(), "rejected-"+id) {
				return fmt.Errorf("wrong download error: %v", err)
			}
			return nil
		}},
		{"generation", func(ctx context.Context, id string) error {
			_, err := d.GenerationStreamURL(ctx, id, false)
			if !errors.Is(err, drives.ErrGenerationStreamUnavailable) {
				return fmt.Errorf("wrong generation error: %v", err)
			}
			return nil
		}},
	}
	var wg sync.WaitGroup
	for _, operation := range operations {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				id := fmt.Sprintf("%s-%d", operation.name, i)
				if err := operation.run(context.WithValue(ctx, operationContextKey{}, id), id); err != nil {
					t.Errorf("%s: %v", operation.name, err)
					return
				}
			}
		}()
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for range operations {
		select {
		case <-arrived:
		case <-timer.C:
			close(release)
			wg.Wait()
			t.Fatal("SDK operations did not reach the transport concurrently")
		}
	}
	close(release)
	wg.Wait()
}

func TestSDKWritesDoNotRetryTransportFailures(t *testing.T) {
	for _, operation := range []string{"rename", "remove"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			calls := 0
			d := newP115ListTestDriver(p115RoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return nil, io.ErrUnexpectedEOF
			}))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var err error
			switch operation {
			case "rename":
				err = d.Rename(ctx, "file", "new.mp4")
			case "remove":
				err = d.Remove(ctx, "file")
			}
			if !errors.Is(err, io.ErrUnexpectedEOF) || calls != 1 {
				t.Fatalf("calls=%d error=%v, want one attempt ending in EOF", calls, err)
			}
		})
	}
}
