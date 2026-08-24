package crawlerupload

import (
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/drives/scriptcrawler"
	"github.com/video-site/backend/internal/scopedproxy"
)

const crawlerUploadWebPBase64 = "UklGRrIBAABXRUJQVlA4TKUBAAAvSsAYAA8w//M///MfeJAkbXvaSG7m8Q3GfYSBJekwQztm/IcZlgwnmWImn2BK7aFmBtnVir6q//8VOkFE/xm4baTIu8c48ArEo6+B3zFKYln3pqClSCKX0begFTAXFOLXHSyF8cCNcZEG4OywuA4KVVfJCiArU7GAgJI8+lJP/OKMT/fBAjevg1cYB7YVkFuWga2lyPi5I0HFy5YTpWIHg0RZpkniRVW9odHAKOwosWuOGdxIyn2OvaCDvhg/we6TwadPBPbqBV58MsLmMJ8yZnOWk8SRz4N+QoyPL+MnamzMvcE1rHNEr91F9GKZPVUcS9w7PhhH36suB9qPeYb/oLk6cuTiJ0wOK3m5h1cKjW6EVZCYMK7dxcKCBdgP9HkKr9gkAO2P8GKZGWVdIAatQa+1IDpt6qyorVwdy01xdW8Jkfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil9w4L2bvMycmjQcqXaMCO6BlY28i+FOLzbfI1vEqxAhotocAAA=="

type fakeRegistry struct {
	byID     map[string]drives.Drive
	allCalls int
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{byID: make(map[string]drives.Drive)}
}

func (r *fakeRegistry) Add(d drives.Drive) {
	r.byID[d.ID()] = d
}

func (r *fakeRegistry) Get(id string) (drives.Drive, bool) {
	d, ok := r.byID[id]
	return d, ok
}

func (r *fakeRegistry) All() []drives.Drive {
	r.allCalls++
	out := make([]drives.Drive, 0, len(r.byID))
	for _, d := range r.byID {
		out = append(out, d)
	}
	return out
}

type fakeUploadDrive struct {
	id          string
	kind        string
	rootID      string
	mu          sync.Mutex
	uploadCalls int
	gotBodies   map[string][]byte
	gotParents  map[string]string
	ensureCalls []string
	ensureProxy bool
	uploadProxy bool
	listCalls   int
	listEntries []drives.Entry
}

func newFakeUploadDrive(id, kind, rootID string) *fakeUploadDrive {
	return &fakeUploadDrive{
		id:         id,
		kind:       kind,
		rootID:     rootID,
		gotBodies:  make(map[string][]byte),
		gotParents: make(map[string]string),
	}
}

func (d *fakeUploadDrive) Kind() string { return d.kind }
func (d *fakeUploadDrive) ID() string   { return d.id }
func (d *fakeUploadDrive) RootID() string {
	return d.rootID
}
func (d *fakeUploadDrive) Init(context.Context) error { return nil }
func (d *fakeUploadDrive) List(context.Context, string) ([]drives.Entry, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.listCalls++
	return append([]drives.Entry(nil), d.listEntries...), nil
}
func (d *fakeUploadDrive) Stat(context.Context, string) (*drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *fakeUploadDrive) StreamURL(context.Context, string) (*drives.StreamLink, error) {
	return nil, drives.ErrNotSupported
}
func (d *fakeUploadDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *fakeUploadDrive) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ensureCalls = append(d.ensureCalls, pathFromRoot)
	d.ensureProxy = scopedproxy.Configured(ctx)
	return d.rootID + "/" + pathFromRoot, nil
}
func (d *fakeUploadDrive) Rename(context.Context, string, string) error {
	return nil
}
func (d *fakeUploadDrive) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, _ int64) (UploadResult, error) {
	body, _ := io.ReadAll(r)
	d.mu.Lock()
	d.uploadCalls++
	d.gotBodies[name] = body
	d.gotParents[name] = parentID
	d.uploadProxy = scopedproxy.Configured(ctx)
	d.mu.Unlock()
	return UploadResult{FileID: "remote-" + name, Hash: strings.Repeat("a", 40), Size: int64(len(body))}, nil
}

