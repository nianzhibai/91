package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/nianzhibai/91/backend/internal/drivelogin"
	"github.com/nianzhibai/91/backend/internal/drives/guangyapan"
	"github.com/nianzhibai/91/backend/internal/drives/p115"
	"github.com/nianzhibai/91/backend/internal/drives/p123"
	"github.com/nianzhibai/91/backend/internal/drives/quark"
	"github.com/nianzhibai/91/backend/internal/drives/wopan"
	"github.com/nianzhibai/91/backend/internal/drives/yun139"
)

func (a *AdminServer) driveLogins() *drivelogin.Manager {
	a.driveLoginOnce.Do(func() { a.driveLoginManager = drivelogin.New(a.driveLoginProviders()) })
	return a.driveLoginManager
}

// driveLoginProviders adapts each drive's QR client to the shared protocol.
// Credential keys match the drive's configuration fields, so clients can
// merge them into the drive form without knowing the provider.
func (a *AdminServer) driveLoginProviders() map[string]drivelogin.Provider {
	// Quark and 139 keep per-login state inside their clients, so every
	// session of those kinds must poll the same client.
	quarkClient := quark.NewQRClient(quark.QRConfig{
		UOPBaseURL: a.QuarkQRUOPBaseURL,
		PanBaseURL: a.QuarkQRPanBaseURL,
		HTTPClient: a.QuarkQRHTTPClient,
	})
	yun139Client := yun139.NewQRClient(yun139.QRConfig{HTTPClient: a.Yun139QRHTTPClient})
	p115Client := p115.NewQRClient(p115.QRConfig{HTTPClient: a.P115QRHTTPClient})
	p123Client := p123.NewQRClient(p123.QRConfig{UserAPIBaseURL: a.P123UserAPIBaseURL, HTTPClient: a.P123HTTPClient})
	wopanClient := wopan.NewQRClient(wopan.QRConfig{APIBaseURL: a.WopanQRAPIBaseURL, HTTPClient: a.WopanQRHTTPClient})
	guangYaPanClient := guangyapan.NewQRClient(guangyapan.QRConfig{
		AccountBaseURL: a.GuangYaPanAccountBaseURL,
		HTTPClient:     a.GuangYaPanHTTPClient,
	})

	return map[string]drivelogin.Provider{
		"quark": func(ctx context.Context) (drivelogin.Challenge, error) {
			s, err := quarkClient.Generate(ctx)
			return drivelogin.Challenge{
				QRCodeURL: s.QRCodeURL, QRImageDataURL: s.QRImageDataURL, ExpiresAt: parseLoginTime(s.ExpiresAt),
				Poll: func(ctx context.Context) (drivelogin.Status, error) {
					st, err := quarkClient.Poll(ctx, s.Token)
					return loginStatus(stateFromName(st.State), st.StatusText, map[string]string{"cookie": st.Cookie}), err
				},
			}, err
		},
		"p115": func(ctx context.Context) (drivelogin.Challenge, error) {
			s, err := p115Client.Generate(ctx)
			return drivelogin.Challenge{
				QRCodeURL: s.QRCodeURL, QRImageDataURL: s.QRImageDataURL,
				Poll: func(ctx context.Context) (drivelogin.Status, error) {
					st, err := p115Client.Poll(ctx, s.UID, s.Time, s.Sign)
					return loginStatus(stateFromName(st.State), st.StatusText, map[string]string{"cookie": st.Cookie}), err
				},
			}, err
		},
		"p123": func(ctx context.Context) (drivelogin.Challenge, error) {
			s, err := p123Client.Generate(ctx)
			return drivelogin.Challenge{
				QRCodeURL: s.QRCodeURL, QRImageDataURL: s.QRImageDataURL, ExpiresAt: parseLoginTime(s.ExpiresAt),
				Interval: 1800 * time.Millisecond,
				Poll: func(ctx context.Context) (drivelogin.Status, error) {
					st, err := p123Client.Poll(ctx, s.LoginUUID, s.UniID)
					states := map[int]drivelogin.State{1: drivelogin.Scanned, 2: drivelogin.Rejected, 3: drivelogin.Success, 4: drivelogin.Expired}
					return loginStatus(stateFromCode(states, st.LoginStatus), st.StatusText, map[string]string{"access_token": st.AccessToken}), err
				},
			}, err
		},
		"yun139": func(ctx context.Context) (drivelogin.Challenge, error) {
			s, err := yun139Client.Generate(ctx)
			return drivelogin.Challenge{
				QRCodeURL: s.QRCodeURL, QRImageDataURL: s.QRImageDataURL, ExpiresAt: parseLoginTime(s.ExpiresAt),
				Poll: func(ctx context.Context) (drivelogin.Status, error) {
					st, err := yun139Client.Poll(ctx, s.SessionID)
					return loginStatus(stateFromName(st.State), st.StatusText, map[string]string{"authorization": st.Authorization}), err
				},
			}, err
		},
		"wopan": func(ctx context.Context) (drivelogin.Challenge, error) {
			s, err := wopanClient.Generate(ctx)
			return drivelogin.Challenge{
				QRImageDataURL: s.QRImageDataURL, ExpiresAt: parseLoginTime(s.ExpiresAt), Interval: 1200 * time.Millisecond,
				Poll: func(ctx context.Context) (drivelogin.Status, error) {
					st, err := wopanClient.Poll(ctx, s.UUID)
					states := map[int]drivelogin.State{2: drivelogin.Scanned, 3: drivelogin.Success, 4: drivelogin.Expired}
					return loginStatus(stateFromCode(states, st.State), st.StatusText, map[string]string{
						"access_token": st.AccessToken, "refresh_token": st.RefreshToken, "family_id": st.FamilyID,
					}), err
				},
			}, err
		},
		"guangyapan": func(ctx context.Context) (drivelogin.Challenge, error) {
			s, err := guangYaPanClient.Generate(ctx)
			return drivelogin.Challenge{
				QRCodeURL: s.QRCodeURL, QRImageDataURL: s.QRImageDataURL, ExpiresAt: parseLoginTime(s.ExpiresAt),
				Interval: time.Duration(s.IntervalSeconds) * time.Second,
				Poll: func(ctx context.Context) (drivelogin.Status, error) {
					st, err := guangYaPanClient.Poll(ctx, s.DeviceCode)
					status := loginStatus(stateFromName(st.State), st.StatusText, map[string]string{
						"access_token": st.AccessToken, "refresh_token": st.RefreshToken,
					})
					// The device-code flow may ask clients to slow down.
					status.Interval = time.Duration(st.IntervalSeconds) * time.Second
					return status, err
				},
			}, err
		},
	}
}

