package applog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T, dir string, maxBytes int64, retentionDays int) *Store {
	t.Helper()
	store, err := Open(StoreConfig{
		Directory:         dir,
		MaxTotalSizeBytes: maxBytes,
		RetentionDays:     retentionDays,
		LevelFunc:         func() Level { return LevelInfo },
	})
	if err != nil {
		t.Fatalf("open log store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func appendTestRecord(t *testing.T, store *Store, rec Record) int64 {
	t.Helper()
	id, err := store.Append(context.Background(), rec)
	if err != nil {
		t.Fatalf("append log record: %v", err)
	}
	return id
}

func TestStorePersistsStructuredRecordsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	store := openTestStore(t, dir, 4<<20, 14)

	id1 := appendTestRecord(t, store, Record{
		Source:    SourceApp,
		Level:     LevelInfo,
		Component: "scan",
		DriveID:   "drive-1",
		Message:   "scan ready",
		Attrs:     map[string]any{"scanned": 5},
	})
	id2 := appendTestRecord(t, store, Record{
		Source: SourceHTTP,
		Level:  LevelInfo,
		HTTP: &HTTPRecord{
			Method:     MethodPOST,
			Path:       "/api/videos",
			Status:     201,
			Bytes:      42,
			DurationMs: 2.5,
			Remote:     "127.0.0.1",
		},
	})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openTestStore(t, dir, 4<<20, 14)
	snap, err := reopened.Query(context.Background(), Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Entries) != 2 || snap.Entries[0].ID != id1 || snap.Entries[1].ID != id2 {
		t.Fatalf("reopened entries = %+v", snap.Entries)
	}
	id3 := appendTestRecord(t, reopened, Record{Level: LevelInfo, Message: "after restart"})
	if id3 != id2+1 {
		t.Fatalf("id after restart = %d, want %d", id3, id2+1)
	}
}

func TestStoreMultiFieldQueriesAndFTS5Search(t *testing.T) {
	store := openTestStore(t, t.TempDir(), 8<<20, 0)
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	appendTestRecord(t, store, Record{
		Timestamp: base,
		Source:    SourceApp,
		Level:     LevelInfo,
		Component: "scan",
		DriveID:   "drive-a",
		TaskID:    "task-1",
		Message:   "Scan started for 元胞自动机",
	})
	appendTestRecord(t, store, Record{
		Timestamp: base.Add(time.Minute),
		Source:    SourceApp,
		Level:     LevelError,
		Component: "preview",
		DriveID:   "drive-b",
		VideoID:   "vid-99",
		Error:     "ffmpeg timeout exceeded",
		Message:   "Preview generation failed",
	})
	appendTestRecord(t, store, Record{
		Timestamp: base.Add(2 * time.Minute),
		Source:    SourceHTTP,
		Level:     LevelWarn,
		RequestID: "req-404",
		HTTP: &HTTPRecord{
			Method:     MethodGET,
			Path:       "/api/video/vid-99",
			Status:     404,
			Bytes:      20,
			DurationMs: 1250.5,
			Remote:     "10.0.0.2",
		},
	})
	appendTestRecord(t, store, Record{
		Timestamp: base.Add(3 * time.Minute),
		Source:    SourceHTTP,
		Level:     LevelError,
		RequestID: "req-500",
		HTTP: &HTTPRecord{
			Method:     MethodPOST,
			Path:       "/admin/api/drives",
			Status:     502,
			Bytes:      60,
			DurationMs: 55.2,
			Remote:     "10.0.0.3",
		},
	})

	// Filter by multiple levels.
	snap, err := store.Query(context.Background(), Query{Levels: []Level{LevelWarn, LevelError}})
	if err != nil || len(snap.Entries) != 3 {
		t.Fatalf("warn+error = %+v, err=%v", snap, err)
	}
	// Filter by component + driveId.
	snap, err = store.Query(context.Background(), Query{Component: "preview", DriveID: "drive-b"})
	if err != nil || len(snap.Entries) != 1 || snap.Entries[0].VideoID != "vid-99" {
		t.Fatalf("component+drive = %+v, err=%v", snap, err)
	}
	// Filter by status range (4xx) + minDurationMs.
	snap, err = store.Query(context.Background(), Query{StatusMin: 400, StatusMax: 499, MinDurationMs: 1000})
	if err != nil || len(snap.Entries) != 1 || snap.Entries[0].RequestID != "req-404" {
		t.Fatalf("status+duration = %+v, err=%v", snap, err)
	}
	// FTS5 trigram substring search (Unicode & ASCII).
	for _, q := range []string{"元胞自", "timeout", "vid-99", "404", "go"} {
		snap, err = store.Query(context.Background(), Query{Search: q})
		if err != nil {
			t.Fatalf("search %q failed: %v", q, err)
		}
		if q != "go" && len(snap.Entries) == 0 {
			t.Fatalf("search %q returned no matches", q)
		}
	}
	// Short (<3 rune) LIKE fallback.
	snap, err = store.Query(context.Background(), Query{Search: "元胞"})
	if err != nil || len(snap.Entries) != 1 {
		t.Fatalf("short search = %+v, err=%v", snap, err)
	}
	// Time range filter.
	snap, err = store.Query(context.Background(), Query{From: base.Add(30 * time.Second), To: base.Add(150 * time.Second)})
	if err != nil || len(snap.Entries) != 2 {
		t.Fatalf("time range = %+v, err=%v", snap, err)
	}
}

func TestStorePaginationBeforeAndAfter(t *testing.T) {
	store := openTestStore(t, t.TempDir(), 8<<20, 0)
	for i := 1; i <= 25; i++ {
		appendTestRecord(t, store, Record{Level: LevelInfo, Message: fmt.Sprintf("msg-%02d", i)})
	}
	// Backward pagination via Before.
	var before int64
	seen := make(map[int64]bool)
	for {
		page, err := store.Query(context.Background(), Query{Limit: 10, Before: before})
		if err != nil {
			t.Fatal(err)
		}
		for i, e := range page.Entries {
			if seen[e.ID] {
				t.Fatalf("duplicate id %d", e.ID)
			}
			if i > 0 && e.ID <= page.Entries[i-1].ID {
				t.Fatalf("entries not chronological: %+v", page.Entries)
			}
			seen[e.ID] = true
		}
		if !page.HasMore {
			break
		}
		before = page.NextBefore
	}
	if len(seen) != 25 {
		t.Fatalf("paged total = %d, want 25", len(seen))
	}
	// Forward catch-up via After.
	page, err := store.Query(context.Background(), Query{Limit: 10, After: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 5 || page.Entries[0].ID != 21 || page.Entries[4].ID != 25 || page.HasMore {
		t.Fatalf("after page = %+v", page)
	}
}

func TestStoreClearPruneAndMeta(t *testing.T) {
	store := openTestStore(t, t.TempDir(), 4<<20, 7)
	now := time.Now().UTC()
	appendTestRecord(t, store, Record{Timestamp: now.Add(-10 * 24 * time.Hour), Component: "old", Level: LevelInfo, Message: "expired"})
	appendTestRecord(t, store, Record{Timestamp: now, Component: "scan", Level: LevelInfo, Message: "fresh"})

	if err := store.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Query(context.Background(), Query{Limit: 10})
	if err != nil || len(snap.Entries) != 1 || snap.Entries[0].Message != "fresh" {
		t.Fatalf("after retention prune = %+v, err=%v", snap, err)
	}

	meta, err := store.Meta(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if meta.TotalCount != 1 || meta.StorageBytes <= 0 || meta.MaxStorageBytes != 4<<20 ||
		meta.RetentionDays != 7 || meta.Level != LevelInfo || len(meta.Components) != 1 || meta.Components[0] != "scan" {
		t.Fatalf("meta = %+v", meta)
	}

	if err := store.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap, err = store.Query(context.Background(), Query{Limit: 10})
	if err != nil || len(snap.Entries) != 0 {
		t.Fatalf("after clear = %+v, err=%v", snap, err)
	}
}

func TestStoreEnforcesMaxSizeBudget(t *testing.T) {
	const budget = 160 * 1024 // 160 KiB
	store := openTestStore(t, t.TempDir(), budget, 0)
	payload := strings.Repeat("diagnostic-payload-", 40)
	for i := 0; i < 600; i++ {
		store.Submit(Record{Level: LevelInfo, Component: "stress", Message: fmt.Sprintf("entry-%04d %s", i, payload)})
	}
	if err := store.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	meta, err := store.Meta(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if meta.TotalCount >= 600 {
		t.Fatalf("expected pruning to remove oldest rows, total=%d bytes=%d", meta.TotalCount, meta.StorageBytes)
	}
}

func TestStoreRecoversFromCorruptDatabaseFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, dbFileName)
	if err := os.WriteFile(dbPath, []byte("not a valid sqlite database file"), 0o640); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, dir, 4<<20, 0)
	appendTestRecord(t, store, Record{Level: LevelInfo, Message: "recovered"})
	snap, err := store.Query(context.Background(), Query{Limit: 10})
	if err != nil || len(snap.Entries) != 1 || snap.Entries[0].Message != "recovered" {
		t.Fatalf("recovered store = %+v, err=%v", snap, err)
	}
	entries, _ := os.ReadDir(dir)
	foundBackup := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), dbFileName+".corrupt-") {
			foundBackup = true
		}
	}
	if !foundBackup {
		t.Fatal("expected corrupt db backup file to be preserved")
	}
}