var _ drives.Drive = (*fakeUploadDrive)(nil)
var _ uploadTarget = (*fakeUploadDrive)(nil)

type fakeReconcileDrive struct {
	*fakeUploadDrive
	existing  *UploadResult
	findCalls int
}

func (d *fakeReconcileDrive) FindExisting(_ context.Context, _, _ string, _ int64) (*UploadResult, error) {
	d.findCalls++
	return d.existing, nil
}

type blockingFakeUploadDrive struct {
	*fakeUploadDrive
	started chan struct{}
	release chan struct{}
}

func (d *blockingFakeUploadDrive) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	select {
	case d.started <- struct{}{}:
	default:
	}
	select {
	case <-d.release:
		return d.fakeUploadDrive.UploadAndReportHash(ctx, parentID, name, r, size)
	case <-ctx.Done():
		return UploadResult{}, ctx.Err()
	}
}

func TestRunOnceUploadsScriptCrawlerLocalVideo(t *testing.T) {
	ctx := context.Background()
	cat := setupCatalog(t)
	src := setupScriptCrawler(t, "crawler-one")
	target := newFakeUploadDrive("target-drive", "pikpak", "target-root")
	reg := newFakeRegistry()
	reg.Add(src)
	reg.Add(target)

	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:     src.ID(),
		Kind:   scriptcrawler.Kind,
		Name:   "Example Crawler",
		RootID: "/",
		Credentials: map[string]string{
			"script_path":     "/tmp/example.py",
			"upload_drive_id": target.ID(),
			"upload_proxy":    "http://upload-proxy.example:7890",
		},
		TeaserEnabled: true,
	}); err != nil {
		t.Fatalf("upsert crawler drive: %v", err)
	}

	videoID := writeCrawlerVideo(t, cat, src, "source-001", ".mp4", []byte("video payload"), true)
	webP, err := base64.StdEncoding.DecodeString(crawlerUploadWebPBase64)
	if err != nil {
		t.Fatalf("decode WebP fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src.ThumbsDir(), "source-001.jpg"), webP, 0o644); err != nil {
		t.Fatalf("replace crawler thumbnail with WebP: %v", err)
	}
	commonThumbDir := filepath.Join(t.TempDir(), "thumbs")
	m := New(Config{Catalog: cat, Registry: reg, CommonThumbDir: commonThumbDir})

	if err := m.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}

	wantName := desiredUploadName("Sample source-001", "source-001", "mp4")
	if target.uploadCalls != 1 {
		t.Fatalf("upload calls = %d, want 1", target.uploadCalls)
	}
	if got := string(target.gotBodies[wantName]); got != "video payload" {
		t.Fatalf("uploaded body = %q, want payload", got)
	}
	if got := target.gotParents[wantName]; got != "target-root/Script Crawlers/crawler-one" {
		t.Fatalf("upload parent = %q, want crawler folder", got)
	}
	if len(target.ensureCalls) != 1 || target.ensureCalls[0] != "Script Crawlers/crawler-one" {
		t.Fatalf("ensure calls = %#v, want crawler upload folder", target.ensureCalls)
	}
	if !target.ensureProxy || !target.uploadProxy {
		t.Fatalf("scoped upload proxy ensure/upload = %v/%v, want true/true", target.ensureProxy, target.uploadProxy)
	}
	if scopedproxy.Configured(ctx) {
		t.Fatal("crawler upload proxy leaked into the caller context")
	}

	got, err := cat.GetVideo(ctx, videoID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.DriveID != target.ID() || !strings.HasPrefix(got.FileID, "remote-") {
		t.Fatalf("catalog target = drive %q file %q, want target drive", got.DriveID, got.FileID)
	}
	if got.ParentID != "target-root/Script Crawlers/crawler-one" || got.DirName != "crawler-one" {
		t.Fatalf("catalog directory = parent %q name %q, want crawler destination", got.ParentID, got.DirName)
	}
	if got.FileName != wantName {
		t.Fatalf("file_name = %q, want %q", got.FileName, wantName)
	}
	if got.Title != strings.TrimSuffix(wantName, filepath.Ext(wantName)) {
		t.Fatalf("title = %q, want uploaded filename without extension", got.Title)
	}
	if _, err := os.Stat(filepath.Join(src.VideosDir(), "source-001.mp4")); !os.IsNotExist(err) {
		t.Fatalf("local video still exists or stat failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(src.ThumbsDir(), "source-001.jpg")); !os.IsNotExist(err) {
		t.Fatalf("local thumb still exists or stat failed: %v", err)
	}
	commonThumb, err := os.Open(filepath.Join(commonThumbDir, videoID+".jpg"))
	if err != nil {
		t.Fatalf("common thumbnail missing: %v", err)
	}
	defer commonThumb.Close()
	if _, err := jpeg.Decode(commonThumb); err != nil {
		t.Fatalf("common thumbnail was not normalized to JPEG: %v", err)
	}
}

func TestStartDriveUploadsOnlySelectedCrawlerWhileRunOnceRemainsGlobal(t *testing.T) {
	ctx := context.Background()
	cat := setupCatalog(t)
	first := setupScriptCrawler(t, "crawler-first")
	second := setupScriptCrawler(t, "crawler-second")
	target := newFakeUploadDrive("target-drive", "pikpak", "target-root")
	reg := newFakeRegistry()
	reg.Add(first)
	reg.Add(second)
	reg.Add(target)
	for _, src := range []*scriptcrawler.Driver{first, second} {
		if err := cat.UpsertDrive(ctx, &catalog.Drive{
			ID: src.ID(), Kind: scriptcrawler.Kind, Name: src.ID(), RootID: "/",
			Credentials: map[string]string{
				"script_path":     "/tmp/example.py",
				"proxy":           "http://crawl-only-proxy.example:7890",
				"upload_drive_id": target.ID(),
			},
			TeaserEnabled: true,
		}); err != nil {
			t.Fatalf("upsert crawler %s: %v", src.ID(), err)
		}
	}
	firstVideoID := writeCrawlerVideo(t, cat, first, "first-source", ".mp4", []byte("first payload"), true)
	secondVideoID := writeCrawlerVideo(t, cat, second, "second-source", ".mp4", []byte("second payload"), true)
	m := New(Config{Catalog: cat, Registry: reg})

	done, accepted := m.StartDrive(ctx, first.ID())
	if !accepted {
		t.Fatal("selected crawler migration was not accepted")
	}
	if err := <-done; err != nil {
		t.Fatalf("run selected crawler: %v", err)
	}
	firstVideo, err := cat.GetVideo(ctx, firstVideoID)
	if err != nil {
		t.Fatalf("get first video: %v", err)
	}
	secondVideo, err := cat.GetVideo(ctx, secondVideoID)
	if err != nil {
		t.Fatalf("get second video: %v", err)
	}
	if firstVideo.DriveID != target.ID() {
		t.Fatalf("first video drive = %q, want target", firstVideo.DriveID)
	}
	if secondVideo.DriveID != second.ID() {
		t.Fatalf("second video drive = %q, want untouched crawler", secondVideo.DriveID)
	}
	if target.uploadCalls != 1 {
		t.Fatalf("selected upload calls = %d, want 1", target.uploadCalls)
	}
	if reg.allCalls != 0 {
		t.Fatalf("selected migration enumerated all drives %d time(s)", reg.allCalls)
	}

	if err := m.RunOnce(ctx); err != nil {
		t.Fatalf("run global migration: %v", err)
	}
	secondVideo, err = cat.GetVideo(ctx, secondVideoID)
	if err != nil {
		t.Fatalf("get globally migrated second video: %v", err)
	}
	if secondVideo.DriveID != target.ID() {
		t.Fatalf("second video drive after RunOnce = %q, want target", secondVideo.DriveID)
	}
	if target.uploadCalls != 2 {
		t.Fatalf("global upload calls = %d, want 2 total", target.uploadCalls)
	}
	if reg.allCalls != 1 {
		t.Fatalf("global migration enumerated all drives %d time(s), want 1", reg.allCalls)
	}
	if target.ensureProxy || target.uploadProxy {
		t.Fatalf("crawl-only proxy leaked into upload ensure/upload = %v/%v", target.ensureProxy, target.uploadProxy)
	}
}

func TestStartDriveRejectsBeforeReportingAcceptedWhenAnotherMigrationRuns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cat := setupCatalog(t)
	first := setupScriptCrawler(t, "crawler-first")
	second := setupScriptCrawler(t, "crawler-second")
	target := &blockingFakeUploadDrive{
		fakeUploadDrive: newFakeUploadDrive("target-drive", "pikpak", "target-root"),
		started:         make(chan struct{}, 1),
		release:         make(chan struct{}),
	}
	reg := newFakeRegistry()
	reg.Add(first)
	reg.Add(second)
	reg.Add(target)
	for _, src := range []*scriptcrawler.Driver{first, second} {
		if err := cat.UpsertDrive(ctx, &catalog.Drive{
			ID: src.ID(), Kind: scriptcrawler.Kind, Name: src.ID(), RootID: "/",
			Credentials:   map[string]string{"script_path": "/tmp/example.py", "upload_drive_id": target.ID()},
			TeaserEnabled: true,
		}); err != nil {
			t.Fatalf("upsert crawler %s: %v", src.ID(), err)
		}
		writeCrawlerVideo(t, cat, src, src.ID()+"-source", ".mp4", []byte(src.ID()), true)
	}
	m := New(Config{Catalog: cat, Registry: reg})

	firstDone, accepted := m.StartDrive(ctx, first.ID())
	if !accepted {
		t.Fatal("first migration was not accepted")
	}
	select {
	case <-target.started:
	case <-ctx.Done():
		t.Fatalf("first migration did not reach upload: %v", ctx.Err())
	}
	if secondDone, accepted := m.StartDrive(ctx, second.ID()); accepted || secondDone != nil {
		t.Fatalf("second migration accepted=%v done=%v, want busy rejection", accepted, secondDone)
	}
	close(target.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("finish first migration: %v", err)
	}

	secondDone, accepted := m.StartDrive(ctx, second.ID())
	if !accepted {
		t.Fatal("second migration was not accepted after the slot was released")
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("finish second migration: %v", err)
	}
}

