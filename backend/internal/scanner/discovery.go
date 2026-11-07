package scanner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/readretry"
)

// Traversal state belongs to one discovery, including its single final retry
// pass. Keep successful/previously visited directories deduplicated in both passes.
type discoveryTraversal struct {
	attemptedDirIDs map[string]struct{}
	pending         []pendingDirectory
	retrying        bool
}

type pendingDirectory struct {
	id               string
	name             string
	ancestorDirIDs   []string
	ancestorDirNames []string
}

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
	traversal := &discoveryTraversal{attemptedDirIDs: make(map[string]struct{})}
	if err := s.discoverDir(ctx, startDirID, startDirName, nil, nil, &snapshot, stats, progress, traversal); err != nil {
		return snapshot, err
	}
	if len(traversal.pending) > 0 {
		// Freeze the queue. Directories first discovered during this pass still
		// get short retries, but cannot extend discovery with another final pass.
		pending := traversal.pending
		traversal.pending = nil
		traversal.retrying = true
		recovered := 0
		log.Printf("[%s] drive=%s final directory retry started queued=%d", s.logPrefix(), s.Drive.ID(), len(pending))
		for _, dir := range pending {
			// Only release this failed directory; successful directories and
			// other queued directories retain their cycle/deduplication guard.
			delete(traversal.attemptedDirIDs, dir.id)
			if err := s.discoverDir(ctx, dir.id, dir.name, dir.ancestorDirIDs, dir.ancestorDirNames, &snapshot, stats, progress, traversal); err != nil {
				return snapshot, err
			}
			if _, enumerated := snapshot.EnumeratedDirIDs[dir.id]; enumerated {
				recovered++
				applog.Info(ctx, "Directory read recovered during final pass: "+dir.name, applog.Fields{Component: s.logPrefix(), DriveID: s.Drive.ID(), FileID: dir.id, Stage: string(IssueDiscovery)})
			}
		}
		log.Printf("[%s] drive=%s final directory retry finished queued=%d recovered=%d failed=%d discovery_issues=%d",
			s.logPrefix(), s.Drive.ID(), len(pending), recovered, len(pending)-recovered, len(snapshot.Issues))
	}
	return snapshot, ctx.Err()
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
	traversal *discoveryTraversal,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, attempted := traversal.attemptedDirIDs[dirID]; attempted {
		return nil
	}
	traversal.attemptedDirIDs[dirID] = struct{}{}
	phase := "discover"
	if traversal.retrying {
		phase = "discover_retry"
	}
	progress(phase, dirName)

	entries, err := s.listDirectory(ctx, dirID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		err = fmt.Errorf("list directory %s: %w", dirID, err)
		if errors.Is(err, ErrRateLimitBudgetExhausted) {
			return err
		}
		if !traversal.retrying && readretry.Transient(err) {
			// The attempted-directory guard ensures one queue entry per ID.
			// Preserve ancestry because the recursion will have unwound by then.
			traversal.pending = append(traversal.pending, pendingDirectory{
				id: dirID, name: dirName,
				ancestorDirIDs:   append([]string(nil), ancestorDirIDs...),
				ancestorDirNames: append([]string(nil), ancestorDirNames...),
			})
			snapshot.FailedDirIDs[dirID] = struct{}{}
			applog.Warn(ctx, "Directory read deferred to final pass: "+dirName, err, applog.Fields{Component: s.logPrefix(), DriveID: s.Drive.ID(), FileID: dirID, Stage: string(IssueDiscovery)})
			return nil
		}
		// A root that remains unreadable is fatal, as before. Other failures
		// become final issues only after any deferred retry has been consumed.
		if dirID == snapshot.StartDirID {
			return err
		}
		snapshot.FailedDirIDs[dirID] = struct{}{}
		snapshot.Issues = append(snapshot.Issues, Issue{Stage: IssueDiscovery, DirID: dirID, Name: dirName, Err: err})
		stats.Errors++
		applog.Error(ctx, "Directory discovery failed: "+dirName, err, applog.Fields{Component: s.logPrefix(), DriveID: s.Drive.ID(), FileID: dirID, Stage: string(IssueDiscovery)})
		return nil
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
			if err := s.discoverDir(ctx, entry.ID, entry.Name, currentAncestorDirIDs, currentAncestorDirNames, snapshot, stats, progress, traversal); err != nil {
				return err
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
		progress(phase, dirName)
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
