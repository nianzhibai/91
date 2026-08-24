package scanner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"strings"

	"github.com/video-site/backend/internal/drives"
)

func (s *Scanner) discover(ctx context.Context, startDirID string, stats *Stats, progress progressFunc) (Snapshot, error) {
	if err := validateSource(s); err != nil {
		return Snapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if startDirID == "" {
		startDirID = s.Drive.RootID()
	}
	snapshot := Snapshot{
		DriveID:        s.Drive.ID(),
		DriveKind:      s.Drive.Kind(),
		StartDirID:     startDirID,
		FullDriveScan:  startDirID == s.Drive.RootID(),
		SeenFileIDs:    stats.SeenFileIDs,
		VisitedDirIDs:  stats.VisitedDirIDs,
		ExcludedDirIDs: make(map[string]struct{}),
	}
	if err := s.discoverDir(ctx, startDirID, "", &snapshot, stats, progress); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (s *Scanner) discoverDir(
	ctx context.Context,
	dirID string,
	dirName string,
	snapshot *Snapshot,
	stats *Stats,
	progress progressFunc,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, visited := snapshot.VisitedDirIDs[dirID]; visited {
		return nil
	}
	snapshot.VisitedDirIDs[dirID] = struct{}{}
	progress("discover", dirName)

	entries, err := s.Drive.List(ctx, dirID)
	if err != nil {
		return fmt.Errorf("list directory %s: %w", dirID, err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir {
			if s.excludedDirectory(entry) {
				snapshot.ExcludedDirIDs[entry.ID] = struct{}{}
				continue
			}
			if err := s.discoverDir(ctx, entry.ID, entry.Name, snapshot, stats, progress); err != nil {
				if fatalDiscoveryError(ctx, err) {
					return err
				}
				issue := Issue{Stage: IssueDiscovery, DirID: entry.ID, Name: entry.Name, Err: err}
				snapshot.Issues = append(snapshot.Issues, issue)
				stats.Errors++
				log.Printf("[scanner] %v", issue)
			}
			continue
		}

		ext := strings.ToLower(path.Ext(entry.Name))
		if !s.Exts[ext] || entry.Size <= 0 {
			continue
		}
		snapshot.Files = append(snapshot.Files, File{
			Entry:    entry,
			ParentID: dirID,
			DirName:  dirName,
		})
		snapshot.SeenFileIDs[entry.ID] = struct{}{}
		stats.Scanned++
		progress("discover", dirName)
	}
	return nil
}

func (s *Scanner) excludedDirectory(entry drives.Entry) bool {
	if strings.EqualFold(entry.Name, "previews") {
		return true
	}
	_, skipped := s.SkipDirIDs[entry.ID]
	return skipped
}

func fatalDiscoveryError(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	_, rateLimited := drives.RateLimitRetryAfter(err)
	return rateLimited
}
