package mediaasset

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/video-site/backend/internal/localpath"
)

var ErrGeneratedAssetRootRequired = errors.New("mediaasset: generated asset root is required")

// RemoveGeneratedVideoAssets removes every locally generated artifact owned by
// videoID. The original media source is deliberately not among the candidates.
// Paths outside localDir are ignored even when a stale catalog row points at
// them.
func RemoveGeneratedVideoAssets(localDir, videoID, previewLocal string) error {
	localDir = strings.TrimSpace(localDir)
	videoID = strings.TrimSpace(videoID)
	if videoID == "" {
		return nil
	}
	if localDir == "" {
		return ErrGeneratedAssetRootRequired
	}
	candidates := []string{previewLocal}
	candidates = append(candidates, PreviewPathCandidates(localDir, videoID)...)
	candidates = append(candidates, ThumbnailAssetPathCandidates(localDir, videoID)...)
	candidates = append(candidates, FrameSignaturePath(localDir, videoID))

	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		clean, ok := localpath.Within(localDir, candidate)
		if !ok {
			continue
		}
		if _, exists := seen[clean]; exists {
			continue
		}
		seen[clean] = struct{}{}
		info, err := os.Stat(clean)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("stat generated asset %s: %w", clean, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(clean); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove generated asset %s: %w", clean, err)
		}
	}
	return nil
}
