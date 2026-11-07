package scanner

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
)

// Source is the read-only subset of a drive needed by a scan. Keeping this
// interface narrow prevents catalog discovery from depending on playback,
// upload, or authentication capabilities.
type Source interface {
	Kind() string
	ID() string
	List(ctx context.Context, dirID string) ([]drives.Entry, error)
	RootID() string
}

type Scanner struct {
	Catalog *catalog.Catalog
	Drive   Source
	Exts    map[string]bool

	// SkipDirIDs contains directory IDs excluded from discovery. The application
	// owns their separate policy-cleanup lifecycle; presence cleanup treats the
	// corresponding ExcludedDirIDs as protected rather than missing.
	SkipDirIDs map[string]struct{}

	// OnNewVideo is retained for callers using Run directly. Application-level
	// orchestration should prefer Scan and dispatch Result.NewVideos explicitly.
	OnNewVideo func(v *catalog.Video)
	// OnProgress receives an immutable-by-convention copy of the current counts.
	OnProgress func(stats Stats)
	// OnCooldown exposes a provider rate-limit wait to application status. A zero
	// time means the wait ended and discovery is retrying the same directory.
	OnCooldown func(until time.Time)
	// RateLimitBudget is shared across every Scanner used by one drive task.
	RateLimitBudget *RateLimitBudget
	// RetryWait is an optional test/integration seam for interruptible waits.
	// Nil uses a context-aware timer.
	RetryWait func(ctx context.Context, duration time.Duration) error
	// ProgressInterval controls heartbeat logging. Zero uses the default; a
	// negative duration disables heartbeat logs.
	ProgressInterval time.Duration
	// LogPrefix identifies an application-owned reuse of discovery. Empty uses
	// "scanner"; skip-policy legacy discovery uses "skip-cleanup".
	LogPrefix string
}

const defaultScanProgressInterval = 30 * time.Second

// New constructs a scanner. skipDirIDs may be nil or empty.
func New(cat *catalog.Catalog, drv Source, exts []string, skipDirIDs []string, onNew func(v *catalog.Video)) *Scanner {
	extensions := make(map[string]bool, len(exts))
	for _, ext := range exts {
		extensions[strings.ToLower(ext)] = true
	}
	skipped := make(map[string]struct{}, len(skipDirIDs))
	for _, id := range skipDirIDs {
		if id = strings.TrimSpace(id); id != "" {
			skipped[id] = struct{}{}
		}
	}
	return &Scanner{
		Catalog:         cat,
		Drive:           drv,
		Exts:            extensions,
		SkipDirIDs:      skipped,
		OnNewVideo:      onNew,
		RateLimitBudget: NewRateLimitBudget(),
	}
}

// Run preserves the original scanner facade. New orchestration code should use
// Scan when it needs snapshot completeness or the newly inserted videos.
func (s *Scanner) Run(ctx context.Context, startDirID string) (Stats, error) {
	result, err := s.Scan(ctx, startDirID)
	return result.Stats, err
}

// Scan executes the explicit discovery -> reconciliation pipeline. Discovery
// performs no catalog writes, so a fatal traversal error cannot leave a
// half-reconciled catalog.
func (s *Scanner) Scan(ctx context.Context, startDirID string) (Result, error) {
	if err := validateScanner(s); err != nil {
		return Result{}, err
	}
	stats := newStats()
	snapshot, err := s.discover(ctx, startDirID, &stats, s.progressReporter(&stats))
	result := newResult(snapshot, stats)
	if err != nil {
		return result, err
	}
	if err := s.reconcile(ctx, &result, s.progressReporter(&result.Stats)); err != nil {
		return result, err
	}
	return result, nil
}

// Discover builds a read-only representation of the provider state. It is
// exported so callers and tests can inspect discovery independently of writes.
func (s *Scanner) Discover(ctx context.Context, startDirID string) (Snapshot, Stats, error) {
	if err := validateSource(s); err != nil {
		return Snapshot{}, Stats{}, err
	}
	stats := newStats()
	progress := s.progressReporter(&stats)
	snapshot, err := s.discover(ctx, startDirID, &stats, progress)
	return snapshot, stats, err
}

// Reconcile applies a previously discovered snapshot to the catalog.
func (s *Scanner) Reconcile(ctx context.Context, snapshot Snapshot) (Result, error) {
	if err := validateScanner(s); err != nil {
		return Result{}, err
	}
	stats := statsForSnapshot(snapshot)
	result := newResult(snapshot, stats)
	progress := s.progressReporter(&result.Stats)
	if err := s.reconcile(ctx, &result, progress); err != nil {
		return result, err
	}
	return result, nil
}

func newStats() Stats {
	return Stats{
		SeenFileIDs:      make(map[string]struct{}),
		EnumeratedDirIDs: make(map[string]struct{}),
	}
}

func statsForSnapshot(snapshot Snapshot) Stats {
	return Stats{
		Scanned:          len(snapshot.Files),
		Errors:           len(snapshot.Issues),
		SeenFileIDs:      snapshot.SeenFileIDs,
		EnumeratedDirIDs: snapshot.EnumeratedDirIDs,
	}
}

type progressFunc func(phase, currentDir string)

func (s *Scanner) progressReporter(stats *Stats) progressFunc {
	interval := s.ProgressInterval
	if interval == 0 {
		interval = defaultScanProgressInterval
	}
	started := time.Now()
	lastBeat := started
	driveID := ""
	if s.Drive != nil {
		driveID = s.Drive.ID()
	}
	return func(phase, currentDir string) {
		if s.OnProgress != nil {
			s.OnProgress(*stats)
		}
		if interval < 0 {
			return
		}
		now := time.Now()
		if now.Sub(lastBeat) < interval {
			return
		}
		lastBeat = now
		if currentDir == "" {
			currentDir = "(root)"
		}
		log.Printf("[%s] drive=%s progress: phase=%s scanned=%d added=%d errors=%d dirs=%d elapsed=%s at=%s",
			s.logPrefix(),
			driveID, phase, stats.Scanned, stats.Added, stats.Errors,
			len(stats.EnumeratedDirIDs), now.Sub(started).Round(time.Second), currentDir)
	}
}

func (s *Scanner) logPrefix() string {
	if s != nil {
		if prefix := strings.TrimSpace(s.LogPrefix); prefix != "" {
			return prefix
		}
	}
	return "scanner"
}

func validateScanner(s *Scanner) error {
	if err := validateSource(s); err != nil {
		return err
	}
	if s.Catalog == nil {
		return errors.New("scanner catalog is nil")
	}
	return nil
}

func validateSource(s *Scanner) error {
	if s == nil {
		return errors.New("scanner is nil")
	}
	if s.Drive == nil {
		return errors.New("scanner drive is nil")
	}
	return nil
}
