package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/video-site/backend/internal/api"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/drives/googledrive"
	"github.com/video-site/backend/internal/drives/guangyapan"
	"github.com/video-site/backend/internal/drives/localstorage"
	"github.com/video-site/backend/internal/drives/localupload"
	"github.com/video-site/backend/internal/drives/onedrive"
	"github.com/video-site/backend/internal/drives/p115"
	"github.com/video-site/backend/internal/drives/p123"
	"github.com/video-site/backend/internal/drives/pikpak"
	"github.com/video-site/backend/internal/drives/quark"
	"github.com/video-site/backend/internal/drives/scriptcrawler"
	"github.com/video-site/backend/internal/drives/webdav"
	"github.com/video-site/backend/internal/drives/wopan"
	"github.com/video-site/backend/internal/fingerprint"
	"github.com/video-site/backend/internal/preview"
)

// guangYaPanLegacyRootPath keeps existing path-based mounts working until the
// user saves a directory ID through the unified root directory field.
func guangYaPanLegacyRootPath(rootID string, credentials map[string]string) string {
	if strings.TrimSpace(rootID) != "" {
		return ""
	}
	return strings.TrimSpace(credentials["root_path"])
}

func (a *App) attachDrive(ctx context.Context, d *catalog.Drive) error {
	a.driveAttachMu.Lock()
	defer a.driveAttachMu.Unlock()
	if d != nil && a.driveConfigPending(d.ID) {
		active, err := a.activeDriveConfig(ctx, d.ID)
		if err != nil {
			return err
		}
		return a.attachDriveSnapshotUnlocked(ctx, active)
	}
	return a.attachDriveUnlocked(ctx, d)
}

// reloadDriveRuntime applies a persisted Driver configuration change. Pure
// metadata saves never call this method. Remounting is deliberately not a
// crawler-upload trigger: uploads start only through their explicit action,
// successful crawl completion, or the nightly pipeline.
func (a *App) reloadDriveRuntime(ctx context.Context, driveID string) error {
	// Serialize the complete registry transition. The per-drive update lease
	// prevents conflicting tasks/config writes; this lock also orders the shared
	// registry and worker replacement against lazy/startup attachment.
	var d *catalog.Drive
	err := func() error {
		a.driveAttachMu.Lock()
		defer a.driveAttachMu.Unlock()

		var err error
		d, err = a.cat.GetDrive(ctx, driveID)
		if err != nil {
			return err
		}

		// The API holds this drive's exclusive runtime-update lease. Reaching this
		// point therefore means every tracked task and generation queue is idle;
		// configuration saves must never stop work implicitly.
		return a.attachDriveUnlocked(ctx, d)
	}()
	if err != nil {
		return err
	}

	// 本地存储开启 .strm 越root后，之前因 strm 指向目录外而失败的封面/
	// 预览/指纹应自动重试，省得用户再手动点三个"重试失败"按钮。
	if d.Kind == localstorage.Kind &&
		parseBoolDefault(strings.TrimSpace(d.Credentials["strm_allow_outside_root"]), false) {
		a.scheduleDriveTaskAfterConfig(ctx, driveID, 0, func(taskCtx context.Context) {
			a.regenFailedThumbnails(taskCtx, driveID)
		})
		a.scheduleDriveTaskAfterConfig(ctx, driveID, driveTaskScopePreview, func(taskCtx context.Context) {
			a.regenFailedPreviews(taskCtx, driveID)
		})
		a.scheduleDriveTaskAfterConfig(ctx, driveID, 0, func(taskCtx context.Context) {
			a.regenFailedFingerprints(taskCtx, driveID)
		})
	}
	return nil
}

func (a *App) ensureDriveAttached(ctx context.Context, driveID string) error {
	if _, ok := a.registry.Get(driveID); ok {
		return nil
	}
	a.driveAttachMu.Lock()
	defer a.driveAttachMu.Unlock()
	if _, ok := a.registry.Get(driveID); ok {
		return nil
	}
	d, err := a.activeDriveConfig(ctx, driveID)
	if err != nil {
		return err
	}
	return a.attachDriveSnapshotUnlocked(ctx, d)
}

func (a *App) attachExistingDrives(ctx context.Context) {
	existing, err := a.cat.ListDrives(ctx)
	if err != nil {
		log.Printf("[drive] list existing drives: %v", err)
		return
	}
	log.Printf("[drive] attaching %d configured drive(s) in background", len(existing))
	for _, d := range existing {
		if err := ctx.Err(); err != nil {
			log.Printf("[drive] background attach stopped: %v", err)
			return
		}
		if err := a.attachDrive(ctx, d); err != nil {
			log.Printf("[drive %s] attach failed: %v", d.ID, err)
		}
	}
	log.Printf("[drive] background attach complete")
}

func (a *App) recordPlaybackDriveStatus(driveID, status, lastError string) {
	a.recordDriveRuntimeStatus(driveID, status, lastError)
}

func (a *App) recordDriveRuntimeStatus(driveID, status, lastError string) {
	if a == nil || a.cat == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.cat.SetDriveRuntimeStatus(ctx, driveID, status, lastError); err != nil {
		log.Printf("[drive %s] persist runtime status %s: %v", driveID, status, err)
	}
}

type driveTaskScope uint8

const (
	driveTaskScopeScan driveTaskScope = 1 << iota
	driveTaskScopePreview
)

type driveTaskAdmission struct {
	gate       *driveOperationGate
	generation uint64
}

type driveTaskAdmissionContextKey struct{}

type driveOperationGate struct {
	// configMu serializes HTTP/config writers. It is deliberately not held while
	// waiting for an old task generation to drain, so subsequent edits can be
	// accepted and coalesced into the same pending transition.
	configMu  sync.Mutex
	controlMu sync.Mutex

	// generationWorkersMu makes replacing, stopping, and restoring the three
	// long-lived generation workers one lifecycle operation. A stop performed
	// while configuration is pending records a restart obligation here; the
	// pending transition fulfills it before publishing the new configuration.
	generationWorkersMu          sync.Mutex
	generationWorkersNeedRestart bool
	generationWorkersContext     context.Context

	mu         sync.Mutex
	generation uint64
	active     int
	blocked    bool
	ready      chan struct{}
	pending    bool
	applying   bool
	deleting   bool
	retired    bool

	applyScheduled bool
	pendingScopes  api.DriveConfigUpdateScope
	runtimeApply   func() error
	previewApply   func() error
	scanApply      func() error
	activeConfig   *catalog.Drive
}

func newDriveOperationGate() *driveOperationGate {
	return &driveOperationGate{generation: 1}
}

func (a *App) driveOperationGate(driveID string) *driveOperationGate {
	driveID = strings.TrimSpace(driveID)
	a.driveOperationGatesMu.Lock()
	defer a.driveOperationGatesMu.Unlock()
	if a.driveOperationGates == nil {
		a.driveOperationGates = make(map[string]*driveOperationGate)
	}
	gate := a.driveOperationGates[driveID]
	if gate == nil {
		gate = newDriveOperationGate()
		a.driveOperationGates[driveID] = gate
	}
	return gate
}

func (a *App) removeDriveOperationGate(driveID string, expected *driveOperationGate) {
	if a == nil || expected == nil {
		return
	}
	driveID = strings.TrimSpace(driveID)
	a.driveOperationGatesMu.Lock()
	if a.driveOperationGates[driveID] == expected {
		delete(a.driveOperationGates, driveID)
	}
	a.driveOperationGatesMu.Unlock()
}

func driveTaskAdmissionFromContext(ctx context.Context, gate *driveOperationGate) (uint64, bool) {
	if ctx == nil || gate == nil {
		return 0, false
	}
	admission, ok := ctx.Value(driveTaskAdmissionContextKey{}).(driveTaskAdmission)
	return admission.generation, ok && admission.gate == gate
}

func withDriveTaskAdmission(ctx context.Context, gate *driveOperationGate, generation uint64) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, driveTaskAdmissionContextKey{}, driveTaskAdmission{
		gate:       gate,
		generation: generation,
	})
}

func (g *driveOperationGate) currentGeneration() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.generation
}

func (g *driveOperationGate) beginBlockedLocked() {
	if g.blocked {
		return
	}
	g.blocked = true
	g.ready = make(chan struct{})
}

func (g *driveOperationGate) endBlockedLocked() {
	if !g.blocked {
		return
	}
	g.blocked = false
	if g.ready != nil {
		close(g.ready)
		g.ready = nil
	}
}

// retireLocked permanently rejects work through this gate and wakes callers
// that were waiting for a configuration transition. A later drive created with
// the same ID receives a fresh gate after this one is removed from the map.
func (g *driveOperationGate) retireLocked() {
	g.retired = true
	g.deleting = false
	g.applying = false
	g.pending = false
	g.pendingScopes = 0
	g.runtimeApply = nil
	g.previewApply = nil
	g.scanApply = nil
	g.applyScheduled = false
	g.blocked = true
	if g.ready != nil {
		close(g.ready)
		g.ready = nil
	}
}

func (g *driveOperationGate) admitLocked(generation uint64, inherited bool) (func(), uint64, bool) {
	if g == nil || g.applying || g.retired {
		return nil, 0, false
	}
	if inherited && generation != g.generation {
		return nil, 0, false
	}
	if g.blocked && !inherited {
		return nil, 0, false
	}
	g.active++
	admittedGeneration := g.generation
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if g.active > 0 {
				g.active--
			}
			g.mu.Unlock()
		})
	}, admittedGeneration, true
}

