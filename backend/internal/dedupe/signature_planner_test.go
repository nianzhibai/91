package dedupe

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/mediasig"
)

// windowSignature gives each window the pictures of a seed; two signatures
// sharing a seed in a window show the same content there.
func windowSignature(seeds ...int64) *mediasig.Signature {
	signature := &mediasig.Signature{Duration: 600 * time.Second}
	for window, seed := range seeds {
		rng := rand.New(rand.NewSource(seed))
		start := time.Duration(90+window*120) * time.Second
		for i := range 6 {
			signature.Frames = append(signature.Frames, mediasig.Frame{
				At: start + time.Duration(i)*mediasig.SampleInterval, Window: window,
				Hash: rng.Uint64(), Informative: true,
			})
		}
	}
	return signature
}

func signedCandidate(id string, size int64, signature *mediasig.Signature) Candidate {
	return Candidate{
		ID: id, Size: size, DurationSeconds: 600, Revision: id,
		CreatedAt: time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC), Signature: signature,
	}
}

func TestSignatureChannelMergesOnlyDirectStrongMatches(t *testing.T) {
	// a~b and b~c are strong, a~c only probable. Transitive closure would
	// merge a into c although they never matched strongly.
	plan, err := Build(context.Background(), []Candidate{
		signedCandidate("a", 100, windowSignature(1, 2, 3, 4)),
		signedCandidate("b", 200, windowSignature(1, 2, 3, 5)),
		signedCandidate("c", 300, windowSignature(6, 2, 3, 5)),
	}, Options{Channels: ChannelSignature})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].VideoID != "b" || plan.Actions[0].CanonicalVideoID != "c" {
		t.Fatalf("actions = %#v, want only b merged into c", plan.Actions)
	}
	evidence := plan.Actions[0].Evidence
	if evidence.Reason != ReasonSignature || evidence.MatchedVideoID != "c" || evidence.Match == nil ||
		evidence.Match.Signature == nil || evidence.Match.Signature.Verdict != mediasig.VerdictStrong {
		t.Fatalf("evidence = %+v, want the direct strong match with c", evidence)
	}

	// a-b was strong but b merged into c, and a-c is only probable. Both point
	// at the surviving pair a-c, which keeps the stronger a-b evidence.
	if len(plan.Reviews) != 1 {
		t.Fatalf("reviews = %+v, want one review of the surviving pair", plan.Reviews)
	}
	review := plan.Reviews[0]
	if review.LeftID != "a" || review.RightID != "c" || review.Match.LeftID != "a" || review.Match.RightID != "b" ||
		review.Match.Signature.Verdict != mediasig.VerdictStrong {
		t.Fatalf("review = %+v, want survivors a-c with the strong a-b evidence", review)
	}
	if stats := plan.Stats.Signature; stats.Candidates != 3 || stats.Comparisons != 3 || stats.Groups != 1 || stats.Deleted != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestSignatureChannelMergesStarIntoLargestFile(t *testing.T) {
	plan, err := Build(context.Background(), []Candidate{
		signedCandidate("small", 100, windowSignature(1, 2, 3, 4)),
		signedCandidate("large", 300, windowSignature(1, 2, 3, 4)),
		signedCandidate("medium", 200, windowSignature(1, 2, 3, 4)),
		signedCandidate("other", 400, windowSignature(7, 8, 9, 10)),
	}, Options{Channels: ChannelSignature})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Actions) != 2 || len(plan.Reviews) != 0 {
		t.Fatalf("actions = %#v reviews = %#v", plan.Actions, plan.Reviews)
	}
	for _, action := range plan.Actions {
		if action.CanonicalVideoID != "large" || action.Evidence.SelectionReason != "larger_file" {
			t.Fatalf("action = %#v, want merge into the larger file", action)
		}
	}
}

func TestSignatureEligibleStartsAtMinDuration(t *testing.T) {
	minimum := int(mediasig.MinDuration / time.Second)
	if !SignatureEligible(Candidate{DurationSeconds: minimum}) || SignatureEligible(Candidate{DurationSeconds: minimum - 1}) {
		t.Fatalf("eligibility must start at %ds", minimum)
	}
}

func TestSignatureChannelSkipsDistinctPairs(t *testing.T) {
	candidates := []Candidate{
		signedCandidate("a", 100, windowSignature(1, 2, 3, 4)),
		signedCandidate("b", 200, windowSignature(1, 2, 3, 4)),
		signedCandidate("c", 300, windowSignature(1, 2, 7, 8)),
	}
	distinct := func(left, right string) bool {
		return (left == "a" && right == "b") || (left == "b" && right == "a")
	}
	plan, err := Build(context.Background(), candidates, Options{Channels: ChannelSignature, Distinct: distinct})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Actions) != 0 {
		t.Fatalf("actions = %#v, want the distinct pair kept apart", plan.Actions)
	}
	// a-c and b-c are probable; a-b is neither compared nor reviewed.
	if len(plan.Reviews) != 2 || plan.Stats.Signature.Comparisons != 2 {
		t.Fatalf("reviews = %+v comparisons = %d", plan.Reviews, plan.Stats.Signature.Comparisons)
	}
	for _, review := range plan.Reviews {
		if review.LeftID == "a" && review.RightID == "b" {
			t.Fatalf("distinct pair reviewed: %+v", review)
		}
	}
}
