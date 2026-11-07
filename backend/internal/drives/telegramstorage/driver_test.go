package telegramstorage

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/proxy"
)

func TestLibraryPlaybackAndDeletionUseOpaqueIdentity(t *testing.T) {
	for _, directory := range []string{"", "海边旅行-1234"} {
		t.Run(directory, func(t *testing.T) {
			ctx := context.Background()
			cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer cat.Close()
			root := filepath.Join(t.TempDir(), "library")
			if err := os.Mkdir(root, 0750); err != nil {
				t.Fatal(err)
			}
			f, err := cat.ReserveTelegramLocalFile(ctx, catalog.TelegramLocalFile{FileID: "tg-opaque.media", JobID: "job", Directory: directory})
			if err != nil {
				t.Fatal(err)
			}
			path, err := Prepare(filepath.Dir(root), f)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("0123456789"), 0640); err != nil {
				t.Fatal(err)
			}
			drv := New(cat, func() string { return filepath.Dir(root) })
			reg := proxy.NewRegistry()
			reg.Set(drv.ID(), drv)
			p := proxy.New(reg)
			req := httptest.NewRequest("GET", "/stream", nil)
			req.Header.Set("Range", "bytes=2-5")
			rr := httptest.NewRecorder()
			p.ServeStream(rr, req, drv.ID(), f.FileID)
			if rr.Code != http.StatusPartialContent || rr.Body.String() != "2345" {
				t.Fatalf("range playback: %d %s", rr.Code, rr.Body.String())
			}
			for _, id := range []string{"../secret", path, "panel-status.json"} {
				if _, err := drv.StreamURL(ctx, id); err == nil {
					t.Fatalf("accepted unregistered path %s", id)
				}
			}
			// The registered drive and file identity survive moving the complete store.
			relocated := filepath.Join(t.TempDir(), "relocated")
			if err := os.Rename(filepath.Dir(root), relocated); err != nil {
				t.Fatal(err)
			}
			root = filepath.Join(relocated, "library")
			path = filepath.Join(root, f.Directory, f.FileID)
			rr = httptest.NewRecorder()
			p.ServeStream(rr, req, drv.ID(), f.FileID)
			if rr.Code != http.StatusPartialContent || rr.Body.String() != "2345" {
				t.Fatalf("relocated range playback: %d %s", rr.Code, rr.Body.String())
			}
			if err := drv.Remove(ctx, f.FileID); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("video file remains")
			}
			if _, err := cat.TelegramLocalFile(ctx, f.FileID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("storage mapping remains")
			}
			if err := drv.Remove(ctx, f.FileID); err != nil {
				t.Fatal("repeat deletion", err)
			}
		})
	}
}

func TestAlbumDeletionPreservesSiblingFiles(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	root := t.TempDir()
	driver := New(cat, func() string { return root })
	for _, id := range []string{"first", "second"} {
		file, err := cat.ReserveTelegramLocalFile(ctx, catalog.TelegramLocalFile{FileID: id + ".media", JobID: id, Directory: "海边旅行-1234", GroupKey: "album"})
		if err != nil {
			t.Fatal(err)
		}
		path, err := Prepare(root, file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(id), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".part", []byte("partial"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := driver.Remove(ctx, "first.media"); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(root, "library", "海边旅行-1234", "second.media")
	if data, err := os.ReadFile(second); err != nil || string(data) != "second" {
		t.Fatal("deleting a video removed another member of its album", err)
	}
	if err := driver.Remove(ctx, "second.media"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(second)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty album directory was retained", err)
	}
	if _, err := os.Stat(filepath.Join(root, "library")); err != nil {
		t.Fatal("deletion removed the library root", err)
	}
}

func TestAlbumDirectoriesRejectTraversalAndSymlinks(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"..", ".", "../outside", "nested/album", "nested\\album", "C:outside", "bad\nname", t.TempDir()} {
		if _, err := Prepare(root, catalog.TelegramLocalFile{FileID: "video.media", Directory: directory}); err == nil {
			t.Fatalf("accepted directory %q", directory)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "library")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid metadata created a library directory", err)
	}
	if err := os.Mkdir(filepath.Join(root, "library"), 0750); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "library", "linked")); err != nil {
		t.Skip(err)
	}
	file := catalog.TelegramLocalFile{FileID: "video.media", Directory: "linked"}
	if _, err := Prepare(root, file); err == nil {
		t.Fatal("album symlink accepted for writing")
	}
	if _, err := Path(root, file); err == nil {
		t.Fatal("album symlink accepted for reading")
	}
	linkedRoot := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(linkedRoot, "library")); err != nil {
		t.Fatal(err)
	}
	file.Directory = "must-not-create"
	if _, err := Prepare(linkedRoot, file); err == nil {
		t.Fatal("library symlink accepted for album creation")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("preparing an album wrote outside managed storage: %v %v", entries, err)
	}
}

func TestLibraryRejectsSymlinksAndTraversal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "library")
	if err := os.Mkdir(root, 0750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "PRIVATE_TOKEN")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked.media")); err != nil {
		t.Skip(err)
	}
	for _, id := range []string{"../PRIVATE_TOKEN", outside, "linked.media", ".", ".."} {
		_, err := Path(filepath.Dir(root), catalog.TelegramLocalFile{FileID: id})
		if err == nil {
			t.Fatalf("accepted %q", id)
		}
		if strings.Contains(err.Error(), "PRIVATE_TOKEN") {
			t.Fatal("path leaked into error")
		}
	}
	linkedRoot := t.TempDir()
	if err := os.Symlink(root, filepath.Join(linkedRoot, "library")); err != nil {
		t.Fatal(err)
	}
	if _, err := Path(linkedRoot, catalog.TelegramLocalFile{FileID: "missing.media"}); err == nil {
		t.Fatal("library symlink accepted")
	}
}