func (g *driveOperationGate) tryBeginTask(ctx context.Context, _ driveTaskScope) (func(), uint64, bool) {
	generation, inherited := driveTaskAdmissionFromContext(ctx, g)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.admitLocked(generation, inherited)
}

// tryBeginTaskGeneration is used by long-lived generation workers. A worker
// belongs to the runtime generation that created it: it may finish already
// queued work while a change is pending, but it can never run after a runtime
// remount has advanced the generation.
func (g *driveOperationGate) tryBeginTaskGeneration(generation uint64) (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	release, _, ok := g.admitLocked(generation, true)
	return release, ok
}

// beginTask waits for a pending configuration transition only for internal
// producers that must not lose work. Calls nested under an already admitted
// task inherit its generation and may finish the current task pipeline.
func (g *driveOperationGate) beginTask(ctx context.Context, scope driveTaskScope) (func(), uint64, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	generation, inherited := driveTaskAdmissionFromContext(ctx, g)
	if inherited {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.admitLocked(generation, true)
	}
	for {
		g.mu.Lock()
		if g.retired {
			g.mu.Unlock()
			return nil, 0, false
		}
		if !g.blocked && !g.applying {
			release, admittedGeneration, ok := g.admitLocked(0, false)
			g.mu.Unlock()
			return release, admittedGeneration, ok
		}
		ready := g.ready
		g.mu.Unlock()
		if ready == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return nil, 0, false
		case <-ready:
		}
	}
}

type driveConfigUpdateLease struct {
	app     *App
	driveID string
	gate    *driveOperationGate

	authorizedScope      api.DriveConfigUpdateScope
	committedScope       api.DriveConfigUpdateScope
	deferred             bool
	createdPending       bool
	immediate            bool
	runtimeAdvanced      bool
	destructive          bool
	destructiveCommitted bool
	releaseOnce          sync.Once
}

func (a *App) beginDriveConfigUpdate(driveID string) (api.DriveConfigUpdateLease, string) {
	driveID = strings.TrimSpace(driveID)
	if driveID == "" {
		return nil, "缺少网盘 ID"
	}
	gate := a.driveOperationGate(driveID)
	if !gate.configMu.TryLock() {
		return nil, "当前网盘已有配置更新正在进行，请稍后重试"
	}
	return &driveConfigUpdateLease{app: a, driveID: driveID, gate: gate}, ""
}

func (lease *driveConfigUpdateLease) Authorize(scope api.DriveConfigUpdateScope) string {
	if lease == nil || lease.gate == nil || lease.app == nil {
		return "网盘配置更新协调器不可用"
	}
	lease.authorizedScope = scope
	if scope == 0 {
		return ""
	}

	if scope&api.DriveConfigUpdateDestructive != 0 {
		lease.gate.mu.Lock()
		defer lease.gate.mu.Unlock()
		if lease.gate.retired {
			return "网盘已删除"
		}
		if lease.gate.deleting {
			return "网盘正在停止任务并删除，请稍后"
		}
		// Deletion supersedes a pending configuration transition. Keep its
		// callbacks intact until deletion succeeds so a failed delete can resume
		// applying the saved configuration. While deleting, reject all new and
		// inherited task admissions and let the API stop/drain the old generation.
		lease.gate.beginBlockedLocked()
		lease.gate.deleting = true
		lease.gate.applying = true
		lease.gate.generation++
		lease.destructive = true
		lease.runtimeAdvanced = true
		lease.immediate = true
		return ""
	}

	// Capture the old task-visible row before the API persists its desired
	// configuration. Existing tasks continue reading this immutable snapshot.
	if err := lease.app.ensureActiveDriveConfigSnapshot(lease.driveID); err != nil {
		return "读取当前网盘运行配置失败：" + err.Error()
	}
	busy := lease.app.driveHasActiveWork(lease.driveID)

	lease.gate.mu.Lock()
	defer lease.gate.mu.Unlock()
	if lease.gate.blocked {
		if !lease.gate.pending || lease.gate.applying {
			return "当前网盘配置正在生效，请稍后重试"
		}
		lease.deferred = true
		return ""
	}

	lease.gate.beginBlockedLocked()
	if lease.gate.active > 0 || busy {
		lease.gate.pending = true
		lease.deferred = true
		lease.createdPending = true
		return ""
	}
	lease.gate.applying = true
	lease.immediate = true
	return ""
}

func (lease *driveConfigUpdateLease) Commit(scope api.DriveConfigUpdateScope, apply func() error) (bool, error) {
	if lease == nil || lease.gate == nil || lease.app == nil {
		return false, errors.New("网盘配置更新协调器不可用")
	}
	if scope == 0 {
		if apply == nil {
			return false, nil
		}
		return false, apply()
	}
	allowed := lease.authorizedScope
	if allowed&api.DriveConfigUpdateRuntime != 0 {
		allowed |= api.DriveConfigUpdatePreview | api.DriveConfigUpdateScan
	}
	if scope&allowed != scope {
		return false, fmt.Errorf("configuration scope %d was not authorized", scope)
	}
	if scope&api.DriveConfigUpdateDestructive != 0 {
		if scope != api.DriveConfigUpdateDestructive || !lease.destructive {
			return false, fmt.Errorf("invalid destructive configuration scope %d", scope)
		}
		lease.gate.mu.Lock()
		lease.gate.retireLocked()
		lease.gate.mu.Unlock()
		lease.app.removeDriveOperationGate(lease.driveID, lease.gate)
		lease.destructiveCommitted = true
		lease.committedScope |= scope
		return false, nil
	}

	if lease.deferred {
		lease.gate.mu.Lock()
		lease.gate.pendingScopes |= scope
		if scope&api.DriveConfigUpdateRuntime != 0 {
			lease.gate.runtimeApply = apply
			apply = nil
		}
		if scope&api.DriveConfigUpdatePreview != 0 {
			lease.gate.previewApply = apply
			apply = nil
		}
		if scope&api.DriveConfigUpdateScan != 0 {
			lease.gate.scanApply = apply
		}
		lease.gate.mu.Unlock()
		lease.committedScope |= scope
		return true, nil
	}

	lease.gate.controlMu.Lock()
	defer lease.gate.controlMu.Unlock()
	if scope&api.DriveConfigUpdateRuntime != 0 && !lease.runtimeAdvanced {
		lease.gate.mu.Lock()
		lease.gate.generation++
		lease.gate.mu.Unlock()
		lease.runtimeAdvanced = true
	}
	var err error
	if apply != nil {
		err = apply()
	}
	lease.app.refreshActiveDriveConfigAfterApply(lease.driveID)
	lease.committedScope |= scope
	return false, err
}

func (lease *driveConfigUpdateLease) Release() {
	if lease == nil || lease.gate == nil {
		return
	}
	lease.releaseOnce.Do(func() {
		startApply := false
		resumeAfterDrain := false
		restoreAfterAbortedUpdate := false
		lease.gate.mu.Lock()
		switch {
		case lease.destructive:
			if !lease.destructiveCommitted {
				// The delete failed or its request was canceled. Resume a saved
				// pending configuration, or keep the stop barrier until canceled
				// tasks have actually returned.
				if lease.gate.pending && lease.gate.pendingScopes != 0 {
					lease.gate.deleting = false
					lease.gate.applying = false
					if !lease.gate.applyScheduled {
						lease.gate.applyScheduled = true
						startApply = true
					}
				} else {
					// Cancellation may have interrupted the HTTP request before the
					// stopped task actually returned. Keep admissions blocked until its
					// gate count and observable queues are both idle.
					resumeAfterDrain = true
				}
			}
		case lease.deferred && lease.committedScope != 0:
			if !lease.gate.applyScheduled {
				lease.gate.applyScheduled = true
				startApply = true
			}
		case lease.createdPending && lease.gate.pendingScopes == 0:
			// Persistence failed before Commit. Keep admissions blocked until any
			// workers stopped concurrently with the write have been restored, then
			// let the old generation continue normally.
			restoreAfterAbortedUpdate = true
		case lease.immediate:
			lease.gate.applying = false
			lease.gate.endBlockedLocked()
		}
		lease.gate.mu.Unlock()
		if restoreAfterAbortedUpdate {
			lease.gate.controlMu.Lock()
			lease.app.restoreDriveGenerationWorkers(lease.driveID, lease.gate)
			lease.gate.mu.Lock()
			if lease.gate.pending && lease.gate.pendingScopes == 0 &&
				!lease.gate.deleting && !lease.gate.retired {
				lease.gate.pending = false
				lease.gate.applying = false
				lease.gate.endBlockedLocked()
			}
			lease.gate.mu.Unlock()
			lease.gate.controlMu.Unlock()
		}
		lease.gate.configMu.Unlock()
		if startApply {
			go lease.app.waitAndApplyPendingDriveConfig(lease.driveID, lease.gate)
		}
		if resumeAfterDrain {
			go lease.app.waitAndReleaseAbortedDriveDelete(lease.driveID, lease.gate)
		}
	})
}

