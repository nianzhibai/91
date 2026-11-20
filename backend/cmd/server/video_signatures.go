package main

import (
	"log"
	"os"
	"path/filepath"
)

// removeRetiredFrameSignatureCache deletes the teaser frame cache that content
// matching kept before signatures were stored in the catalog.
func removeRetiredFrameSignatureCache(previewDir string) {
	if err := os.RemoveAll(filepath.Join(previewDir, "framesigs")); err != nil {
		log.Printf("[startup] remove retired frame signature cache: %v", err)
	}
}
