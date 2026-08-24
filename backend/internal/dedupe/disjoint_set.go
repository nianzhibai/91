package dedupe

type disjointSet struct {
	parent []int
	rank   []int
}

func newDisjointSet(n int) *disjointSet {
	parent := make([]int, n)
	rank := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	return &disjointSet{parent: parent, rank: rank}
}

func (s *disjointSet) find(x int) int {
	if s.parent[x] != x {
		s.parent[x] = s.find(s.parent[x])
	}
	return s.parent[x]
}

func (s *disjointSet) union(a, b int) {
	rootA := s.find(a)
	rootB := s.find(b)
	if rootA == rootB {
		return
	}
	if s.rank[rootA] < s.rank[rootB] {
		s.parent[rootA] = rootB
		return
	}
	if s.rank[rootA] > s.rank[rootB] {
		s.parent[rootB] = rootA
		return
	}
	s.parent[rootB] = rootA
	s.rank[rootA]++
}
