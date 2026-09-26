package scanner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"strings"
	"time"

	"github.com/video-site/backend/internal/applog"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/readretry"
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
	startDirName := ""
	if provider, ok := s.Drive.(drives.DirectoryNameProvider); ok {
		name, err := provider.DirectoryName(ctx, startDirID)
		if err == nil {
			startDirName = name
		} else if ctx.Err() != nil {
			return Snapshot{}, ctx.Err()
		} else if !errors.Is(err, drives.ErrNotSupported) {
			log.Printf("[%s] drive=%s start directory name unavailable: %v", s.logPrefix(), s.Drive.ID(), err)
		}
	}
	snapshot := Snapshot{
		DriveID:          s.Drive.ID(),
		DriveKind:        s.Drive.Kind(),
		StartDirID:       startDirID,
		SeenFileIDs:      stats.SeenFileIDs,
		EnumeratedDirIDs: stats.EnumeratedDirIDs,
		FailedDirIDs:     make(map[string]struct{}),
		ExcludedDirIDs:   make(map[string]struct{}),
	}
	attemptedDirIDs := make(map[string]struct{})
	if err := s.discoverDir(ctx, startDirID, startDirName, nil, nil, &snapshot, stats, progress, attemptedDirIDs); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (s *Scanner) discoverDir(
	ctx context.Context,
	dirID string,
	dirName string,
	ancestorDirIDs []string,
	ancestorDirNames []string,
	snapshot *Snapshot,
	stats *Stats,
	progress progressFunc,
	attemptedDirIDs map[string]struct{},
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, attempted := attemptedDirIDs[dirID]; attempted {
		return nil
	}
	attemptedDirIDs[dirID] = struct{}{}
	progress("discover", dirName)

	entries, err := s.listDirectory(ctx, dirID)
	if err != nil {
		return fmt.Errorf("list directory %s: %w", dirID, err)
	}
	delete(snapshot.FailedDirIDs, dirID)
	delete(snapshot.ExcludedDirIDs, dirID)
	snapshot.EnumeratedDirIDs[dirID] = struct{}{}
	currentAncestorDirIDs := appendDirID(ancestorDirIDs, dirID)
	currentAncestorDirNames := append(append([]string(nil), ancestorDirNames...), dirName)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir {
			if s.excludedDirectory(entry) {
				if _, enumerated := snapshot.EnumeratedDirIDs[entry.ID]; !enumerated {
					if _, failed := snapshot.FailedDirIDs[entry.ID]; !failed {
						snapshot.ExcludedDirIDs[entry.ID] = struct{}{}
					}
				}
				continue
			}
			if err := s.discoverDir(ctx, entry.ID, entry.Name, currentAncestorDirIDs, currentAncestorDirNames, snapshot, stats, progress, attemptedDirIDs); err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				if errors.Is(err, ErrRateLimitBudgetExhausted) {
					return err
				}
				if _, excluded := snapshot.ExcludedDirIDs[entry.ID]; !excluded {
					if _, enumerated := snapshot.EnumeratedDirIDs[entry.ID]; !enumerated {
						snapshot.FailedDirIDs[entry.ID] = struct{}{}
					}
				}
				issue := Issue{Stage: IssueDiscovery, DirID: entry.ID, Name: entry.Name, Err: err}
				snapshot.Issues = append(snapshot.Issues, issue)
				stats.Errors++
				applog.Error(ctx, "Directory discovery failed: "+entry.Name, err, applog.Fields{Component: s.logPrefix(), DriveID: s.Drive.ID(), FileID: entry.ID, Stage: string(IssueDiscovery)})
			}
			continue
		}

		ext := strings.ToLower(path.Ext(entry.Name))
		if !s.Exts[ext] || entry.Size <= 0 {
			continue
		}
		snapshot.Files = append(snapshot.Files, File{
			Entry:            entry,
			ParentID:         dirID,
			DirName:          dirName,
			AncestorDirIDs:   append([]string(nil), currentAncestorDirIDs...),
			AncestorDirNames: append([]string(nil), currentAncestorDirNames...),
		})
		snapshot.SeenFileIDs[entry.ID] = struct{}{}
		stats.Scanned++
		progress("discover", dirName)
	}
	return nil
}

func (s *Scanner) excludedDirectory(entry drives.Entry) bool {
	_, skipped := s.SkipDirIDs[entry.ID]
	return skipped
}

func appendDirID(ancestorDirIDs []string, dirID string) []string {
	out := make([]string, len(ancestorDirIDs), len(ancestorDirIDs)+1)
	copy(out, ancestorDirIDs)
	return append(out, dirID)
}

func (s *Scanner) listDirectory(ctx context.Context, dirID string) ([]drives.Entry, error) {
	transportRetries := 0
	for {
		entries, err := s.Drive.List(ctx, dirID)
		if err == nil {
			return entries, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if _, rateLimited := drives.RateLimitRetryAfter(err); rateLimited {
			retry, allowed := s.retryBudget().reserveRetry()
			if !allowed {
				return nil, fmt.Errorf(
					"%w: drive=%s directory=%s retries=%d",
					ErrRateLimitBudgetExhausted, s.Drive.ID(), dirID, retry,
				)
			}
			wait := RateLimitCooldown
			until := time.Now().Add(wait)
			if s.OnCooldown != nil {
				s.OnCooldown(until)
			}
			log.Printf(
				"[%s] drive=%s directory=%s rate limited; cooldown=%s retry=%d/%d: %v",
				s.logPrefix(), s.Drive.ID(), dirID, wait, retry, RateLimitRetryLimit, err,
			)
			waitErr := s.waitForRetry(ctx, wait)
			if s.OnCooldown != nil {
				s.OnCooldown(time.Time{})
			}
			if waitErr != nil {
				return nil, waitErr
			}
			continue
		}
		if readretry.Transient(err) && transportRetries < readretry.MaxRetries {
			transportRetries++
			delay := readretry.Delay(transportRetries)
			log.Printf(
				"[%s] drive=%s directory=%s read failed; retry=%d/%d delay=%s: %v",
				s.logPrefix(), s.Drive.ID(), dirID, transportRetries, readretry.MaxRetries, delay, err,
			)
			if waitErr := s.waitForRetry(ctx, delay); waitErr != nil {
				return nil, waitErr
			}
			continue
		}
		return nil, err
	}
}

func (s *Scanner) waitForRetry(ctx context.Context, duration time.Duration) error {
	if s.RetryWait != nil {
		return s.RetryWait(ctx, duration)
	}
	return readretry.Wait(ctx, duration)
}
