package mediasig

import (
	"encoding/json"
	"math/rand"
	"testing"
	"time"
)

var testFractions = []float64{0.15, 0.38, 0.62, 0.85}

// syntheticVideo returns a hash for any source time: the picture changes
// every second and every video seed shows different pictures.
func syntheticVideo(seed int64) func(at time.Duration) uint64 {
	return func(at time.Duration) uint64 {
		return rand.New(rand.NewSource(seed*1_000_003 + int64(at/time.Second))).Uint64()
	}
}

func flipBits(hash uint64, count int, seed int64) uint64 {
	rng := rand.New(rand.NewSource(seed))
	for _, bit := range rng.Perm(64)[:count] {
		hash ^= 1 << uint(bit)
	}
	return hash
}

// sample builds a signature of a video sampled at testFractions of duration.
// shift moves the sampling positions, as a rounded duration or a seek would.
func sample(video func(time.Duration) uint64, duration, shift time.Duration, noise int) *Signature {
	signature := &Signature{Duration: duration}
	for window, fraction := range testFractions {
		start := time.Duration(fraction*float64(duration)) + shift
		for i := range 6 {
			at := start + time.Duration(i)*SampleInterval
			hash := flipBits(video(at), noise, int64(at))
			signature.Frames = append(signature.Frames, Frame{At: at, Window: window, Hash: hash, Informative: true})
		}
	}
	return signature
}

func TestCompareMatchesShiftedReencode(t *testing.T) {
	video := syntheticVideo(1)
	original := sample(video, 600*time.Second, 0, 0)
	copy := sample(video, 601*time.Second, 700*time.Millisecond, 6)

	got := Compare(original, copy)
	if got.Verdict != VerdictStrong {
		t.Fatalf("comparison = %+v, want strong", got)
	}
	if reverse := Compare(copy, original); reverse != got {
		t.Fatalf("comparison is not symmetric: %+v vs %+v", got, reverse)
	}
}

func TestCompareRejectsDifferentVideos(t *testing.T) {
	got := Compare(sample(syntheticVideo(1), 600*time.Second, 0, 0), sample(syntheticVideo(2), 600*time.Second, 0, 0))
	if got.Verdict != VerdictNone || got.MatchedFrames != 0 {
		t.Fatalf("comparison = %+v, want none", got)
	}
}

func TestCompareToleratesOneDisplacedWindow(t *testing.T) {
	video := syntheticVideo(3)
	original := sample(video, 900*time.Second, 0, 0)
	copy := sample(video, 900*time.Second, 0, 4)
	// A failed segment fell back to another start; its frames match nothing
	// but must not misalign the remaining windows.
	for i := range copy.Frames {
		if copy.Frames[i].Window == 1 {
			copy.Frames[i].Hash = syntheticVideo(99)(copy.Frames[i].At)
		}
	}
	if got := Compare(original, copy); got.Verdict != VerdictStrong || got.MatchedWindows != 3 {
		t.Fatalf("comparison = %+v, want strong over three windows", got)
	}
}

func TestCompareTwoMatchingWindowsIsProbable(t *testing.T) {
	video := syntheticVideo(4)
	original := sample(video, 900*time.Second, 0, 0)
	copy := sample(video, 900*time.Second, 0, 0)
	for i := range copy.Frames {
		if copy.Frames[i].Window >= 2 {
			copy.Frames[i].Hash = syntheticVideo(98)(copy.Frames[i].At)
		}
	}
	if got := Compare(original, copy); got.Verdict != VerdictProbable {
		t.Fatalf("comparison = %+v, want probable", got)
	}
}

// fixedCamera shows nearly the same picture all the time: every second
// differs from the room's base picture by a few bits.
func fixedCamera(at time.Duration) uint64 {
	return flipBits(0x5a5a_1234_9876_f0f0, 4, int64(at/time.Second))
}

func TestCompareStaticSegmentsOfOneRecordingAreProbable(t *testing.T) {
	// Two segments of one recording show other moments of the same scene.
	first := sample(fixedCamera, 600*time.Second, 0, 0)
	laterSegment := func(at time.Duration) uint64 { return fixedCamera(at + 37*time.Second) }
	second := sample(laterSegment, 600*time.Second, 0, 0)
	got := Compare(first, second)
	if !got.Static || got.Verdict != VerdictProbable || got.MedianDistance <= StaticMaxMedianDistance {
		t.Fatalf("comparison = %+v, want static probable", got)
	}
}

func TestCompareStaticRepostIsStrong(t *testing.T) {
	// A repost shows the same moments; only encoding noise differs.
	original := sample(fixedCamera, 600*time.Second, 0, 0)
	repost := sample(fixedCamera, 601*time.Second, 0, 2)
	got := Compare(original, repost)
	if !got.Static || got.Verdict != VerdictStrong {
		t.Fatalf("comparison = %+v, want static strong", got)
	}
}

func TestCompareIgnoresUninformativeFrames(t *testing.T) {
	// The picture holds for a minute, so every frame of the original finds
	// its match. The copy keeps one informative frame per window, which is
	// too little evidence in its direction.
	slow := func(at time.Duration) uint64 { return syntheticVideo(5)(at / time.Minute * time.Minute) }
	original := sample(slow, 600*time.Second, 0, 0)
	copy := sample(slow, 600*time.Second, 0, 0)
	for i := range copy.Frames {
		copy.Frames[i].Informative = i%6 == 0
	}
	if got := Compare(original, copy); got.Verdict != VerdictNone || got.Frames >= MinComparableFrames {
		t.Fatalf("comparison = %+v, want too little evidence", got)
	}
}

func TestComparisonJSONRoundTrip(t *testing.T) {
	for _, verdict := range []Verdict{VerdictNone, VerdictProbable, VerdictStrong} {
		original := Comparison{Verdict: verdict, Windows: 4, MatchedWindows: 3, Frames: 20, MatchedFrames: 15, MedianDistance: 4, Static: true}
		data, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		var decoded Comparison
		if err := json.Unmarshal(data, &decoded); err != nil || decoded != original {
			t.Fatalf("decoded = %+v err = %v from %s", decoded, err, data)
		}
	}
	var verdict Verdict
	if err := json.Unmarshal([]byte(`"certain"`), &verdict); err == nil {
		t.Fatal("unknown verdict decoded")
	}
}
