// Package natural provides the deterministic "natural" text ordering shared by
// the directory collection view and the shorts recommendation feed.
//
// Digit runs compare numerically, so "第2集" sorts before "第10集", while every
// other rune keeps a case-insensitive order. Callers that need a total order
// append their own tiebreaker (video id) after this comparison.
package natural

import "strings"

// Compare returns -1, 0 or 1 for left vs right.
func Compare(left, right string) int {
	leftRunes := []rune(strings.ToLower(strings.TrimSpace(left)))
	rightRunes := []rune(strings.ToLower(strings.TrimSpace(right)))
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
