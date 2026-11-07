package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/nianzhibai/91/backend/internal/api"
	"github.com/nianzhibai/91/backend/internal/catalog"
)

type driveGenerationWork struct {
	busy    bool
	kinds   catalog.GenerationKinds
	load    func(context.Context) ([]*catalog.Video, error)
	enqueue func(context.Context, *catalog.Video) bool
}

func generationLabel(kind api.DriveGenerationKind) string {
	switch kind {
	case api.DriveGenerationThumbnails:
		return "封面"
	case api.DriveGenerationPreviews:
		return "预览"
	case api.DriveGenerationFingerprints:
		return "指纹"
	default:
		return string(kind)
	}
}

func generationScope(kind api.DriveGenerationKind) driveTaskScope {
	if kind == api.DriveGenerationPreviews {
		return driveTaskScopePreview
	}
	return 0
}

func (a *App) driveGenerationWork(driveID string, kind api.DriveGenerationKind) (driveGenerationWork, error) {
	a.mu.Lock()
	previewWorker, thumbWorker, fingerprintWorker := a.workers[driveID], a.thumbWorkers[driveID], a.fingerprintWorkers[driveID]
	a.mu.Unlock()
	switch kind {
	case api.DriveGenerationThumbnails:
		if thumbWorker != nil {
			return driveGenerationWork{
				busy: previewTaskBusy(thumbWorker.Status()), kinds: catalog.GenerationKinds{Thumbnails: true},
				load: func(ctx context.Context) ([]*catalog.Video, error) {
					return a.cat.ListVideosNeedingThumbnail(ctx, driveID, 0)
				},
				enqueue: thumbWorker.EnqueueBlocking,
			}, nil
		}
	case api.DriveGenerationPreviews:
		if !a.previewEnabled() {
			return driveGenerationWork{}, fmt.Errorf("预览生成已关闭")
		}
		if previewWorker != nil {
			return driveGenerationWork{
				busy: previewTaskBusy(previewWorker.Status()), kinds: catalog.GenerationKinds{Previews: true},
				load: func(ctx context.Context) ([]*catalog.Video, error) {
					return a.cat.ListVideosByPreviewStatus(ctx, driveID, "pending", 0)
				},
				enqueue: previewWorker.EnqueueBlocking,
			}, nil
		}
	case api.DriveGenerationFingerprints:
		if fingerprintWorker != nil {
			return driveGenerationWork{
				busy: fingerprintTaskBusy(fingerprintWorker.Status()) || a.fingerprintQueueingBusy(driveID), kinds: catalog.GenerationKinds{Fingerprints: true},
				load: func(ctx context.Context) ([]*catalog.Video, error) {
					return a.cat.ListVideosNeedingFingerprint(ctx, driveID, 0)
				},
				enqueue: fingerprintWorker.EnqueueBlocking,
			}, nil
		}
	default:
		return driveGenerationWork{}, fmt.Errorf("unknown generation kind: %s", kind)
	}
	return driveGenerationWork{}, fmt.Errorf("%s生成服务尚未就绪，请稍后重试", generationLabel(kind))
}

func (a *App) prepareDriveGeneration(ctx context.Context, driveID string, work driveGenerationWork) ([]*catalog.Video, error) {
	if _, err := a.resetFailedGeneration(ctx, driveID, work.kinds); err != nil {
		return nil, err
	}
	return work.load(ctx)
}

func enqueueDriveGenerationWork(ctx context.Context, driveID string, kind api.DriveGenerationKind, work driveGenerationWork, items []*catalog.Video) {
	for i, video := range items {
		if ctx.Err() != nil || !work.enqueue(ctx, video) {
			log.Printf("[generation] enqueue canceled drive=%s kind=%s queued=%d", driveID, kind, i)
			return
		}
	}
	log.Printf("[generation] enqueued drive=%s kind=%s count=%d", driveID, kind, len(items))
}

