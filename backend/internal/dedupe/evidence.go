package dedupe

import "fmt"

// EvidenceVersion identifies the interpretation of the recorded rules and
// scores. Increment it when their semantics change.
const EvidenceVersion = 1

const (
	ReasonUnknown       = "unknown"
	ReasonContentHash   = "content_hash"
	ReasonFileNameSize  = "file_name_size"
	ReasonSampledSHA256 = "sampled_sha256"
	ReasonTitleThumb    = "title_duration_thumbnail"
	ReasonContent       = "content_frames"
)

// Evidence keeps the actual matching edge separate from the eventual survivor.
// For A -> B -> C, A's evidence still names B, even though C is retained.
// SelectedVideoID identifies the winner whose selection is explained by
// SelectionReason; a later channel may replace that winner as well.
type Evidence struct {
	Version         int    `json:"version"`
	Reason          string `json:"reason"`
	MatchedVideoID  string `json:"matchedVideoId,omitempty"`
	SelectedVideoID string `json:"selectedVideoId,omitempty"`
	SelectionReason string `json:"selectionReason,omitempty"`
	Match           *Match `json:"match,omitempty"`
}

func NewEvidence(reason, matchedID, selectedID, selectionReason string) Evidence {
	return Evidence{
		Version: EvidenceVersion, Reason: reason, MatchedVideoID: matchedID,
		SelectedVideoID: selectedID, SelectionReason: selectionReason,
	}
}

func canonicalSelectionReason(stage Stage, winner, other Candidate) string {
	if stage != StageExact && winner.Size != other.Size {
		return "larger_file"
	}
	if winner.AssetScore != other.AssetScore {
		return "more_complete_assets"
	}
	if !winner.CreatedAt.Equal(other.CreatedAt) {
		return "earlier_created"
	}
	return "stable_id"
}

// attachEvidence walks the observed matching graph once. Each retired video
// records one edge towards its final survivor; the records together preserve
// transitive explanations without copying a whole group into every row.
func (s *plannerState) attachEvidence() error {
	adjacent := make(map[string][]int)
	for i, match := range s.plan.Matches {
		adjacent[match.LeftID] = append(adjacent[match.LeftID], i)
		adjacent[match.RightID] = append(adjacent[match.RightID], i)
	}
	next := make(map[string]string)
	edges := make(map[string]Match)
	for _, action := range s.plan.Actions {
		root := action.CanonicalVideoID
		if _, visited := next[root]; visited {
			continue
		}
		next[root] = root
		queue := []string{root}
		for i := 0; i < len(queue); i++ {
			id := queue[i]
			for _, index := range adjacent[id] {
				match := s.plan.Matches[index]
				other := match.LeftID
				if other == id {
					other = match.RightID
				}
				if _, visited := next[other]; visited {
					continue
				}
				next[other], edges[other] = id, match
				queue = append(queue, other)
			}
		}
	}
	for i := range s.plan.Actions {
		action := &s.plan.Actions[i]
		match, ok := edges[action.VideoID]
		if !ok {
			return fmt.Errorf("dedupe: no matching evidence for %s", action.VideoID)
		}
		action.Evidence.MatchedVideoID = next[action.VideoID]
		action.Evidence.Reason = match.Reason
		action.Evidence.Match = &match
	}
	return nil
}
