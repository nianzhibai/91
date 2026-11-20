package mediasig

import (
	"sort"
	"time"
)

// Matching thresholds, calibrated on a real library with cmd/dedupe-dryrun.
const (
	// MaxFrameDistance is the largest hash distance of two frames showing the
	// same picture. Re-encodes and small watermarks stay well below it;
	// unrelated pictures differ in about half of the 64 bits.
	MaxFrameDistance = 10
	// SearchRadius bounds the source-time mismatch tolerated between two
	// frames: rounded durations, seek inaccuracy and slightly trimmed copies.
	SearchRadius = 1500 * time.Millisecond

	// MinComparableFrames is the least evidence a verdict is based on.
	MinComparableFrames = 6
	// A window counts once it has minWindowFrames comparable frames, and
	// matches when at least half of them match.
	minWindowFrames = 2
	// A strong match spans StrongMinWindows windows and most of the frames.
	StrongMinWindows    = 3
	StrongMinFrameRatio = 0.6
	// A probable match spans ProbableMinWindows windows.
	ProbableMinWindows = 2
	// StaticMaxMedianDistance is the stricter frame distance a strong match
	// needs when a signature is static. Different segments of a fixed-camera
	// recording match at about 8 bits; reposts of one video stay within 4.
	StaticMaxMedianDistance = 4

	// MinDuration is the shortest source a signature verdict is trusted for.
	MinDuration = 30 * time.Second
)

// Verdict grades the evidence that two signatures show the same video.
type Verdict int

const (
	// VerdictNone means the signatures do not show the same video, or there
	// is too little evidence to tell.
	VerdictNone Verdict = iota
	// VerdictProbable needs a person to confirm.
	VerdictProbable
	// VerdictStrong is reliable enough to merge automatically.
	VerdictStrong
)

func (v Verdict) String() string {
	switch v {
	case VerdictStrong:
		return "strong"
	case VerdictProbable:
		return "probable"
	default:
		return "none"
	}
}

func (v Verdict) MarshalText() ([]byte, error) { return []byte(v.String()), nil }

// Comparison is the evidence for a pair, taken from the direction with the
// weaker result so that Compare(a, b) equals Compare(b, a).
type Comparison struct {
	Verdict        Verdict `json:"verdict"`
	Windows        int     `json:"windows"`
	MatchedWindows int     `json:"matchedWindows"`
	Frames         int     `json:"frames"`
	MatchedFrames  int     `json:"matchedFrames"`
	MedianDistance int     `json:"medianDistance"`
	// Static reports that a signature's windows look alike, as in a fixed
	// camera recording. Different segments of such a recording match frame by
	// frame, so a strong verdict additionally needs the frames to be the same
	// picture: MedianDistance at most StaticMaxMedianDistance.
	Static bool `json:"static,omitempty"`
}

// FrameRatio is the share of comparable frames that matched.
func (c Comparison) FrameRatio() float64 {
	if c.Frames == 0 {
		return 0
	}
	return float64(c.MatchedFrames) / float64(c.Frames)
}

// Compare decides whether a and b show the same video. Each informative frame
// is matched against the other signature's frames near the same relative
// position, so frames from shifted or partially failed windows reduce
// coverage instead of misaligning the rest.
func Compare(a, b *Signature) Comparison {
	if a == nil || b == nil {
		return Comparison{MedianDistance: 64}
	}
	out := compareDirected(a, b)
	if reverse := compareDirected(b, a); weaker(reverse, out) {
		out = reverse
	}
	if isStatic(a) || isStatic(b) {
		out.Static = true
		if out.Verdict == VerdictStrong && out.MedianDistance > StaticMaxMedianDistance {
			out.Verdict = VerdictProbable
		}
	}
	return out
}

func weaker(left, right Comparison) bool {
	if left.Verdict != right.Verdict {
		return left.Verdict < right.Verdict
	}
	return left.FrameRatio() < right.FrameRatio()
}

type windowTally struct{ comparable, matched int }

func compareDirected(a, b *Signature) Comparison {
	scale := 1.0
	if a.Duration > 0 && b.Duration > 0 {
		scale = float64(b.Duration) / float64(a.Duration)
	}
	tallies := make(map[int]*windowTally)
	distances := make([]int, 0, len(a.Frames))
	out := Comparison{}
	for _, frame := range a.Frames {
		if !frame.Informative {
			continue
		}
		expected := time.Duration(float64(frame.At) * scale)
		best, found := 65, false
		for _, other := range b.Frames {
			if !other.Informative || absDuration(other.At-expected) > SearchRadius {
				continue
			}
			found = true
			best = min(best, Distance(frame.Hash, other.Hash))
		}
		if !found {
			continue
		}
		tally := tallies[frame.Window]
		if tally == nil {
			tally = &windowTally{}
			tallies[frame.Window] = tally
		}
		tally.comparable++
		out.Frames++
		distances = append(distances, best)
		if best <= MaxFrameDistance {
			tally.matched++
			out.MatchedFrames++
		}
	}
	for _, tally := range tallies {
		if tally.comparable < minWindowFrames {
			continue
		}
		out.Windows++
		if 2*tally.matched >= tally.comparable {
			out.MatchedWindows++
		}
	}
	out.MedianDistance = median(distances)
	switch {
	case out.Frames < MinComparableFrames:
		out.Verdict = VerdictNone
	case out.MatchedWindows >= StrongMinWindows && out.FrameRatio() >= StrongMinFrameRatio:
		out.Verdict = VerdictStrong
	case out.MatchedWindows >= ProbableMinWindows:
		out.Verdict = VerdictProbable
	}
	return out
}

// isStatic reports whether most pairs of windows contain a matching frame.
func isStatic(s *Signature) bool {
	byWindow := make(map[int][]uint64)
	for _, frame := range s.Frames {
		if frame.Informative {
			byWindow[frame.Window] = append(byWindow[frame.Window], frame.Hash)
		}
	}
	windows := make([][]uint64, 0, len(byWindow))
	for _, hashes := range byWindow {
		windows = append(windows, hashes)
	}
	pairs, alike := 0, 0
	for i := range windows {
		for j := i + 1; j < len(windows); j++ {
			pairs++
			if windowsAlike(windows[i], windows[j]) {
				alike++
			}
		}
	}
	return pairs > 0 && 2*alike > pairs
}

func windowsAlike(left, right []uint64) bool {
	for _, l := range left {
		for _, r := range right {
			if Distance(l, r) <= MaxFrameDistance {
				return true
			}
		}
	}
	return false
}

func median(values []int) int {
	if len(values) == 0 {
		return 64
	}
	sorted := append([]int(nil), values...)
	sort.Ints(sorted)
	return sorted[len(sorted)/2]
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
