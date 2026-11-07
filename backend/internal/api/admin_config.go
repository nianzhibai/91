package api

import (
	"errors"
	"io"
	"net/http"

	"github.com/nianzhibai/91/backend/internal/config"
)

const maxConfigYAMLBytes = 2 << 20

func (a *AdminServer) previewEnabled() bool {
	return a.ConfigManager.LiveSettings().PreviewEnabled
}

func (a *AdminServer) requirePreviewEnabled(w http.ResponseWriter) bool {
	if !a.previewEnabled() {
		http.Error(w, "全局预览视频生成已关闭", http.StatusConflict)
		return false
	}
	return true
}

func (a *AdminServer) handleGetConfigYAML(w http.ResponseWriter, r *http.Request) {
	if a.ConfigManager == nil {
		writeErr(w, r, http.StatusServiceUnavailable, errors.New("configuration manager is unavailable"))
		return
	}
	data, version, err := a.ConfigManager.ReadYAML()
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("ETag", `"`+version+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (a *AdminServer) handlePutConfigYAML(w http.ResponseWriter, r *http.Request) {
	if a.ConfigManager == nil {
		writeErr(w, r, http.StatusServiceUnavailable, errors.New("configuration manager is unavailable"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigYAMLBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeErr(w, r, http.StatusRequestEntityTooLarge, errors.New("config.yaml exceeds 2 MiB"))
			return
		}
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	expectedVersion := r.Header.Get("If-Match")
	if expectedVersion == "*" {
		expectedVersion = ""
	}
	if candidate, parseErr := config.Parse(data); parseErr == nil && candidate.Generation.UseGPU && !a.ConfigManager.LiveSettings().UseGPU {
		if a.GPU == nil {
			writeErr(w, r, http.StatusServiceUnavailable, errors.New("GPU 检测服务不可用"))
			return
		}
		status := a.GPU.Check(r.Context())
		if !status.Available {
			writeErr(w, r, http.StatusBadRequest, errors.New(status.Reason))
			return
		}
	}
	result, err := a.ConfigManager.ReplaceYAML(data, expectedVersion)
	if err != nil {
		switch {
		case errors.Is(err, config.ErrVersionConflict):
			writeErr(w, r, http.StatusConflict, err)
		case errors.Is(err, config.ErrInvalidNightlyStartTime),
			errors.Is(err, config.ErrInvalidNightlyTimezone),
			errors.Is(err, config.ErrAdminConfigRemoved):
			writeErr(w, r, http.StatusBadRequest, err)
		default:
			// YAML syntax and type errors are validation failures too. Disk I/O
			// errors retain a 500 response; the parser prefixes its errors.
			if configYAMLInvalid(data) {
				writeErr(w, r, http.StatusBadRequest, err)
			} else {
				writeErr(w, r, http.StatusInternalServerError, err)
			}
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("ETag", `"`+result.Version+`"`)
	writeJSON(w, http.StatusOK, result)
}

func (a *AdminServer) handleGPUStatus(w http.ResponseWriter, r *http.Request) {
	if a.GPU == nil {
		writeErr(w, r, http.StatusServiceUnavailable, errors.New("GPU 检测服务不可用"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, a.GPU.Status())
}

func (a *AdminServer) handleGPUProbe(w http.ResponseWriter, r *http.Request) {
	if a.GPU == nil {
		writeErr(w, r, http.StatusServiceUnavailable, errors.New("GPU 检测服务不可用"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, a.GPU.Probe(r.Context()))
}

func configYAMLInvalid(data []byte) bool {
	_, err := config.Parse(data)
	return err != nil
}