func (a *App) waitAndApplyPendingDriveConfig(driveID string, gate *driveOperationGate) {
	if a == nil || gate == nil {
		return
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		gate.mu.Lock()
		if gate.retired || !gate.pending || gate.pendingScopes == 0 {
			gate.applyScheduled = false
			gate.mu.Unlock()
			return
		}
		active := gate.active
		blockedByDelete := gate.deleting
		gate.mu.Unlock()
		if active == 0 && !blockedByDelete && !a.driveHasActiveWork(driveID) && gate.configMu.TryLock() {
			gate.controlMu.Lock()
			// configMu is reserved before publishing applying=true. This prevents a
			// delete/config writer from acquiring the writer slot while the pending
			// worker merely waits to enter it.
			stillIdle := !a.driveHasActiveWork(driveID)
			gate.mu.Lock()
			applyNow := stillIdle && gate.pending && gate.pendingScopes != 0 &&
				gate.active == 0 && !gate.applying && !gate.deleting && !gate.retired
			if applyNow {
				gate.applying = true
				if gate.pendingScopes&api.DriveConfigUpdateRuntime != 0 {
					gate.generation++
				}
			}
			gate.mu.Unlock()
			if applyNow {
				a.applyPendingDriveConfigLocked(driveID, gate)
			}
			gate.controlMu.Unlock()
			gate.configMu.Unlock()
			if applyNow {
				return
			}
		}
		<-ticker.C
	}
}

func (a *App) waitAndReleaseAbortedDriveDelete(driveID string, gate *driveOperationGate) {
	if a == nil || gate == nil {
		return
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		gate.mu.Lock()
		active := gate.active
		waiting := gate.deleting && !gate.retired && !gate.pending
		gate.mu.Unlock()
		if !waiting {
			return
		}
		if active == 0 && !a.driveHasActiveWork(driveID) {
			gate.mu.Lock()
			if gate.deleting && !gate.retired && !gate.pending && gate.active == 0 {
				gate.deleting = false
				gate.applying = false
				gate.endBlockedLocked()
			}
			gate.mu.Unlock()
			return
		}
		<-ticker.C
	}
}

// applyPendingDriveConfigLocked runs with configMu and controlMu held.
func (a *App) applyPendingDriveConfigLocked(driveID string, gate *driveOperationGate) {
	gate.mu.Lock()
	scopes := gate.pendingScopes
	runtimeApply := gate.runtimeApply
	previewApply := gate.previewApply
	scanApply := gate.scanApply
	gate.mu.Unlock()

	if scopes&api.DriveConfigUpdateRuntime != 0 {
		if err := safelyApplyDriveConfig(runtimeApply); err != nil {
			log.Printf("[drive %s] apply deferred runtime configuration: %v", driveID, err)
		}
	}
	// At this point the old task generation is fully drained. Publish the latest
	// persisted snapshot before notifying scope-specific consumers, then restore
	// workers stopped during the wait. Runtime remounts already replace workers;
	// the restart marker makes this equally reliable for preview/scan-only saves.
	a.refreshActiveDriveConfigAfterApply(driveID)
	a.restoreDriveGenerationWorkers(driveID, gate)
	if scopes&api.DriveConfigUpdatePreview != 0 {
		if err := safelyApplyDriveConfig(previewApply); err != nil {
			log.Printf("[drive %s] apply deferred preview configuration: %v", driveID, err)
		}
	}
	if scopes&api.DriveConfigUpdateScan != 0 {
		if err := safelyApplyDriveConfig(scanApply); err != nil {
			log.Printf("[drive %s] apply deferred scan configuration: %v", driveID, err)
		}
	}

	gate.mu.Lock()
	gate.pendingScopes = 0
	gate.runtimeApply = nil
	gate.previewApply = nil
	gate.scanApply = nil
	gate.pending = false
	gate.applying = false
	gate.applyScheduled = false
	gate.endBlockedLocked()
	gate.mu.Unlock()
	log.Printf("[drive %s] deferred configuration is now active", driveID)
}

func safelyApplyDriveConfig(apply func() error) (err error) {
	if apply == nil {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic while applying configuration: %v", recovered)
		}
	}()
	return apply()
}

func (a *App) driveConfigPending(driveID string) bool {
	if a == nil {
		return false
	}
	gate := a.driveOperationGate(driveID)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.pending
}

func cloneDriveConfig(d *catalog.Drive) *catalog.Drive {
	if d == nil {
		return nil
	}
	cloned := *d
	cloned.Credentials = make(map[string]string, len(d.Credentials))
	for key, value := range d.Credentials {
		cloned.Credentials[key] = value
	}
	cloned.SkipDirIDs = append([]string(nil), d.SkipDirIDs...)
	return &cloned
}

