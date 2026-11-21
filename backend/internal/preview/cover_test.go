package preview

import (
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

// writeCoverJPEG writes a cover whose luma ramps from low to low+span across
// its width: span sets the contrast, low+span/2 the brightness.
func writeCoverJPEG(t *testing.T, path string, low, span uint8) {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 160, 90))
	for y := range 90 {
		for x := range 160 {
			img.SetGray(x, y, color.Gray{Y: low + uint8(int(span)*x/159)})
		}
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := jpeg.Encode(file, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
}

func TestPickCoverPrefersUsableFrameClosestToMiddle(t *testing.T) {
	dir := t.TempDir()
	cover := func(name string, low, span uint8) string {
		path := filepath.Join(dir, name+".jpg")
		writeCoverJPEG(t, path, low, span)
		return path
	}
	black := cover("black", 0, 10)
	washed := cover("washed", 235, 20)
	flat := cover("flat", 120, 8)
	early := cover("early", 40, 160)
	late := cover("late", 60, 120)
	broken := filepath.Join(dir, "broken.jpg")
	if err := os.WriteFile(broken, []byte("not a picture"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"black", "washed", "flat"} {
		if quality, _ := measureCover(filepath.Join(dir, name+".jpg")); quality.usable() {
			t.Fatalf("%s cover %+v counted as usable", name, quality)
		}
	}
	got := pickCover([]coverCandidate{
		{path: broken, at: 50}, {path: black, at: 51}, {path: early, at: 20}, {path: late, at: 70}, {path: flat, at: 50},
	}, 50)
	if got != late {
		t.Fatalf("cover = %s, want the usable frame closest to the middle", filepath.Base(got))
	}
	// Without a usable frame, the one with the most detail is the cover.
	if got := pickCover([]coverCandidate{{path: black, at: 50}, {path: washed, at: 10}, {path: flat, at: 90}}, 50); got != washed {
		t.Fatalf("cover = %s, want the frame with the most contrast", filepath.Base(got))
	}
	if got := pickCover([]coverCandidate{{path: broken, at: 50}}, 50); got != "" {
		t.Fatalf("cover = %s, want none from unreadable candidates", got)
	}
}

func TestThumbnailOffsetsPreferMiddleFrame(t *testing.T) {
	for _, tc := range []struct {
		name     string
		duration float64
		want     []float64
	}{
		{"unknown duration", 0, []float64{5, 1, 0}},
		{"long video", 1000, []float64{500, 300, 700, 5, 1, 0}},
		{"short video", 8, []float64{4, 2.4, 5.6, 5, 1, 0}},
		{"shorter than a span", 1.5, []float64{0}},
	} {
		got := thumbnailOffsets(tc.duration)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: offsets = %v, want %v", tc.name, got, tc.want)
		}
		for i := range got {
			if diff := got[i] - tc.want[i]; diff > 0.001 || diff < -0.001 {
				t.Fatalf("%s: offsets = %v, want %v", tc.name, got, tc.want)
			}
		}
	}
}
