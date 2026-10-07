package main

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/video-site/backend/internal/api"
	"github.com/video-site/backend/internal/drives/scriptcrawler"
)

func (a *App) beginTagJob(kind string) bool {
	if a == nil || a.cat == nil {
		return false
	}
	a.tagJobMu.Lock()
	defer a.tagJobMu.Unlock()
	if a.tagJobState.Running {
		return false
	}
	a.tagJobState = api.TagJobStatus{
		State:     "running",
		Running:   true,
		Kind:      kind,
		StartedAt: time.Now().Format(time.RFC3339),
	}
	return true
}

func (a *App) finishTagJob(state string, err error) {
	a.tagJobMu.Lock()
	defer a.tagJobMu.Unlock()
	a.tagJobState.State = state
	a.tagJobState.Running = false
	a.tagJobState.LastFinishedAt = time.Now().Format(time.RFC3339)
	if err != nil {
		a.tagJobState.LastError = err.Error()
	}
}

func (a *App) tagJobStatus() api.TagJobStatus {
	if a == nil {
		return api.TagJobStatus{State: "idle"}
	}
	a.tagJobMu.Lock()
	status := a.tagJobState
	a.tagJobMu.Unlock()
	if status.State == "" {
		status.State = "idle"
	}
	return status
}

func (a *App) startTagRetag(ctx context.Context) bool {
	if !a.beginTagJob("retag") {
		return false
	}
	go a.runTagRetag(ctx)
	return true
}

func (a *App) runTagRetag(ctx context.Context) {
	total, err := a.cat.CountVideosForRetag(ctx)
	if err != nil {
		a.finishTagJob("failed", err)
		return
	}
	a.tagJobMu.Lock()
	a.tagJobState.Total = total
	a.tagJobMu.Unlock()

	if err := ctx.Err(); err != nil {
		a.finishTagJob("canceled", err)
		return
	}
	if err := a.cat.ReconcileVideoTags(ctx); err != nil {
		a.finishTagJob("failed", err)
		return
	}
	if err := a.ensureAllScriptCrawlerNameTags(ctx); err != nil {
		log.Printf("[tag-retag] ensure crawler name tags: %v", err)
	}
	a.tagJobMu.Lock()
	a.tagJobState.Processed = total
	a.tagJobMu.Unlock()
	a.finishTagJob("completed", nil)
}

func (a *App) ensureAllScriptCrawlerNameTags(ctx context.Context) error {
	drives, err := a.cat.ListDrives(ctx)
	if err != nil {
		return err
	}
	for _, drive := range drives {
		if drive == nil || drive.Kind != scriptcrawler.Kind {
			continue
		}
		tagName := strings.TrimSpace(drive.Name)
		if tagName == "" {
			tagName = strings.TrimSpace(drive.ID)
		}
		if tagName == "" {
			continue
		}
		prefix := scriptcrawler.BuildVideoID(drive.ID, "")
		if _, err := a.cat.EnsureCrawlerTagForVideoIDPrefix(ctx, prefix, tagName); err != nil {
			return err
		}
	}
	return nil
}
