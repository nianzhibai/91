package mediasig

import (
	"math"
	"math/rand"
	"testing"
)

// picture renders a deterministic natural-looking image: random waves whose
// amplitude falls with frequency, like the spectrum of real photographs.
// Different seeds give unrelated pictures.
func picture(seed int64) func(u, v float64) float64 {
	rng := rand.New(rand.NewSource(seed))
	type wave struct{ fu, fv, phase, amplitude float64 }
	waves := make([]wave, 96)
	for i := range waves {
		fu, fv := rng.Float64()*8, rng.Float64()*8
		waves[i] = wave{fu: fu, fv: fv, phase: rng.Float64() * 2 * math.Pi, amplitude: 24 / (1 + math.Hypot(fu, fv))}
	}
	return func(u, v float64) float64 {
		value := 128.0
		for _, w := range waves {
			value += w.amplitude * math.Sin(2*math.Pi*(w.fu*u+w.fv*v)+w.phase)
		}
		return value
	}
}

// render draws img into the frame region rows [top, bottom), filling the rest
// with black bars.
func render(img func(u, v float64) float64, top, bottom int) []byte {
	frame := make([]byte, frameBytes)
	for y := top; y < bottom; y++ {
		for x := range GridSize {
			u := (float64(x) + 0.5) / GridSize
			v := (float64(y-top) + 0.5) / float64(bottom-top)
			frame[y*GridSize+x] = byte(math.Max(0, math.Min(255, img(u, v))))
		}
	}
	return frame
}

func addNoise(frame []byte, amplitude int, seed int64) []byte {
	rng := rand.New(rand.NewSource(seed))
	out := make([]byte, len(frame))
	for i, v := range frame {
		out[i] = byte(max(0, min(255, int(v)+rng.Intn(2*amplitude+1)-amplitude)))
	}
	return out
}

func mustHash(t *testing.T, frame []byte) uint64 {
	t.Helper()
	hash, informative := HashFrame(frame)
	if !informative {
		t.Fatal("frame is not informative")
	}
	return hash
}

func TestHashFrameToleratesNoiseAndLetterbox(t *testing.T) {
	original := mustHash(t, render(picture(1), 0, GridSize))
	noisy := mustHash(t, addNoise(render(picture(1), 0, GridSize), 12, 1))
	letterboxed := mustHash(t, render(picture(1), 16, GridSize-16))

	if d := Distance(original, noisy); d > MaxFrameDistance/2 {
		t.Fatalf("noisy copy distance = %d", d)
	}
	if d := Distance(original, letterboxed); d > MaxFrameDistance/2 {
		t.Fatalf("letterboxed copy distance = %d", d)
	}
}

func TestHashFrameSeparatesDifferentPictures(t *testing.T) {
	base := mustHash(t, render(picture(1), 0, GridSize))
	for _, seed := range []int64{2, 3, 4, 5} {
		other := mustHash(t, render(picture(seed), 0, GridSize))
		if d := Distance(base, other); d <= 2*MaxFrameDistance {
			t.Fatalf("seed %d distance = %d, want clearly different", seed, d)
		}
	}
}

func TestHashFrameRejectsFlatFrames(t *testing.T) {
	for name, frame := range map[string][]byte{
		"black": make([]byte, frameBytes),
		"gray":  render(func(u, v float64) float64 { return 140 }, 0, GridSize),
		"fade":  addNoise(make([]byte, frameBytes), 3, 2),
	} {
		if _, informative := HashFrame(frame); informative {
			t.Fatalf("%s frame is informative", name)
		}
	}
	if _, informative := HashFrame(make([]byte, 10)); informative {
		t.Fatal("short frame is informative")
	}
}
