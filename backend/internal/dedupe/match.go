package dedupe

import (
	"strconv"
	"strings"

	"github.com/nianzhibai/91/backend/internal/mediasim"
)

// The rules in this file are the single definition of a duplicate pair. The
// maintenance planner and crawler ingress both call them; callers only decide
// which pairs are worth comparing and where the media inputs come from.

// ExactKey is one exact-identity signal. Two videos are exact duplicates when
// they share any key. The catalog's SQL lookups mirror these signals.
type ExactKey struct {
	Reason string
	Value  string
}

// ExactKeys returns the provider content hash and the size-qualified sampled
// fingerprint of a candidate, when present. Hash strings are compared as
// opaque values; no algorithm is inferred from their length.
func ExactKeys(c Candidate) []ExactKey {
	keys := make([]ExactKey, 0, 2)
	if hash := NormalizeHash(c.ContentHash); hash != "" {
		keys = append(keys, ExactKey{Reason: ReasonContentHash, Value: hash})
	}
	if sampled := NormalizeHash(c.SampledSHA256); sampled != "" && c.Size > 0 {
		keys = append(keys, ExactKey{Reason: ReasonSampledSHA256, Value: sampledKey(c.Size, sampled)})
	}
	return keys
}

func NormalizeHash(hash string) string {
	return strings.ToLower(strings.TrimSpace(hash))
}

// MatchNear applies the title, duration and thumbnail rule. compare is only
// called after the cheaper title and duration checks pass.
func MatchNear(left, right Candidate, compare ImageComparator) (Match, bool, error) {
	if !durationsWithinTolerance(left, right) ||
		strings.TrimSpace(left.ThumbnailPath) == "" || strings.TrimSpace(right.ThumbnailPath) == "" {
		return Match{}, false, nil
	}
	titleScore := mediasim.TitleSimilarity(left.Title, right.Title)
	if titleScore < mediasim.NearDuplicateTitleThreshold {
		return Match{}, false, nil
	}
	ssim, err := compare(left.ThumbnailPath, right.ThumbnailPath)
	if err != nil {
		return Match{}, false, err
	}
	if ssim < mediasim.NearDuplicateThumbSSIMThreshold {
		return Match{}, false, nil
	}
	return Match{
		Stage: StageNear, Reason: ReasonTitleThumb, LeftID: left.ID, RightID: right.ID,
		Score: ssim, TitleScore: titleScore,
	}, true, nil
}

// ContentEligible reports whether a candidate is long enough for duration to
// carry signal. Callers use it to avoid extracting signatures needlessly.
func ContentEligible(c Candidate) bool {
	return c.DurationSeconds >= mediasim.ContentDuplicateMinDurationSeconds
}

// MatchContent compares frame signatures sampled at the same duration-derived
// offsets. Aligned frames decide first; when both durations are exactly equal,
// a bidirectional cross match recovers teasers whose segments were shifted.
func MatchContent(left, right Candidate, leftSignature, rightSignature *mediasim.FrameSignature) (Match, bool) {
	if !ContentEligible(left) || !ContentEligible(right) || !durationsWithinTolerance(left, right) ||
		leftSignature == nil || rightSignature == nil {
		return Match{}, false
	}
	aligned := mediasim.CompareFrameSignatures(leftSignature, rightSignature)
	if aligned.IsContentDuplicate() {
		return Match{
			Stage: StageContent, Reason: ReasonContent, LeftID: left.ID, RightID: right.ID,
			Score: aligned.MedianSSIM, Comparisons: aligned.Comparisons,
		}, true
	}
	if left.DurationSeconds != right.DurationSeconds {
		return Match{}, false
	}
	cross := mediasim.CompareFrameSignaturesCross(leftSignature, rightSignature)
	if !cross.IsContentDuplicate() {
		return Match{}, false
	}
	return Match{
		Stage: StageContent, Reason: ReasonContent, LeftID: left.ID, RightID: right.ID,
		Score: cross.MedianBest, Comparisons: min(cross.LeftFrames, cross.RightFrames),
		Cross: true, CrossDetails: &cross,
	}, true
}

func sampledKey(size int64, sampled string) string {
	return strconv.FormatInt(size, 10) + ":" + sampled
}

func durationsWithinTolerance(left, right Candidate) bool {
	if left.DurationSeconds <= 0 || right.DurationSeconds <= 0 {
		return false
	}
	delta := left.DurationSeconds - right.DurationSeconds
	if delta < 0 {
		delta = -delta
	}
	return delta <= mediasim.NearDuplicateDurationToleranceSeconds
}
