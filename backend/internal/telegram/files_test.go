package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/mediaimport"
)

func TestMediaGroupDirectoryUsesCaptionOrDateAndSeparatesSenders(t *testing.T) {
	source := catalog.TelegramSource{BotID: 123, SenderID: 42, MediaGroupID: "album/../untrusted", MediaGroupTitle: "海边旅行\n下一行", MediaGroupDate: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC).Unix()}
	directory, key := mediaGroupStorage(source)
	if !strings.HasPrefix(directory, "海边旅行-") || filepath.Base(directory) != directory || key == "" {
		t.Fatalf("invalid album directory: %q", directory)
	}
	source.MediaGroupTitle = ""
	dateDirectory, sameKey := mediaGroupStorage(source)
	if !strings.HasPrefix(dateDirectory, "2030-01-02_03-04-05-") || sameKey != key {
		t.Fatalf("missing caption did not use a readable date: %q", dateDirectory)
	}
	source.SenderID++
	otherDirectory, otherKey := mediaGroupStorage(source)
	if otherDirectory == dateDirectory || otherKey == key {
		t.Fatal("different senders shared an album directory")
	}
	source.BotID++
	if next, _ := mediaGroupStorage(source); next == otherDirectory {
		t.Fatal("different bots shared an album directory")
	}
	source.MediaGroupID = ""
	if directory, key := mediaGroupStorage(source); directory != "" || key != "" {
		t.Fatal("single video acquired an album directory")
	}
}

func TestDownloadWriterAcceptsConcurrentOutOfOrderChunks(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "video-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	const threads, chunkSize = 6, 1024
	var reported int64
	w := &downloadWriter{file: file, limit: threads * chunkSize, progress: func(_ string, done, total int64) error {
		if done < reported || done > total {
			t.Errorf("invalid progress: %d after %d, total %d", done, reported, total)
		}
		reported = done
		return nil
	}}
	var wg sync.WaitGroup
	for i := threads - 1; i >= 0; i-- {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data := bytes.Repeat([]byte{byte(i)}, chunkSize)
			if n, err := w.WriteAt(data, int64(i*chunkSize)); err != nil || n != len(data) {
				t.Errorf("chunk %d: wrote %d bytes: %v", i, n, err)
			}
		}()
	}
	wg.Wait()
	actual, err := os.ReadFile(file.Name())
	if err != nil || len(actual) != threads*chunkSize || w.done != threads*chunkSize || reported == 0 {
		t.Fatalf("invalid output: size=%d written=%d progress=%d error=%v", len(actual), w.done, reported, err)
	}
	for i := range threads {
		if !bytes.Equal(actual[i*chunkSize:(i+1)*chunkSize], bytes.Repeat([]byte{byte(i)}, chunkSize)) {
			t.Fatalf("chunk %d written at the wrong offset", i)
		}
	}
}

func TestDownloadWriterRejectsOutOfBoundsChunks(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "video-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	w := &downloadWriter{file: file, limit: 5, progress: func(string, int64, int64) error {
		t.Error("invalid chunk reported progress")
		return nil
	}}
	for _, offset := range []int64{-1, 4, 6, 1<<63 - 1} {
		if n, err := w.WriteAt([]byte("vi"), offset); err == nil || n != 0 {
			t.Fatalf("accepted invalid offset %d: wrote %d, error %v", offset, n, err)
		}
	}
	if info, err := file.Stat(); err != nil || info.Size() != 0 || w.done != 0 {
		t.Fatal("invalid chunks changed the file or progress", err)
	}
}

type shortWriterAt struct{}

func (shortWriterAt) WriteAt(data []byte, _ int64) (int, error) { return len(data) - 1, nil }

func TestDownloadWriterPropagatesWriteAndProgressErrors(t *testing.T) {
	w := &downloadWriter{file: shortWriterAt{}, limit: 5}
	if n, err := w.WriteAt([]byte("video"), 0); !errors.Is(err, io.ErrShortWrite) || n != 4 || w.done != 4 {
		t.Fatalf("short write: n=%d done=%d error=%v", n, w.done, err)
	}
	file, err := os.CreateTemp(t.TempDir(), "video-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	progressErr := errors.New("progress persistence failed")
	w = &downloadWriter{file: file, limit: 5, progress: func(string, int64, int64) error { return progressErr }}
	if n, err := w.WriteAt([]byte("video"), 0); !errors.Is(err, progressErr) || n != 5 || w.done != 5 {
		t.Fatalf("progress failure: n=%d done=%d error=%v", n, w.done, err)
	}
}

type failingWriterAt struct{ err error }

func (w failingWriterAt) WriteAt([]byte, int64) (int, error) { return 0, w.err }

func TestDownloadFailuresKeepTheirOrigin(t *testing.T) {
	for _, kind := range []string{"disk", "progress", "cancel", "size", "connection", "permission"} {
		t.Run(kind, func(t *testing.T) {
			s, cat := testService(t)
			ctx := context.Background()
			if err := s.accept(ctx, videoUpdate(1, 42)); err != nil {
				t.Fatal(err)
			}
			jobs, _ := cat.ListImportJobs(ctx, "telegram", "", 0, 30)
			s.client = &fakeTransport{download: func(ctx context.Context, _ string, output io.WriterAt) error {
				writer := output.(*downloadWriter)
				switch kind {
				case "disk":
					writer.file = failingWriterAt{&os.PathError{Op: "write", Path: "PRIVATE_PATH", Err: syscall.ENOSPC}}
				case "permission":
					cfg := s.settings()
					cfg.AllowedUserIDs = nil
					s.applySettings(cfg)
				case "connection":
					return telegramError(errors.New("PRIVATE_PROXY_PASSWORD"))
				}
				data := []byte("video")
				if kind == "size" {
					data = []byte("too large")
				}
				_, err := output.WriteAt(data, 0)
				if err == nil {
					t.Fatal("expected download output failure")
				}
				// The SDK wraps errors returned by its writer; transport conversion
				// must retain the local category through that wrapper.
				return telegramError(fmt.Errorf("download: %w", err))
			}}
			_, err := s.Fetch(ctx, jobs[0], func(_ string, done, _ int64) error {
				if done > 0 {
					switch kind {
					case "progress":
						return errors.New("PRIVATE_DATABASE_PATH")
					case "cancel":
						return catalog.ErrRemoteUploadCanceled
					}
				}
				return nil
			})
			if kind == "cancel" {
				if !errors.Is(err, catalog.ErrRemoteUploadCanceled) {
					t.Fatalf("cancellation became a retry: %v", err)
				}
				return
			}
			var source *mediaimport.SourceError
			if !errors.As(err, &source) || strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), "503") {
				t.Fatalf("incorrect or unsafe error: %v", err)
			}
			if source.WaitForAvailability != (kind == "disk" || kind == "progress") || (source.RetryAfter > 0) != (kind == "connection") {
				t.Fatalf("incorrect retry policy: %+v", source)
			}
			want := map[string]string{"disk": "磁盘空间", "progress": "下载进度", "size": "声明的文件大小", "connection": "连接或通信", "permission": "允许用户"}[kind]
			if !strings.Contains(source.Message, want) {
				t.Fatalf("lost failure category %s: %s", kind, source.Message)
			}
		})
	}
}
