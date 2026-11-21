package preview

import (
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"math"
	"os"
)

// Covers only serve viewers, so any span a job reads may supply one. Within a
// span, ffmpeg's thumbnail filter keeps the frame closest to the span's
// average picture, which skips cuts, flashes and motion blur. Across spans,
// the usable frame closest to the middle of the video wins: neither black,
// washed out nor flat.
const (
	// coverSampleRate is the frames per second the thumbnail filter compares.
	coverSampleRate = 4
	// coverSpan is the length of the span the thumbnail worker samples at
	// each offset.
	coverSpan = 2.0

	coverMinBrightness = 28.0
	coverMaxBrightness = 228.0
	coverMinContrast   = 16.0
	// coverGrid bounds the sampled points per axis when measuring a cover.
	coverGrid = 64
)

// coverFilter selects the most representative frame of a span lasting span
// seconds and prepares it for JPEG encoding. Frames are scaled before the
// thumbnail filter buffers them.
func coverFilter(width int, span float64) string {
	frames := int(math.Ceil(span*coverSampleRate)) + 1
	return fmt.Sprintf("fps=%d,%s,thumbnail=n=%d", coverSampleRate, thumbnailVideoFilter(width), frames)
}

// coverQuality is the luma brightness and contrast of a cover, both 0–255.
type coverQuality struct {
	brightness float64
	contrast   float64
}

func (q coverQuality) usable() bool {
	return q.brightness >= coverMinBrightness && q.brightness <= coverMaxBrightness && q.contrast >= coverMinContrast
}

// measureCover samples the luma of an encoded cover on a coarse grid.
func measureCover(path string) (coverQuality, error) {
	file, err := os.Open(path)
	if err != nil {
		return coverQuality{}, err
	}
	defer file.Close()
	img, _, err := image.Decode(file)
	if err != nil {
		return coverQuality{}, fmt.Errorf("decode cover: %w", err)
	}
	bounds := img.Bounds()
	if bounds.Empty() {
		return coverQuality{}, fmt.Errorf("decode cover: empty image")
	}
	stepX := max(1, bounds.Dx()/coverGrid)
	stepY := max(1, bounds.Dy()/coverGrid)
	var sum, sumSquares, n float64
	for y := bounds.Min.Y + stepY/2; y < bounds.Max.Y; y += stepY {
		for x := bounds.Min.X + stepX/2; x < bounds.Max.X; x += stepX {
			luma := float64(color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y)
			sum += luma
			sumSquares += luma * luma
			n++
		}
	}
	mean := sum / n
	return coverQuality{brightness: mean, contrast: math.Sqrt(max(0, sumSquares/n-mean*mean))}, nil
}

// coverCandidate is an encoded frame selected from the span centered at the
// source time at.
type coverCandidate struct {
	path string
	at   float64
}

// pickCover returns the usable candidate closest to middle or, when none is
// usable, the one with the most contrast. Unreadable candidates are skipped;
// the result is empty when no candidate can be read.
func pickCover(candidates []coverCandidate, middle float64) string {
	best, bestUsable := "", false
	var bestDistance, bestContrast float64
	for _, candidate := range candidates {
		quality, err := measureCover(candidate.path)
		if err != nil {
			continue
		}
		distance := math.Abs(candidate.at - middle)
		better := best == ""
		switch {
		case better:
		case quality.usable() != bestUsable:
			better = quality.usable()
		case bestUsable:
			better = distance < bestDistance
		default:
			better = quality.contrast > bestContrast
		}
		if better {
			best, bestUsable, bestDistance, bestContrast = candidate.path, quality.usable(), distance, quality.contrast
		}
	}
	return best
}
