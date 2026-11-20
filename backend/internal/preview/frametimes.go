package preview

import (
	"time"

	"github.com/nianzhibai/91/backend/internal/mediasig"
)

// TeaserSourceWindows returns the source windows of the primary teaser plan
// for duration, in teaser order. A teaser whose segment fell back to another
// start shows different content in that segment.
//
// The windows are also where perceptual signatures are sampled, and only
// signatures sampled at the same relative positions are comparable. Changing
// the plan requires recomputing every stored signature.
func TeaserSourceWindows(duration float64) []mediasig.Window {
	plan := buildTeaserPlan(Config{}, duration)
	if plan.eachSec <= 0 {
		return nil
	}
	windows := make([]mediasig.Window, 0, len(plan.starts))
	for _, start := range plan.starts {
		windows = append(windows, mediasig.Window{
			Start:    time.Duration(start * float64(time.Second)),
			Duration: time.Duration(plan.eachSec * float64(time.Second)),
		})
	}
	return windows
}
