package mediasig

import (
	"math"
	"math/bits"
	"sort"
)

const (
	// GridSize is the edge of the square grayscale frame decoded for hashing.
	// Frames are stretched to a square; trimming black bars afterwards maps
	// letterboxed and full-frame versions of a picture onto the same region.
	GridSize   = 128
	frameBytes = GridSize * GridSize

	// dctSize is the edge of the content image fed to the DCT. The hash keeps
	// the 8×8 lowest frequencies after the DC row and column.
	dctSize  = 32
	hashEdge = 8

	// A bar row or column is dark and flat. Bars are only trimmed from the
	// edges, at most maxBarLines (30% of the grid) from each.
	barMaxMean   = 32.0
	barMaxStdDev = 10.0
	maxBarLines  = GridSize * 3 / 10

	// informativeMinStdDev rejects black, faded and flat frames, which match
	// one another regardless of the video they come from.
	informativeMinStdDev = 6.0
)

// Distance returns the number of differing bits between two frame hashes.
func Distance(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

// HashFrame returns the perceptual hash of a GridSize×GridSize grayscale frame
// and whether the frame is informative. Black bars are trimmed first so that
// letterboxed, pillarboxed and full-frame versions hash alike.
func HashFrame(gray []byte) (uint64, bool) {
	if len(gray) != frameBytes {
		return 0, false
	}
	region := contentRegion(gray)
	if regionStdDev(gray, region) < informativeMinStdDev {
		return 0, false
	}
	return phash(resizeRegion(gray, region)), true
}

type rect struct{ x0, y0, x1, y1 int }

func (r rect) width() int  { return r.x1 - r.x0 }
func (r rect) height() int { return r.y1 - r.y0 }

func contentRegion(gray []byte) rect {
	r := rect{0, 0, GridSize, GridSize}
	isBarRow := func(y int) bool { return isBar(gray, rect{r.x0, y, r.x1, y + 1}) }
	for r.y0 < maxBarLines && isBarRow(r.y0) {
		r.y0++
	}
	for GridSize-r.y1 < maxBarLines && isBarRow(r.y1-1) {
		r.y1--
	}
	isBarColumn := func(x int) bool { return isBar(gray, rect{x, r.y0, x + 1, r.y1}) }
	for r.x0 < maxBarLines && isBarColumn(r.x0) {
		r.x0++
	}
	for GridSize-r.x1 < maxBarLines && isBarColumn(r.x1-1) {
		r.x1--
	}
	return r
}

func isBar(gray []byte, r rect) bool {
	mean, stdDev := regionStats(gray, r)
	return mean <= barMaxMean && stdDev <= barMaxStdDev
}

func regionStdDev(gray []byte, r rect) float64 {
	_, stdDev := regionStats(gray, r)
	return stdDev
}

func regionStats(gray []byte, r rect) (mean, stdDev float64) {
	n := float64(r.width() * r.height())
	if n <= 0 {
		return 0, 0
	}
	var sum, sumSquares float64
	for y := r.y0; y < r.y1; y++ {
		for _, v := range gray[y*GridSize+r.x0 : y*GridSize+r.x1] {
			f := float64(v)
			sum += f
			sumSquares += f * f
		}
	}
	mean = sum / n
	variance := sumSquares/n - mean*mean
	return mean, math.Sqrt(math.Max(0, variance))
}

// resizeRegion box-averages the region onto a dctSize×dctSize grid. Trimming
// keeps at least 40% of each dimension, so every cell covers a source pixel.
func resizeRegion(gray []byte, r rect) []float64 {
	out := make([]float64, dctSize*dctSize)
	for cy := 0; cy < dctSize; cy++ {
		y0 := r.y0 + cy*r.height()/dctSize
		y1 := max(y0+1, r.y0+(cy+1)*r.height()/dctSize)
		for cx := 0; cx < dctSize; cx++ {
			x0 := r.x0 + cx*r.width()/dctSize
			x1 := max(x0+1, r.x0+(cx+1)*r.width()/dctSize)
			var sum float64
			for y := y0; y < y1; y++ {
				for _, v := range gray[y*GridSize+x0 : y*GridSize+x1] {
					sum += float64(v)
				}
			}
			out[cy*dctSize+cx] = sum / float64((y1-y0)*(x1-x0))
		}
	}
	return out
}

var dctCos = func() [hashEdge + 1][dctSize]float64 {
	var table [hashEdge + 1][dctSize]float64
	for u := range table {
		for x := range dctSize {
			table[u][x] = math.Cos(float64(2*x+1) * float64(u) * math.Pi / (2 * dctSize))
		}
	}
	return table
}()

// phash sets one bit per low-frequency DCT coefficient above their median.
// The DC row and column are excluded: they encode brightness, not structure.
func phash(pixels []float64) uint64 {
	// Separable DCT restricted to the frequencies the hash keeps.
	var rows [dctSize][hashEdge]float64
	for y := range dctSize {
		line := pixels[y*dctSize : (y+1)*dctSize]
		for v := 1; v <= hashEdge; v++ {
			var sum float64
			for x, p := range line {
				sum += p * dctCos[v][x]
			}
			rows[y][v-1] = sum
		}
	}
	coefficients := make([]float64, 0, hashEdge*hashEdge)
	for u := 1; u <= hashEdge; u++ {
		for v := range hashEdge {
			var sum float64
			for y := range dctSize {
				sum += rows[y][v] * dctCos[u][y]
			}
			coefficients = append(coefficients, sum)
		}
	}
	sorted := append([]float64(nil), coefficients...)
	sort.Float64s(sorted)
	median := (sorted[len(sorted)/2-1] + sorted[len(sorted)/2]) / 2
	var hash uint64
	for i, c := range coefficients {
		if c > median {
			hash |= 1 << uint(i)
		}
	}
	return hash
}
