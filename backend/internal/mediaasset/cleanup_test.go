package mediaasset

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveGeneratedVideoAssetsRequiresConfiguredRoot(t *testing.T) {
	if err := RemoveGeneratedVideoAssets("", "video-a", ""); !errors.Is(err, ErrGeneratedAssetRootRequired) {
		t.Fatalf("error = %v, want ErrGeneratedAssetRootRequired", err)
	}
}

func TestRemoveGeneratedVideoAssetsRemovesOwnedFilesOnly(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.mp4")
	paths := []string{
		PreviewPath(root, "video-a"),
		ThumbnailPath(root, "video-a"),
		ShortsBackgroundPath(root, "video-a"),
		FrameSignaturePath(root, "video-a"),
		outside,
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("asset"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	if err := RemoveGeneratedVideoAssets(root, "video-a", outside); err != nil {
		t.Fatalf("RemoveGeneratedVideoAssets: %v", err)
	}
	for _, path := range paths[:len(paths)-1] {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("generated asset %s remains, stat error=%v", path, err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside file changed: %v", err)
	}
}