func loginStatus(state drivelogin.State, message string, credentials map[string]string) drivelogin.Status {
	status := drivelogin.Status{State: state, Message: message}
	if state == drivelogin.Success {
		status.Credentials = credentials
	}
	return status
}

// stateFromName maps the state names used by provider clients.
func stateFromName(name string) drivelogin.State {
	switch name {
	case "waiting", "pending":
		return drivelogin.Waiting
	case "scanned":
		return drivelogin.Scanned
	case "success":
		return drivelogin.Success
	case "expired":
		return drivelogin.Expired
	case "canceled", "denied":
		return drivelogin.Rejected
	default:
		return drivelogin.Failed
	}
}

// stateFromCode maps numeric provider states; unlisted codes are still waiting.
func stateFromCode(states map[int]drivelogin.State, code int) drivelogin.State {
	if state, ok := states[code]; ok {
		return state
	}
	return drivelogin.Waiting
}

func parseLoginTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339, value)
	return parsed
}

type startDriveLoginRequest struct {
	Kind string `json:"kind"`
}

func (a *AdminServer) handleStartDriveLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body startDriveLoginRequest
	if err := decodeStrictJSON(w, r, &body, 1024); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	session, err := a.driveLogins().Start(r.Context(), body.Kind)
	if errors.Is(err, drivelogin.ErrUnsupported) {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	if err != nil {
		writeErr(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusCreated, session)
}

func (a *AdminServer) handleDriveLoginStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	result, err := a.driveLogins().Poll(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, drivelogin.ErrNotFound) {
		writeErr(w, r, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *AdminServer) handleCancelDriveLogin(w http.ResponseWriter, r *http.Request) {
	a.driveLogins().Cancel(chi.URLParam(r, "id"))
	w.WriteHeader(http.StatusNoContent)
}

// decodeStrictJSON reads exactly one JSON object without unknown fields.
func decodeStrictJSON(w http.ResponseWriter, r *http.Request, target any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}
