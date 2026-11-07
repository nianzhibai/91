package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/backup"
)

const (
	restoreRestartGracePeriod  = 2 * time.Second
	maxRestoreResponseMessages = 20
)

func (a *AdminServer) handleListBackups(w http.ResponseWriter, r *http.Request) {
	if !a.backupsAvailable(w, r) {
		return
	}
	result, err := a.Backups.List(r.Context())
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

func (a *AdminServer) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	if !a.backupsAvailable(w, r) {
		return
	}
	var input struct {
		backup.BackupSelection
		Selection *backup.BackupSelection `json:"selection,omitempty"`
	}
	bodyPresent := false
	decoder := json.NewDecoder(io.LimitReader(r.Body, 8<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		if !errors.Is(err, io.EOF) {
			writeErr(w, r, http.StatusBadRequest, err)
			return
		}
	} else {
		bodyPresent = true
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			writeErr(w, r, http.StatusBadRequest, errors.New("备份选项请求包含单个 JSON 对象"))
			return
		}
	}
	selection := input.BackupSelection
	if input.Selection != nil {
		selection = *input.Selection
	}
	if bodyPresent {
		if !selection.Any() {
			writeErr(w, r, http.StatusBadRequest, backup.ErrNoBackupContent)
			return
		}
	}
	requested := []backup.BackupSelection(nil)
	if bodyPresent {
		requested = append(requested, selection)
	}
	status, err := a.Backups.Create(r.Context(), requested...)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, backup.ErrTaskRunning) || errors.Is(err, backup.ErrRestorePending) {
			code = http.StatusConflict
		} else if errors.Is(err, backup.ErrInsufficientSpace) {
			code = http.StatusInsufficientStorage
		} else if errors.Is(err, backup.ErrNoBackupContent) {
			code = http.StatusBadRequest
		}
		writeErr(w, r, code, err)
		return
	}
	writeJSON(w, http.StatusAccepted, status)
}

func (a *AdminServer) handleCancelBackup(w http.ResponseWriter, r *http.Request) {
	if !a.backupsAvailable(w, r) {
		return
	}
	if err := a.Backups.Cancel(); err != nil {
		code := http.StatusConflict
		if !errors.Is(err, backup.ErrNoRunningTask) {
			code = http.StatusInternalServerError
		}
		writeErr(w, r, code, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (a *AdminServer) handleDownloadBackup(w http.ResponseWriter, r *http.Request) {
	if !a.backupsAvailable(w, r) {
		return
	}
	file, info, name, err := a.Backups.OpenBackup(routeParam(r, "id"))
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, backup.ErrBackupNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, r, code, err)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, info.ModTime(), file)
}

func (a *AdminServer) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	if !a.backupsAvailable(w, r) {
		return
	}
	if a.BackupTransfers != nil && a.BackupTransfers.BackupInUse(routeParam(r, "id")) {
		writeErr(w, r, http.StatusConflict, errors.New("该备份正在发送到其它服务器，不能删除"))
		return
	}
	if err := a.Backups.Delete(routeParam(r, "id")); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, backup.ErrBackupNotFound) {
			code = http.StatusNotFound
		} else if errors.Is(err, backup.ErrRestorePending) ||
			strings.Contains(err.Error(), "等待恢复") {
			code = http.StatusConflict
		}
		writeErr(w, r, code, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *AdminServer) handleBeginBackupUpload(w http.ResponseWriter, r *http.Request) {
	if !a.backupsAvailable(w, r) {
		return
	}
	var input struct {
		FileName string `json:"fileName"`
		Size     int64  `json:"size"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 8<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	session, err := a.Backups.BeginUpload(r.Context(), backup.BeginUploadInput{
		FileName: input.FileName,
		Size:     input.Size,
	})
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, backup.ErrInsufficientSpace) {
			code = http.StatusInsufficientStorage
		}
		writeErr(w, r, code, err)
		return
	}
	writeJSON(w, http.StatusCreated, session)
}

func (a *AdminServer) handleBackupUploadStatus(w http.ResponseWriter, r *http.Request) {
	if !a.backupsAvailable(w, r) {
		return
	}
	session, err := a.Backups.UploadStatus(routeParam(r, "id"))
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, backup.ErrUploadNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, r, code, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, session)
}

func (a *AdminServer) handleBackupUploadChunk(w http.ResponseWriter, r *http.Request) {
	if !a.backupsAvailable(w, r) {
		return
	}
	index, err := strconv.Atoi(routeParam(r, "index"))
	if err != nil || index < 0 {
		writeErr(w, r, http.StatusBadRequest, errors.New("分片序号无效"))
		return
	}
	session, err := a.Backups.PutChunk(
		r.Context(),
		routeParam(r, "id"),
		index,
		http.MaxBytesReader(w, r.Body, backup.ChunkSize+1),
	)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, backup.ErrUploadNotFound) {
			code = http.StatusNotFound
		} else if errors.Is(err, backup.ErrUploadFinalizing) {
			code = http.StatusConflict
		}
		writeErr(w, r, code, err)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (a *AdminServer) handleFinalizeBackupUpload(w http.ResponseWriter, r *http.Request) {
	if !a.backupsAvailable(w, r) {
		return
	}
	var input struct {
		SHA256 string `json:"sha256"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("请求只能包含一个 JSON 对象")
		}
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	record, err := a.Backups.FinalizeUpload(r.Context(), routeParam(r, "id"), input.SHA256)
	if err != nil {
		code := http.StatusBadRequest
		switch {
		case errors.Is(err, backup.ErrUploadNotFound):
			code = http.StatusNotFound
		case errors.Is(err, backup.ErrUploadIncomplete), errors.Is(err, backup.ErrUploadFinalizing):
			code = http.StatusConflict
		case errors.Is(err, backup.ErrInsufficientSpace):
			code = http.StatusInsufficientStorage
		}
		writeErr(w, r, code, err)
		return
	}
	writeJSON(w, http.StatusCreated, record)
}

