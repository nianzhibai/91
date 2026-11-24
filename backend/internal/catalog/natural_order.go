package catalog

import (
	"strings"
	"time"
)

// NaturalVideoOrderLess is the natural order of videos inside one directory:
// file names compare digit runs numerically, so episode 2 precedes episode 10.
func NaturalVideoOrderLess(left, right *Video) bool {
	if left == nil || right == nil {
		return left != nil
	}
	return NaturalVideoOrderFieldsLess(
		left.FileName,
		left.Title,
		left.ID,
		left.CreatedAt,
		right.FileName,
		right.Title,
		right.ID,
		right.CreatedAt,
	)
}

// NaturalVideoOrderFieldsLess is NaturalVideoOrderLess over the fields it
// reads, for callers that load only those columns.
func NaturalVideoOrderFieldsLess(
	leftFileName, leftTitle, leftID string,
	leftCreatedAt time.Time,
	rightFileName, rightTitle, rightID string,
	rightCreatedAt time.Time,
) bool {
	left := newNaturalVideoKey(leftFileName, leftTitle, leftID, leftCreatedAt)
	right := newNaturalVideoKey(rightFileName, rightTitle, rightID, rightCreatedAt)
	return left.compare(right) < 0
}

// naturalVideoKey is what the natural video order compares, normalized once so
// sorting many videos does not redo it on every comparison.
type naturalVideoKey struct {
	name      []rune
	createdAt time.Time
	id        string
}

func newNaturalVideoKey(fileName, title, id string, createdAt time.Time) naturalVideoKey {
	return naturalVideoKey{
		name:      naturalRunes(firstNonEmptyString(fileName, title, id)),
		createdAt: createdAt,
		id:        id,
	}
}

func (k naturalVideoKey) compare(other naturalVideoKey) int {
	if compared := compareNaturalRunes(k.name, other.name); compared != 0 {
		return compared
	}
	if compared := k.createdAt.Compare(other.createdAt); compared != 0 {
		return compared
	}
	return strings.Compare(k.id, other.id)
}

func naturalRunes(value string) []rune {
	return []rune(strings.ToLower(strings.TrimSpace(value)))
}

// naturalCompare compares ASCII digit runs numerically while keeping all other
// Unicode runes in a deterministic case-insensitive order (episode 2 < 10).
func naturalCompare(left, right string) int {
	return compareNaturalRunes(naturalRunes(left), naturalRunes(right))
}

func compareNaturalRunes(leftRunes, rightRunes []rune) int {
	for li, ri := 0, 0; li < len(leftRunes) && ri < len(rightRunes); {
		if isASCIIDigit(leftRunes[li]) && isASCIIDigit(rightRunes[ri]) {
			leftEnd, rightEnd := li, ri
			for leftEnd < len(leftRunes) && isASCIIDigit(leftRunes[leftEnd]) {
				leftEnd++
			}
			for rightEnd < len(rightRunes) && isASCIIDigit(rightRunes[rightEnd]) {
				rightEnd++
			}
			leftSignificant, rightSignificant := li, ri
			for leftSignificant < leftEnd-1 && leftRunes[leftSignificant] == '0' {
				leftSignificant++
			}
			for rightSignificant < rightEnd-1 && rightRunes[rightSignificant] == '0' {
				rightSignificant++
			}
			leftDigits := leftRunes[leftSignificant:leftEnd]
			rightDigits := rightRunes[rightSignificant:rightEnd]
			if len(leftDigits) != len(rightDigits) {
				if len(leftDigits) < len(rightDigits) {
					return -1
				}
				return 1
			}
			for index := range leftDigits {
				if leftDigits[index] < rightDigits[index] {
					return -1
				}
				if leftDigits[index] > rightDigits[index] {
					return 1
				}
			}
			// Numerically equal runs use the shorter spelling first: 2 before 02.
			if leftEnd-li != rightEnd-ri {
				if leftEnd-li < rightEnd-ri {
					return -1
				}
				return 1
			}
			li, ri = leftEnd, rightEnd
			continue
		}
		if leftRunes[li] < rightRunes[ri] {
			return -1
		}
		if leftRunes[li] > rightRunes[ri] {
			return 1
		}
		li++
		ri++
	}
	if len(leftRunes) < len(rightRunes) {
		return -1
	}
	if len(leftRunes) > len(rightRunes) {
		return 1
	}
	return 0
}

func isASCIIDigit(value rune) bool {
	return value >= '0' && value <= '9'
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
