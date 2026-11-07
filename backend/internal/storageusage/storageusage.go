package storageusage

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/nianzhibai/91/backend/internal/localpath"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
)

type VideoAssetRef struct {
	ID           string
	DriveID      string
	PreviewLocal string
}

type DiskStats struct {
	AvailableBytes int64 `json:"availableBytes"`
	CapacityBytes  int64 `json:"capacityBytes"`
}

type DriveUsage struct {
	ThumbnailBytes int64 `json:"thumbnailBytes"`
	TeaserBytes    int64 `json:"teaserBytes"`
	TotalBytes     int64 `json:"totalBytes"`
}

type Usage struct {
	ThumbnailBytes int64                 `json:"thumbnailBytes"`
	TeaserBytes    int64                 `json:"teaserBytes"`
	TotalBytes     int64                 `json:"totalBytes"`
	AvailableBytes int64                 `json:"availableBytes"`
	CapacityBytes  int64                 `json:"capacityBytes"`
	Drives         map[string]DriveUsage `json:"drives"`
}

func Compute(
	localDir string,
	refs []VideoAssetRef,
	driveIDs []string,
	diskStats func(string) (DiskStats, error),
) (Usage, error) {
	return ComputeContext(context.Background(), localDir, refs, driveIDs, diskStats)
}

func ComputeContext(ctx context.Context, localDir string, refs []VideoAssetRef, driveIDs []string, diskStats func(string) (DiskStats, error)) (Usage, error) {
	localDir = strings.TrimSpace(localDir)
	if localDir == "" {
		return Usage{}, errors.New("local preview dir is not configured")
	}
	if diskStats == nil {
		diskStats = func(string) (DiskStats, error) { return DiskStats{}, nil }
	}
	stats, err := diskStats(localDir)
	if err != nil {
		return Usage{}, err
	}

	out := Usage{
		AvailableBytes: stats.AvailableBytes,
		CapacityBytes:  stats.CapacityBytes,
		Drives:         make(map[string]DriveUsage, len(driveIDs)),
	}
	allowed := make(map[string]bool, len(driveIDs))
	for _, id := range driveIDs {
		if id == "" {
			continue
		}
		allowed[id] = true
		out.Drives[id] = DriveUsage{}
	}

	seen := make(map[string]bool)
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return Usage{}, err
		}
		if ref.ID == "" || ref.DriveID == "" || !allowed[ref.DriveID] {
			continue
		}
		driveUsage := out.Drives[ref.DriveID]
		for _, thumbPath := range mediaasset.ThumbnailAssetPathCandidates(localDir, ref.ID) {
			if size, exists, err := regularFileSize(thumbPath); err != nil {
				return Usage{}, err
			} else if exists {
				key := ref.DriveID + "\x00thumb\x00" + thumbPath
				if !seen[key] {
					driveUsage.ThumbnailBytes += size
					seen[key] = true
				}
			}
		}

		if previewPath, ok := localpath.Managed(localDir, ref.PreviewLocal); ok {
			if size, exists, err := regularFileSize(previewPath); err != nil {
				return Usage{}, err
			} else if exists {
				key := ref.DriveID + "\x00teaser\x00" + previewPath
				if !seen[key] {
					driveUsage.TeaserBytes += size
					seen[key] = true
				}
			}
		}

		driveUsage.TotalBytes = driveUsage.ThumbnailBytes + driveUsage.TeaserBytes
		out.Drives[ref.DriveID] = driveUsage
	}

	for _, driveUsage := range out.Drives {
		out.ThumbnailBytes += driveUsage.ThumbnailBytes
		out.TeaserBytes += driveUsage.TeaserBytes
	}
	out.TotalBytes = out.ThumbnailBytes + out.TeaserBytes
	return out, nil
}

func regularFileSize(path string) (int64, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if !info.Mode().IsRegular() {
		return 0, false, nil
	}
	return info.Size(), true, nil
}

func pathWithin(root, path string) (string, bool) {
	return localpath.Within(root, path)
}
