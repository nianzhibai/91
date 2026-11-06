package telegram

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives/telegramstorage"
	"github.com/video-site/backend/internal/mediaimport"
)

func TestDirectTelegramImportValidatesPublishesAndDeduplicates(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	for _, album := range []bool{false, true} {
		t.Run(map[bool]string{false: "video", true: "album"}[album], func(t *testing.T) {
			s, cat := testService(t)
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "fixture.mp4")
			cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=blue:s=32x32:d=0.2", "-c:v", "mpeg4", "-y", path)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("ffmpeg: %v %s", err, output)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			s.client = &fakeTransport{download: func(ctx context.Context, id string, w io.WriterAt) error { _, err := w.WriteAt(data, 0); return err }}
			saved := make(chan *catalog.Video, 2)
			manager, err := mediaimport.New(mediaimport.Config{Catalog: cat, TelegramSource: s, UploadDir: s.uploadDir, DiskReserve: 1, OnVideoUploaded: func(v *catalog.Video) { saved <- v }})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := manager.Shutdown(ctx); err != nil {
					t.Fatal(err)
				}
			}()
			s.SetWake(manager.Wake)
			u := videoUpdate(10, 42)
			u.Message.Video.Size = int64(len(data))
			if album {
				u.Message.MediaGroupID = "album"
				u.Message.Caption = ""
				u.Message.Video.Name = ""
			}
			if err := s.accept(ctx, u); err != nil {
				t.Fatal(err)
			}
			if album {
				photo := albumPhoto(9, "共同标题")
				if err := s.accept(ctx, photo); err != nil {
					t.Fatal(err)
				}
				second := albumVideo(11, "")
				second.Message.Video.Size = int64(len(data))
				if err := s.accept(ctx, second); err != nil {
					t.Fatal(err)
				}
				flushTestAlbum(t, s)
			}
			var video *catalog.Video
			select {
			case video = <-saved:
			case <-time.After(5 * time.Second):
				t.Fatal("video not imported")
			}
			if video.DriveID != telegramstorage.DriveID || video.PreviewStatus != "pending" {
				t.Fatalf("wrong video: %+v", video)
			}
			if album && video.Title != "共同标题 - 1" {
				t.Fatalf("album title: %s", video.Title)
			}
			stored, err := telegramstorage.New(cat, func() string { return s.cfg.LocalFilesRoot }).LocalPath(ctx, video.FileID)
			if err != nil {
				t.Fatal(err)
			}
			parent := filepath.Dir(stored)
			if album {
				if filepath.Dir(parent) != filepath.Join(s.cfg.LocalFilesRoot, "library") || !strings.HasPrefix(filepath.Base(parent), "共同标题-") {
					t.Fatalf("album was not saved in its named directory: %s", stored)
				}
				if video.ParentID != filepath.Base(parent) || video.DirName != filepath.Base(parent) {
					t.Fatalf("album directory was not recorded for collections: %+v", video)
				}
				select {
				case second := <-saved:
					if second.ParentID != video.ParentID || second.DirName != video.DirName {
						t.Fatal("same-directory videos were placed in different collections")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("second album video not imported")
				}
				collection, err := cat.ListVisibleVideosByDirectory(ctx, video.DriveID, video.ParentID)
				if err != nil || len(collection) != 2 {
					t.Fatalf("album collection has %d videos: %v", len(collection), err)
				}
			} else if parent != filepath.Join(s.cfg.LocalFilesRoot, "library") {
				t.Fatalf("single video moved into an album directory: %s", stored)
			} else if video.ParentID != catalog.TelegramLibraryRootID || video.DirName != "library" {
				t.Fatalf("library root was omitted from directory collections: %+v", video)
			}
			actual, err := os.ReadFile(stored)
			if err != nil || string(actual) != string(data) {
				t.Fatal("stored content differs", err)
			}
			if entries, err := os.ReadDir(s.uploadDir); err != nil || len(entries) != 0 {
				t.Fatal("copied file into uploads")
			}
			u.ID += 10
			u.Message.ID += 10
			if err := s.accept(ctx, u); err != nil {
				t.Fatal(err)
			}
			if album {
				flushTestAlbum(t, s)
			}
			jobs, err := cat.ListImportJobs(ctx, "telegram", "", 0, 30)
			wantJobs := 1
			if album {
				wantJobs = 2
			}
			if err != nil || len(jobs) != wantJobs || jobs[0].State != catalog.RemoteUploadCompleted {
				t.Fatalf("duplicate import: %v %v", jobs, err)
			}
			receipts, err := cat.PendingTelegramReceipts(ctx, 123)
			if err != nil {
				t.Fatal(err)
			}
			foundOriginal, foundDuplicate := false, false
			for _, r := range receipts {
				duplicate := r.MessageID == u.Message.ID
				if duplicate {
					foundDuplicate = true
					if len(r.DuplicateJobIDs) != 1 {
						t.Fatalf("saved video was not marked duplicate: %+v", r)
					}
				} else {
					foundOriginal = true
					if len(r.DuplicateJobIDs) != 0 {
						t.Fatalf("new import became a duplicate after completion: %+v", r)
					}
				}
				s.notifyOne(ctx, &fakeTransport{send: func(ctx context.Context, receipt catalog.TelegramReceipt, message botMessage) (int64, error) {
					if strings.Contains(message.text, "重复") != duplicate {
						t.Fatalf("incorrect duplicate notification: %+v %s", receipt, message.text)
					}
					return 55, nil
				}}, r)
			}
			if !foundOriginal || !foundDuplicate {
				t.Fatalf("missing original or duplicate notification: %+v", receipts)
			}
		})
	}
}
