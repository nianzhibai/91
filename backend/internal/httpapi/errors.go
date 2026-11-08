// Package httpapi contains the shared wire representation of HTTP API errors.
package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
)

func Code(status int) string {
	return strings.ReplaceAll(strings.ToLower(http.StatusText(status)), " ", "_")
}

func Error(w http.ResponseWriter, message string, status int) {
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"code":  Code(status),
		"error": message,
	})
}