func (a *App) ensureActiveDriveConfigSnapshot(driveID string) error {
	if a == nil || a.cat == nil {
		return nil
	}
	gate := a.driveOperationGate(driveID)
	gate.mu.Lock()
	if gate.pending {
		hasSnapshot := gate.activeConfig != nil
		gate.mu.Unlock()
		if hasSnapshot {
			return nil
		}
		return errors.New("active configuration snapshot is unavailable")
	}
	gate.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	d, err := a.cat.GetDrive(ctx, driveID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	gate.mu.Lock()
	if !gate.pending {
		gate.activeConfig = cloneDriveConfig(d)
	}
	gate.mu.Unlock()
	return nil
}

// activeDriveConfig returns the immutable configuration snapshot visible to
// the currently admitted task generation. The catalog may already contain a
// newer desired configuration while this snapshot intentionally stays old.
func (a *App) activeDriveConfig(ctx context.Context, driveID string) (*catalog.Drive, error) {
	if a == nil || a.cat == nil {
		return nil, errors.New("drive catalog unavailable")
	}
	gate := a.driveOperationGate(driveID)
	gate.mu.Lock()
	if gate.pending {
		if gate.activeConfig != nil {
			d := cloneDriveConfig(gate.activeConfig)
			gate.mu.Unlock()
			return d, nil
		}
		gate.mu.Unlock()
		return nil, errors.New("active drive configuration unavailable during pending update")
	}
	gate.mu.Unlock()

	// Without a pending transition the catalog is the active configuration.
	// Refreshing here also observes internal changes such as the transcode
	// worker adding its output directory to skip_dir_ids.
	d, err := a.cat.GetDrive(ctx, driveID)
	if err != nil {
		return nil, err
	}
	gate.mu.Lock()
	if !gate.pending {
		gate.activeConfig = cloneDriveConfig(d)
	} else if gate.activeConfig != nil {
		d = cloneDriveConfig(gate.activeConfig)
	}
	gate.mu.Unlock()
	return d, nil
}

func (a *App) setActiveDriveConfig(d *catalog.Drive) {
	if a == nil || d == nil {
		return
	}
	gate := a.driveOperationGate(d.ID)
	gate.mu.Lock()
	gate.activeConfig = cloneDriveConfig(d)
	gate.mu.Unlock()
}

// setActiveDriveConfigIfStable publishes a catalog row only when no
// configuration transition became active after that row was read. This is the
// compare-with-gate half of refreshActiveDriveConfigIfStable and prevents a
// stale background refresh from replacing the immutable old-generation view.
func (a *App) setActiveDriveConfigIfStable(d *catalog.Drive) bool {
	if a == nil || d == nil {
		return false
	}
	gate := a.driveOperationGate(d.ID)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.pending || gate.applying || gate.deleting || gate.retired {
		return false
	}
	gate.activeConfig = cloneDriveConfig(d)
	return true
}

// refreshActiveDriveConfigAfterApply is the forced publication path owned by
// a configuration writer after it has drained the old task generation.
func (a *App) refreshActiveDriveConfigAfterApply(driveID string) {
	if a == nil || a.cat == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	d, err := a.cat.GetDrive(ctx, driveID)
	if err != nil {
		log.Printf("[drive %s] refresh active configuration snapshot: %v", driveID, err)
		return
	}
	a.setActiveDriveConfig(d)
}

// refreshActiveDriveConfigIfStable is for background/system writers such as a
// completed transcode. The gate state is checked after the catalog read, which
// closes the check-then-refresh race with a newly authorized configuration save.
func (a *App) refreshActiveDriveConfigIfStable(driveID string) {
	if a == nil || a.cat == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	d, err := a.cat.GetDrive(ctx, driveID)
	if err != nil {
		log.Printf("[drive %s] refresh stable configuration snapshot: %v", driveID, err)
		return
	}
	a.setActiveDriveConfigIfStable(d)
}

type driveCredentialState struct {
	mu          sync.Mutex
	generation  uint64
	kind        string
	anchorKey   string
	anchorValue string
}

type driveCredentialLease struct {
	driveID    string
	generation uint64
	state      *driveCredentialState
}

func (a *App) driveCredentialState(driveID string) *driveCredentialState {
	a.driveCredentialStatesMu.Lock()
	defer a.driveCredentialStatesMu.Unlock()
	if a.driveCredentialStates == nil {
		a.driveCredentialStates = make(map[string]*driveCredentialState)
	}
	state := a.driveCredentialStates[driveID]
	if state == nil {
		state = &driveCredentialState{}
		a.driveCredentialStates[driveID] = state
	}
	return state
}

// beginDriveCredentialLease invalidates every callback issued to an older
// instance of the same drive. The state lock also orders the transition after
// any credential write that already started, so the caller can safely reload
// the latest row before constructing the replacement driver.
func (a *App) beginDriveCredentialLease(driveID string) driveCredentialLease {
	state := a.driveCredentialState(driveID)
	state.mu.Lock()
	state.generation++
	state.kind = ""
	state.anchorKey = ""
	state.anchorValue = ""
	lease := driveCredentialLease{driveID: driveID, generation: state.generation, state: state}
	state.mu.Unlock()
	return lease
}

func configureDriveCredentialLease(lease driveCredentialLease, d *catalog.Drive) {
	if lease.state == nil || d == nil {
		return
	}
	anchorKey := ""
	switch d.Kind {
	case "quark":
		anchorKey = "cookie"
	case p123.Kind:
		anchorKey = "access_token"
	case "pikpak", "wopan", guangyapan.Kind, "onedrive", googledrive.Kind:
		anchorKey = "refresh_token"
	}
	lease.state.mu.Lock()
	defer lease.state.mu.Unlock()
	if lease.generation != lease.state.generation {
		return
	}
	lease.state.kind = d.Kind
	lease.state.anchorKey = anchorKey
	lease.state.anchorValue = d.Credentials[anchorKey]
}

// persistDriveCredentials applies only the credential keys produced by the
// active runtime instance. Besides avoiding whole-row lost updates, the lease
// prevents a superseded driver from rolling back credentials after a remount.
// The credential anchor closes the smaller save-before-remount race: if an
// administrator has already replaced the refresh token/cookie, a callback
// derived from its predecessor is rejected atomically by SQLite.
func (a *App) persistDriveCredentials(lease driveCredentialLease, updates map[string]string) {
	if a == nil || a.cat == nil || lease.state == nil || len(updates) == 0 {
		return
	}
	lease.state.mu.Lock()
	defer lease.state.mu.Unlock()
	if lease.generation != lease.state.generation {
		return
	}

	persistCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	applied := true
	var err error
	if lease.state.anchorKey == "" {
		err = a.cat.PatchDriveCredentials(persistCtx, lease.driveID, updates)
	} else {
		applied, err = a.cat.PatchDriveCredentialsIfMatch(
			persistCtx,
			lease.driveID,
			lease.state.kind,
			lease.state.anchorKey,
			lease.state.anchorValue,
			updates,
		)
	}

	// Stream links may snapshot an Authorization header or a rotated cookie.
	// Invalidate even on a persistence error so the proxy cannot keep serving a
	// credential the active provider session has already rejected.
	if a.proxy != nil {
		a.proxy.InvalidateDrive(lease.driveID)
	}
	if err != nil {
		log.Printf("[drive %s] persist refreshed credentials: %v", lease.driveID, err)
		return
	}
	if !applied {
		log.Printf("[drive %s] discard refreshed credentials: persisted %s changed", lease.driveID, lease.state.anchorKey)
		return
	}
	if value, ok := updates[lease.state.anchorKey]; ok {
		lease.state.anchorValue = value
	}
}

func (a *App) attachDriveUnlocked(ctx context.Context, d *catalog.Drive) error {
	return a.attachDriveConfigUnlocked(ctx, d, true)
}

func (a *App) attachDriveSnapshotUnlocked(ctx context.Context, d *catalog.Drive) error {
	return a.attachDriveConfigUnlocked(ctx, d, false)
}

func (a *App) attachDriveConfigUnlocked(ctx context.Context, d *catalog.Drive, reloadLatest bool) error {
	if d == nil {
		return errors.New("nil drive")
	}
	credentialLease := a.beginDriveCredentialLease(d.ID)
	if reloadLatest {
		// A refresh callback may have completed after the caller loaded d but
		// before this remount acquired its lease. Reload under the new generation
		// so the replacement never starts from that stale snapshot.
		latest, err := a.cat.GetDrive(ctx, d.ID)
		if err != nil {
			return err
		}
		d = latest
	} else {
		// A task admitted before a deferred save must lazily mount exactly the
		// snapshot it was admitted against, not the newer desired catalog row.
		d = cloneDriveConfig(d)
	}
	a.setActiveDriveConfig(d)
	configureDriveCredentialLease(credentialLease, d)
	// A configured drive has exactly one runtime owner. Remove the superseded
	// instance before initialization so a failed remount cannot silently keep
	// serving or scanning with credentials the user just replaced.
	a.retireDriveRuntime(d.ID)
	if d.Kind == scriptcrawler.Kind && !scriptcrawler.IsConfigured(d.Credentials) {
		return nil
	}

	var drv drives.Drive
	switch d.Kind {
	case "quark":
		drv = quark.New(quark.Config{
			ID:            d.ID,
			Cookie:        d.Credentials["cookie"],
			RootID:        d.RootID,
			UploadTempDir: a.uploadWorkDir("quark"),
			OnCookieUpdate: func(cookie string) {
				a.persistDriveCredentials(credentialLease, map[string]string{"cookie": cookie})
			},
		})
	case "p115":
		drv = p115.New(p115.Config{
			ID:            d.ID,
			Cookie:        d.Credentials["cookie"],
			RootID:        d.RootID,
			UploadTempDir: a.uploadWorkDir("p115"),
		})
	case p123.Kind:
		drv = p123.New(p123.Config{
			ID:            d.ID,
			Username:      d.Credentials["username"],
			Password:      d.Credentials["password"],
			AccessToken:   d.Credentials["access_token"],
			Platform:      d.Credentials["platform"],
			RootID:        d.RootID,
			UploadTempDir: a.uploadWorkDir(p123.Kind),
			OnTokenUpdate: func(access string) {
				a.persistDriveCredentials(credentialLease, map[string]string{"access_token": access})
			},
		})
	case "pikpak":
		drv = pikpak.New(pikpak.Config{
			ID:               d.ID,
			Username:         d.Credentials["username"],
			Password:         d.Credentials["password"],
			Platform:         d.Credentials["platform"],
			RefreshToken:     d.Credentials["refresh_token"],
			AccessToken:      d.Credentials["access_token"],
			CaptchaToken:     d.Credentials["captcha_token"],
			DeviceID:         d.Credentials["device_id"],
			RootID:           d.RootID,
			DisableMediaLink: pikpak.ParseBoolDefault(d.Credentials["disable_media_link"], true),
			UploadTempDir:    a.uploadWorkDir("pikpak"),
			OnTokenUpdate: func(access, refresh, captcha, deviceID string) {
				a.persistDriveCredentials(credentialLease, map[string]string{
					"access_token":  access,
					"refresh_token": refresh,
					"captcha_token": captcha,
					"device_id":     deviceID,
				})
			},
		})
	case "wopan":
		drv = wopan.New(wopan.Config{
			ID:            d.ID,
			AccessToken:   d.Credentials["access_token"],
			RefreshToken:  d.Credentials["refresh_token"],
			FamilyID:      d.Credentials["family_id"],
			RootID:        d.RootID,
			UploadTempDir: a.uploadWorkDir("wopan"),
			OnTokenUpdate: func(access, refresh string) {
				a.persistDriveCredentials(credentialLease, map[string]string{
					"access_token":  access,
					"refresh_token": refresh,
				})
			},
		})
	case guangyapan.Kind:
		drv = guangyapan.New(guangyapan.Config{
			ID:             d.ID,
			RootID:         d.RootID,
			RootPath:       guangYaPanLegacyRootPath(d.RootID, d.Credentials),
			PhoneNumber:    d.Credentials["phone_number"],
			CaptchaToken:   d.Credentials["captcha_token"],
			SendCode:       parseBoolDefault(strings.TrimSpace(d.Credentials["send_code"]), false),
			VerifyCode:     d.Credentials["verify_code"],
			VerificationID: d.Credentials["verification_id"],
			AccessToken:    d.Credentials["access_token"],
			RefreshToken:   d.Credentials["refresh_token"],
			ClientID:       d.Credentials["client_id"],
			DeviceID:       d.Credentials["device_id"],
			PageSize:       parseIntDefault(strings.TrimSpace(d.Credentials["page_size"]), 100),
			OrderBy:        parseIntDefault(strings.TrimSpace(d.Credentials["order_by"]), 3),
			SortType:       parseIntDefault(strings.TrimSpace(d.Credentials["sort_type"]), 1),
			AccountBaseURL: d.Credentials["account_base_url"],
			APIBaseURL:     d.Credentials["api_base_url"],
			UploadTempDir:  a.uploadWorkDir(guangyapan.Kind),
			OnCredentialsUpdate: func(updated map[string]string) {
				a.persistDriveCredentials(credentialLease, updated)
			},
		})
	case "onedrive":
		drv = onedrive.New(onedrive.Config{
			ID:           d.ID,
			RootID:       d.RootID,
			Region:       d.Credentials["region"],
			AccessToken:  d.Credentials["access_token"],
			RefreshToken: d.Credentials["refresh_token"],
			AuthMode:     d.Credentials["auth_mode"],
			ClientID:     d.Credentials["client_id"],
			ClientSecret: d.Credentials["client_secret"],
			IsSharePoint: parseBoolDefault(d.Credentials["is_sharepoint"], false),
			SiteID:       d.Credentials["site_id"],
			RenewAPIURL:  d.Credentials["api_url_address"],
			OnTokenUpdate: func(access, refresh string) {
				a.persistDriveCredentials(credentialLease, map[string]string{
					"access_token":  access,
					"refresh_token": refresh,
				})
			},
		})
	case googledrive.Kind:
		drv = googledrive.New(googledrive.Config{
			ID:           d.ID,
			RootID:       d.RootID,
			AccessToken:  d.Credentials["access_token"],
			RefreshToken: d.Credentials["refresh_token"],
			ClientID:     d.Credentials["client_id"],
			ClientSecret: d.Credentials["client_secret"],
			OAuthURL:     d.Credentials["oauth_url"],
			APIBaseURL:   d.Credentials["api_base_url"],
			OnTokenUpdate: func(access, refresh string) {
				a.persistDriveCredentials(credentialLease, map[string]string{
					"access_token":  access,
					"refresh_token": refresh,
				})
			},
		})
	case webdav.Kind:
		drv = webdav.New(webdav.Config{
			ID:       d.ID,
			BaseURL:  d.Credentials["base_url"],
			Username: d.Credentials["username"],
			Password: d.Credentials["password"],
			RootID:   d.RootID,
		})
	case localstorage.Kind:
		drv = localstorage.New(localstorage.Config{
			ID:                   d.ID,
			RootPath:             d.Credentials["path"],
			STRMAllowOutsideRoot: parseBoolDefault(strings.TrimSpace(d.Credentials["strm_allow_outside_root"]), false),
		})
	case scriptcrawler.Kind:
		drv = scriptcrawler.New(scriptcrawler.Config{
			ID:      d.ID,
			RootDir: a.scriptCrawlerDriveDir(d.ID),
		})
	default:
		return fmt.Errorf("unknown drive kind: %s", d.Kind)
	}

	if err := drv.Init(ctx); err != nil {
		if a.proxy != nil {
			a.proxy.InvalidateDrive(d.ID)
			a.proxy.SetDriveInitError(d.ID, d.Kind, err)
		}
		d.Status = "error"
		d.LastError = err.Error()
		if persistErr := a.cat.SetDriveRuntimeStatus(ctx, d.ID, d.Status, d.LastError); persistErr != nil {
			log.Printf("[drive %s] persist attach failure: %v", d.ID, persistErr)
		}
		return err
	}

	d.Status = "ok"
	d.LastError = ""
	if err := a.cat.SetDriveRuntimeStatus(ctx, d.ID, d.Status, d.LastError); err != nil {
		log.Printf("[drive %s] persist attach success: %v", d.ID, err)
	}
	if a.proxy != nil {
		a.proxy.InvalidateDrive(d.ID)
	}

	a.registry.Set(d.ID, drv)

	a.startDriveGenerationWorkers(ctx, d.ID, drv, true)

	if sd, ok := drv.(*scriptcrawler.Driver); ok {
		a.attachScriptCrawler(d, sd)
	}

	return nil
}

func (a *App) attachLocalUpload(ctx context.Context) error {
	drv := localupload.New(a.localUploadDir())
	if err := drv.Init(ctx); err != nil {
		return err
	}
	a.maintainLocalUploadFileNames(ctx)
	a.registry.Set(drv.ID(), drv)

	a.startDriveGenerationWorkers(ctx, drv.ID(), drv, true)
	return nil
}

func (a *App) newDriveGenerationWorkers(drv drives.Drive) (*preview.Worker, *preview.ThumbWorker, *fingerprint.Worker) {
	previewCfg := preview.Config{}
	if a.cfg != nil {
		previewCfg = preview.Config{
			FFmpegPath:      a.cfg.Preview.FFmpegPath,
			FFprobePath:     a.cfg.Preview.FFprobePath,
			DurationSeconds: a.cfg.Preview.DurationSeconds,
			Width:           a.cfg.Preview.Width,
			Segments:        a.cfg.Preview.Segments,
			LocalDir:        a.cfg.Storage.LocalPreviewDir,
		}
	}
	gen := preview.New(previewCfg)
	previewWorker := preview.NewWorker(gen, a.cat, drv)
	thumbWorker := preview.NewThumbWorker(gen, a.cat, drv)
	if cooldown := generationCooldownForDrive(drv); cooldown > 0 {
		previewWorker.RateLimitCooldown = cooldown
		thumbWorker.RateLimitCooldown = cooldown
	}
	fingerprintWorker := fingerprint.NewWorker(a.cat, drv, fingerprintConfigForDrive(drv))
	driveID := drv.ID()
	gate := a.driveOperationGate(driveID)
	generation := gate.currentGeneration()
	previewWorker.TaskGuard = func() func() {
		release, admitted := gate.tryBeginTaskGeneration(generation)
		if !admitted {
			return nil
		}
		return release
	}
	thumbWorker.TaskGuard = func() func() {
		release, admitted := gate.tryBeginTaskGeneration(generation)
		if !admitted {
			return nil
		}
		return release
	}
	fingerprintWorker.TaskGuard = func() func() {
		release, admitted := gate.tryBeginTaskGeneration(generation)
		if !admitted {
			return nil
		}
		return release
	}
	return previewWorker, thumbWorker, fingerprintWorker
}

func generationCooldownForDrive(drv drives.Drive) time.Duration {
	if drv == nil {
		return 0
	}
	switch strings.ToLower(drv.Kind()) {
	case "wopan", "guangyapan":
		return 10 * time.Minute
	}
	return 0
}

func (a *App) startDriveGenerationWorkers(ctx context.Context, driveID string, drv drives.Drive, enqueue bool) {
	gate := a.driveOperationGate(driveID)
	gate.generationWorkersMu.Lock()
	defer gate.generationWorkersMu.Unlock()

	a.startDriveGenerationWorkersLocked(ctx, driveID, drv, enqueue)
	gate.generationWorkersNeedRestart = false
	gate.generationWorkersContext = nil
}

// startDriveGenerationWorkersLocked replaces the registered worker set while
// gate.generationWorkersMu is held.
func (a *App) startDriveGenerationWorkersLocked(ctx context.Context, driveID string, drv drives.Drive, enqueue bool) {
	worker, thumbWorker, fingerprintWorker := a.newDriveGenerationWorkers(drv)
	workerCtx, cancel := context.WithCancel(ctx)
	go worker.Run(workerCtx)
	go thumbWorker.Run(workerCtx)
	go fingerprintWorker.Run(workerCtx)

	a.registerPreviewWorkersWithOptions(workerCtx, driveID, worker, thumbWorker, fingerprintWorker, cancel, enqueue)
}

func (a *App) localUploadDir() string {
	return filepath.Join(filepath.Dir(a.cfg.Storage.LocalPreviewDir), "uploads")
}

func (a *App) uploadWorkDir(kind string) string {
	if a == nil || a.cfg == nil || strings.TrimSpace(a.cfg.Storage.LocalPreviewDir) == "" {
		return ""
	}
	kind = strings.Trim(strings.ToLower(strings.TrimSpace(kind)), string(filepath.Separator))
	if kind == "" {
		kind = "generic"
	}
	return filepath.Join(filepath.Dir(a.cfg.Storage.LocalPreviewDir), "upload-tmp", kind)
}

func fingerprintConfigForDrive(drv drives.Drive) fingerprint.Config {
	cfg := fingerprint.Config{RateLimitCooldown: 5 * time.Minute}
	if drv == nil {
		return cfg
	}
	switch strings.ToLower(drv.Kind()) {
	case "p115", "p123", "onedrive", "wopan", "guangyapan":
		cfg.RateLimitCooldown = 10 * time.Minute
	case "pikpak":
		cfg.RateLimitCooldown = 5 * time.Minute
	}
	return cfg
}

// scriptCrawlerRootDir 是所有通用脚本爬虫 drive 共享的根目录。
func (a *App) scriptCrawlerRootDir() string {
	return filepath.Join(filepath.Dir(a.cfg.Storage.LocalPreviewDir), "scriptcrawlers")
}

// scriptCrawlerDriveDir 是单个 scriptcrawler drive 的存储目录：<root>/<driveID>。
func (a *App) scriptCrawlerDriveDir(driveID string) string {
	return filepath.Join(a.scriptCrawlerRootDir(), driveID)
}

// commonThumbsDir 是所有 drive 共享的封面目录，/p/thumb/{videoID} 路由命中这里。
func (a *App) commonThumbsDir() string {
	return filepath.Join(a.cfg.Storage.LocalPreviewDir, "thumbs")
}

// attachScriptCrawler 创建通用脚本爬虫 runner，并注册到 a.scriptCrawlers。
func (a *App) attachScriptCrawler(d *catalog.Drive, drv *scriptcrawler.Driver) {
	pythonPath := strings.TrimSpace(d.Credentials["python_path"])
	if pythonPath == "" {
		pythonPath = "python3"
	}
	scriptPath := strings.TrimSpace(d.Credentials["script_path"])
	proxyURL := strings.TrimSpace(d.Credentials["proxy"])
	configJSON := strings.TrimSpace(d.Credentials["config_json"])
	workDir := ""
	if scriptPath != "" {
		workDir = filepath.Dir(scriptPath)
	}
	protocol := scriptcrawler.ProtocolV1
	if meta, err := scriptcrawler.ReadMetadata(scriptPath); err == nil {
		protocol = meta.Protocol
	}

	driveID := d.ID
	c := scriptcrawler.NewCrawler(scriptcrawler.CrawlerConfig{
		Driver:         drv,
		Catalog:        a.cat,
		GetDriveConfig: a.activeDriveConfig,
		CrawlerName:    d.Name,
		Protocol:       protocol,
		PythonPath:     pythonPath,
		FFmpegPath:     a.cfg.Preview.FFmpegPath,
		FFprobePath:    a.cfg.Preview.FFprobePath,
		ScriptPath:     scriptPath,
		WorkDir:        workDir,
		CommonThumbDir: a.commonThumbsDir(),
		ProxyURL:       proxyURL,
		ConfigJSON:     configJSON,
		DisablePreview: !d.TeaserEnabled,
		OnProgress: func(progress scriptcrawler.CrawlProgress) {
			scanned := progress.Checked
			if scanned < progress.TotalEntries {
				scanned = progress.TotalEntries
			}
			added := progress.Emitted
			if added < progress.NewVideos {
				added = progress.NewVideos
			}
			a.updateDriveScanProgress(driveID, scanned, added)
		},
	})

	a.mu.Lock()
	a.scriptCrawlers[driveID] = c
	a.mu.Unlock()

	a.ensureScriptCrawlerNameTag(driveID, d.Name)
}

func (a *App) ensureScriptCrawlerNameTag(driveID, crawlerName string) {
	tagName := strings.TrimSpace(crawlerName)
	if tagName == "" {
		tagName = strings.TrimSpace(driveID)
	}
	if tagName == "" {
		return
	}
	bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	go func() {
		defer cancel()
		prefix := scriptcrawler.BuildVideoID(driveID, "")
		if _, err := a.cat.EnsureCrawlerTagForVideoIDPrefix(bgCtx, prefix, tagName); err != nil {
			log.Printf("[scriptcrawler] drive=%s ensure crawler tag %q: %v", driveID, tagName, err)
		}
	}()
}

func (a *App) registerPreviewWorkers(ctx context.Context, driveID string, worker *preview.Worker, thumbWorker *preview.ThumbWorker, fingerprintWorker *fingerprint.Worker, cancel context.CancelFunc) {
	a.registerPreviewWorkersWithOptions(ctx, driveID, worker, thumbWorker, fingerprintWorker, cancel, true)
}

func (a *App) registerPreviewWorkersWithOptions(ctx context.Context, driveID string, worker *preview.Worker, thumbWorker *preview.ThumbWorker, fingerprintWorker *fingerprint.Worker, cancel context.CancelFunc, enqueue bool) {
	a.mu.Lock()
	if a.cancels == nil {
		a.cancels = make(map[string]context.CancelFunc)
	}
	if a.workers == nil {
		a.workers = make(map[string]*preview.Worker)
	}
	if a.thumbWorkers == nil {
		a.thumbWorkers = make(map[string]*preview.ThumbWorker)
	}
	if a.fingerprintWorkers == nil {
		a.fingerprintWorkers = make(map[string]*fingerprint.Worker)
	}
	if old, ok := a.cancels[driveID]; ok && old != nil {
		old()
	}
	if worker != nil {
		a.workers[driveID] = worker
	} else {
		delete(a.workers, driveID)
	}
	if thumbWorker != nil {
		a.thumbWorkers[driveID] = thumbWorker
	} else {
		delete(a.thumbWorkers, driveID)
	}
	if fingerprintWorker != nil {
		a.fingerprintWorkers[driveID] = fingerprintWorker
	} else {
		delete(a.fingerprintWorkers, driveID)
	}
	if cancel != nil {
		a.cancels[driveID] = cancel
	} else {
		delete(a.cancels, driveID)
	}
	a.mu.Unlock()

	if !enqueue {
		return
	}
	a.scheduleDriveGenerationEnqueue(ctx, driveID, worker, thumbWorker)
	if fingerprintWorker != nil {
		go a.scheduleFingerprintBackfillWaiting(ctx, driveID, fingerprintWorker)
	}
}

func (a *App) registerDriveTaskContext(
	ctx context.Context,
	driveID string,
	scope driveTaskScope,
) (context.Context, func(), bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	gate := a.driveOperationGate(driveID)
	gateRelease, generation, ok := gate.tryBeginTask(ctx, scope)
	if !ok {
		return ctx, func() {}, false
	}
	taskCtx, done := a.registerDriveTaskContextWithGate(ctx, driveID, gate, generation, gateRelease)
	return taskCtx, done, true
}

func (a *App) registerDriveTaskContextWaiting(
	ctx context.Context,
	driveID string,
	scope driveTaskScope,
) (context.Context, func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	gate := a.driveOperationGate(driveID)
	gateRelease, generation, ok := gate.beginTask(ctx, scope)
	if !ok {
		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		return canceledCtx, func() {}
	}
	return a.registerDriveTaskContextWithGate(ctx, driveID, gate, generation, gateRelease)
}

func (a *App) registerDriveTaskContextWithGate(
	ctx context.Context,
	driveID string,
	gate *driveOperationGate,
	generation uint64,
	gateRelease func(),
) (context.Context, func()) {
	ctx = withDriveTaskAdmission(ctx, gate, generation)
	taskCtx, cancel := context.WithCancel(ctx)

	a.taskCancelMu.Lock()
	if a.driveTaskCancels == nil {
		a.driveTaskCancels = make(map[string]map[uint64]context.CancelFunc)
	}
	a.driveTaskCancelSeq++
	token := a.driveTaskCancelSeq
	if a.driveTaskCancels[driveID] == nil {
		a.driveTaskCancels[driveID] = make(map[uint64]context.CancelFunc)
	}
	a.driveTaskCancels[driveID][token] = cancel
	a.taskCancelMu.Unlock()

	var doneOnce sync.Once
	done := func() {
		doneOnce.Do(func() {
			cancel()
			a.taskCancelMu.Lock()
			if cancels := a.driveTaskCancels[driveID]; cancels != nil {
				delete(cancels, token)
				if len(cancels) == 0 {
					delete(a.driveTaskCancels, driveID)
				}
			}
			a.taskCancelMu.Unlock()
			gateRelease()
		})
	}
	return taskCtx, done
}

func (a *App) cancelDriveTaskContexts(driveID string) int {
	a.taskCancelMu.Lock()
	cancelsByToken := a.driveTaskCancels[driveID]
	delete(a.driveTaskCancels, driveID)
	a.taskCancelMu.Unlock()

	for _, cancel := range cancelsByToken {
		if cancel != nil {
			cancel()
		}
	}
	return len(cancelsByToken)
}

func (a *App) clearQueuedDriveTask(driveID string) bool {
	a.scanQueueMu.Lock()
	queued := a.scanQueued[driveID]
	delete(a.scanQueued, driveID)
	delete(a.scanProgress, driveID)
	a.scanQueueMu.Unlock()
	return queued
}

func (a *App) clearFingerprintQueueing(driveID string) bool {
	a.fingerprintQueueMu.Lock()
	queued := a.fingerprintQueueing[driveID]
	delete(a.fingerprintQueueing, driveID)
	a.fingerprintQueueMu.Unlock()
	return queued
}

func (a *App) beginDriveScanOrCrawl(driveID string) bool {
	driveID = strings.TrimSpace(driveID)
	if driveID == "" {
		return false
	}
	a.scanQueueMu.Lock()
	defer a.scanQueueMu.Unlock()
	if a.scanQueued == nil {
		a.scanQueued = make(map[string]bool)
	}
	if a.scanQueued[driveID] {
		return false
	}
	a.scanQueued[driveID] = true
	if a.scanProgress == nil {
		a.scanProgress = make(map[string]driveScanProgress)
	}
	a.scanProgress[driveID] = driveScanProgress{}
	return true
}

func (a *App) endDriveScanOrCrawl(driveID string) {
	a.scanQueueMu.Lock()
	delete(a.scanQueued, driveID)
	delete(a.scanProgress, driveID)
	a.scanQueueMu.Unlock()
}

func (a *App) updateDriveScanProgress(driveID string, scanned, added int) {
	driveID = strings.TrimSpace(driveID)
	if driveID == "" {
		return
	}
	a.scanQueueMu.Lock()
	if a.scanQueued[driveID] {
		if a.scanProgress == nil {
			a.scanProgress = make(map[string]driveScanProgress)
		}
		progress := a.scanProgress[driveID]
		progress.Scanned = scanned
		progress.Added = added
		a.scanProgress[driveID] = progress
	}
	a.scanQueueMu.Unlock()
}

func (a *App) updateDriveScanCooldown(driveID string, until time.Time) {
	driveID = strings.TrimSpace(driveID)
	if driveID == "" {
		return
	}
	a.scanQueueMu.Lock()
	if a.scanQueued[driveID] {
		if a.scanProgress == nil {
			a.scanProgress = make(map[string]driveScanProgress)
		}
		progress := a.scanProgress[driveID]
		progress.CooldownUntil = until
		a.scanProgress[driveID] = progress
	}
	a.scanQueueMu.Unlock()
}

func (a *App) driveHasActiveWork(driveID string) bool {
	driveID = strings.TrimSpace(driveID)
	if driveID == "" {
		return true
	}

	a.scanQueueMu.Lock()
	scanning := a.scanQueued[driveID]
	a.scanQueueMu.Unlock()
	if scanning {
		return true
	}

	a.taskCancelMu.Lock()
	taskContexts := len(a.driveTaskCancels[driveID])
	a.taskCancelMu.Unlock()
	if taskContexts > 0 {
		return true
	}

	a.fingerprintQueueMu.Lock()
	fingerprintQueueing := a.fingerprintQueueing[driveID]
	a.fingerprintQueueMu.Unlock()
	if fingerprintQueueing {
		return true
	}

	a.uploadProgressMu.Lock()
	uploading := a.uploadProgress[driveID].State != ""
	a.uploadProgressMu.Unlock()
	if uploading {
		return true
	}

	a.crawlerUploadMu.Lock()
	crawlerUploading := a.crawlerUploadRunning[driveID]
	a.crawlerUploadMu.Unlock()
	if crawlerUploading {
		return true
	}

	a.transcodeMu.Lock()
	transcoding := a.transcodeWorkers[driveID] != nil
	a.transcodeMu.Unlock()
	if transcoding {
		return true
	}

	a.mu.Lock()
	previewWorker := a.workers[driveID]
	thumbWorker := a.thumbWorkers[driveID]
	fingerprintWorker := a.fingerprintWorkers[driveID]
	a.mu.Unlock()

	if previewTaskBusy(thumbWorker.Status()) {
		return true
	}
	if previewTaskBusy(previewWorker.Status()) {
		return true
	}
	if fingerprintTaskBusy(fingerprintWorker.Status()) {
		return true
	}
	return false
}

// waitDriveTasksStopped waits for cooperative cancellation to reach every
// tracked task and generation queue. The destructive lease has already blocked
// admissions, so once both counters are idle no new work can appear.
func (a *App) waitDriveTasksStopped(ctx context.Context, driveID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	driveID = strings.TrimSpace(driveID)
	if driveID == "" {
		return errors.New("missing drive ID")
	}
	gate := a.driveOperationGate(driveID)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		gate.mu.Lock()
		active := gate.active
		retired := gate.retired
		gate.mu.Unlock()
		if retired || (active == 0 && !a.driveHasActiveWork(driveID)) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func previewTaskBusy(status preview.TaskStatus) bool {
	return status.State != "" && status.State != "idle"
}

func fingerprintTaskBusy(status fingerprint.TaskStatus) bool {
	return status.State != "" && status.State != "idle"
}

func (a *App) resetDriveGenerationWorkers(ctx context.Context, driveID string) bool {
	gate := a.driveOperationGate(driveID)
	gate.generationWorkersMu.Lock()
	defer gate.generationWorkersMu.Unlock()

	var drv drives.Drive
	var attached bool
	if a.registry != nil {
		drv, attached = a.registry.Get(driveID)
	}

	a.mu.Lock()
	hadWorkers := a.workers[driveID] != nil ||
		a.thumbWorkers[driveID] != nil ||
		a.fingerprintWorkers[driveID] != nil ||
		a.cancels[driveID] != nil
	oldCancel := a.cancels[driveID]
	a.mu.Unlock()

	gate.mu.Lock()
	pending := gate.pending
	retired := gate.retired
	gate.mu.Unlock()
	if attached && drv != nil && !pending && !retired {
		a.startDriveGenerationWorkersLocked(ctx, driveID, drv, false)
		gate.generationWorkersNeedRestart = false
		gate.generationWorkersContext = nil
		return hadWorkers
	}

	if oldCancel != nil {
		oldCancel()
	}
	a.mu.Lock()
	delete(a.workers, driveID)
	delete(a.thumbWorkers, driveID)
	delete(a.fingerprintWorkers, driveID)
	delete(a.cancels, driveID)
	a.mu.Unlock()
	if attached && drv != nil && pending && !retired {
		gate.generationWorkersNeedRestart = true
		gate.generationWorkersContext = ctx
	} else {
		gate.generationWorkersNeedRestart = false
		gate.generationWorkersContext = nil
	}
	return hadWorkers
}

// restoreDriveGenerationWorkers fulfills a restart obligation recorded by a
// stop during a pending configuration transition. It runs while admissions are
// still blocked, so scope callbacks can safely capture the replacement workers
// before the new configuration is exposed to tasks.
func (a *App) restoreDriveGenerationWorkers(driveID string, gate *driveOperationGate) {
	if a == nil || gate == nil {
		return
	}
	gate.generationWorkersMu.Lock()
	defer gate.generationWorkersMu.Unlock()
	if !gate.generationWorkersNeedRestart {
		return
	}

	gate.mu.Lock()
	retired := gate.retired
	gate.mu.Unlock()
	if retired {
		gate.generationWorkersNeedRestart = false
		gate.generationWorkersContext = nil
		return
	}
	if a.registry == nil {
		return
	}
	drv, attached := a.registry.Get(driveID)
	if !attached || drv == nil {
		return
	}
	workerCtx := gate.generationWorkersContext
	if workerCtx == nil {
		workerCtx = context.Background()
	}
	a.startDriveGenerationWorkersLocked(workerCtx, driveID, drv, false)
	gate.generationWorkersNeedRestart = false
	gate.generationWorkersContext = nil
}

func (a *App) stopDriveTasks(ctx context.Context, driveID string) bool {
	driveID = strings.TrimSpace(driveID)
	if driveID == "" {
		return false
	}

	// Cancellation must remain available while a configuration change is
	// waiting for these very tasks to drain. Taking ordinary task admission here
	// would make "stop" wait behind the pending change and create a deadlock.
	canceled := a.cancelDriveTaskContexts(driveID)
	queued := a.clearQueuedDriveTask(driveID)
	fingerprintQueued := a.clearFingerprintQueueing(driveID)
	uploading := a.clearCrawlerUploadProgress(driveID)
	transcoding := a.stopDriveTranscode(driveID)
	gate := a.driveOperationGate(driveID)
	gate.controlMu.Lock()
	hadWorkers := a.resetDriveGenerationWorkers(ctx, driveID)
	gate.controlMu.Unlock()
	stopped := canceled > 0 || queued || fingerprintQueued || uploading || transcoding || hadWorkers
	log.Printf("[tasks] stop drive=%s stopped=%v canceled_tasks=%d queued=%v fingerprint_queue=%v uploading=%v transcoding=%v workers=%v",
		driveID, stopped, canceled, queued, fingerprintQueued, uploading, transcoding, hadWorkers)
	return stopped
}

func (a *App) stopAllDriveTasks(ctx context.Context) int {
	if a.nightlyRunner != nil && a.nightlyRunner.StopCurrent() {
		log.Printf("[tasks] requested nightly pipeline stop")
	}
	stopped := 0
	for _, driveID := range a.driveTaskIDs() {
		if a.stopDriveTasks(ctx, driveID) {
			stopped++
		}
	}
	log.Printf("[tasks] stop all drive tasks drives=%d", stopped)
	return stopped
}

func (a *App) driveTaskIDs() []string {
	ids := make(map[string]struct{})
	if a.registry != nil {
		for _, drv := range a.registry.All() {
			if drv != nil {
				ids[drv.ID()] = struct{}{}
			}
		}
	}
	a.taskCancelMu.Lock()
	for id := range a.driveTaskCancels {
		ids[id] = struct{}{}
	}
	a.taskCancelMu.Unlock()
	a.scanQueueMu.Lock()
	for id := range a.scanQueued {
		ids[id] = struct{}{}
	}
	a.scanQueueMu.Unlock()
	a.fingerprintQueueMu.Lock()
	for id := range a.fingerprintQueueing {
		ids[id] = struct{}{}
	}
	a.fingerprintQueueMu.Unlock()
	a.uploadProgressMu.Lock()
	for id := range a.uploadProgress {
		ids[id] = struct{}{}
	}
	a.uploadProgressMu.Unlock()
	a.transcodeMu.Lock()
	for id := range a.transcodeWorkers {
		ids[id] = struct{}{}
	}
	a.transcodeMu.Unlock()
	a.mu.Lock()
	for id := range a.workers {
		ids[id] = struct{}{}
	}
	for id := range a.thumbWorkers {
		ids[id] = struct{}{}
	}
	for id := range a.fingerprintWorkers {
		ids[id] = struct{}{}
	}
	a.mu.Unlock()

	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (a *App) scheduleDriveTaskAfterConfig(
	ctx context.Context,
	driveID string,
	scope driveTaskScope,
	run func(context.Context),
) {
	if run == nil {
		return
	}
	go func() {
		taskCtx, done := a.registerDriveTaskContextWaiting(ctx, driveID, scope)
		defer done()
		if err := taskCtx.Err(); err != nil {
			return
		}
		run(taskCtx)
	}()
}

func (a *App) scheduleDriveGenerationEnqueue(
	ctx context.Context,
	driveID string,
	worker *preview.Worker,
	thumbWorker *preview.ThumbWorker,
) {
	go func() {
		taskCtx, done := a.registerDriveTaskContextWaiting(ctx, driveID, driveTaskScopePreview)
		defer done()
		if err := taskCtx.Err(); err != nil {
			return
		}
		a.enqueueDriveGeneration(taskCtx, driveID, worker, thumbWorker)
	}()
}

func (a *App) enqueuePending(ctx context.Context, driveID string, w *preview.Worker) {
	release, _, admitted := a.driveOperationGate(driveID).beginTask(ctx, driveTaskScopePreview)
	if !admitted {
		return
	}
	defer release()
	pending, err := a.cat.ListVideosByPreviewStatus(ctx, driveID, "pending", 0)
	if err != nil {
		log.Printf("[preview] list pending %s: %v", driveID, err)
		return
	}
	if len(pending) == 0 {
		return
	}
	log.Printf("[preview] enqueue %d pending videos for drive=%s", len(pending), driveID)
	for _, v := range pending {
		if !w.EnqueueBlocking(ctx, v) {
			log.Printf("[preview] enqueue pending canceled for drive=%s", driveID)
			return
		}
	}
}

func (a *App) enqueueDriveGeneration(ctx context.Context, driveID string, worker *preview.Worker, thumbWorker *preview.ThumbWorker) {
	// 封面 worker 始终入队（与早期"全局 preview.enabled=false 时仍然生成封面"
	// 的行为一致）；预览视频 worker 仅在该 drive 的 TeaserEnabled 为 true 时入队。
	// 两条队列互不等待，避免封面批量生成拖住预览视频生成。
	if thumbWorker != nil {
		a.enqueueThumbnails(ctx, driveID, thumbWorker)
	}
	if worker == nil || !a.teaserEnabledForDrive(ctx, driveID) {
		return
	}
	a.enqueuePending(ctx, driveID, worker)
}

func (a *App) enqueueThumbnails(ctx context.Context, driveID string, w *preview.ThumbWorker) {
	release, _, admitted := a.driveOperationGate(driveID).beginTask(ctx, 0)
	if !admitted {
		return
	}
	defer release()
	pending, err := a.cat.ListVideosNeedingThumbnail(ctx, driveID, 0)
	if err != nil {
		log.Printf("[thumb] list pending %s: %v", driveID, err)
		return
	}
	if len(pending) == 0 {
		return
	}
	log.Printf("[thumb] enqueue %d thumbnail/duration tasks for drive=%s", len(pending), driveID)
	for _, v := range pending {
		if !w.EnqueueBlocking(ctx, v) {
			log.Printf("[thumb] enqueue thumbnail/duration tasks canceled for drive=%s", driveID)
			return
		}
	}
}

func (a *App) runFingerprintReconciler(ctx context.Context) {
	ticker := time.NewTicker(fingerprintReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.enqueueAllPendingFingerprints(ctx)
		}
	}
}

func (a *App) enqueueAllPendingFingerprints(ctx context.Context) {
	a.mu.Lock()
	workers := make(map[string]*fingerprint.Worker, len(a.fingerprintWorkers))
	for id, worker := range a.fingerprintWorkers {
		workers[id] = worker
	}
	a.mu.Unlock()
	for driveID, worker := range workers {
		a.scheduleFingerprintBackfill(ctx, driveID, worker)
	}
}

func (a *App) scheduleFingerprintBackfill(ctx context.Context, driveID string, w *fingerprint.Worker) {
	if w == nil {
		return
	}
	taskCtx, done, ok := a.registerDriveTaskContext(ctx, driveID, 0)
	if !ok {
		return
	}
	a.startFingerprintBackfill(taskCtx, driveID, w, done)
}

func (a *App) scheduleFingerprintBackfillWaiting(ctx context.Context, driveID string, w *fingerprint.Worker) {
	if w == nil {
		return
	}
	taskCtx, done := a.registerDriveTaskContextWaiting(ctx, driveID, 0)
	a.startFingerprintBackfill(taskCtx, driveID, w, done)
}

func (a *App) startFingerprintBackfill(
	taskCtx context.Context,
	driveID string,
	w *fingerprint.Worker,
	done func(),
) {
	a.fingerprintQueueMu.Lock()
	if a.fingerprintQueueing == nil {
		a.fingerprintQueueing = make(map[string]bool)
	}
	if a.fingerprintQueueing[driveID] {
		a.fingerprintQueueMu.Unlock()
		done()
		return
	}
	a.fingerprintQueueing[driveID] = true
	a.fingerprintQueueMu.Unlock()

	go func() {
		defer func() {
			a.fingerprintQueueMu.Lock()
			delete(a.fingerprintQueueing, driveID)
			a.fingerprintQueueMu.Unlock()
			done()
		}()
		a.enqueueFingerprints(taskCtx, driveID, w)
	}()
}

func (a *App) enqueueFingerprints(ctx context.Context, driveID string, w *fingerprint.Worker) {
	if w == nil {
		return
	}
	release, _, admitted := a.driveOperationGate(driveID).beginTask(ctx, 0)
	if !admitted {
		return
	}
	defer release()
	pending, err := a.cat.ListVideosNeedingFingerprint(ctx, driveID, 0)
	if err != nil {
		log.Printf("[fingerprint] list pending %s: %v", driveID, err)
		return
	}
	if len(pending) == 0 {
		return
	}
	log.Printf("[fingerprint] enqueue %d videos for drive=%s", len(pending), driveID)
	for _, v := range pending {
		if !w.EnqueueBlocking(ctx, v) {
			log.Printf("[fingerprint] enqueue canceled for drive=%s", driveID)
			return
		}
	}
}

func (a *App) detachDrive(id string) {
	// Order deletion after any startup/lazy attach already constructing this
	// runtime. Without the shared attach lock, a slow Init could publish its
	// driver after deletion had removed the previously mounted instance.
	a.driveAttachMu.Lock()
	defer a.driveAttachMu.Unlock()

	// Revoke the active driver's persistence lease before removing it from the
	// registry. In-flight provider calls may still invoke their callbacks.
	a.beginDriveCredentialLease(id)
	a.cancelDriveTaskContexts(id)
	a.clearQueuedDriveTask(id)
	a.clearFingerprintQueueing(id)
	a.retireDriveRuntime(id)
}

func (a *App) retireDriveRuntime(id string) {
	if a.registry != nil {
		a.registry.Remove(id)
	}
	if a.proxy != nil {
		a.proxy.InvalidateDrive(id)
	}
	a.mu.Lock()
	if cancel, ok := a.cancels[id]; ok {
		cancel()
		delete(a.cancels, id)
	}
	delete(a.workers, id)
	delete(a.thumbWorkers, id)
	delete(a.fingerprintWorkers, id)
	delete(a.scriptCrawlers, id)
	a.mu.Unlock()
}

// listDriveDirChildren 实现 AdminServer.ListDriveDirChildren：
// 列指定 drive 在 parentID 下的直接子目录，仅返回目录条目（IsDir=true），文件忽略。
//
// parentID 为空时使用 drive 实例的 RootID()。用户在"设置跳过目录"弹窗里
// 浏览的是整个网盘逻辑根，方便从根目录起逐层挑跳过点。
//
// 性能优化：p115 的 Driver.List 走 SDK 的 ListWithLimit，会把目录里全部文件 +
// 目录分页拉完才返回；某些 115 根目录累积了几万个视频，单次列目录可能卡几十
// 秒（叠加 driver 的 2s 间隔限频）。所以 p115 走 ListDirsOnly 快路径：单页
// (1150)、按 file_type 排序，扫一遍只挑目录条目，1 次 API 调用搞定。其它网盘
// 走标准 List + IsDir 过滤 —— 它们的根目录通常不会有几万个文件。
//
// drive 未挂载（如凭证错误未通过 Init）时返回 error；前端展示 5xx 给用户。
type driveNotAttachedError struct {
	driveID   string
	lastError string
}

func (e *driveNotAttachedError) Error() string {
	message := fmt.Sprintf("drive %s not attached", e.driveID)
	if e.lastError != "" {
		message += ": " + e.lastError
	}
	return message
}

func (a *App) currentDriveNotAttachedError(ctx context.Context, driveID string) error {
	err := &driveNotAttachedError{driveID: driveID}
	if a != nil && a.cat != nil {
		if d, getErr := a.cat.GetDrive(ctx, driveID); getErr == nil && d != nil {
			err.lastError = strings.TrimSpace(d.LastError)
		}
	}
	return err
}

func (a *App) listDriveDirChildren(ctx context.Context, driveID, parentID string) (children []api.DriveDirEntry, resultErr error) {
	taskCtx, done, admitted := a.registerDriveTaskContext(ctx, driveID, 0)
	if !admitted {
		return nil, fmt.Errorf("drive %s configuration is waiting to take effect", driveID)
	}
	defer done()
	ctx = taskCtx
	defer func() {
		// Closing the directory picker (or navigating away) cancels its request;
		// that says nothing about the provider's connection state.
		if errors.Is(resultErr, context.Canceled) {
			return
		}
		var notAttached *driveNotAttachedError
		if errors.As(resultErr, &notAttached) && notAttached.lastError != "" {
			// The attach path already persisted the provider's root cause. A
			// directory picker cannot add new information while no driver exists,
			// so do not replace that cause with a generic secondary error.
			return
		}
		if resultErr != nil {
			a.recordDriveRuntimeStatus(driveID, "error", resultErr.Error())
			return
		}
		a.recordDriveRuntimeStatus(driveID, "ok", "")
	}()

	drv, ok := a.registry.Get(driveID)
	if !ok {
		return nil, a.currentDriveNotAttachedError(ctx, driveID)
	}
	if parentID == "" {
		parentID = drv.RootID()
	}
	// p115 快路径：避免拉全部分页文件
	if fast, ok := drv.(interface {
		ListDirsOnly(ctx context.Context, dirID string) ([]drives.Entry, error)
	}); ok {
		entries, err := fast.ListDirsOnly(ctx, parentID)
		if err != nil {
			return nil, fmt.Errorf("list drive %s parent %s dirs-only: %w", driveID, parentID, err)
		}
		out := make([]api.DriveDirEntry, 0, len(entries))
		for _, e := range entries {
			out = append(out, api.DriveDirEntry{ID: e.ID, Name: e.Name})
		}
		return out, nil
	}
	// 通用路径
	entries, err := drv.List(ctx, parentID)
	if err != nil {
		return nil, fmt.Errorf("list drive %s parent %s: %w", driveID, parentID, err)
	}
	out := make([]api.DriveDirEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir {
			continue
		}
		out = append(out, api.DriveDirEntry{ID: e.ID, Name: e.Name})
	}
	return out, nil
}
