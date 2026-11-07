package telegramupload

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nianzhibai/91/backend/internal/drives/webdav"
)

func TestTransferProxyRoutesCloudRequestsAndPreservesDefaultClient(t *testing.T) {
	for _, mode := range []string{"proxy", "default", "failed_proxy"} {
		t.Run(mode, func(t *testing.T) {
			cfg, v, _ := fixture(t)
			var originCalls, proxyCalls, mkdirCalls, uploadCalls atomic.Int32
			var folderExists, uploaded atomic.Bool
			filePath := "/Telegram/" + destinationName(v)
			dav := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestPath := strings.TrimSuffix(r.URL.Path, "/")
				switch r.Method {
				case "MKCOL":
					mkdirCalls.Add(1)
					folderExists.Store(true)
					w.WriteHeader(http.StatusCreated)
				case http.MethodPut:
					body, err := io.ReadAll(r.Body)
					if err != nil || string(body) != "video" || requestPath != filePath {
						t.Errorf("unexpected uploaded file: path=%s bytes=%q error=%v", requestPath, body, err)
					}
					uploadCalls.Add(1)
					uploaded.Store(true)
					w.WriteHeader(http.StatusCreated)
				case "PROPFIND":
					if (requestPath == "/Telegram" && !folderExists.Load()) || (requestPath == filePath && !uploaded.Load()) {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					resourceType := "<d:collection/>"
					if requestPath == filePath {
						resourceType = ""
					}
					w.Header().Set("Content-Type", "application/xml")
					w.WriteHeader(http.StatusMultiStatus)
					fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:"><d:response><d:href>%s</d:href><d:propstat><d:prop><d:resourcetype>%s</d:resourcetype><d:getcontentlength>5</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, r.URL.Path, resourceType)
				default:
					t.Errorf("unexpected WebDAV method: %s", r.Method)
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			})
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				originCalls.Add(1)
				if r.Header.Get("Proxy-Authorization") != "" {
					t.Error("proxy credentials leaked to origin")
				}
				dav.ServeHTTP(w, r)
			}))
			t.Cleanup(origin.Close)
			originURL, _ := url.Parse(origin.URL)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				proxyCalls.Add(1)
				if !r.URL.IsAbs() || r.URL.Host != originURL.Host {
					t.Errorf("unexpected proxy target: %s", r.URL)
				}
				if r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("user:private_password")) {
					t.Error("missing proxy authentication")
				}
				if mode == "failed_proxy" {
					http.Error(w, "private_password", http.StatusBadGateway)
					return
				}
				dav.ServeHTTP(w, r)
			}))
			t.Cleanup(proxy.Close)
			proxyURL, _ := url.Parse(proxy.URL)
			proxyURL.User = url.UserPassword("user", "private_password")
			if mode != "default" {
				cfg.UploadProxy = proxyURL.String()
			}
			cfg.Target = webdav.New(webdav.Config{ID: "cloud", BaseURL: origin.URL})
			ctx := context.Background()
			if _, err := cfg.Target.List(ctx, "/"); err != nil {
				t.Fatal(err)
			}
			err := Run(ctx, cfg)
			if mode == "failed_proxy" {
				if err == nil || strings.Contains(err.Error(), "private_password") || uploaded.Load() {
					t.Fatalf("expected sanitized proxy failure without upload: %v", err)
				}
			} else if err != nil || mkdirCalls.Load() != 1 || uploadCalls.Load() != 1 {
				t.Fatalf("transfer did not create directory and upload: %v", err)
			}
			if mode == "default" {
				if proxyCalls.Load() != 0 || originCalls.Load() <= 1 {
					t.Fatal("empty proxy did not use default transport")
				}
			} else if proxyCalls.Load() == 0 || originCalls.Load() != 1 {
				t.Fatal("configured transfer bypassed its proxy")
			}
			saved, err := cfg.Catalog.GetVideo(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, fileErr := os.Stat(filepath.Join(cfg.LocalDirectory, v.FileID))
			if mode == "failed_proxy" {
				if saved.DriveID != v.DriveID || fileErr != nil {
					t.Fatal("proxy failure changed storage or removed local file")
				}
			} else if saved.DriveID != "cloud" || !errors.Is(fileErr, os.ErrNotExist) {
				t.Fatal("successful upload did not migrate storage and clean up")
			}
			beforeOrigin, beforeProxy := originCalls.Load(), proxyCalls.Load()
			if _, err := cfg.Target.List(ctx, "/"); err != nil {
				t.Fatal(err)
			}
			if originCalls.Load() != beforeOrigin+1 || proxyCalls.Load() != beforeProxy {
				t.Fatal("transfer proxy affected later ordinary drive requests")
			}
		})
	}
}

func TestTransferRejectsInvalidProxyBeforeChangingStorage(t *testing.T) {
	cfg, v, cloud := fixture(t)
	cfg.UploadProxy = "ftp://user:private_password@proxy.example"
	err := Run(context.Background(), cfg)
	if err == nil || strings.Contains(err.Error(), "private_password") || cloud.uploads != 0 {
		t.Fatalf("expected sanitized invalid proxy error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.LocalDirectory, v.FileID)); err != nil {
		t.Fatal("invalid proxy removed local file")
	}
}
