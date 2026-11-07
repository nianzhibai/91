package scanner

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
)

type concurrentScanSource struct {
	id      string
	entries []drives.Entry
	started chan<- string
	release <-chan struct{}
}

func (s *concurrentScanSource) ID() string     { return s.id }
func (s *concurrentScanSource) Kind() string   { return "fake" }
func (s *concurrentScanSource) RootID() string { return "root" }
func (s *concurrentScanSource) List(ctx context.Context, _ string) ([]drives.Entry, error) {
	s.started <- s.id
	select {
	case <-s.release:
		return s.entries, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestConcurrentScansAdmitAndDispatchEachSharedVideoOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	const scanCount = 4
	started := make(chan string, scanCount)
	release := make(chan struct{})
	type outcome struct {
		result Result
		err    error
	}
	finished := make(chan outcome, scanCount)
	var callbacks atomic.Int32
	for i := range scanCount {
		source := &concurrentScanSource{
			id: fmt.Sprintf("drive-%d", i), started: started, release: release,
			entries: []drives.Entry{
				{ID: "hash-copy", Name: fmt.Sprintf("renamed-%d.mp4", i), Hash: "shared-hash", Size: 123},
				{ID: "name-copy", Name: "shared-name.mp4", Size: 456},
				{ID: "unique", Name: fmt.Sprintf("unique-%d.mp4", i), Size: 789},
			},
		}
		go func() {
			scan := New(cat, source, []string{".mp4"}, nil, func(*catalog.Video) { callbacks.Add(1) })
			result, err := scan.Scan(ctx, "root")
			finished <- outcome{result, err}
		}()
	}
	// Every provider must be traversed concurrently before any scan can finish.
	for range scanCount {
		select {
		case <-started:
		case <-ctx.Done():
			t.Error("scans did not start concurrently")
		}
	}
	close(release)
	added, duplicates, generated := 0, 0, 0
	for range scanCount {
		got := <-finished
		if got.err != nil || len(got.result.Issues) != 0 {
			t.Errorf("scan error=%v issues=%+v", got.err, got.result.Issues)
		}
		if got.result.Stats.Scanned != 3 {
			t.Errorf("scanned=%d, want 3", got.result.Stats.Scanned)
		}
		added += got.result.Stats.Added
		duplicates += got.result.Duplicates
		generated += len(got.result.NewVideos)
	}
	wantNew := scanCount + 2
	if added != wantNew || generated != wantNew || int(callbacks.Load()) != wantNew || duplicates != 2*(scanCount-1) {
		t.Fatalf("added=%d generation=%d callbacks=%d duplicates=%d, want %d/%d/%d/%d",
			added, generated, callbacks.Load(), duplicates, wantNew, wantNew, wantNew, 2*(scanCount-1))
	}
	stored := 0
	for i := range scanCount {
		videos, err := cat.ListVideosByDrive(ctx, fmt.Sprintf("drive-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		stored += len(videos)
	}
	if stored != wantNew {
		t.Fatalf("stored=%d, want %d physical rows", stored, wantNew)
	}
}

func TestConcurrentScansPreserveTagsWithMetadataWrites(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	const label = "scan-keyword"
	if _, err := cat.EnsureTag(ctx, label, "user"); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpsertVideo(ctx, &catalog.Video{ID: "background", DriveID: "other", FileID: "background", Title: "Background"}); err != nil {
		t.Fatal(err)
	}

	const scanCount, filesPerScan = 6, 20
	started := make(chan string, scanCount)
	release := make(chan struct{})
	type outcome struct {
		result Result
		err    error
	}
	finished := make(chan outcome, scanCount)
	var tasks sync.WaitGroup
	defer func() {
		cancel()
		tasks.Wait()
	}()
	for i := range scanCount {
		source := &concurrentScanSource{id: fmt.Sprintf("drive-%d", i), started: started, release: release}
		for j := range filesPerScan {
			source.entries = append(source.entries, drives.Entry{
				ID: fmt.Sprintf("file-%d", j), Name: fmt.Sprintf("%s-%d-%d.mp4", label, i, j), Size: 123,
			})
		}
		tasks.Add(1)
		go func() {
			defer tasks.Done()
			scan := New(cat, source, []string{".mp4"}, nil, nil)
			result, err := scan.Scan(ctx, "root")
			finished <- outcome{result, err}
		}()
	}
	for range scanCount {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("scans did not start concurrently")
		}
	}
	metadataFinished := make(chan error, 1)
	tasks.Add(1)
	go func() {
		defer tasks.Done()
		<-release
		for i := 1; i <= scanCount*filesPerScan*2; i++ {
			if err := cat.UpdateVideoMeta(ctx, "background", catalog.VideoMetaPatch{DurationSeconds: i}); err != nil {
				metadataFinished <- err
				return
			}
		}
		metadataFinished <- nil
	}()
	close(release)
	for range scanCount {
		got := <-finished
		if got.err != nil || len(got.result.Issues) != 0 {
			t.Errorf("scan error=%v issues=%+v", got.err, got.result.Issues)
		}
		if got.result.Stats.Scanned != filesPerScan || got.result.Stats.Added != filesPerScan {
			t.Errorf("scanned=%d added=%d, want %d of each", got.result.Stats.Scanned, got.result.Stats.Added, filesPerScan)
		}
	}
	if err := <-metadataFinished; err != nil {
		t.Fatalf("background metadata write: %v", err)
	}
	for i := range scanCount {
		videos, err := cat.ListVideosByDrive(ctx, fmt.Sprintf("drive-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if len(videos) != filesPerScan {
			t.Fatalf("drive-%d stored %d videos, want %d", i, len(videos), filesPerScan)
		}
		for _, video := range videos {
			if len(video.Tags) != 1 || video.Tags[0] != label {
				t.Errorf("video %s tags=%v, want [%s]", video.ID, video.Tags, label)
			}
		}
	}
}
