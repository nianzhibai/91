package mediaasset

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

const maxPlainStemBytes = 180
const maxLegacyFilenameBytes = 255

func PreviewPath(localDir, videoID string) string {
	return filepath.Join(localDir, PreviewFilename(videoID))
}

func ThumbnailPath(localDir, videoID string) string {
	return ThumbnailPathInDir(filepath.Join(localDir, "thumbs"), videoID)
}

func ThumbnailPathInDir(thumbDir, videoID string) string {
	return filepath.Join(thumbDir, ThumbnailFilename(videoID))
}

func PreviewPathCandidates(localDir, videoID string) []string {
	return pathCandidates(localDir, videoID, ".mp4", "")
}

func ThumbnailPathCandidates(localDir, videoID string) []string {
	return pathCandidates(localDir, videoID, ".jpg", "thumbs")
}

// ThumbnailAssetPathCandidates returns every local thumbnail-derived asset
// owned by a video. Keep deletion and storage accounting on this shared list so
// lazily generated variants cannot outlive their source video unnoticed.
func ThumbnailAssetPathCandidates(localDir, videoID string) []string {
	paths := ThumbnailPathCandidates(localDir, videoID)
	return append(paths, ShortsBackgroundPath(localDir, videoID))
}


func PreviewFilename(videoID string) string {
	return safeFilename(videoID, ".mp4")
}

func ThumbnailFilename(videoID string) string {
	return safeFilename(videoID, ".jpg")
}

func pathCandidates(localDir, videoID, ext, subdir string) []string {
	safe := safeFilename(videoID, ext)
	legacy := videoID + ext
	base := localDir
	if subdir != "" {
		base = filepath.Join(base, subdir)
	}
	out := []string{filepath.Join(base, safe)}
	if legacy != safe && isPlainSafeStem(videoID) && len([]byte(legacy)) <= maxLegacyFilenameBytes {
		out = append(out, filepath.Join(base, legacy))
	}
	return out
}

func safeFilename(videoID, ext string) string {
	if isPlainSafeStem(videoID) && len([]byte(videoID))+len(ext) <= maxPlainStemBytes {
		return videoID + ext
	}
	sum := sha256.Sum256([]byte(videoID))
	return "v-" + hex.EncodeToString(sum[:]) + ext
}

func isPlainSafeStem(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." {
		return false
	}
	return !strings.ContainsAny(value, `/\`+"\x00")
}
