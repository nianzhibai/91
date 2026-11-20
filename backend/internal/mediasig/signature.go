// Package mediasig computes compact perceptual signatures of video content and
// decides whether two signatures show the same video.
//
// A signature is a list of frames sampled from a few windows of the source.
// Every frame keeps its own source timestamp, so comparison never depends on
// how the frames were obtained: frames decoded from the source, from locally
// copied clips or from an existing teaser are all compared in source time.
package mediasig

import "time"

// Window is a span of source time.
type Window struct {
	Start    time.Duration
	Duration time.Duration
}

// Frame is one sampled source frame.
type Frame struct {
	// At is the frame's source timestamp.
	At time.Duration
	// Window indexes the sampled window the frame belongs to.
	Window int
	// Hash is the 64-bit perceptual hash of the frame's content region.
	Hash uint64
	// Informative frames carry enough detail to be compared. Black, flat and
	// mostly letterboxed frames match each other regardless of content.
	Informative bool
}

// Signature is the perceptual signature of one video.
type Signature struct {
	// Duration is the source duration. Comparison maps timestamps between
	// two videos proportionally to their durations.
	Duration time.Duration
	// Frames are ordered by At.
	Frames []Frame
}

// InformativeFrames returns the number of frames that take part in comparison.
func (s *Signature) InformativeFrames() int {
	if s == nil {
		return 0
	}
	n := 0
	for _, frame := range s.Frames {
		if frame.Informative {
			n++
		}
	}
	return n
}
