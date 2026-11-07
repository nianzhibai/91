package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/nianzhibai/91/backend/internal/drives/yun139"
)

func (a *AdminServer) getYun139QRClient() *yun139.QRClient {
	a.yun139QRMu.Lock()
	defer a.yun139QRMu.Unlock()
	if a.yun139QRClient == nil {
		a.yun139QRClient = yun139.NewQRClient(yun139.QRConfig{HTTPClient: a.Yun139QRHTTPClient})
	}
	return a.yun139QRClient
}

func (a *AdminServer) handleYun139QRStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	session, err := a.getYun139QRClient().Generate(r.Context())
	if err != nil {
		writeErr(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (a *AdminServer) handleYun139QRStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	var body struct {
		SessionID string `json:"sessionId"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeErr(w, r, http.StatusBadRequest, errors.New("request body must contain one JSON object"))
		return
	}
	if strings.TrimSpace(body.SessionID) == "" {
		writeErr(w, r, http.StatusBadRequest, errors.New("sessionId is required"))
		return
	}
	status, err := a.getYun139QRClient().Poll(r.Context(), body.SessionID)
	if err != nil {
		writeErr(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}
