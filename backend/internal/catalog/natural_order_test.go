package catalog

import "testing"

func TestNaturalCompareOrdersEpisodeNumbers(t *testing.T) {
	for _, test := range []struct {
		left  string
		right string
		want  int
	}{
		{left: "episode 2", right: "episode 10", want: -1},
		{left: "episode 02", right: "episode 2", want: 1},
		{left: "Episode 10", right: "episode 10", want: 0},
		{left: "part 12a", right: "part 12b", want: -1},
	} {
		got := naturalCompare(test.left, test.right)
		if got < 0 {
			got = -1
		} else if got > 0 {
			got = 1
		}
		if got != test.want {
			t.Fatalf("naturalCompare(%q, %q) = %d, want %d", test.left, test.right, got, test.want)
		}
	}
}
