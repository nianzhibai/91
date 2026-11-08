package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/driveview"
	"github.com/nianzhibai/91/backend/internal/httpapi"
	"github.com/nianzhibai/91/backend/internal/storageusage"
)

func (a *AdminServer) snapshots() *driveview.Service {
	a.driveSnapshotsOnce.Do(func() {
		a.driveSnapshots = driveview.New(a.Catalog.DriveEvents(), a.loadDriveResource)
	})
	return a.driveSnapshots
}

func (a *AdminServer) addDriveSnapshot(ctx context.Context, response map[string]any, id string, resource driveview.Resource) {
	if a.Catalog == nil {
		return
	}
	snapshot, err := a.snapshots().Get(ctx, id, resource, true)
	if err == nil && snapshot.Error == "" {
		response["snapshot"] = snapshot
	}
}

func (a *AdminServer) loadDetailDrive(ctx context.Context, id string) (*catalog.Drive, error) {
	drive, err := a.Catalog.GetDrive(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && isCrawlerDriveKind(drive.Kind)) {
		return nil, &driveview.LoadError{Status: http.StatusNotFound, Err: errors.New("网盘不存在")}
	}
	return drive, err
}

func (a *AdminServer) loadDriveResource(ctx context.Context, id string, resource driveview.Resource) (any, error) {
	drive, err := a.loadDetailDrive(ctx, id)
	if err != nil {
		return nil, err
	}
	switch resource {
	case driveview.Config:
		return a.driveConfig(drive), nil
	case driveview.Runtime:
		generation := DriveGenerationStatuses{}
		if a.GetDriveGenerationStatus != nil {
			generation = a.GetDriveGenerationStatus(id)
		} else if a.GetDriveGenerationStatuses != nil {
			generation = a.GetDriveGenerationStatuses()[id]
		}
		// Read the saved result after runtime state: a just-finished task must
		// not be paired with the previous scan's outcome.
		result, err := a.Catalog.LatestScanResult(ctx, id)
		if err != nil {
			return nil, err
		}
		runtime := driveRuntime(generation, result)
		status := a.nightlyJobStatus()
		runtime.MaintenanceStatus = &status
		return runtime, nil
	case driveview.Stats:
		stats, err := a.Catalog.CountDriveAssetStatsForDrive(ctx, id)
		if err != nil {
			return nil, err
		}
		return driveStats(id, stats), nil
	case driveview.Storage:
		refs, err := a.Catalog.ListLocalMediaRefsForDrive(ctx, id)
		if err != nil {
			return nil, err
		}
		assets := make([]storageusage.VideoAssetRef, 0, len(refs))
		for _, ref := range refs {
			assets = append(assets, storageusage.VideoAssetRef{ID: ref.VideoID, DriveID: id, PreviewLocal: ref.PreviewLocal})
		}
		usage, err := storageusage.ComputeContext(ctx, a.LocalPreviewDir, assets, []string{id}, localDiskStats)
		if err != nil {
			return nil, err
		}
		return usage.Drives[id], nil
	default:
		return nil, errors.New("unknown drive resource")
	}
}

func (a *AdminServer) writeDriveSnapshot(w http.ResponseWriter, r *http.Request, resource driveview.Resource) {
	w.Header().Set("Cache-Control", "no-store")
	snapshot, err := a.snapshots().Get(r.Context(), chi.URLParam(r, "id"), resource, r.URL.Query().Get("refresh") == "true")
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	status := http.StatusOK
	if snapshot.Error != "" {
		status = snapshot.Status
	}
	writeJSON(w, status, snapshot)
}

func (a *AdminServer) handleDriveConfigSnapshot(w http.ResponseWriter, r *http.Request) {
	a.writeDriveSnapshot(w, r, driveview.Config)
}
func (a *AdminServer) handleDriveRuntimeSnapshot(w http.ResponseWriter, r *http.Request) {
	a.writeDriveSnapshot(w, r, driveview.Runtime)
}
func (a *AdminServer) handleDriveStatsSnapshot(w http.ResponseWriter, r *http.Request) {
	a.writeDriveSnapshot(w, r, driveview.Stats)
}
func (a *AdminServer) handleDriveStorageSnapshot(w http.ResponseWriter, r *http.Request) {
	a.writeDriveSnapshot(w, r, driveview.Storage)
}

func (a *AdminServer) handleDriveEvents(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := a.loadDetailDrive(r.Context(), id); err != nil {
		status := http.StatusInternalServerError
		var loadError *driveview.LoadError
		if errors.As(err, &loadError) {
			status = loadError.Status
		}
		writeErr(w, r, status, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpapi.Error(w, "streaming unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	// Bound connection lifetime so reconnects recheck administrator/session state.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	snapshots := make(chan driveview.Snapshot, len(driveview.Resources))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = a.snapshots().Stream(ctx, id, func(snapshot driveview.Snapshot) error {
			select {
			case snapshots <- snapshot:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	defer func() { cancel(); <-done }()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	writer := http.NewResponseController(w)
	_ = writer.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		return
	}
	flusher.Flush()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			_ = writer.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := fmt.Fprint(w, "event: heartbeat\ndata: {}\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case snapshot := <-snapshots:
			_ = writer.SetWriteDeadline(time.Now().Add(10 * time.Second))
			payload, err := json.Marshal(snapshot)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
			if snapshot.Resource == driveview.Config && snapshot.Status == http.StatusNotFound {
				return
			}
		}
	}
}