func TestStoreExportTextAndJSONL(t *testing.T) {
	store := openTestStore(t, t.TempDir(), 4<<20, 0)
	appendTestRecord(t, store, Record{
		Source:    SourceApp,
		Level:     LevelError,
		Component: "preview",
		DriveID:   "drive-1",
		Message:   "failed job",
		Error:     "boom",
	})
	var textBuf bytes.Buffer
	if err := store.Export(context.Background(), Query{Levels: []Level{LevelError}}, "text", &textBuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(textBuf.String(), "[preview]") || !strings.Contains(textBuf.String(), "driveId=drive-1") || !strings.Contains(textBuf.String(), "error=boom") {
		t.Fatalf("text export = %q", textBuf.String())
	}

	var jsonlBuf bytes.Buffer
	if err := store.Export(context.Background(), Query{}, "jsonl", &jsonlBuf); err != nil {
		t.Fatal(err)
	}
	var decoded Record
	if err := json.Unmarshal(bytes.TrimSpace(jsonlBuf.Bytes()), &decoded); err != nil {
		t.Fatalf("jsonl export invalid: %v (%s)", err, jsonlBuf.String())
	}
	if decoded.Component != "preview" || decoded.Error != "boom" {
		t.Fatalf("decoded jsonl = %+v", decoded)
	}
}

func TestStoreSubscribeDeliversMatchingLiveRecords(t *testing.T) {
	store := openTestStore(t, t.TempDir(), 4<<20, 0)
	ch, cancel := store.Subscribe(Query{Levels: []Level{LevelError}, Component: "scan"})
	defer cancel()

	store.Submit(Record{Level: LevelInfo, Component: "scan", Message: "ignored info"})
	store.Submit(Record{Level: LevelError, Component: "preview", Message: "ignored component"})
	expectedID := store.Submit(Record{Level: LevelError, Component: "scan", Message: "matched error"})

	select {
	case got := <-ch:
		if got.ID != expectedID || got.Message != "matched error" {
			t.Fatalf("live event = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live record")
	}
}

func TestStoreConcurrentWritesAndQueries(t *testing.T) {
	store := openTestStore(t, t.TempDir(), 16<<20, 0)
	var wg sync.WaitGroup
	for worker := 0; worker < 6; worker++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				store.Submit(Record{Level: LevelInfo, Component: "worker", Message: fmt.Sprintf("worker-%d line-%d", w, i)})
				if i%10 == 0 {
					_, _ = store.Query(context.Background(), Query{Limit: 20, Search: "line"})
				}
			}
		}(worker)
	}
	wg.Wait()
	snap, err := store.Query(context.Background(), Query{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Entries) != 300 {
		t.Fatalf("entries = %d, want 300", len(snap.Entries))
	}
}
