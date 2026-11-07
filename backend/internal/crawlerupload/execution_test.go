package crawlerupload

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/drives/scriptcrawler"
	"github.com/nianzhibai/91/backend/internal/uploadjob"
)

func uploadTestSetup(t *testing.T) (*catalog.Catalog, *scriptcrawler.Driver, *fakeUploadDrive, *Migrator) {
	t.Helper()
	cat := setupCatalog(t)
	src := setupScriptCrawler(t, "crawler-results")
	target := newFakeUploadDrive("target", "onedrive", "root")
	reg := newFakeRegistry()
	reg.Add(src)
	reg.Add(target)
	if err := cat.UpsertDrive(context.Background(), &catalog.Drive{ID: src.ID(), Kind: scriptcrawler.Kind, RootID: "/",
		Credentials: map[string]string{"script_path": "/tmp/example.py", "upload_drive_id": target.ID()}}); err != nil {
		t.Fatal(err)
	}
	return cat, src, target, New(Config{Catalog: cat, Registry: reg})
}

func TestRunDriveWaitsForUploadSlotAndHonorsHardCancellation(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprint(stop), func(t *testing.T) {
			cat, src, target, m := uploadTestSetup(t)
			writeCrawlerVideo(t, cat, src, "waiting", ".mp4", []byte("payload"), true)
			if !m.tryBeginRun() {
				t.Fatal("could not reserve upload slot")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- m.RunDrive(ctx, src.ID()) }()
			select {
			case err := <-done:
				t.Fatalf("completion rejected a busy upload slot: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			if stop {
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("waiting upload did not cancel: %v", err)
				}
				if !m.running || target.uploadCalls != 0 {
					t.Fatal("canceled waiter released another run's slot or uploaded")
				}
				m.finishRun()
			} else {
				m.finishRun()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if target.uploadCalls != 1 {
					t.Fatalf("uploads=%d", target.uploadCalls)
				}
			}
			if !m.tryBeginRun() {
				t.Fatal("migration slot leaked")
			}
			m.finishRun()
		})
	}
}

func TestUploadIdleStatusExposesCurrentOutcome(t *testing.T) {
	for _, tc := range []struct {
		name           string
		assetsReady    bool
		missingFile    bool
		cancelOnUpload bool
		wantState      string
		wantError      bool
	}{
		{name: "success", assetsReady: true, wantState: "succeeded"},
		{name: "blocked", wantState: "blocked"},
		{name: "failure", assetsReady: true, missingFile: true, wantState: "failed", wantError: true},
		{name: "cancellation", assetsReady: true, cancelOnUpload: true, wantState: "canceled", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat, src, _, m := uploadTestSetup(t)
			writeCrawlerVideo(t, cat, src, "outcome", ".mp4", []byte("payload"), tc.assetsReady)
			if tc.missingFile {
				if err := os.Remove(filepath.Join(src.VideosDir(), "outcome.mp4")); err != nil {
					t.Fatal(err)
				}
			}
			previous := uploadjob.Result{TaskID: "previous-task", DriveID: src.ID(), State: "failed"}
			if err := cat.SaveCrawlerUploadResult(context.Background(), previous); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			idleCount := 0
			m.cfg.OnUploadProgress = func(progress UploadProgress) {
				if tc.cancelOnUpload && progress.State == "uploading" {
					cancel()
				}
				if progress.State != "idle" {
					return
				}
				idleCount++
				// A polling client may stop refreshing as soon as it observes idle.
				// Its result read must already return this run's persisted outcome.
				results, err := cat.LatestCrawlerUploadResults(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				result := results[src.ID()]
				if result.TaskID == "" || result.TaskID == previous.TaskID || result.State != tc.wantState || result.FinishedAt.IsZero() {
					t.Errorf("outcome at idle = %+v, want current %s outcome", result, tc.wantState)
				}
			}
			if err := m.RunDrives(ctx, []string{src.ID()}); (err != nil) != tc.wantError {
				t.Fatalf("run error = %v, wantError = %v", err, tc.wantError)
			}
			if idleCount != 1 {
				t.Fatalf("idle notifications = %d, want 1", idleCount)
			}
		})
	}
}

