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
	"github.com/nianzhibai/91/backend/internal/backuptransfer"
)

const maxBackupTransferJSONBytes int64 = 32 << 10

func (a *AdminServer) handleListBackupTransfers(w http.ResponseWriter, r *http.Request) {
	if !a.backupTransfersAvailable(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, a.BackupTransfers.ListTransfers())
}

func (a *AdminServer) handleListBackupReceiveTransfers(w http.ResponseWriter, r *http.Request) {
	if !a.backupTransfersAvailable(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, a.BackupTransfers.ListReceiveTransfers(r.Context()))
}

func (a *AdminServer) handleCancelBackupReceiveTransfer(w http.ResponseWriter, r *http.Request) {
	if !a.backupTransfersAvailable(w, r) {
		return
	}
	if err := a.BackupTransfers.CancelReceiveTransfer(routeParam(r, "id")); err != nil {
		code := http.StatusInternalServerError
		switch {
		case errors.Is(err, backuptransfer.ErrImportNotFound):
			code = http.StatusNotFound
		case errors.Is(err, backuptransfer.ErrImportConflict),
			errors.Is(err, backuptransfer.ErrTransferTerminal):
			code = http.StatusConflict
		}
		writeErr(w, r, code, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (a *AdminServer) handleCreateBackupTransfer(w http.ResponseWriter, r *http.Request) {
	if !a.backupTransfersAvailable(w, r) {
		return
	}
	var input struct {
		TargetURL    string `json:"targetUrl"`
		ReceiveToken string `json:"receiveToken"`
	}
	if err := decodeBackupTransferJSON(w, r, &input); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	job, err := a.BackupTransfers.CreateTransfer(r.Context(), backuptransfer.CreateTransferInput{
		BackupID:     routeParam(r, "id"),
		TargetURL:    input.TargetURL,
		ReceiveToken: input.ReceiveToken,
	})
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, backup.ErrBackupNotFound) {
			code = http.StatusNotFound
		} else if errors.Is(err, backuptransfer.ErrTransferBusy) {
			code = http.StatusConflict
		}
		writeErr(w, r, code, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (a *AdminServer) handleCancelBackupTransfer(w http.ResponseWriter, r *http.Request) {
	if !a.backupTransfersAvailable(w, r) {
		return
	}
	job, err := a.BackupTransfers.CancelTransfer(routeParam(r, "id"))
	if err != nil {
		code := http.StatusConflict
		if errors.Is(err, backuptransfer.ErrTransferNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, r, code, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (a *AdminServer) handleRetryBackupTransfer(w http.ResponseWriter, r *http.Request) {
	if !a.backupTransfersAvailable(w, r) {
		return
	}
	job, err := a.BackupTransfers.RetryTransfer(routeParam(r, "id"))
	if err != nil {
		code := http.StatusConflict
		if errors.Is(err, backuptransfer.ErrTransferNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, r, code, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (a *AdminServer) handleListBackupReceiveTokens(w http.ResponseWriter, r *http.Request) {
	if !a.backupTransfersAvailable(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, a.BackupTransfers.ListReceiveTokens())
}

func (a *AdminServer) handleCreateBackupReceiveToken(w http.ResponseWriter, r *http.Request) {
	if !a.backupTransfersAvailable(w, r) {
		return
	}
	token, err := a.BackupTransfers.GenerateReceiveToken(10 * time.Minute)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, token)
}

func (a *AdminServer) handleRevokeBackupReceiveToken(w http.ResponseWriter, r *http.Request) {
	if !a.backupTransfersAvailable(w, r) {
		return
	}
	if err := a.BackupTransfers.RevokeReceiveToken(routeParam(r, "id")); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, backuptransfer.ErrUnauthorized) {
			code = http.StatusNotFound
		}
		writeErr(w, r, code, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *AdminServer) handlePeerBackupCapabilities(w http.ResponseWriter, r *http.Request) {
	if !a.backupTransfersAvailable(w, r) {
		return
	}
	if err := a.BackupTransfers.AuthorizeReceiveToken(peerBearerToken(r)); err != nil {
		writePeerTransferError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, a.BackupTransfers.Capabilities())
}

func (a *AdminServer) handlePeerBeginBackupImport(w http.ResponseWriter, r *http.Request) {
	if !a.backupTransfersAvailable(w, r) {
		return
	}
	var input backuptransfer.ImportRequest
	if err := decodeBackupTransferJSON(w, r, &input); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	status, err := a.BackupTransfers.BeginImport(r.Context(), peerBearerToken(r), input)
	if err != nil {
		writePeerTransferError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, status)
}

func (a *AdminServer) handlePeerBackupImportStatus(w http.ResponseWriter, r *http.Request) {
	if !a.backupTransfersAvailable(w, r) {
		return
	}
	status, err := a.BackupTransfers.ImportStatus(r.Context(), peerBearerToken(r), routeParam(r, "id"))
	if err != nil {
		writePeerTransferError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, status)
}

func (a *AdminServer) handlePeerBackupImportRange(w http.ResponseWriter, r *http.Request) {
	if !a.backupTransfersAvailable(w, r) {
		return
	}
	index, err := strconv.Atoi(routeParam(r, "index"))
	if err != nil || index < 0 {
		writeErr(w, r, http.StatusBadRequest, errors.New("传输区间序号无效"))
		return
	}
	offset, size, totalSize, err := peerContentRange(r.Header.Get("Content-Range"))
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	if r.ContentLength >= 0 && r.ContentLength != size {
		writeErr(w, r, http.StatusBadRequest, errors.New("Content-Length 与传输区间不一致"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, backuptransfer.TransferRangeSize+1)
	if err := a.BackupTransfers.PutImportRange(
		r.Context(),
		peerBearerToken(r),
		routeParam(r, "id"),
		index,
		offset,
		size,
		totalSize,
		r.Body,
	); err != nil {
		writePeerTransferError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (a *AdminServer) handlePeerFinalizeBackupImport(w http.ResponseWriter, r *http.Request) {
	if !a.backupTransfersAvailable(w, r) {
		return
	}
	status, err := a.BackupTransfers.FinalizeImport(r.Context(), peerBearerToken(r), routeParam(r, "id"))
	if err != nil {
		writePeerTransferError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, status)
}

func (a *AdminServer) handlePeerCancelBackupImport(w http.ResponseWriter, r *http.Request) {
	if !a.backupTransfersAvailable(w, r) {
		return
	}
	if err := a.BackupTransfers.CancelImport(r.Context(), peerBearerToken(r), routeParam(r, "id")); err != nil {
		writePeerTransferError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func decodeBackupTransferJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBackupTransferJSONBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("请求只能包含一个 JSON 对象")
		}
		return err
	}
	return nil
}

func peerBearerToken(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	parts := strings.Fields(value)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func peerContentRange(value string) (offset, size, total int64, err error) {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "bytes") {
		return 0, 0, 0, errors.New("必须提供有效的 Content-Range")
	}
	rangeAndTotal := strings.Split(fields[1], "/")
	if len(rangeAndTotal) != 2 {
		return 0, 0, 0, errors.New("Content-Range 格式无效")
	}
	bounds := strings.Split(rangeAndTotal[0], "-")
	if len(bounds) != 2 {
		return 0, 0, 0, errors.New("Content-Range 格式无效")
	}
	start, startErr := strconv.ParseInt(bounds[0], 10, 64)
	end, endErr := strconv.ParseInt(bounds[1], 10, 64)
	total, totalErr := strconv.ParseInt(rangeAndTotal[1], 10, 64)
	if startErr != nil || endErr != nil || totalErr != nil || start < 0 || end < start || total <= end {
		return 0, 0, 0, errors.New("Content-Range 范围无效")
	}
	return start, end - start + 1, total, nil
}

func writePeerTransferError(w http.ResponseWriter, r *http.Request, err error) {
	w.Header().Set("Cache-Control", "no-store")
	code := http.StatusBadRequest
	switch {
	case errors.Is(err, backuptransfer.ErrUnavailable):
		code = http.StatusServiceUnavailable
	case errors.Is(err, backuptransfer.ErrUnauthorized):
		code = http.StatusUnauthorized
		w.Header().Set("WWW-Authenticate", `Bearer realm="backup-transfer"`)
	case errors.Is(err, backuptransfer.ErrTokenBound):
		code = http.StatusForbidden
	case errors.Is(err, backuptransfer.ErrImportNotFound), errors.Is(err, backup.ErrUploadNotFound):
		code = http.StatusNotFound
	case errors.Is(err, backuptransfer.ErrImportCanceled):
		code = http.StatusGone
	case errors.Is(err, backuptransfer.ErrImportConflict),
		errors.Is(err, backup.ErrUploadIncomplete),
		errors.Is(err, backup.ErrUploadFinalizing),
		errors.Is(err, backup.ErrUploadRangeBusy),
		errors.Is(err, backuptransfer.ErrTransferTerminal):
		code = http.StatusConflict
	case errors.Is(err, backup.ErrInsufficientSpace):
		code = http.StatusInsufficientStorage
	}
	writeErr(w, r, code, err)
}

func (a *AdminServer) backupTransfersAvailable(w http.ResponseWriter, r *http.Request) bool {
	if a.BackupTransfers != nil {
		return true
	}
	writeErr(w, r, http.StatusServiceUnavailable, backuptransfer.ErrUnavailable)
	return false
}
