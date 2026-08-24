// Package dedupe plans duplicate-video maintenance without mutating catalog
// rows or generated media assets.
package dedupe

import (
	"errors"
	"time"
)

// Stage identifies the maintenance channel that connected a duplicate group.
type Stage string

const (
	StageExact   Stage = "exact"
	StageNear    Stage = "near"
	StageContent Stage = "content"
)

// Channels selects which maintenance channels a planner run evaluates.
type Channels uint8

const (
	ChannelExact Channels = 1 << iota
	ChannelNear
	ChannelContent

	AllChannels = ChannelExact | ChannelNear | ChannelContent
)

func (c Channels) includes(channel Channels) bool {
	return c&channel != 0
}

// Candidate is the dedupe-owned projection of a catalog video. Paths and
// AssetScore are prepared by the caller because only the application knows
// which configured local directory owns generated assets.
type Candidate struct {
	ID                string
	Title             string
	DurationSeconds   int
	Size              int64
	SampledSHA256     string
	AssetScore        int
	CreatedAt         time.Time
	ExpectedUpdatedAt int64
	ThumbnailPath     string
	TeaserPath        string
}

// DeleteAction removes VideoID in favor of CanonicalVideoID. CanonicalVideoID
// is finalized after all selected channels have run, so it always points at a
// survivor rather than an intermediate winner from an earlier channel.
type DeleteAction struct {
	Stage                      Stage
	VideoID                    string
	CanonicalVideoID           string
	ExpectedUpdatedAt          int64
	CanonicalExpectedUpdatedAt int64
}

// Group is one connected component produced by a channel before later-channel
// canonical redirects are resolved. CanonicalVideoID is rewritten to the final
// survivor during plan finalization.
type Group struct {
	Stage            Stage
	CanonicalVideoID string
	MemberIDs        []string
}

// Match records a perceptual edge used by the disjoint-set grouping. It is
// returned for operational logging; group membership remains transitive even
// when two members do not have a direct edge.
type Match struct {
	Stage       Stage
	LeftID      string
	RightID     string
	Score       float64
	Comparisons int
	Cross       bool
}

// Issue is a non-fatal media comparison problem. The affected candidate or
// pair is skipped while the rest of the plan continues.
type Issue struct {
	Stage   Stage
	VideoID string
	LeftID  string
	RightID string
	Err     error
}

type ChannelStats struct {
	Candidates    int
	Extracted     int
	ExtractFailed int
	Comparisons   int
	CrossMatched  int
	Groups        int
	Deleted       int
}

type Stats struct {
	Videos  int
	Exact   ChannelStats
	Near    ChannelStats
	Content ChannelStats
}

// Plan is the complete read-only result of a planner run. Redirects contains
// every deleted ID mapped directly to its final surviving canonical ID.
type Plan struct {
	Actions   []DeleteAction
	Redirects map[string]string
	Groups    []Group
	Matches   []Match
	Issues    []Issue
	Stats     Stats
}

func (p Plan) Validate() error {
	deleted := make(map[string]struct{}, len(p.Actions))
	for _, action := range p.Actions {
		if action.VideoID == "" || action.CanonicalVideoID == "" {
			return errors.New("dedupe: deletion has an empty video ID")
		}
		if action.VideoID == action.CanonicalVideoID {
			return errors.New("dedupe: deletion points at itself")
		}
		if _, exists := deleted[action.VideoID]; exists {
			return errors.New("dedupe: video appears in more than one deletion")
		}
		deleted[action.VideoID] = struct{}{}
	}
	for _, action := range p.Actions {
		if _, canonicalDeleted := deleted[action.CanonicalVideoID]; canonicalDeleted {
			return errors.New("dedupe: deletion points at a non-final canonical")
		}
	}
	for _, group := range p.Groups {
		if group.CanonicalVideoID == "" || len(group.MemberIDs) < 2 {
			return errors.New("dedupe: invalid duplicate group")
		}
		if _, canonicalDeleted := deleted[group.CanonicalVideoID]; canonicalDeleted {
			return errors.New("dedupe: group points at a non-final canonical")
		}
	}
	return nil
}