func TestRunOnceRequiresPerCrawlerUploadTarget(t *testing.T) {
	ctx := context.Background()
	cat := setupCatalog(t)
	src := setupScriptCrawler(t, "crawler-local-only")
	target := newFakeUploadDrive("target-drive", "pikpak", "target-root")
	reg := newFakeRegistry()
	reg.Add(src)
	reg.Add(target)

	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:            src.ID(),
		Kind:          scriptcrawler.Kind,
		Name:          "Local Only",
		RootID:        "/",
		Credentials:   map[string]string{"script_path": "/tmp/example.py"},
		TeaserEnabled: true,
	}); err != nil {
		t.Fatalf("upsert crawler drive: %v", err)
	}
	videoID := writeCrawlerVideo(t, cat, src, "source-002", ".mp4", []byte("video payload"), true)

	m := New(Config{Catalog: cat, Registry: reg})
	if err := m.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}
	if target.uploadCalls != 0 {
		t.Fatalf("upload calls = %d, want 0", target.uploadCalls)
	}
	got, err := cat.GetVideo(ctx, videoID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.DriveID != src.ID() {
		t.Fatalf("drive_id = %q, want local crawler drive", got.DriveID)
	}
}

func TestRunOnceIgnoresUnconfiguredScriptCrawler(t *testing.T) {
	ctx := context.Background()
	cat := setupCatalog(t)
	src := setupScriptCrawler(t, "crawler-deleted")
	target := newFakeUploadDrive("target-drive", "pikpak", "target-root")
	reg := newFakeRegistry()
	reg.Add(src)
	reg.Add(target)

	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:     src.ID(),
		Kind:   scriptcrawler.Kind,
		Name:   "Deleted Crawler",
		RootID: "/",
		Credentials: map[string]string{
			"upload_drive_id": target.ID(),
		},
		TeaserEnabled: true,
	}); err != nil {
		t.Fatalf("upsert deleted crawler drive: %v", err)
	}
	videoID := writeCrawlerVideo(t, cat, src, "source-ghost", ".mp4", []byte("video payload"), true)

	m := New(Config{Catalog: cat, Registry: reg})
	if err := m.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}
	if target.uploadCalls != 0 {
		t.Fatalf("upload calls = %d, want deleted crawler ignored", target.uploadCalls)
	}
	got, err := cat.GetVideo(ctx, videoID)
	if err != nil {
		t.Fatalf("get untouched video: %v", err)
	}
	if got.DriveID != src.ID() {
		t.Fatalf("drive_id = %q, want deleted crawler source %q", got.DriveID, src.ID())
	}
	if _, err := os.Stat(filepath.Join(src.VideosDir(), "source-ghost.mp4")); err != nil {
		t.Fatalf("deleted crawler local video changed: %v", err)
	}
}

