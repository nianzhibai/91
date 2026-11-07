package mediasim

import (
	"image"
	"math"
	"strings"
	"unicode"

	"github.com/nianzhibai/91/backend/internal/mediaasset"
)

const (
	ssimSampleSize    = 96
	minCoreTitleRunes = 12
)

// 近重复判定（标题 + 封面通道）的统一阈值。爬虫导入与夜间维护共用，
// 避免两处各自定义后漂移。
const (
	// NearDuplicateTitleThreshold 标题相似度下限。
	NearDuplicateTitleThreshold = 0.90
	// NearDuplicateThumbSSIMThreshold 封面 SSIM 下限。
	NearDuplicateThumbSSIMThreshold = 0.95
	// NearDuplicateDurationToleranceSeconds 近重复候选的时长容差（秒），
	// 标题/封面通道与内容级通道共用。
	NearDuplicateDurationToleranceSeconds = 2
)

var titleCoreSeparators = []string{
	" - ",
	" -- ",
	" — ",
	" – ",
	" ｜ ",
	" | ",
	"＿",
	"_",
	"－",
	"—",
	"–",
	"-",
	"|",
}

// TitleSimilarity returns the best normalized Levenshtein similarity in [0, 1]
// between the full titles and their leading core title segments.
func TitleSimilarity(a, b string) float64 {
	leftVariants := titleVariants(a)
	rightVariants := titleVariants(b)
	if len(leftVariants) == 0 && len(rightVariants) == 0 {
		return 1
	}
	if len(leftVariants) == 0 || len(rightVariants) == 0 {
		return 0
	}
	best := 0.0
	for _, left := range leftVariants {
		for _, right := range rightVariants {
			score := normalizedLevenshteinSimilarity(left, right)
			if score > best {
				best = score
			}
		}
	}
	return best
}

// TitleKeys returns the normalized full title and core-title variants used by
// TitleSimilarity. It is intended for cheap caller-side prefiltering before
// running the heavier Levenshtein comparison.
func TitleKeys(value string) []string {
	return append([]string(nil), titleVariants(value)...)
}

func normalizedLevenshteinSimilarity(left, right string) float64 {
	leftRunes := []rune(left)
	rightRunes := []rune(right)
	if len(leftRunes) == 0 && len(rightRunes) == 0 {
		return 1
	}
	if len(leftRunes) == 0 || len(rightRunes) == 0 {
		return 0
	}
	maxLen := len(leftRunes)
	if len(rightRunes) > maxLen {
		maxLen = len(rightRunes)
	}
	return 1 - float64(levenshtein(leftRunes, rightRunes))/float64(maxLen)
}

func titleVariants(value string) []string {
	full := normalizeTitle(value)
	if full == "" {
		return nil
	}
	out := appendTitleVariant(nil, full)
	if core := normalizeTitleCore(value); core != "" && core != full {
		out = appendTitleVariant(out, core)
	}
	for _, tail := range titleTailVariants(value) {
		normalized := normalizeTitle(tail)
		if len([]rune(normalized)) >= minCoreTitleRunes {
			out = appendTitleVariant(out, normalized)
		}
	}
	return out
}

func appendTitleVariant(out []string, value string) []string {
	for _, existing := range out {
		if existing == value {
			return out
		}
	}
	return append(out, value)
}

func titleTailVariants(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var out []string
	for _, sep := range []string{"@", "＠"} {
		if idx := strings.LastIndex(value, sep); idx >= 0 && idx+len(sep) < len(value) {
			out = append(out, strings.TrimSpace(value[idx+len(sep):]))
		}
	}
	return out
}

func normalizeTitleCore(value string) string {
	head := strings.TrimSpace(value)
	for _, sep := range titleCoreSeparators {
		if idx := strings.Index(head, sep); idx > 0 {
			head = strings.TrimSpace(head[:idx])
			break
		}
	}
	normalized := normalizeTitle(head)
	if len([]rune(normalized)) < minCoreTitleRunes {
		return ""
	}
	return normalized
}

func normalizeTitle(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, ext := range []string{".mp4", ".m4v", ".mkv", ".mov", ".avi", ".webm", ".ts", ".m3u8"} {
		if strings.HasSuffix(value, ext) {
			value = strings.TrimSuffix(value, ext)
			break
		}
	}
	var b strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		return b.String()
	}
	return strings.Join(strings.Fields(value), "")
}

func levenshtein(a, b []rune) int {
	if len(a) < len(b) {
		a, b = b, a
	}
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			current[j] = minInt(
				previous[j]+1,
				current[j-1]+1,
				previous[j-1]+cost,
			)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}

func minInt(values ...int) int {
	min := values[0]
	for _, value := range values[1:] {
		if value < min {
			min = value
		}
	}
	return min
}

// ImageSSIM compares two local images using luminance SSIM over a fixed grid.
func ImageSSIM(leftPath, rightPath string) (float64, error) {
	left, err := decodeImage(leftPath)
	if err != nil {
		return 0, err
	}
	right, err := decodeImage(rightPath)
	if err != nil {
		return 0, err
	}
	return SSIM(left, right), nil
}

func decodeImage(path string) (image.Image, error) {
	return mediaasset.DecodeImage(path)
}

// SSIM compares two images after nearest-neighbor sampling onto the same grid.
func SSIM(left, right image.Image) float64 {
	if left == nil || right == nil {
		return 0
	}
	leftSamples := grayscaleSamples(left, ssimSampleSize, ssimSampleSize)
	rightSamples := grayscaleSamples(right, ssimSampleSize, ssimSampleSize)
	if len(leftSamples) == 0 || len(leftSamples) != len(rightSamples) {
		return 0
	}

	var leftMean, rightMean float64
	for i := range leftSamples {
		leftMean += leftSamples[i]
		rightMean += rightSamples[i]
	}
	n := float64(len(leftSamples))
	leftMean /= n
	rightMean /= n

	var leftVariance, rightVariance, covariance float64
	for i := range leftSamples {
		leftDelta := leftSamples[i] - leftMean
		rightDelta := rightSamples[i] - rightMean
		leftVariance += leftDelta * leftDelta
		rightVariance += rightDelta * rightDelta
		covariance += leftDelta * rightDelta
	}
	leftVariance /= n
	rightVariance /= n
	covariance /= n

	const c1 = 6.5025  // (0.01 * 255)^2
	const c2 = 58.5225 // (0.03 * 255)^2
	denominator := (leftMean*leftMean + rightMean*rightMean + c1) * (leftVariance + rightVariance + c2)
	if denominator == 0 {
		return 0
	}
	score := ((2*leftMean*rightMean + c1) * (2*covariance + c2)) / denominator
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return 0
	}
	return score
}

func grayscaleSamples(img image.Image, width, height int) []float64 {
	bounds := img.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 || width <= 0 || height <= 0 {
		return nil
	}
	out := make([]float64, 0, width*height)
	for y := 0; y < height; y++ {
		sourceY := bounds.Min.Y + y*bounds.Dy()/height
		for x := 0; x < width; x++ {
			sourceX := bounds.Min.X + x*bounds.Dx()/width
			r, g, b, _ := img.At(sourceX, sourceY).RGBA()
			out = append(out, 0.299*float64(r>>8)+0.587*float64(g>>8)+0.114*float64(b>>8))
		}
	}
	return out
}
