package scanner

import (
	"fmt"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
)

type Stats struct {
	Scanned          int
	Added            int
	Errors           int
	SeenFileIDs      map[string]struct{}
	EnumeratedDirIDs map[string]struct{}
}

// File is a video candidate with directory identity supplied by the traversal,
// rather than the provider's optional Entry.ParentID field. AncestorDirIDs runs
// from the scan start through the direct parent, including both endpoints.
// AncestorDirNames follows the same order; an unknown start name is empty.
type File struct {
	Entry            drives.Entry
	ParentID         string
	DirName          string
	AncestorDirIDs   []string
	AncestorDirNames []string
}

type IssueStage string

const (
	IssueDiscovery IssueStage = "discovery"
	IssueTombstone IssueStage = "tombstone"
	IssueLookup    IssueStage = "lookup"
	IssueDuplicate IssueStage = "duplicate"
	IssueMetadata  IssueStage = "metadata"
	IssueTags      IssueStage = "tags"
	IssueUpsert    IssueStage = "upsert"
)

// Issue is a recoverable per-directory or per-file failure. Fatal failures are
// returned as errors by Discover, Reconcile, or Scan.
type Issue struct {
	Stage  IssueStage
	DirID  string
	FileID string
	Name   string
	Err    error
}

func (i Issue) Error() string {
	target := i.Name
	if target == "" {
		target = i.FileID
	}
	if target == "" {
		target = i.DirID
	}
	return fmt.Sprintf("%s %s: %v", i.Stage, target, i.Err)
}

// Snapshot is the discovery phase output. EnumeratedDirIDs, FailedDirIDs, and
// ExcludedDirIDs are the mutually exclusive E/F/X directory classifications
// used by presence cleanup.
type Snapshot struct {
	DriveID          string
	DriveKind        string
	StartDirID       string
	Files            []File
	SeenFileIDs      map[string]struct{}
	EnumeratedDirIDs map[string]struct{}
	FailedDirIDs     map[string]struct{}
	ExcludedDirIDs   map[string]struct{}
	Issues           []Issue
}

func (s Snapshot) Complete() bool {
	return len(s.Issues) == 0
}

// PresenceAuthoritative reports whether discovery completely covered the
// configured scan scope. It deliberately ignores reconciliation issues: E/F/X
// and SeenFileIDs are finalized before reconciliation starts.
func (s Snapshot) PresenceAuthoritative() bool {
	if len(s.FailedDirIDs) > 0 || len(s.Issues) > 0 {
		return false
	}
	_, startEnumerated := s.EnumeratedDirIDs[s.StartDirID]
	return startEnumerated
}

// Result combines the discovery snapshot with reconciliation output.
// NewVideos is the application-layer handoff point for derived asset jobs.
type Result struct {
	Snapshot   Snapshot
	Stats      Stats
	NewVideos  []*catalog.Video
	Issues     []Issue
	Updated    int
	Duplicates int
	Tombstoned int
}

func newResult(snapshot Snapshot, stats Stats) Result {
	issues := append([]Issue(nil), snapshot.Issues...)
	return Result{Snapshot: snapshot, Stats: stats, Issues: issues}
}
