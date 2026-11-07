package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/nianzhibai/91/backend/internal/auth"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/driveevents"
	"github.com/nianzhibai/91/backend/internal/driveview"
)

func newDriveSnapshotServer(t *testing.T) (*AdminServer, http.Handler) {
	t.Helper()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	for _, id := range []string{"a", "b"} {
		if err := cat.UpsertDrive(context.Background(), &catalog.Drive{ID: id, Kind: "quark", Name: id, Credentials: map[string]string{"cookie": "secret-cookie"}}); err != nil {
			t.Fatal(err)
		}
	}
	server := &AdminServer{Catalog: cat, Auth: &auth.Authenticator{Catalog: cat}, LocalPreviewDir: t.TempDir()}
	userID, err := cat.CreateUser(context.Background(), "admin", "unused-hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.CreateSession(context.Background(), "admin-token", time.Hour, userID); err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	server.Register(router)
	return server, router
}

func getDriveSnapshotForTest(t *testing.T, router http.Handler, path string) driveview.Snapshot {
	t.Helper()
	req := httptest.NewRequest("GET", "/admin/api/drives/"+path, nil)
	req.AddCookie(&http.Cookie{Name: "vs_admin", Value: "admin-token"})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != 200 {
		t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "secret-cookie") {
		t.Fatal("snapshot exposed credentials")
	}
	var snapshot driveview.Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision == 0 || snapshot.Epoch == "" {
		t.Fatalf("missing version: %+v", snapshot)
	}
	return snapshot
}

func TestDriveSnapshotsAreScopedAndInvalidateAfterCommittedWrites(t *testing.T) {
	server, router := newDriveSnapshotServer(t)
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"a", "b"} {
		if err := server.Catalog.UpsertVideo(ctx, &catalog.Video{ID: id, DriveID: id, FileID: id, FileName: id + ".mp4", Title: id, Size: 10, PreviewStatus: "ready", PreviewLocal: id + ".mp4", CreatedAt: now, PublishedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(server.LocalPreviewDir, id+".mp4"), []byte("1234567"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	config := getDriveSnapshotForTest(t, router, "a")
	stats := getDriveSnapshotForTest(t, router, "a/stats")
	var counts driveStatsDTO
	json.Unmarshal(stats.Data, &counts)
	if counts.TeaserReadyCount != 1 {
		t.Fatalf("included other drive's video: %+v", counts)
	}
	storage := getDriveSnapshotForTest(t, router, "a/storage")
	var usage struct {
		TotalBytes int64 `json:"totalBytes"`
	}
	json.Unmarshal(storage.Data, &usage)
	if usage.TotalBytes != 7 {
		t.Fatalf("included other drive's storage: %+v", usage)
	}
	if err := server.Catalog.SetDriveSkipDirIDs(ctx, "a", []string{"hidden"}); err != nil {
		t.Fatal(err)
	}
	updated := getDriveSnapshotForTest(t, router, "a")
	if updated.Revision <= config.Revision || !strings.Contains(string(updated.Data), "hidden") {
		t.Fatal("configuration remained stale")
	}
	if err := server.Catalog.DeleteVideo(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	updated = getDriveSnapshotForTest(t, router, "a/stats")
	json.Unmarshal(updated.Data, &counts)
	if counts.TeaserReadyCount != 0 || updated.Revision <= stats.Revision {
		t.Fatal("deleted video remained in counts")
	}
}

func TestDriveStorageFailureDoesNotBlockRuntimeSnapshot(t *testing.T) {
	server, router := newDriveSnapshotServer(t)
	server.LocalPreviewDir = ""
	runtime := getDriveSnapshotForTest(t, router, "a/runtime")
	if runtime.Resource != driveview.Runtime {
		t.Fatal("missing runtime")
	}
	req := httptest.NewRequest("GET", "/admin/api/drives/a/storage", nil)
	req.AddCookie(&http.Cookie{Name: "vs_admin", Value: "admin-token"})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != 500 {
		t.Fatalf("storage response: %d", response.Code)
	}
	getDriveSnapshotForTest(t, router, "a/runtime")
}

func TestDriveDirectoryRevisionChangesOnlyAfterRuntimeApplication(t *testing.T) {
	server, router := newDriveSnapshotServer(t)
	read := func(id string) (driveview.Snapshot, driveConfigDTO) {
		t.Helper()
		snapshot := getDriveSnapshotForTest(t, router, id)
		var config driveConfigDTO
		if err := json.Unmarshal(snapshot.Data, &config); err != nil {
			t.Fatal(err)
		}
		return snapshot, config
	}
	_, initial := read("a")
	_, other := read("b")
	drive, err := server.Catalog.GetDrive(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	drive.Credentials["cookie"] = "replacement-cookie"
	if err := server.Catalog.UpsertDrive(context.Background(), drive); err != nil {
		t.Fatal(err)
	}
	if err := server.Catalog.SetDriveSkipDirIDs(context.Background(), "a", []string{"hidden"}); err != nil {
		t.Fatal(err)
	}
	pendingSnapshot, pending := read("a")
	if pending.DirectoryRevision != initial.DirectoryRevision {
		t.Fatal("persisting desired credentials or skip directories invalidated directory reads")
	}
	server.Catalog.DriveEvents().Notify("a", true, driveevents.DriveRuntimeChanged)
	activeSnapshot, active := read("a")
	if active.DirectoryRevision <= pending.DirectoryRevision || activeSnapshot.Revision <= pendingSnapshot.Revision {
		t.Fatal("completed runtime transition did not invalidate the cached config snapshot")
	}
	if len(active.SkipDirIDs) != 1 || active.SkipDirIDs[0] != "hidden" {
		t.Fatal("directory invalidation changed skip-directory selections")
	}
	_, unchanged := read("b")
	if unchanged.DirectoryRevision != other.DirectoryRevision {
		t.Fatal("runtime transition invalidated another drive's directories")
	}
}

func TestDriveSnapshotRoutesRequireAdministratorAndMissingDrivesReturn404(t *testing.T) {
	_, router := newDriveSnapshotServer(t)
	for _, suffix := range []string{"", "/runtime", "/stats", "/storage", "/events"} {
		req := httptest.NewRequest("GET", "/admin/api/drives/a"+suffix, nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != 401 {
			t.Fatalf("unprotected %s: %d", suffix, response.Code)
		}
		req = httptest.NewRequest("GET", "/admin/api/drives/missing"+suffix, nil)
		req.AddCookie(&http.Cookie{Name: "vs_admin", Value: "admin-token"})
		response = httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != 404 {
			t.Fatalf("missing %s: %d %s", suffix, response.Code, response.Body.String())
		}
	}
}

func TestDriveEventStreamPublishesChangesAndReconnectsToLatestState(t *testing.T) {
	server, router := newDriveSnapshotServer(t)
	var scanned atomic.Int32
	server.GetDriveGenerationStatus = func(id string) DriveGenerationStatuses {
		return DriveGenerationStatuses{Scan: GenerationStatus{State: "scanning", ScannedCount: int(scanned.Load())}}
	}
	httpServer := httptest.NewServer(router)
	defer httpServer.Close()
	connect := func() (*http.Response, context.CancelFunc) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", httpServer.URL+"/admin/api/drives/a/events", nil)
		req.AddCookie(&http.Cookie{Name: "vs_admin", Value: "admin-token"})
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("X-Accel-Buffering") != "no" {
			t.Fatal("stream headers missing")
		}
		return response, cancel
	}
	readRuntime := func(reader *bufio.Reader) driveview.Snapshot {
		return readDriveEventSnapshot(t, reader, driveview.Runtime)
	}
	response, cancel := connect()
	reader := bufio.NewReader(response.Body)
	initial := readRuntime(reader)
	scanned.Store(17)
	server.Catalog.DriveEvents().Notify("a", true, driveevents.ActivityChanged)
	updated := readRuntime(reader)
	if updated.Revision <= initial.Revision || !strings.Contains(string(updated.Data), `"scannedCount":17`) {
		t.Fatalf("stale stream: %+v", updated)
	}
	response.Body.Close()
	cancel()
	response, cancel = connect()
	defer cancel()
	defer response.Body.Close()
	reconnected := readRuntime(bufio.NewReader(response.Body))
	if reconnected.Revision != updated.Revision {
		t.Fatalf("reconnect lost current state: %+v", reconnected)
	}
}

func TestDriveEventStreamConfirmsExistenceThroughConfig(t *testing.T) {
	server, router := newDriveSnapshotServer(t)
	var staleRuntime atomic.Bool
	staleRuntime.Store(true)
	server.driveSnapshotsOnce.Do(func() {
		server.driveSnapshots = driveview.New(server.Catalog.DriveEvents(), func(ctx context.Context, id string, resource driveview.Resource) (any, error) {
			if resource == driveview.Runtime && staleRuntime.Load() {
				return nil, &driveview.LoadError{Status: 404, Err: errors.New("old drive missing")}
			}
			return server.loadDriveResource(ctx, id, resource)
		})
	})
	httpServer := httptest.NewServer(router)
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", httpServer.URL+"/admin/api/drives/a/events", nil)
	req.AddCookie(&http.Cookie{Name: "vs_admin", Value: "admin-token"})
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	missing := readDriveEventSnapshot(t, reader, driveview.Runtime)
	if missing.Status != 404 {
		t.Fatalf("missing stale error: %+v", missing)
	}
	staleRuntime.Store(false)
	server.Catalog.DriveEvents().Notify("a", true, driveevents.ActivityChanged)
	current := readDriveEventSnapshot(t, reader, driveview.Runtime)
	if current.Error != "" || current.Revision <= missing.Revision {
		t.Fatalf("resource 404 stopped the live drive's stream: %+v", current)
	}
}

func readDriveEventSnapshot(t *testing.T, reader *bufio.Reader, resource driveview.Resource) driveview.Snapshot {
	t.Helper()
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var snapshot driveview.Snapshot
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data: "))), &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.Resource == resource {
			return snapshot
		}
	}
}