type uncertainUploadDrive struct {
	*fakeUploadDrive
	existingUploadCache
	storeBeforeFailure bool
}

func (d *uncertainUploadDrive) FindExisting(ctx context.Context, parent, name string, size int64) (*UploadResult, error) {
	return findExistingDriveUpload(ctx, d, &d.existingUploadCache, parent, name, size)
}

func (d *uncertainUploadDrive) UploadAndReportHash(ctx context.Context, parent, name string, body io.Reader, size int64) (UploadResult, error) {
	result, err := d.fakeUploadDrive.UploadAndReportHash(ctx, parent, name, body, size)
	if err != nil {
		return result, err
	}
	if d.storeBeforeFailure {
		d.listEntries = []drives.Entry{{ID: result.FileID, Name: name, Size: result.Size, Hash: result.Hash}}
	}
	return UploadResult{}, io.EOF
}

func TestUploadTransportRetryRefreshesDestinationAndIsBounded(t *testing.T) {
	for _, stored := range []bool{true, false} {
		t.Run(fmt.Sprint(stored), func(t *testing.T) {
			cat, src, target, m := uploadTestSetup(t)
			writeCrawlerVideo(t, cat, src, "uncertain", ".mp4", []byte("payload"), true)
			uncertain := &uncertainUploadDrive{fakeUploadDrive: target, storeBeforeFailure: stored}
			m.cfg.Registry.(*fakeRegistry).Add(uncertain)
			err := m.RunOnce(context.Background())
			results, readErr := cat.LatestCrawlerUploadResults(context.Background())
			if readErr != nil {
				t.Fatal(readErr)
			}
			r := results[src.ID()]
			if stored {
				if err != nil || target.uploadCalls != 1 || r.ReusedCount != 1 || r.FailedCount != 0 {
					t.Fatalf("result=%+v uploads=%d err=%v", r, target.uploadCalls, err)
				}
			} else {
				if err == nil || target.uploadCalls != 2 || r.FailedCount != 1 {
					t.Fatalf("result=%+v uploads=%d err=%v", r, target.uploadCalls, err)
				}
				if _, err := os.Stat(filepath.Join(src.VideosDir(), "uncertain.mp4")); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

type committedRemoteDrive struct{ *fakeReconcileDrive }

type unavailableUploadDrive struct {
	*fakeUploadDrive
	attempts int
}

func (d *unavailableUploadDrive) UploadAndReportHash(context.Context, string, string, io.Reader, int64) (UploadResult, error) {
	d.attempts++
	return UploadResult{}, errors.New("graph api error: status=507 insufficient storage")
}

func TestTargetFailureStopsOtherCrawlersOnSameDestinationOnly(t *testing.T) {
	ctx := context.Background()
	cat, first, target, m := uploadTestSetup(t)
	reg := m.cfg.Registry.(*fakeRegistry)
	unavailable := &unavailableUploadDrive{fakeUploadDrive: target}
	reg.Add(unavailable)
	second := setupScriptCrawler(t, "crawler-second")
	third := setupScriptCrawler(t, "crawler-third")
	otherTarget := newFakeUploadDrive("other-target", "onedrive", "root")
	reg.Add(otherTarget)
	for _, pair := range []struct {
		source   *scriptcrawler.Driver
		targetID string
	}{{first, target.ID()}, {second, target.ID()}, {third, otherTarget.ID()}} {
		reg.Add(pair.source)
		if err := cat.UpsertDrive(ctx, &catalog.Drive{ID: pair.source.ID(), Kind: scriptcrawler.Kind, Credentials: map[string]string{"script_path": "/tmp/example.py", "upload_drive_id": pair.targetID}}); err != nil {
			t.Fatal(err)
		}
		writeCrawlerVideo(t, cat, pair.source, "file", ".mp4", []byte("payload"), true)
	}
	if err := m.RunDrives(ctx, []string{first.ID(), second.ID(), third.ID()}); err == nil {
		t.Fatal("destination failure was swallowed")
	}
	results, err := cat.LatestCrawlerUploadResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if unavailable.attempts != 1 || results[second.ID()].RemainingCount != 1 || results[third.ID()].UploadedCount != 1 {
		t.Fatalf("attempts=%d results=%+v", unavailable.attempts, results)
	}
}

func (d *committedRemoteDrive) UploadAndReportHash(ctx context.Context, parent, name string, body io.Reader, size int64) (UploadResult, error) {
	result, err := d.fakeUploadDrive.UploadAndReportHash(ctx, parent, name, body, size)
	if err == nil {
		d.existing = &result
	}
	return result, err
}

func TestUploadKeepsLocalFileWhenCatalogCommitFailsAndReusesRemoteOnRetry(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	src := setupScriptCrawler(t, "crawler-commit")
	target := &committedRemoteDrive{&fakeReconcileDrive{fakeUploadDrive: newFakeUploadDrive("target", "onedrive", "root")}}
	reg := newFakeRegistry()
	reg.Add(src)
	reg.Add(target)
	if err := cat.UpsertDrive(ctx, &catalog.Drive{ID: src.ID(), Kind: scriptcrawler.Kind, Credentials: map[string]string{"script_path": "/tmp/example.py", "upload_drive_id": target.ID()}}); err != nil {
		t.Fatal(err)
	}
	id := writeCrawlerVideo(t, cat, src, "commit", ".mp4", []byte("payload"), true)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_upload_commit BEFORE UPDATE OF drive_id ON videos BEGIN SELECT RAISE(FAIL, 'simulated catalog write failure'); END`); err != nil {
		t.Fatal(err)
	}
	m := New(Config{Catalog: cat, Registry: reg})
	if err := m.RunOnce(ctx); err == nil {
		t.Fatal("catalog failure was swallowed")
	}
	if _, err := os.Stat(filepath.Join(src.VideosDir(), "commit.mp4")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_upload_commit`); err != nil {
		t.Fatal(err)
	}
	if err := m.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	v, err := cat.GetVideo(ctx, id)
	if err != nil || v.DriveID != target.ID() || target.uploadCalls != 1 {
		t.Fatalf("video=%+v err=%v uploads=%d", v, err, target.uploadCalls)
	}
	if _, err := os.Stat(filepath.Join(src.VideosDir(), "commit.mp4")); !os.IsNotExist(err) {
		t.Fatalf("local cleanup error=%v", err)
	}
}

func TestUploadReportsEachLocalProblemAndContinuesReadyVideos(t *testing.T) {
	ctx := context.Background()
	cat, src, target, m := uploadTestSetup(t)
	ready := writeCrawlerVideo(t, cat, src, "a-ready", ".mp4", []byte("ready"), true)
	missing := writeCrawlerVideo(t, cat, src, "b-missing", ".mp4", []byte("missing"), true)
	if err := os.Remove(filepath.Join(src.VideosDir(), "b-missing.mp4")); err != nil {
		t.Fatal(err)
	}
	fingerprint := writeCrawlerVideo(t, cat, src, "c-fingerprint", ".mp4", []byte("fingerprint"), false)
	preview := writeCrawlerVideo(t, cat, src, "d-preview", ".mp4", []byte("preview"), true)
	if err := cat.UpdateVideoFingerprint(ctx, preview, "unique-preview-fingerprint", "ready", ""); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdatePreview(ctx, preview, "", "failed"); err != nil {
		t.Fatal(err)
	}
	if err := m.RunOnce(ctx); err == nil {
		t.Fatal("missing file failure was swallowed")
	}
	results, err := cat.LatestCrawlerUploadResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result := results[src.ID()]
	if result.State != "partial" || result.CandidateCount != 4 || result.UploadedCount != 1 || result.BlockedCount != 2 || result.FailedCount != 1 || result.RemainingCount != 0 {
		t.Fatalf("result = %+v", result)
	}
	reasons := map[string]string{}
	for _, issue := range result.Issues {
		reasons[issue.VideoID] = issue.Reason
	}
	if reasons[missing] != "missing_file" || reasons[fingerprint] != "fingerprint_pending" || reasons[preview] != "preview_failed" {
		t.Fatalf("issues = %+v", result.Issues)
	}
	v, err := cat.GetVideo(ctx, ready)
	if err != nil || v.DriveID != target.ID() {
		t.Fatalf("ready video = %+v, %v", v, err)
	}
}

func TestUploadPagesPastFiftyWithoutRevisitingMigratedRows(t *testing.T) {
	ctx := context.Background()
	cat, src, target, m := uploadTestSetup(t)
	for i := 0; i < 61; i++ {
		id := writeCrawlerVideo(t, cat, src, fmt.Sprintf("video-%03d", i), ".mp4", []byte("payload"), true)
		if err := cat.UpdateVideoFingerprint(ctx, id, fmt.Sprintf("%064x", i+1), "ready", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	results, err := cat.LatestCrawlerUploadResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := results[src.ID()]
	if r.CandidateCount != 61 || r.UploadedCount+r.ReusedCount != 61 || r.RemainingCount != 0 || r.State != "succeeded" {
		t.Fatalf("result = %+v", r)
	}
	_, count, err := cat.CrawlerUploadScope(ctx, src.ID())
	if err != nil || count != 0 || target.uploadCalls == 0 {
		t.Fatalf("remaining=%d uploads=%d err=%v", count, target.uploadCalls, err)
	}
}

func TestUploadCandidatePagesIgnoreHistoryAndNewInsertions(t *testing.T) {
	ctx := context.Background()
	cat, src, _, m := uploadTestSetup(t)
	m.cfg.PageSize = 1
	writeCrawlerVideo(t, cat, src, "first", ".mp4", []byte("first"), true)
	writeCrawlerVideo(t, cat, src, "last", ".mp4", []byte("last"), true)
	history := writeCrawlerVideo(t, cat, src, "history", ".mp4", []byte("history"), false)
	if err := cat.MigrateVideoToDrive(ctx, history, catalog.VideoDriveMigration{DriveID: "old-target", FileID: "old-remote"}); err != nil {
		t.Fatal(err)
	}
	inserted := false
	m.cfg.OnMigrated = func(string) {
		if inserted {
			return
		}
		inserted = true
		writeCrawlerVideo(t, cat, src, "between", ".mp4", []byte("later"), true)
	}
	if err := m.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	results, err := cat.LatestCrawlerUploadResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := results[src.ID()]
	if r.CandidateCount != 2 || r.UploadedCount+r.ReusedCount != 2 || r.BlockedCount != 0 {
		t.Fatalf("result = %+v", r)
	}
	_, count, err := cat.CrawlerUploadScope(ctx, src.ID())
	if err != nil || count != 1 {
		t.Fatalf("new candidate count = %d, %v", count, err)
	}
}

// Hold the worker's first cancellation check until StartDrive has returned and
// the test has canceled the accepted task, without relying on goroutine timing.
type delayedCancellationContext struct {
	context.Context
	release <-chan struct{}
}

func (c delayedCancellationContext) Err() error {
	<-c.release
	return c.Context.Err()
}

func TestUploadCanceledImmediatelyAfterAdmissionPersistsOutcome(t *testing.T) {
	cat, src, target, m := uploadTestSetup(t)
	videoID := writeCrawlerVideo(t, cat, src, "cancel-before-start", ".mp4", []byte("payload"), true)
	previous := uploadjob.Result{TaskID: "previous-task", DriveID: src.ID(), State: "succeeded"}
	if err := cat.SaveCrawlerUploadResult(context.Background(), previous); err != nil {
		t.Fatal(err)
	}
	idleCount := 0
	m.cfg.OnUploadProgress = func(progress UploadProgress) {
		if progress.State != "idle" {
			t.Errorf("unexpected progress for canceled task: %+v", progress)
			return
		}
		idleCount++
		results, err := cat.LatestCrawlerUploadResults(context.Background())
		if err != nil {
			t.Error(err)
			return
		}
		result := results[src.ID()]
		if result.TaskID == "" || result.TaskID == previous.TaskID || result.State != "canceled" {
			t.Errorf("outcome at idle = %+v, want current canceled outcome", result)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	done, accepted := m.StartDrive(delayedCancellationContext{Context: ctx, release: release}, src.ID())
	cancel()
	close(release)
	if !accepted {
		t.Fatal("upload task was not accepted")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("completion error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled task did not finish")
	}
	results, err := cat.LatestCrawlerUploadResults(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := results[src.ID()]
	if result.TaskID == "" || result.TaskID == previous.TaskID || result.State != "canceled" || result.StartedAt.IsZero() || result.FinishedAt.IsZero() {
		t.Fatalf("outcome = %+v, want completed canceled task", result)
	}
	if idleCount != 1 || target.uploadCalls != 0 || len(target.ensureCalls) != 0 {
		t.Fatalf("idle=%d uploads=%d directory requests=%d", idleCount, target.uploadCalls, len(target.ensureCalls))
	}
	v, err := cat.GetVideo(context.Background(), videoID)
	if err != nil || v.DriveID != src.ID() {
		t.Fatalf("source video = %+v, error = %v", v, err)
	}
	if _, err := os.Stat(filepath.Join(src.VideosDir(), "cancel-before-start.mp4")); err != nil {
		t.Fatal(err)
	}
	if !m.tryBeginRun() {
		t.Fatal("canceled task did not release the upload slot")
	}
	m.finishRun()
}

func TestCanceledUploadSweepDoesNotStartRemainingDrives(t *testing.T) {
	for _, tc := range []struct {
		cancelState string
		wantState   string
		wantUploads int
	}{
		{cancelState: "uploading", wantState: "canceled"},
		{cancelState: "idle", wantState: "succeeded", wantUploads: 1},
	} {
		t.Run(tc.cancelState, func(t *testing.T) {
			cat, src, target, m := uploadTestSetup(t)
			writeCrawlerVideo(t, cat, src, "cancel-sweep", ".mp4", []byte("payload"), true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			const nextDriveID = "crawler-next"
			if err := cat.UpsertDrive(ctx, &catalog.Drive{ID: nextDriveID, Kind: scriptcrawler.Kind}); err != nil {
				t.Fatal(err)
			}
			previous := uploadjob.Result{TaskID: "previous-task", DriveID: nextDriveID, State: "succeeded"}
			if err := cat.SaveCrawlerUploadResult(ctx, previous); err != nil {
				t.Fatal(err)
			}
			m.cfg.OnUploadProgress = func(progress UploadProgress) {
				if progress.DriveID != src.ID() {
					t.Errorf("unexpected progress for unstarted drive: %+v", progress)
				}
				if progress.State == tc.cancelState {
					cancel()
				}
			}
			if err := m.RunDrives(ctx, []string{src.ID(), nextDriveID}); !errors.Is(err, context.Canceled) {
				t.Fatalf("sweep error = %v, want context.Canceled", err)
			}
			results, err := cat.LatestCrawlerUploadResults(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if results[src.ID()].State != tc.wantState || results[nextDriveID].TaskID != previous.TaskID {
				t.Fatalf("unexpected sweep outcomes: %+v", results)
			}
			if target.uploadCalls != tc.wantUploads || len(target.ensureCalls) != tc.wantUploads {
				t.Fatalf("uploads=%d directory requests=%d, want %d", target.uploadCalls, len(target.ensureCalls), tc.wantUploads)
			}
		})
	}
}

func TestCanceledUploadPersistsOutcomeAndRetainsSource(t *testing.T) {
	cat, src, target, m := uploadTestSetup(t)
	writeCrawlerVideo(t, cat, src, "cancel", ".mp4", []byte("cancel"), true)
	blocking := &blockingFakeUploadDrive{fakeUploadDrive: target, started: make(chan struct{}, 1), release: make(chan struct{})}
	m.cfg.Registry.(*fakeRegistry).Add(blocking)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done, accepted := m.StartDrive(ctx, src.ID())
	if !accepted {
		t.Fatal("not accepted")
	}
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("upload did not start")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	results, err := cat.LatestCrawlerUploadResults(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := results[src.ID()]
	if r.State != "canceled" || r.UploadedCount != 0 || r.FailedCount != 0 || r.RemainingCount != 1 {
		t.Fatalf("result=%+v", r)
	}
	if _, err := os.Stat(filepath.Join(src.VideosDir(), "cancel.mp4")); err != nil {
		t.Fatal(err)
	}
}