func TestRunOnceReconcilesRemoteWriteAfterCatalogCrashWithoutReupload(t *testing.T) {
	ctx := context.Background()
	cat := setupCatalog(t)
	src := setupScriptCrawler(t, "crawler-reconcile")
	target := &fakeReconcileDrive{
		fakeUploadDrive: newFakeUploadDrive("target-drive", "quark", "target-root"),
		existing:        &UploadResult{FileID: "existing-remote-fid", Hash: strings.Repeat("c", 40), Size: 13},
	}
	reg := newFakeRegistry()
	reg.Add(src)
	reg.Add(target)
	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID: src.ID(), Kind: scriptcrawler.Kind, Name: "Reconcile", RootID: "/",
		Credentials:   map[string]string{"script_path": "/tmp/example.py", "upload_drive_id": target.ID()},
		TeaserEnabled: true,
	}); err != nil {
		t.Fatalf("upsert crawler drive: %v", err)
	}
	videoID := writeCrawlerVideo(t, cat, src, "source-crash", ".mp4", []byte("video payload"), true)

	m := New(Config{Catalog: cat, Registry: reg})
	if err := m.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}
	if target.findCalls != 1 || target.uploadCalls != 0 {
		t.Fatalf("find calls=%d upload calls=%d, want reconciliation only", target.findCalls, target.uploadCalls)
	}
	got, err := cat.GetVideo(ctx, videoID)
	if err != nil {
		t.Fatalf("get migrated video: %v", err)
	}
	if got.DriveID != target.ID() || got.FileID != "existing-remote-fid" || got.ContentHash != strings.Repeat("c", 40) {
		t.Fatalf("migrated video = drive %q file %q hash %q", got.DriveID, got.FileID, got.ContentHash)
	}
	if got.ParentID != "target-root/Script Crawlers/crawler-reconcile" || got.DirName != "crawler-reconcile" {
		t.Fatalf("reconciled directory = parent %q name %q, want destination directory", got.ParentID, got.DirName)
	}
	if _, err := os.Stat(filepath.Join(src.VideosDir(), "source-crash.mp4")); !os.IsNotExist(err) {
		t.Fatalf("local source was not cleaned after reconciliation: %v", err)
	}
}

