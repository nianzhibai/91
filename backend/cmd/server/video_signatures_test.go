package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveRetiredFrameSignatureCache(t *testing.T) {
	previews := t.TempDir()
	cache := filepath.Join(previews, "framesigs", "video.fsig")
	teaser := filepath.Join(previews, "video.mp4")
	for _, path := range []string{cache, teaser} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("asset"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	removeRetiredFrameSignatureCache(previews)
	removeRetiredFrameSignatureCache(previews)
	if _, err := os.Stat(filepath.Dir(cache)); !os.IsNotExist(err) {
		t.Fatalf("frame signature cache remains: %v", err)
	}
	if _, err := os.Stat(teaser); err != nil {
		t.Fatalf("teaser removed: %v", err)
	}
}
