package main

func tagClusterPairKey(left, right int) uint64 {
	if left > right {
		left, right = right, left
	}
	return uint64(uint32(left))<<32 | uint64(uint32(right))
}

type tagClusterDisjointSet struct {
	parent []int
	rank   []int
}

func newTagClusterDisjointSet(n int) *tagClusterDisjointSet {
	parent := make([]int, n)
	rank := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	return &tagClusterDisjointSet{parent: parent, rank: rank}
}

func (s *tagClusterDisjointSet) find(x int) int {
	if s.parent[x] != x {
		s.parent[x] = s.find(s.parent[x])
	}
	return s.parent[x]
}

func (s *tagClusterDisjointSet) union(a, b int) {
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
