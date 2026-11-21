package dedupe

import (
	"context"
	"testing"
	"time"
)

func TestBuildCompressesIntermediateCanonicalToFinalSurvivor(t *testing.T) {
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	shared := windowSignature(1, 2, 3, 4)
	candidates := []Candidate{
		{ID: "exact-loser", Size: 100, SampledSHA256: "same", AssetScore: 0, CreatedAt: now, DurationSeconds: 300, Signature: shared, Revision: "1"},
		{ID: "exact-winner", Size: 100, SampledSHA256: "same", AssetScore: 1, CreatedAt: now.Add(time.Second), DurationSeconds: 300, Signature: shared, Revision: "2"},
		{ID: "content-winner", Size: 200, SampledSHA256: "different", AssetScore: 0, CreatedAt: now.Add(2 * time.Second), DurationSeconds: 300, Signature: shared, Revision: "3"},
	}

	plan, err := Build(context.Background(), candidates, Options{Channels: AllChannels})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Actions) != 2 {
		t.Fatalf("actions = %#v, want two deletions", plan.Actions)
	}
	for _, action := range plan.Actions {
		if action.CanonicalVideoID != "content-winner" {
			t.Fatalf("action = %#v, want final canonical content-winner", action)
		}
		if action.VideoID == "exact-loser" {
			if action.Evidence.Reason != ReasonSampledSHA256 || action.Evidence.MatchedVideoID != "exact-winner" || action.Evidence.SelectedVideoID != "exact-winner" || action.Evidence.SelectionReason != "more_complete_assets" {
				t.Fatalf("exact evidence must keep its original match and selection: %+v", action.Evidence)
			}
		} else if action.Evidence.Reason != ReasonSignature || action.Evidence.MatchedVideoID != "content-winner" || action.Evidence.SelectionReason != "larger_file" {
			t.Fatalf("content evidence = %+v", action.Evidence)
		}
	}
	if plan.Redirects["exact-loser"] != "content-winner" || plan.Redirects["exact-winner"] != "content-winner" {
		t.Fatalf("redirects = %#v", plan.Redirects)
	}
	if plan.Stats.Exact.Groups != 1 || plan.Stats.Exact.Deleted != 1 || plan.Stats.Signature.Groups != 1 || plan.Stats.Signature.Deleted != 1 {
		t.Fatalf("stats = %#v", plan.Stats)
	}
}

func TestBuildExactCanonicalPrefersAssetsThenAge(t *testing.T) {
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	plan, err := Build(context.Background(), []Candidate{
		{ID: "old", Size: 100, SampledSHA256: "same", AssetScore: 1, CreatedAt: now},
		{ID: "new-complete", Size: 100, SampledSHA256: "same", AssetScore: 2, CreatedAt: now.Add(time.Hour)},
		{ID: "newer-complete", Size: 100, SampledSHA256: "same", AssetScore: 2, CreatedAt: now.Add(2 * time.Hour)},
	}, Options{Channels: ChannelExact})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Actions) != 2 {
		t.Fatalf("actions = %#v", plan.Actions)
	}
	for _, action := range plan.Actions {
		if action.CanonicalVideoID != "new-complete" {
			t.Fatalf("action = %#v, want most complete then earliest", action)
		}
	}
}

func TestBuildExactJoinsProviderHashAndSampledFingerprint(t *testing.T) {
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	plan, err := Build(context.Background(), []Candidate{
		{ID: "a", ContentHash: "ABC", Size: 100, CreatedAt: now},
		{ID: "b", ContentHash: "abc", Size: 100, SampledSHA256: "s", CreatedAt: now.Add(time.Second)},
		{ID: "c", Size: 100, SampledSHA256: "S", CreatedAt: now.Add(2 * time.Second)},
		{ID: "other-size", Size: 101, SampledSHA256: "s", CreatedAt: now.Add(3 * time.Second)},
	}, Options{Channels: ChannelExact})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Actions) != 2 || plan.Stats.Exact.Groups != 1 {
		t.Fatalf("actions=%#v stats=%#v, want a, b and c in one group", plan.Actions, plan.Stats.Exact)
	}
	reasons := map[string]string{}
	for _, action := range plan.Actions {
		if action.CanonicalVideoID != "a" {
			t.Fatalf("action = %#v, want earliest a", action)
		}
		reasons[action.VideoID] = action.Evidence.Reason + ":" + action.Evidence.MatchedVideoID
	}
	// c shares only the sampled fingerprint with b, so its evidence must not
	// claim the provider hash that links b to a.
	if reasons["b"] != ReasonContentHash+":a" || reasons["c"] != ReasonSampledSHA256+":b" {
		t.Fatalf("evidence = %#v", reasons)
	}
}
