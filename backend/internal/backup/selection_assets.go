package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nianzhibai/91/backend/internal/drives/telegramstorage"
	"github.com/nianzhibai/91/backend/internal/localpath"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
)

func (m *Manager) snapshotSelectedPreviews(
	ctx context.Context,
	snapshotRoot string,
	state snapshotSelectionState,
) error {
	destination := filepath.Join(snapshotRoot, "payload", "previews")
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	videoIDs := sortedSetValues(state.SelectedVideoIDs)
	seen := make(map[string]struct{})
	for _, videoID := range videoIDs {
		candidates := append([]string{}, mediaasset.PreviewPathCandidates(m.previewPath, videoID)...)
		candidates = append(candidates, mediaasset.ThumbnailAssetPathCandidates(m.previewPath, videoID)...)
		candidates = append(candidates, mediaasset.FrameSignaturePath(m.previewPath, videoID))
		if previewPath := strings.TrimSpace(state.SelectedPreviewPaths[videoID]); previewPath != "" {
			if absolute, ok := localpath.Managed(m.previewPath, previewPath); ok {
				candidates = append(candidates, absolute)
			}
		}
		for _, candidate := range candidates {
			relative, ok := localpath.RelativeWithin(m.previewPath, candidate)
			if !ok {
				continue
			}
			relative = filepath.Clean(relative)
			if _, exists := seen[relative]; exists {
				continue
			}
			seen[relative] = struct{}{}
			if err := snapshotSelectedFile(ctx, m.previewPath, destination, candidate); err != nil {
				return fmt.Errorf("backup: snapshot preview asset %s: %w", videoID, err)
			}
		}
	}
	return nil
}

