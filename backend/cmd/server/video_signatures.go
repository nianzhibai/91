package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/nianzhibai/91/backend/internal/applog"
)

// removeRetiredFrameSignatureCache deletes the teaser frame cache that content
// matching kept before signatures were stored in the catalog.
func removeRetiredFrameSignatureCache(previewDir string) {
	if err := os.RemoveAll(filepath.Join(previewDir, "framesigs")); err != nil {
		applog.Warn(context.Background(), "remove retired frame signature cache failed", err, "component", "startup")
	}
}
