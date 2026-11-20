// Package dedupe plans duplicate-video maintenance without mutating catalog
// rows or generated media assets.
package dedupe

import (
	"errors"
	"time"

	"github.com/nianzhibai/91/backend/internal/mediasig"
	"github.com/nianzhibai/91/backend/internal/mediasim"
)

// Stage identifies the maintenance channel that connected a duplicate group.
type Stage string

const (
	StageExact   Stage = "exact"
	StageNear    Stage = "near"
	StageContent Stage = "content"
	// StageSignature compares perceptual signatures. It will replace
	// StageContent once signatures are generated for every video.
	StageSignature Stage = "signature"
)

// Channels selects which maintenance channels a planner run evaluates.
type Channels uint8

const (
	ChannelExact Channels = 1 << iota
	ChannelNear
	ChannelContent
	// ChannelSignature is evaluated only when selected explicitly; production
	// maintenance keeps ChannelContent until signatures replace it.
	ChannelSignature

	AllChannels = ChannelExact | ChannelNear | ChannelContent
)

func (c Channels) includes(channel Channels) bool {
	return c&channel != 0
}

// Candidate is the dedupe-owned projection of a catalog video. Paths and
// AssetScore are prepared by the caller because only the application knows
// which configured local directory owns generated assets. Revision is an
// opaque token of the catalog state the decision depends on; the catalog
// rejects a plan when it no longer matches.
type Candidate struct {
	ID              string
	Title           string
	DurationSeconds int
	Size            int64
	ContentHash     string
	SampledSHA256   string
	AssetScore      int
	CreatedAt       time.Time
	Revision        string
	ThumbnailPath   string
	TeaserPath      string
	Signature       *mediasig.Signature
}

// DeleteAction removes VideoID in favor of CanonicalVideoID. CanonicalVideoID
// is finalized after all selected channels have run, so it always points at a
// survivor rather than an intermediate winner from an earlier channel.
type DeleteAction struct {
	Stage                     Stage
	VideoID                   string
	CanonicalVideoID          string
	ExpectedRevision          string
	CanonicalExpectedRevision string
	Evidence                  Evidence
}

// Group is one connected component produced by a channel before later-channel
// canonical redirects are resolved. CanonicalVideoID is rewritten to the final
// survivor during plan finalization.
type Group struct {
	Stage            Stage
	CanonicalVideoID string
	MemberIDs        []string
}

// Match records an observed duplicate edge used by the disjoint-set grouping.
// Group membership remains transitive even when two members do not have a
// direct edge. Reason names the rule that accepted the pair.
type Match struct {
	Stage       Stage   `json:"stage"`
	Reason      string  `json:"reason"`
	LeftID      string  `json:"leftId"`
	RightID     string  `json:"rightId"`
	Score       float64 `json:"score,omitempty"`
	Comparisons int     `json:"comparisons,omitempty"`
	Cross       bool    `json:"cross,omitempty"`
	TitleScore  float64 `json:"titleScore,omitempty"`
	// CrossDetails preserves both directional counts; MedianBest alone is not
	// the criterion used to accept a cross match.
	CrossDetails *mediasim.FrameSignatureCrossComparison `json:"crossDetails,omitempty"`
	// Signature is the evidence of a signature-channel pair.
	Signature *mediasig.Comparison `json:"signature,omitempty"`
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
	Videos    int
	Exact     ChannelStats
	Near      ChannelStats
	Content   ChannelStats
	Signature ChannelStats
}

// Plan is the complete read-only result of a planner run. Redirects contains
// every deleted ID mapped directly to its final surviving canonical ID.
// Reviews are pairs the planner did not merge but a person should judge.
type Plan struct {
	Actions   []DeleteAction
	Redirects map[string]string
	Groups    []Group
	Matches   []Match
	Reviews   []Match
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
