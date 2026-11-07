package backup

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nianzhibai/91/backend/internal/localpath"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
)

// prepareMergedPreview creates one replacement preview root. Target assets are
// retained for merge-owned resources, while source assets win for cloud and
// crawler resources that keep replacement semantics.
func prepareMergedPreview(
	ctx context.Context,
	stageRoot string,
	source string,
	target string,
	preservedTargetPaths []string,
	preserveAllTarget bool,
	sourceWins []string,
) (string, error) {
	merged := filepath.Join(stageRoot, "payload", "previews-merged")
	if err := snapshotSource(ctx, source, merged); err != nil {
		return "", fmt.Errorf("backup: copy restored preview root: %w", err)
	}
	if preserveAllTarget {
		if err := overlaySource(ctx, target, merged); err != nil {
			return "", fmt.Errorf("backup: overlay target preview root: %w", err)
		}
	} else if err := overlayRelativeFiles(ctx, target, merged, preservedTargetPaths); err != nil {
		return "", fmt.Errorf("backup: preserve merged target previews: %w", err)
	}
	if err := overlayRelativeFiles(ctx, source, merged, sourceWins); err != nil {
		return "", fmt.Errorf("backup: overlay replacement previews: %w", err)
	}
	return merged, nil
}

func prepareMergedUploadStorage(ctx context.Context, stageRoot, source, target string) (string, error) {
	merged := filepath.Join(stageRoot, "payload", "uploads-merged")
	if err := snapshotSource(ctx, source, merged); err != nil {
		return "", fmt.Errorf("backup: copy restored upload storage: %w", err)
	}
	if err := overlaySource(ctx, target, merged); err != nil {
		return "", fmt.Errorf("backup: preserve target upload storage: %w", err)
	}
	return merged, nil
}

func collectPreviewAssetPaths(
	ctx context.Context,
	databasePath string,
	previewRoot string,
	mergeOwned bool,
) ([]string, error) {
	database, err := sql.Open("sqlite", databasePath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	rows, err := database.QueryContext(ctx, `
		SELECT videos.id, videos.drive_id, COALESCE(videos.preview_local, ''), COALESCE(drives.kind, '')
		  FROM videos
		  LEFT JOIN drives ON drives.id = videos.drive_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paths := make(map[string]struct{})
	for rows.Next() {
		var videoID, driveID, previewLocal, driveKind string
		if err := rows.Scan(&videoID, &driveID, &previewLocal, &driveKind); err != nil {
			return nil, err
		}
		if driveUsesMergeRestore(driveID, driveKind) != mergeOwned {
			continue
		}
		candidates := append([]string{}, mediaasset.PreviewPathCandidates(previewRoot, videoID)...)
		candidates = append(candidates, mediaasset.ThumbnailAssetPathCandidates(previewRoot, videoID)...)
		candidates = append(candidates, mediaasset.FrameSignaturePath(previewRoot, videoID))
		if strings.TrimSpace(previewLocal) != "" {
			if absolute, ok := localpath.Managed(previewRoot, previewLocal); ok {
				candidates = append(candidates, absolute)
			}
		}
		for _, candidate := range candidates {
			relative, ok := localpath.RelativeWithin(previewRoot, candidate)
			if !ok || relative == "." {
				continue
			}
			paths[filepath.ToSlash(filepath.Clean(relative))] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]string, 0, len(paths))
	for relative := range paths {
		result = append(result, relative)
	}
	sort.Strings(result)
	return result, nil
}

type preparedLocalStorage struct {
	DriveID string
	Source  string
	Target  string
}

func prepareIsolatedLocalStorage(
	ctx context.Context,
	stageRoot string,
	manifestRoots []LocalStorageRoot,
	plans []isolatedLocalStorageRestore,
) ([]preparedLocalStorage, error) {
	if len(plans) == 0 {
		return nil, nil
	}
	rootsByDrive := make(map[string]LocalStorageRoot, len(manifestRoots))
	for _, root := range manifestRoots {
		rootsByDrive[root.DriveID] = root
	}
	orderedPlans := append([]isolatedLocalStorageRestore(nil), plans...)
	sort.Slice(orderedPlans, func(i, j int) bool {
		return orderedPlans[i].SourceDriveID < orderedPlans[j].SourceDriveID
	})
	prepared := make([]preparedLocalStorage, 0, len(orderedPlans))
	for _, plan := range orderedPlans {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		root, exists := rootsByDrive[plan.SourceDriveID]
		if !exists {
			return nil, fmt.Errorf("backup: local storage restore plan references undeclared drive %s", plan.SourceDriveID)
		}
		source := filepath.Join(stageRoot, "payload", "localstorage", root.ArchivePath)
		// ZIP directory entries are metadata-only and are not extracted. Create an
		// empty source root so a selected drive with no referenced files still
		// restores as a valid empty isolated drive.
		if err := os.MkdirAll(source, 0o755); err != nil {
			return nil, err
		}
		prepared = append(prepared, preparedLocalStorage{
			DriveID: plan.DriveID,
			Source:  source,
			Target:  plan.TargetPath,
		})
	}
	return prepared, nil
}

func overlaySource(ctx context.Context, source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if !info.IsDir() {
		return overlayFile(ctx, source, destination, info)
	}
	if err := os.MkdirAll(destination, info.Mode().Perm()); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if current == source {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(source, current)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			if excludedBackupDir(entry.Name()) {
				return filepath.SkipDir
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if excludedBackupFile(entry.Name()) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return overlayFile(ctx, current, target, info)
	})
}

func overlayRelativeFiles(ctx context.Context, sourceRoot, destinationRoot string, relativePaths []string) error {
	for _, relative := range relativePaths {
		cleanRelative := filepath.Clean(filepath.FromSlash(relative))
		if cleanRelative == "." || cleanRelative == ".." || filepath.IsAbs(cleanRelative) ||
			strings.HasPrefix(cleanRelative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("backup: invalid preview asset path %q", relative)
		}
		source, _, info, ok, err := resolveContainedRegularFile(
			sourceRoot,
			filepath.Join(sourceRoot, cleanRelative),
		)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := overlayFile(ctx, source, filepath.Join(destinationRoot, cleanRelative), info); err != nil {
			return err
		}
	}
	return nil
}

func overlayFile(ctx context.Context, source, destination string, info fs.FileInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if existing, err := os.Lstat(destination); err == nil {
		if existing.IsDir() {
			if err := os.RemoveAll(destination); err != nil {
				return err
			}
		} else if existing.Mode()&os.ModeSymlink != 0 || existing.Mode().IsRegular() {
			if err := os.Remove(destination); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("backup: unsupported existing overlay target %s", destination)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return linkOrCopy(source, destination, info.Mode().Perm())
}

func restoreTargetPathsOverlap(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if left == right {
		return true
	}
	relative, err := filepath.Rel(left, right)
	if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return true
	}
	relative, err = filepath.Rel(right, left)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