func TestDestinationReconciliationCachesOneDirectorySnapshot(t *testing.T) {
	drive := newFakeUploadDrive("target", "quark", "root")
	drive.listEntries = []drives.Entry{{ID: "remote", Name: "movie.mp4", Size: 10}}
	cache := &existingUploadCache{}
	first, err := findExistingDriveUpload(context.Background(), drive, cache, "parent", "movie.mp4", 10)
	if err != nil || first == nil || first.FileID != "remote" {
		t.Fatalf("first lookup = %#v, err=%v", first, err)
	}
	second, err := findExistingDriveUpload(context.Background(), drive, cache, "parent", "missing.mp4", 10)
	if err != nil || second != nil {
		t.Fatalf("second lookup = %#v, err=%v", second, err)
	}
	if drive.listCalls != 1 {
		t.Fatalf("List calls = %d, want one cached directory snapshot", drive.listCalls)
	}
}

func TestRunOncePreservesCrawlerVideoPendingDirectoryRestore(t *testing.T) {
	ctx := context.Background()
	cat := setupCatalog(t)
	src := setupScriptCrawler(t, "crawler-restore")
	target := newFakeUploadDrive("target-drive", "pikpak", "target-root")
	reg := newFakeRegistry()
	reg.Add(src)
	reg.Add(target)

	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:            src.ID(),
		Kind:          scriptcrawler.Kind,
		Name:          "Restore Crawler",
		RootID:        "/",
		Credentials:   map[string]string{"script_path": "/tmp/example.py", "upload_drive_id": target.ID()},
		TeaserEnabled: true,
	}); err != nil {
		t.Fatalf("upsert crawler drive: %v", err)
	}
	videoID := writeCrawlerVideo(t, cat, src, "source-restore", ".mp4", []byte("retained payload"), true)
	if err := cat.DeleteVideoWithTombstone(ctx, videoID); err != nil {
		t.Fatalf("delete video with tombstone: %v", err)
	}
	if err := cat.RemoveDeletedVideo(ctx, videoID); err != nil {
		t.Fatalf("request restore: %v", err)
	}

	m := New(Config{Catalog: cat, Registry: reg})
	if err := m.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}
	if target.uploadCalls != 0 {
		t.Fatalf("upload calls = %d, want pending restore skipped", target.uploadCalls)
	}
	if _, err := os.Stat(filepath.Join(src.VideosDir(), "source-restore.mp4")); err != nil {
		t.Fatalf("pending restore source removed during migration: %v", err)
	}
	if requests, err := cat.ListCrawlerRestoreRequests(ctx, src.ID()); err != nil || len(requests) != 1 {
		t.Fatalf("restore requests = %#v err=%v, want one", requests, err)
	}
}