func (m *Manager) snapshotSelectedUploads(
	ctx context.Context,
	snapshotRoot string,
	state snapshotSelectionState,
) error {
	sourceRoot := filepath.Join(m.assetRoot, "uploads")
	destination := filepath.Join(snapshotRoot, "payload", "uploads")
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	for _, fileName := range sortedSetValues(state.SelectedUploadFiles) {
		if filepath.Base(fileName) != fileName || strings.ContainsAny(fileName, `/\`+"\x00") {
			return fmt.Errorf("backup: invalid local upload file id %q", fileName)
		}
		if _, ok := state.TelegramUploadFiles[fileName]; ok {
			continue
		}

		if err := snapshotSelectedFile(ctx, sourceRoot, destination, filepath.Join(sourceRoot, fileName)); err != nil {
			return fmt.Errorf("backup: snapshot upload %s: %w", fileName, err)
		}
	}
	return nil
}

func (m *Manager) snapshotSelectedCrawlerVideos(
	ctx context.Context,
	snapshotRoot string,
	state snapshotSelectionState,
) error {
	sourceRoot := filepath.Join(m.assetRoot, "scriptcrawlers")
	destinationRoot := filepath.Join(snapshotRoot, "payload", "scriptcrawlers")
	if err := os.MkdirAll(destinationRoot, 0o755); err != nil {
		return err
	}
	for _, driveID := range sortedSetValues(state.SelectedCrawlerDrives) {
		component, err := safeDirectoryComponent(driveID)
		if err != nil {
			return err
		}
		if err := snapshotSource(
			ctx,
			filepath.Join(sourceRoot, component),
			filepath.Join(destinationRoot, component),
		); err != nil {
			return fmt.Errorf("backup: snapshot crawler videos for %s: %w", driveID, err)
		}
	}
	return nil
}

func (m *Manager) snapshotSelectedLocalStorage(
	ctx context.Context,
	snapshotRoot string,
	state snapshotSelectionState,
) error {
	if len(state.LocalStorageRoots) == 0 {
		return nil
	}
	destinationRoot := filepath.Join(snapshotRoot, "payload", "localstorage")
	if err := os.MkdirAll(destinationRoot, 0o755); err != nil {
		return err
	}
	roots := append([]snapshotLocalStorageRoot(nil), state.LocalStorageRoots...)
	sort.Slice(roots, func(i, j int) bool { return roots[i].DriveID < roots[j].DriveID })
	for _, root := range roots {
		if root.ArchivePath == "" || filepath.Base(root.ArchivePath) != root.ArchivePath {
			return fmt.Errorf("backup: invalid local storage archive path for %s", root.DriveID)
		}
		destination := filepath.Join(destinationRoot, root.ArchivePath)
		if err := os.MkdirAll(destination, 0o755); err != nil {
			return err
		}
		for _, relative := range sortedSetValues(root.Files) {
			cleanRelative := filepath.Clean(filepath.FromSlash(relative))
			if cleanRelative == "." || cleanRelative == ".." ||
				strings.HasPrefix(cleanRelative, ".."+string(filepath.Separator)) || filepath.IsAbs(cleanRelative) {
				return fmt.Errorf("backup: invalid local storage relative path %q", relative)
			}
			if err := snapshotSelectedFile(
				ctx,
				root.SourcePath,
				destination,
				filepath.Join(root.SourcePath, cleanRelative),
			); err != nil {
				return fmt.Errorf("backup: snapshot local storage %s: %w", root.DriveID, err)
			}
		}
	}
	return nil
}

func snapshotSelectedFile(ctx context.Context, sourceRoot, destinationRoot, candidate string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	source, relative, info, ok, err := resolveContainedRegularFile(sourceRoot, candidate)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	target := filepath.Join(destinationRoot, relative)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	return linkOrCopy(source, target, info.Mode().Perm())
}

// resolveContainedRegularFile applies both lexical and filesystem-resolved
// containment. The latter is essential for individually selected files: Lstat
// rejects a final symlink, but it still follows symlinks in parent directories.
func resolveContainedRegularFile(
	sourceRoot string,
	candidate string,
) (string, string, os.FileInfo, bool, error) {
	clean, ok := localpath.Within(sourceRoot, candidate)
	if !ok {
		return "", "", nil, false, nil
	}
	info, err := os.Lstat(clean)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", nil, false, nil
		}
		return "", "", nil, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || excludedBackupFile(info.Name()) {
		return "", "", nil, false, nil
	}
	realRoot, err := filepath.EvalSymlinks(sourceRoot)
	if err != nil {
		return "", "", nil, false, err
	}
	realCandidate, err := filepath.EvalSymlinks(clean)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", nil, false, nil
		}
		return "", "", nil, false, err
	}
	if _, inside := localpath.Within(realRoot, realCandidate); !inside {
		return "", "", nil, false, nil
	}
	realInfo, err := os.Lstat(realCandidate)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", nil, false, nil
		}
		return "", "", nil, false, err
	}
	if realInfo.Mode()&os.ModeSymlink != 0 || !realInfo.Mode().IsRegular() ||
		!os.SameFile(info, realInfo) {
		return "", "", nil, false, nil
	}
	relative, ok := localpath.RelativeWithin(sourceRoot, clean)
	if !ok || relative == "." {
		return "", "", nil, false, nil
	}
	return realCandidate, relative, realInfo, true, nil
}

func safeDirectoryComponent(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." || filepath.Base(value) != value ||
		strings.ContainsAny(value, `/\`+"\x00") {
		return "", fmt.Errorf("backup: invalid drive id %q", value)
	}
	return value, nil
}

func sortedSetValues(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func (m *Manager) snapshotTelegramUploads(ctx context.Context, snapshotRoot string, state snapshotSelectionState) error {
	root := m.telegramFilesRoot()
	destination := filepath.Join(snapshotRoot, "payload", "uploads")
	if err := os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	for fileName, local := range state.TelegramUploadFiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		source, err := telegramstorage.Path(root, local)
		if err != nil {
			return fmt.Errorf("backup: TG video %s is unavailable", fileName)
		}
		info, err := os.Stat(source)
		if err != nil {
			return fmt.Errorf("backup: TG video %s is missing", fileName)
		}
		if err := linkOrCopy(source, filepath.Join(destination, fileName), info.Mode().Perm()); err != nil {
			return fmt.Errorf("backup: cannot snapshot TG video %s", fileName)
		}
	}
	return nil
}