func (a *App) regenerateDriveResources(ctx context.Context, driveID string, kind api.DriveGenerationKind) {
	taskCtx, done, admitted := a.registerDriveTaskContext(ctx, driveID, generationScope(kind))
	if !admitted {
		return
	}
	defer done()
	finishEnqueue := a.beginDriveResourceEnqueue(driveID)
	defer finishEnqueue()
	work, err := a.driveGenerationWork(driveID, kind)
	if err != nil {
		log.Printf("[generation] prepare drive=%s kind=%s: %v", driveID, kind, err)
		return
	}
	items, err := a.prepareDriveGeneration(taskCtx, driveID, work)
	if err != nil {
		log.Printf("[generation] prepare drive=%s kind=%s: %v", driveID, kind, err)
		return
	}
	enqueueDriveGenerationWork(taskCtx, driveID, kind, work, items)
}

func (a *App) requestDriveGeneration(reqCtx, runCtx context.Context, driveID string, kind api.DriveGenerationKind) (api.DriveGenerationResult, error) {
	if _, err := a.cat.GetDrive(reqCtx, driveID); err != nil {
		return api.DriveGenerationResult{}, err
	}
	gate := a.driveOperationGate(driveID)
	gate.controlMu.Lock()
	controlLocked := true
	defer func() {
		if controlLocked {
			gate.controlMu.Unlock()
		}
	}()
	label := generationLabel(kind)
	taskCtx, done, admitted := a.registerDriveTaskContext(runCtx, driveID, generationScope(kind))
	if !admitted {
		return api.DriveGenerationResult{State: "busy", Message: "当前网盘配置正在切换或正在删除，请稍后重试"}, nil
	}
	reserved := false
	finish := func() {
		if reserved {
			gate.mu.Lock()
			delete(gate.generationRequests, kind)
			gate.mu.Unlock()
		}
		done()
		if reserved {
			a.notifyDriveRuntime(driveID, true)
		}
	}
	started := false
	defer func() {
		if !started {
			finish()
		}
	}()
	// Task admission keeps this worker snapshot alive across configuration
	// updates. The reservation also covers preparation before queueing starts.
	work, err := a.driveGenerationWork(driveID, kind)
	if err != nil {
		return api.DriveGenerationResult{}, err
	}
	gate.mu.Lock()
	if gate.generationRequests[kind] || work.busy {
		gate.mu.Unlock()
		return api.DriveGenerationResult{State: "busy", Message: label + "生成任务正在进行"}, nil
	}
	if gate.generationRequests == nil {
		gate.generationRequests = make(map[api.DriveGenerationKind]bool)
	}
	gate.generationRequests[kind] = true
	reserved = true
	gate.mu.Unlock()
	controlLocked = false
	gate.controlMu.Unlock()
	prepareCtx, cancel := context.WithTimeout(taskCtx, 10*time.Second)
	defer cancel()
	stopCancel := context.AfterFunc(reqCtx, cancel)
	defer stopCancel()
	items, err := a.prepareDriveGeneration(prepareCtx, driveID, work)
	if err != nil {
		return api.DriveGenerationResult{}, err
	}
	if err := taskCtx.Err(); err != nil {
		return api.DriveGenerationResult{}, err
	}
	if len(items) == 0 {
		stats, err := a.cat.CountDriveAssetStatsForDrive(prepareCtx, driveID)
		if err != nil {
			return api.DriveGenerationResult{}, err
		}
		remaining := 0
		switch kind {
		case api.DriveGenerationThumbnails:
			counts := stats.Thumbnails[driveID]
			remaining = counts.Pending + counts.Failed
		case api.DriveGenerationPreviews:
			counts := stats.Teasers[driveID]
			remaining = counts.Pending + counts.Failed
		case api.DriveGenerationFingerprints:
			counts := stats.Fingerprints[driveID]
			remaining = counts.Pending + counts.Failed
		}
		if remaining > 0 {
			return api.DriveGenerationResult{}, fmt.Errorf("%s资源尚未就绪，但当前没有可生成的项目", label)
		}
		a.scheduleDriveDurationBackfill(driveID)
		return api.DriveGenerationResult{State: "ready", Message: label + "已全部就绪"}, nil
	}
	started = true
	go func() {
		defer finish()
		finishEnqueue := a.beginDriveResourceEnqueue(driveID)
		defer finishEnqueue()
		enqueueDriveGenerationWork(taskCtx, driveID, kind, work, items)
	}()
	return api.DriveGenerationResult{State: "started", Message: "已触发" + label + "生成"}, nil
}
