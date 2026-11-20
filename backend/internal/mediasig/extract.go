package mediasig

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// SampleInterval is the spacing of sampled frames inside a window.
	SampleInterval = 500 * time.Millisecond

	extractTimeout = 2 * time.Minute
)

// Clip is a local media file holding the source window Window. Offset is the
// position inside the file where the window begins: a full source file uses
// Offset == Window.Start, a clip cut at the window start uses zero.
type Clip struct {
	Path   string
	Offset time.Duration
	Window Window
}

// Extract samples every clip at SampleInterval with one ffmpeg process and
// returns the signature of a source of the given duration. A clip that yields
// no frames, for example a window past the end of the file, only contributes
// nothing; an unreadable file fails the extraction.
func Extract(ctx context.Context, ffmpegPath string, duration time.Duration, clips []Clip) (*Signature, error) {
	if len(clips) == 0 {
		return nil, errors.New("mediasig: no clips")
	}
	dir, err := os.MkdirTemp("", "mediasig-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	filters := make([]string, 0, len(clips))
	outputs := make([]string, 0, len(clips)*6)
	for i, clip := range clips {
		args = append(args,
			"-ss", seconds(clip.Offset),
			"-t", seconds(clip.Window.Duration),
			"-i", clip.Path,
		)
		filters = append(filters, fmt.Sprintf("[%d:v:0]%s[s%d]", i, sampleFilter, i))
		outputs = append(outputs, "-map", fmt.Sprintf("[s%d]", i), "-f", "rawvideo", clipOutputPath(dir, i))
	}
	args = append(args, "-filter_complex", strings.Join(filters, ";"))
	args = append(args, outputs...)
	if err := runFFmpeg(ctx, ffmpegPath, args, nil); err != nil {
		return nil, err
	}

	signature := &Signature{Duration: duration}
	for i, clip := range clips {
		raw, err := os.ReadFile(clipOutputPath(dir, i))
		if err != nil {
			return nil, err
		}
		for j, gray := range splitFrames(raw) {
			offset := time.Duration(j) * SampleInterval
			if offset >= clip.Window.Duration {
				break
			}
			signature.Frames = append(signature.Frames, newFrame(clip.Window.Start+offset, i, gray))
		}
	}
	return finish(signature)
}

var sampleFilter = fmt.Sprintf("fps=%s,scale=%d:%d:flags=area,format=gray",
	strconv.FormatFloat(float64(time.Second)/float64(SampleInterval), 'f', -1, 64), GridSize, GridSize)

func newFrame(at time.Duration, window int, gray []byte) Frame {
	hash, informative := HashFrame(gray)
	return Frame{At: at, Window: window, Hash: hash, Informative: informative}
}

func finish(signature *Signature) (*Signature, error) {
	if len(signature.Frames) == 0 {
		return nil, errors.New("mediasig: no frames sampled")
	}
	return signature, nil
}

func splitFrames(raw []byte) [][]byte {
	frames := make([][]byte, 0, len(raw)/frameBytes)
	for len(raw) >= frameBytes {
		frames = append(frames, raw[:frameBytes])
		raw = raw[frameBytes:]
	}
	return frames
}

func clipOutputPath(dir string, index int) string {
	return filepath.Join(dir, strconv.Itoa(index)+".gray")
}

func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 3, 64)
}

func runFFmpeg(ctx context.Context, ffmpegPath string, args []string, stdout *bytes.Buffer) error {
	if strings.TrimSpace(ffmpegPath) == "" {
		ffmpegPath = "ffmpeg"
	}
	ctx, cancel := context.WithTimeout(ctx, extractTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	var stderr bytes.Buffer
	if stdout != nil {
		cmd.Stdout = stdout
	}
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if len(message) > 300 {
			message = message[:300] + "..."
		}
		return fmt.Errorf("mediasig: ffmpeg: %w, stderr: %s", err, message)
	}
	return nil
}
