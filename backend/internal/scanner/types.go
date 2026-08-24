package scanner

import (
	"fmt"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
)

type Stats struct {
	Scanned       int
	Added         int
	Errors        int
	SeenFileIDs   map[string]struct{}
	VisitedDirIDs map[string]struct{}
}

// File is a video candidate with directory identity supplied by the traversal,
// rather than the provider's optional Entry.ParentID field.
type File struct {
	Entry    drives.Entry
	ParentID string
	DirName  string
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

// Snapshot is the discovery phase output. FullDriveScan is deliberately true
// only when the resolved scan root equals the provider root. In that case,
// files under ExcludedDirIDs are missing from the snapshot by policy and may be
// removed after the configured consecutive-miss confirmation.
type Snapshot struct {
	DriveID        string
	DriveKind      string
	StartDirID     string
	FullDriveScan  bool
	Files          []File
	SeenFileIDs    map[string]struct{}
	VisitedDirIDs  map[string]struct{}
	ExcludedDirIDs map[string]struct{}
	Issues         []Issue
}

func (s Snapshot) Complete() bool {
	return len(s.Issues) == 0
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
