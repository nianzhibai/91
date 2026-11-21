package dedupe

import (
	"strconv"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/mediasig"
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
// opaque values; no algorithm is inferred from their length. Only a
// fingerprint of the current sampling scheme is a key: an earlier scheme
// could give different files the same value, so its fingerprints may tell
// files apart but never prove them equal.
func ExactKeys(c Candidate) []ExactKey {
	keys := make([]ExactKey, 0, 2)
	if hash := NormalizeHash(c.ContentHash); hash != "" {
		keys = append(keys, ExactKey{Reason: ReasonContentHash, Value: hash})
	}
	if sampled := NormalizeHash(c.SampledSHA256); IsCurrentFingerprint(sampled) && c.Size > 0 {
		keys = append(keys, ExactKey{Reason: ReasonSampledSHA256, Value: sampledKey(c.Size, sampled)})
	}
	return keys
}

// SampledFingerprintPrefix starts every sampled fingerprint made by the
// current sampling scheme. A fingerprint of an earlier scheme never equals a
// current one, so the two schemes cannot confirm each other as duplicates.
const SampledFingerprintPrefix = "s2:"

// IsCurrentFingerprint reports whether sampled was made by the current
// sampling scheme. A video keeps an earlier scheme's fingerprint only until
// the fingerprint worker replaces it.
func IsCurrentFingerprint(sampled string) bool {
	return strings.HasPrefix(NormalizeHash(sampled), SampledFingerprintPrefix)
}

func NormalizeHash(hash string) string {
	return strings.ToLower(strings.TrimSpace(hash))
}

// DurationToleranceSeconds is the largest duration difference of two videos
// compared by signature. Copies differ by rounding and container overhead;
// edited versions differ by more.
const DurationToleranceSeconds = 2

// SignatureEligible reports whether a candidate is long enough for its
// signature verdict to be trusted.
func SignatureEligible(c Candidate) bool {
	return time.Duration(c.DurationSeconds)*time.Second >= mediasig.MinDuration
}

// MatchSignature compares the perceptual signatures of two candidates whose
// durations are within tolerance. The returned match carries the comparison
// whatever its verdict; callers act on the verdict.
func MatchSignature(left, right Candidate) (Match, mediasig.Verdict) {
	if !SignatureEligible(left) || !SignatureEligible(right) || !durationsWithinTolerance(left, right) ||
		left.Signature == nil || right.Signature == nil {
		return Match{}, mediasig.VerdictNone
	}
	comparison := mediasig.Compare(left.Signature, right.Signature)
	return Match{
		Stage: StageSignature, Reason: ReasonSignature, LeftID: left.ID, RightID: right.ID,
		Score: comparison.FrameRatio(), Comparisons: comparison.Frames, Signature: &comparison,
	}, comparison.Verdict
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
	return delta <= DurationToleranceSeconds
}