func (a *AdminServer) handleCancelBackupUpload(w http.ResponseWriter, r *http.Request) {
	if !a.backupsAvailable(w, r) {
		return
	}
	if err := a.Backups.CancelUpload(routeParam(r, "id")); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, backup.ErrUploadNotFound) {
			code = http.StatusNotFound
		} else if errors.Is(err, backup.ErrUploadFinalizing) {
			code = http.StatusConflict
		}
		writeErr(w, r, code, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *AdminServer) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	if !a.backupsAvailable(w, r) {
		return
	}
	var request backup.RestoreRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 8<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	if request.Confirmation != "确认恢复" {
		writeErr(w, r, http.StatusBadRequest, errors.New("请输入固定确认文本“确认恢复”"))
		return
	}
	report, err := a.Backups.PrepareRestore(r.Context(), routeParam(r, "id"))
	if err != nil {
		code := http.StatusBadRequest
		switch {
		case errors.Is(err, backup.ErrBackupNotFound):
			code = http.StatusNotFound
		case errors.Is(err, backup.ErrTaskRunning), errors.Is(err, backup.ErrRestorePending):
			code = http.StatusConflict
		case errors.Is(err, backup.ErrInsufficientSpace):
			code = http.StatusInsufficientStorage
		}
		writeErr(w, r, code, err)
		return
	}
	writeRestoreAccepted(w, a.Backups.RestartManaged(), report)
	// The response is intentionally bounded and flushed before the process
	// begins a controlled restart. A restore can contain tens of thousands of
	// files, so returning its complete manifest here can race the restart and
	// make a successfully staged restore look like a failed browser fetch.
	time.AfterFunc(restoreRestartGracePeriod, a.Backups.RequestRestart)
}

func writeRestoreAccepted(w http.ResponseWriter, restartManaged bool, report backup.ValidationReport) {
	writeJSON(w, http.StatusAccepted, backup.RestoreResult{
		OK:             true,
		Restarting:     true,
		RestartManaged: restartManaged,
		Report:         compactRestoreReport(report),
	})
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func compactRestoreReport(report backup.ValidationReport) backup.ValidationReport {
	// The durable restore report retains the complete manifest. The initial
	// acknowledgement only needs a summary; keeping it small makes it safe to
	// send immediately before the service restart.
	report.Manifest.Files = nil
	report.PathRewrites = limitRestoreResponseMessages(report.PathRewrites)
	report.LocalStorageWarnings = limitRestoreResponseMessages(report.LocalStorageWarnings)
	report.MissingAssets = limitRestoreResponseMessages(report.MissingAssets)
	report.Warnings = limitRestoreResponseMessages(report.Warnings)
	return report
}

func limitRestoreResponseMessages(messages []string) []string {
	if len(messages) == 0 {
		return nil
	}
	if len(messages) > maxRestoreResponseMessages {
		messages = messages[:maxRestoreResponseMessages]
	}
	return append([]string(nil), messages...)
}

func (a *AdminServer) backupsAvailable(w http.ResponseWriter, r *http.Request) bool {
	if a.Backups != nil {
		return true
	}
	writeErr(w, r, http.StatusServiceUnavailable, errors.New("备份服务未配置"))
	return false
}