func TestAdaptUploadTargetRejectsUnsupportedTarget(t *testing.T) {
	src := scriptcrawler.New(scriptcrawler.Config{ID: "crawler", RootDir: t.TempDir()})
	_, err := adaptUploadTarget(src)
	if err == nil || !strings.Contains(err.Error(), "does not support crawler upload") {
		t.Fatalf("err = %v, want unsupported crawler upload target", err)
	}
}

func setupCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "video-site.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	return cat
}

func setupScriptCrawler(t *testing.T, id string) *scriptcrawler.Driver {
	t.Helper()
	d := scriptcrawler.New(scriptcrawler.Config{ID: id, RootDir: t.TempDir()})
	if err := d.Init(context.Background()); err != nil {
		t.Fatalf("scriptcrawler init: %v", err)
	}
	return d
}

func writeCrawlerVideo(t *testing.T, cat *catalog.Catalog, d *scriptcrawler.Driver, sourceID, ext string, content []byte, readyAssets bool) string {
	t.Helper()
	ctx := context.Background()
	fileID := sourceID + ext
	videoPath, err := d.VideoPath(fileID)
	if err != nil {
		t.Fatalf("video path: %v", err)
	}
	if err := os.WriteFile(videoPath, content, 0o644); err != nil {
		t.Fatalf("write video: %v", err)
	}
	thumbPath, err := d.ThumbPath(sourceID + ".jpg")
	if err != nil {
		t.Fatalf("thumb path: %v", err)
	}
	thumb, err := os.Create(thumbPath)
	if err != nil {
		t.Fatalf("create thumb: %v", err)
	}
	frame := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < frame.Bounds().Dy(); y++ {
		for x := 0; x < frame.Bounds().Dx(); x++ {
			frame.SetRGBA(x, y, color.RGBA{R: 80, G: 120, B: 160, A: 255})
		}
	}
	if err := jpeg.Encode(thumb, frame, &jpeg.Options{Quality: 90}); err != nil {
		_ = thumb.Close()
		t.Fatalf("encode thumb: %v", err)
	}
	if err := thumb.Close(); err != nil {
		t.Fatalf("close thumb: %v", err)
	}

	now := time.Now()
	videoID := scriptcrawler.BuildVideoID(d.ID(), sourceID)
	previewStatus := "pending"
	fingerprintStatus := "pending"
	sampled := ""
	if readyAssets {
		previewStatus = "ready"
		fingerprintStatus = "ready"
		sampled = strings.Repeat("b", 64)
	}
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:                videoID,
		DriveID:           d.ID(),
		FileID:            fileID,
		FileName:          fileID,
		Title:             "Sample " + sourceID,
		Author:            "tester",
		Ext:               strings.TrimPrefix(ext, "."),
		Size:              int64(len(content)),
		PreviewStatus:     previewStatus,
		FingerprintStatus: fingerprintStatus,
		SampledSHA256:     sampled,
		PublishedAt:       now,
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatalf("upsert video: %v", err)
	}
	return videoID
}
