package preview

import (
	"math"
	"testing"
)

func TestTeaserFrameSourceTimesLongVideo(t *testing.T) {
	// ≥10 分钟：4 段按 20%/40%/60%/80% 布置，每段 3 秒。
	times := TeaserFrameSourceTimes(1129, 12)
	if len(times) != 12 {
		t.Fatalf("len = %d, want 12", len(times))
	}
	starts := []float64{225.8, 451.6, 677.4, 903.2}
	for k, got := range times {
		want := starts[k/3] + float64(k%3)
		if math.Abs(got-want) > 1e-9 {
			t.Fatalf("times[%d] = %f, want %f", k, got, want)
		}
	}
}

func TestTeaserFrameSourceTimesMatchesPlan(t *testing.T) {
	for _, duration := range []float64{45, 130, 599, 600, 3600} {
		plan := buildTeaserPlan(Config{}, duration)
		times := TeaserFrameSourceTimes(duration, 12)
		if len(times) == 0 {
			t.Fatalf("duration %f: no times", duration)
		}
		for k, got := range times {
			seg := int(float64(k) / plan.eachSec)
			if seg >= len(plan.starts) {
				t.Fatalf("duration %f: times[%d] beyond plan segments", duration, k)
			}
			want := plan.starts[seg] + float64(k) - float64(seg)*plan.eachSec
			if math.Abs(got-want) > 1e-9 {
				t.Fatalf("duration %f: times[%d] = %f, want %f", duration, k, got, want)
			}
		}
	}
}

func TestTeaserFrameSourceTimesCapsAtPlan(t *testing.T) {
	// 短视频段数少，帧数不能超过段数 × 每段秒数。
	times := TeaserFrameSourceTimes(20, 12)
	plan := buildTeaserPlan(Config{}, 20)
	max := int(float64(len(plan.starts)) * plan.eachSec)
	if len(times) > max {
		t.Fatalf("len = %d, want <= %d", len(times), max)
	}
	if TeaserFrameSourceTimes(0, 12) != nil && len(TeaserFrameSourceTimes(0, 12)) > 12 {
		t.Fatalf("zero duration should not overflow")
	}
	if got := TeaserFrameSourceTimes(1129, 0); got != nil {
		t.Fatalf("zero frame count = %v, want nil", got)
	}
}

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
