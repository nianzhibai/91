package dedupe

import (
	"context"
	"testing"
	"time"

	"github.com/video-site/backend/internal/mediasim"
)

func TestBuildCompressesIntermediateCanonicalToFinalSurvivor(t *testing.T) {
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	shared := &mediasim.FrameSignature{Frames: syntheticFrames(7)}
	candidates := []Candidate{
		{ID: "exact-loser", Size: 100, SampledSHA256: "same", AssetScore: 0, CreatedAt: now, DurationSeconds: 300, TeaserPath: "loser", ExpectedUpdatedAt: 1},
		{ID: "exact-winner", Size: 100, SampledSHA256: "same", AssetScore: 1, CreatedAt: now.Add(time.Second), DurationSeconds: 300, TeaserPath: "winner", ExpectedUpdatedAt: 2},
		{ID: "content-winner", Size: 200, SampledSHA256: "different", AssetScore: 0, CreatedAt: now.Add(2 * time.Second), DurationSeconds: 300, TeaserPath: "content", ExpectedUpdatedAt: 3},
	}

	plan, err := Build(context.Background(), candidates, Options{
		Channels: AllChannels,
		LoadContentSignature: func(context.Context, Candidate) (*mediasim.FrameSignature, error) {
			return shared, nil
		},
	})
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
	}
	if plan.Redirects["exact-loser"] != "content-winner" || plan.Redirects["exact-winner"] != "content-winner" {
		t.Fatalf("redirects = %#v", plan.Redirects)
	}
	if plan.Stats.Exact.Groups != 1 || plan.Stats.Exact.Deleted != 1 || plan.Stats.Content.Groups != 1 || plan.Stats.Content.Deleted != 1 {
		t.Fatalf("stats = %#v", plan.Stats)
	}
}

func TestBuildNearGroupsUseTransitiveClosure(t *testing.T) {
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	candidates := []Candidate{
		{ID: "a", Title: "同一个视频标题", Size: 100, CreatedAt: now, DurationSeconds: 300, ThumbnailPath: "a"},
		{ID: "b", Title: "同一个视频标题", Size: 200, CreatedAt: now.Add(time.Second), DurationSeconds: 300, ThumbnailPath: "b"},
		{ID: "c", Title: "同一个视频标题", Size: 300, CreatedAt: now.Add(2 * time.Second), DurationSeconds: 300, ThumbnailPath: "c"},
	}
	scores := map[string]float64{
		"a:b": 0.96,
		"a:c": 0.10,
		"b:c": 0.96,
	}
	plan, err := Build(context.Background(), candidates, Options{
		Channels: ChannelNear,
		CompareImages: func(left, right string) (float64, error) {
			if left > right {
				left, right = right, left
			}
			return scores[left+":"+right], nil
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if plan.Stats.Near.Groups != 1 || plan.Stats.Near.Deleted != 2 {
		t.Fatalf("stats = %#v, want one transitive group and two deletions", plan.Stats.Near)
	}
	for _, action := range plan.Actions {
		if action.CanonicalVideoID != "c" {
			t.Fatalf("action = %#v, want largest member c", action)
		}
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

func syntheticFrames(seed byte) [][]byte {
	frames := make([][]byte, mediasim.FrameSignatureMaxFrames)
	for i := range frames {
		frame := make([]byte, mediasim.FrameSignatureGridSize*mediasim.FrameSignatureGridSize)
		for j := range frame {
			frame[j] = byte((int(seed) + i*17 + j*31) % 256)
		}
		frames[i] = frame
	}
	return frames
}
