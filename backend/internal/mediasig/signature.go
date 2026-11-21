// Package mediasig computes compact perceptual signatures of video content and
// decides whether two signatures show the same video.
//
// A signature is a list of frames sampled from a few windows of the source.
// Every frame keeps its own source timestamp, so comparison never depends on
// how the frames were obtained: frames decoded from the source, from locally
// copied clips or from an existing teaser are all compared in source time.
package mediasig

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

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

// SampledWindows returns the number of windows that contributed frames.
func (s *Signature) SampledWindows() int {
	if s == nil {
		return 0
	}
	windows := make(map[int]struct{})
	for _, frame := range s.Frames {
		windows[frame.Window] = struct{}{}
	}
	return len(windows)
}

// SignatureFormat is the first byte of every encoded signature. It versions
// the encoding and the sampling: format 3 samples the decoded source picture
// at the fixed fractions of preview.longVideoSegmentFractions. Bump it when
// the sampling or hashing changes so that stored signatures of the old scheme
// are renewed instead of compared against incompatible ones.
const SignatureFormat = 3

// MarshalBinary encodes the signature compactly for storage: about 13 bytes
// per frame.
func (s *Signature) MarshalBinary() ([]byte, error) {
	out := make([]byte, 0, 8+len(s.Frames)*14)
	out = append(out, SignatureFormat)
	out = binary.AppendUvarint(out, uint64(s.Duration/time.Millisecond))
	out = binary.AppendUvarint(out, uint64(len(s.Frames)))
	for _, frame := range s.Frames {
		if frame.At < 0 || frame.Window < 0 {
			return nil, errors.New("mediasig: negative frame position")
		}
		out = binary.AppendUvarint(out, uint64(frame.At/time.Millisecond))
		out = binary.AppendUvarint(out, uint64(frame.Window))
		informative := byte(0)
		if frame.Informative {
			informative = 1
		}
		out = append(out, informative)
		out = binary.LittleEndian.AppendUint64(out, frame.Hash)
	}
	return out, nil
}

// UnmarshalBinary decodes a signature written by MarshalBinary. Data of
// another format version is an error.
func (s *Signature) UnmarshalBinary(data []byte) error {
	if len(data) == 0 || data[0] != SignatureFormat {
		return errors.New("mediasig: unsupported signature format")
	}
	r := bytes.NewReader(data[1:])
	durationMS, err := binary.ReadUvarint(r)
	if err != nil {
		return fmt.Errorf("mediasig: decode duration: %w", err)
	}
	count, err := binary.ReadUvarint(r)
	if err != nil || count > uint64(r.Len()) {
		return errors.New("mediasig: decode frame count")
	}
	decoded := Signature{Duration: time.Duration(durationMS) * time.Millisecond, Frames: make([]Frame, 0, count)}
	for i := uint64(0); i < count; i++ {
		atMS, err := binary.ReadUvarint(r)
		if err != nil {
			return fmt.Errorf("mediasig: decode frame %d: %w", i, err)
		}
		window, err := binary.ReadUvarint(r)
		if err != nil {
			return fmt.Errorf("mediasig: decode frame %d: %w", i, err)
		}
		var fixed [9]byte
		if _, err := io.ReadFull(r, fixed[:]); err != nil {
			return fmt.Errorf("mediasig: decode frame %d: %w", i, err)
		}
		decoded.Frames = append(decoded.Frames, Frame{
			At:          time.Duration(atMS) * time.Millisecond,
			Window:      int(window),
			Informative: fixed[0] == 1,
			Hash:        binary.LittleEndian.Uint64(fixed[1:]),
		})
	}
	if r.Len() != 0 {
		return errors.New("mediasig: trailing signature bytes")
	}
	*s = decoded
	return nil
}
