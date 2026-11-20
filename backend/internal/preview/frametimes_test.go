package preview

import (
	"math"
	"testing"
)

func TestTeaserSourceWindowsFollowPrimaryPlan(t *testing.T) {
	windows := TeaserSourceWindows(1000)
	plan := buildTeaserPlan(Config{}, 1000)
	if len(windows) != len(plan.starts) {
		t.Fatalf("windows = %d, want %d", len(windows), len(plan.starts))
	}
	for i, window := range windows {
		if math.Abs(window.Start.Seconds()-plan.starts[i]) > 1e-6 || math.Abs(window.Duration.Seconds()-plan.eachSec) > 1e-6 {
			t.Fatalf("window %d = %+v, want start %.2f length %.2f", i, window, plan.starts[i], plan.eachSec)
		}
	}
}
