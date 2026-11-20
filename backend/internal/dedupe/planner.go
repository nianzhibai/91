package dedupe

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nianzhibai/91/backend/internal/mediasig"
	"github.com/nianzhibai/91/backend/internal/mediasim"
)

type ImageComparator func(leftPath, rightPath string) (float64, error)

type Options struct {
	Channels      Channels
	CompareImages ImageComparator
	// Distinct reports pairs a person judged not to be duplicates. The
	// signature channel neither merges nor reviews them.
	Distinct func(leftID, rightID string) bool
}

type plannerState struct {
	candidates map[string]Candidate
	alive      map[string]bool
	redirects  map[string]string
	distinct   func(leftID, rightID string) bool
	reviews    []Match
	plan       Plan
}

// Build runs the selected channels in production order. A channel only sees
// survivors from earlier channels, but no database or filesystem mutation is
// performed until the returned plan is applied by a caller.
func Build(ctx context.Context, candidates []Candidate, options Options) (Plan, error) {
	if options.Channels == 0 {
		options.Channels = AllChannels
	}
	if options.CompareImages == nil {
		options.CompareImages = mediasim.ImageSSIM
	}
	if options.Distinct == nil {
		options.Distinct = func(string, string) bool { return false }
	}
	state := plannerState{
		candidates: make(map[string]Candidate, len(candidates)),
		alive:      make(map[string]bool, len(candidates)),
		redirects:  make(map[string]string),
		distinct:   options.Distinct,
		plan: Plan{
			Redirects: make(map[string]string),
			Stats:     Stats{Videos: len(candidates)},
		},
	}
	ordered := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		candidate.ID = strings.TrimSpace(candidate.ID)
		if candidate.ID == "" {
			continue
		}
		if _, exists := state.candidates[candidate.ID]; exists {
			return Plan{}, fmt.Errorf("dedupe: duplicate candidate ID %q", candidate.ID)
		}
		state.candidates[candidate.ID] = candidate
		state.alive[candidate.ID] = true
		ordered = append(ordered, candidate)
	}
	state.plan.Stats.Videos = len(ordered)

	if options.Channels.includes(ChannelExact) {
		if err := state.planExact(ctx, ordered); err != nil {
			return Plan{}, err
		}
	}
	if options.Channels.includes(ChannelNear) {
		if err := state.planNear(ctx, ordered, options.CompareImages); err != nil {
			return Plan{}, err
		}
	}
	if options.Channels.includes(ChannelSignature) {
		if err := state.planSignature(ctx, ordered); err != nil {
			return Plan{}, err
		}
	}
	if err := state.finalize(); err != nil {
		return Plan{}, err
	}
	if err := state.plan.Validate(); err != nil {
		return Plan{}, err
	}
	return state.plan, nil
}

// planExact connects candidates sharing any exact identity key. Each recorded
// edge names the key that matched, so evidence never claims a signal that a
// transitively connected pair does not share.
func (s *plannerState) planExact(ctx context.Context, candidates []Candidate) error {
	exact := make([]Candidate, 0, len(candidates))
	buckets := make(map[ExactKey][]int)
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !s.alive[candidate.ID] {
			continue
		}
		keys := ExactKeys(candidate)
		if len(keys) == 0 {
			continue
		}
		index := len(exact)
		exact = append(exact, candidate)
		for _, key := range keys {
			buckets[key] = append(buckets[key], index)
		}
	}
	s.plan.Stats.Exact.Candidates = len(exact)
	keys := make([]ExactKey, 0, len(buckets))
	for key, members := range buckets {
		if len(members) > 1 {
			keys = append(keys, key)
		}
	}
	// content_hash sorts before sampled_sha256, so a pair sharing both keys
	// records the stronger provider hash.
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Reason != keys[j].Reason {
			return keys[i].Reason < keys[j].Reason
		}
		return keys[i].Value < keys[j].Value
	})
	sets := newDisjointSet(len(exact))
	linked := make(map[uint64]struct{})
	for _, key := range keys {
		members := buckets[key]
		first := members[0]
		for _, member := range members[1:] {
			pair := pairKey(first, member)
			if _, exists := linked[pair]; exists {
				continue
			}
			linked[pair] = struct{}{}
			sets.union(first, member)
			s.plan.Matches = append(s.plan.Matches, Match{
				Stage: StageExact, Reason: key.Reason, LeftID: exact[member].ID, RightID: exact[first].ID,
			})
		}
	}
	for _, group := range groupedCandidates(exact, sets) {
		canonical := selectCanonical(group, betterExactCanonical)
		if canonical.ID == "" {
			continue
		}
		s.plan.Stats.Exact.Groups++
		s.plan.Stats.Exact.Deleted += s.markGroup(StageExact, group, canonical)
	}
	return nil
}

type nearCandidate struct {
	Candidate
	titleKeys    []string
	titleQGrams  map[string]struct{}
	titleBuckets []string
}

func (s *plannerState) planNear(ctx context.Context, candidates []Candidate, compare ImageComparator) error {
	near := make([]nearCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if !s.alive[candidate.ID] || strings.TrimSpace(candidate.Title) == "" || candidate.DurationSeconds <= 0 || strings.TrimSpace(candidate.ThumbnailPath) == "" {
			continue
		}
		keys := mediasim.TitleKeys(candidate.Title)
		if len(keys) == 0 {
			continue
		}
		buckets := mediasim.TitlePrefixBuckets(keys, 12)
		if len(buckets) == 0 {
			continue
		}
		near = append(near, nearCandidate{
			Candidate:    candidate,
			titleKeys:    keys,
			titleQGrams:  mediasim.TitleQGrams(keys, 4),
			titleBuckets: buckets,
		})
	}
	sort.Slice(near, func(i, j int) bool {
		if near[i].DurationSeconds != near[j].DurationSeconds {
			return near[i].DurationSeconds < near[j].DurationSeconds
		}
		return earlier(near[i].Candidate, near[j].Candidate)
	})
	s.plan.Stats.Near.Candidates = len(near)
	if len(near) < 2 {
		return nil
	}

	countedCompare := func(leftPath, rightPath string) (float64, error) {
		s.plan.Stats.Near.Comparisons++
		return compare(leftPath, rightPath)
	}
	sets := newDisjointSet(len(near))
	bucketIndex := make(map[int]map[string][]int)
	seenPairs := make(map[uint64]struct{})
	for i, right := range near {
		if err := ctx.Err(); err != nil {
			return err
		}
		for duration := right.DurationSeconds - mediasim.NearDuplicateDurationToleranceSeconds; duration <= right.DurationSeconds+mediasim.NearDuplicateDurationToleranceSeconds; duration++ {
			byBucket := bucketIndex[duration]
			if len(byBucket) == 0 {
				continue
			}
			for _, bucket := range right.titleBuckets {
				for _, j := range byBucket[bucket] {
					key := pairKey(i, j)
					if _, seen := seenPairs[key]; seen {
						continue
					}
					seenPairs[key] = struct{}{}
					left := near[j]
					if !nearTitlePrefilter(left, right) {
						continue
					}
					match, matched, err := MatchNear(left.Candidate, right.Candidate, countedCompare)
					if err != nil {
						s.plan.Issues = append(s.plan.Issues, Issue{Stage: StageNear, LeftID: left.ID, RightID: right.ID, Err: err})
						continue
					}
					if matched {
						sets.union(i, j)
						s.plan.Matches = append(s.plan.Matches, match)
					}
				}
			}
		}
		byBucket := bucketIndex[right.DurationSeconds]
		if byBucket == nil {
			byBucket = make(map[string][]int)
			bucketIndex[right.DurationSeconds] = byBucket
		}
		for _, bucket := range right.titleBuckets {
			byBucket[bucket] = append(byBucket[bucket], i)
		}
	}

	groups := groupedNearCandidates(near, sets)
	for _, group := range groups {
		plain := make([]Candidate, len(group))
		for i := range group {
			plain[i] = group[i].Candidate
		}
		canonical := selectCanonical(plain, betterPerceptualCanonical)
		if canonical.ID == "" {
			continue
		}
		s.plan.Stats.Near.Groups++
		s.plan.Stats.Near.Deleted += s.markGroup(StageNear, plain, canonical)
	}
	return nil
}

// planSignature merges strong pairs only, and only into a canonical the member
// matched directly. Transitive closure would let a chain of similar videos
// merge two that never matched each other; such pairs and probable pairs are
// left for review instead.
func (s *plannerState) planSignature(ctx context.Context, candidates []Candidate) error {
	signed := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if s.alive[candidate.ID] && SignatureEligible(candidate) &&
			candidate.Signature.InformativeFrames() >= mediasig.MinComparableFrames {
			signed = append(signed, candidate)
		}
	}
	sort.Slice(signed, func(i, j int) bool {
		if signed[i].DurationSeconds != signed[j].DurationSeconds {
			return signed[i].DurationSeconds < signed[j].DurationSeconds
		}
		return earlier(signed[i], signed[j])
	})
	stats := &s.plan.Stats.Signature
	stats.Candidates = len(signed)

	strong := make(map[uint64]Match)
	var strongKeys []uint64
	sets := newDisjointSet(len(signed))
	windowStart := 0
	for i := range signed {
		if err := ctx.Err(); err != nil {
			return err
		}
		for windowStart < i && signed[i].DurationSeconds-signed[windowStart].DurationSeconds > mediasim.NearDuplicateDurationToleranceSeconds {
			windowStart++
		}
		for j := windowStart; j < i; j++ {
			if s.distinct(signed[j].ID, signed[i].ID) {
				continue
			}
			stats.Comparisons++
			match, verdict := MatchSignature(signed[j], signed[i])
			switch verdict {
			case mediasig.VerdictStrong:
				key := pairKey(i, j)
				strong[key] = match
				strongKeys = append(strongKeys, key)
				sets.union(i, j)
			case mediasig.VerdictProbable:
				s.reviews = append(s.reviews, match)
			}
		}
	}

	// survivor records the canonical each merged candidate joined. A strong
	// pair is settled when both sides end up with the same survivor.
	survivor := make(map[int]int)
	for _, remaining := range groupIndices(len(signed), sets) {
		for len(remaining) > 1 {
			canonical := remaining[0]
			for _, k := range remaining[1:] {
				if betterPerceptualCanonical(signed[k], signed[canonical]) {
					canonical = k
				}
			}
			star := []Candidate{signed[canonical]}
			rest := make([]int, 0, len(remaining))
			for _, k := range remaining {
				if k == canonical {
					continue
				}
				match, direct := strong[pairKey(k, canonical)]
				if !direct {
					rest = append(rest, k)
					continue
				}
				survivor[k] = canonical
				s.plan.Matches = append(s.plan.Matches, match)
				star = append(star, signed[k])
			}
			if len(star) > 1 {
				survivor[canonical] = canonical
				stats.Groups++
				stats.Deleted += s.markGroup(StageSignature, star, signed[canonical])
			}
			remaining = rest
		}
	}
	for _, key := range strongKeys {
		left, right := int(key>>32), int(uint32(key))
		leftSurvivor, leftMerged := survivor[left]
		rightSurvivor, rightMerged := survivor[right]
		if !leftMerged || !rightMerged || leftSurvivor != rightSurvivor {
			s.reviews = append(s.reviews, strong[key])
		}
	}
	return nil
}

func (s *plannerState) markGroup(stage Stage, group []Candidate, canonical Candidate) int {
	members := make([]string, 0, len(group))
	for _, candidate := range group {
		if candidate.ID != "" && s.alive[candidate.ID] {
			members = append(members, candidate.ID)
		}
	}
	if len(members) > 1 {
		s.plan.Groups = append(s.plan.Groups, Group{
			Stage: stage, CanonicalVideoID: canonical.ID, MemberIDs: members,
		})
	}
	deleted := 0
	for _, candidate := range group {
		if candidate.ID == canonical.ID || !s.alive[candidate.ID] {
			continue
		}
		s.alive[candidate.ID] = false
		s.redirects[candidate.ID] = canonical.ID
		s.plan.Actions = append(s.plan.Actions, DeleteAction{
			Stage: stage, VideoID: candidate.ID, CanonicalVideoID: canonical.ID,
			ExpectedRevision: candidate.Revision,
			Evidence:         NewEvidence(ReasonUnknown, "", canonical.ID, canonicalSelectionReason(stage, canonical, candidate)),
		})
		deleted++
	}
	return deleted
}

func (s *plannerState) finalize() error {
	resolve := func(id string) (string, error) {
		seen := make(map[string]struct{})
		for {
			if _, duplicate := seen[id]; duplicate {
				return "", fmt.Errorf("dedupe: canonical redirect cycle at %q", id)
			}
			seen[id] = struct{}{}
			next, redirected := s.redirects[id]
			if !redirected {
				if !s.alive[id] {
					return "", fmt.Errorf("dedupe: canonical %q is not a survivor", id)
				}
				return id, nil
			}
			id = next
		}
	}
	for i := range s.plan.Actions {
		canonical, err := resolve(s.plan.Actions[i].CanonicalVideoID)
		if err != nil {
			return err
		}
		s.plan.Actions[i].CanonicalVideoID = canonical
		s.plan.Actions[i].CanonicalExpectedRevision = s.candidates[canonical].Revision
	}
	for i := range s.plan.Groups {
		canonical, err := resolve(s.plan.Groups[i].CanonicalVideoID)
		if err != nil {
			return err
		}
		s.plan.Groups[i].CanonicalVideoID = canonical
	}
	for duplicate := range s.redirects {
		canonical, err := resolve(duplicate)
		if err != nil {
			return err
		}
		s.plan.Redirects[duplicate] = canonical
	}
	if err := s.finalizeReviews(resolve); err != nil {
		return err
	}
	return s.attachEvidence()
}

// finalizeReviews points every review at the surviving videos and keeps the
// strongest evidence per pair. Pairs that collapse into one video, or that a
// person already judged distinct, are dropped.
func (s *plannerState) finalizeReviews(resolve func(string) (string, error)) error {
	index := make(map[[2]string]int)
	for _, match := range s.reviews {
		left, err := resolve(match.LeftID)
		if err != nil {
			return err
		}
		right, err := resolve(match.RightID)
		if err != nil {
			return err
		}
		if left == right || s.distinct(left, right) {
			continue
		}
		if left > right {
			left, right = right, left
		}
		key := [2]string{left, right}
		if i, exists := index[key]; exists {
			if reviewVerdict(match) > reviewVerdict(s.plan.Reviews[i].Match) {
				s.plan.Reviews[i].Match = match
			}
			continue
		}
		index[key] = len(s.plan.Reviews)
		s.plan.Reviews = append(s.plan.Reviews, Review{LeftID: left, RightID: right, Match: match})
	}
	return nil
}

func reviewVerdict(match Match) mediasig.Verdict {
	if match.Signature == nil {
		return mediasig.VerdictNone
	}
	return match.Signature.Verdict
}

func groupedNearCandidates(candidates []nearCandidate, sets *disjointSet) [][]nearCandidate {
	return groupedBy(candidates, sets)
}

func groupedCandidates(candidates []Candidate, sets *disjointSet) [][]Candidate {
	return groupedBy(candidates, sets)
}

func groupedBy[T any](items []T, sets *disjointSet) [][]T {
	indices := groupIndices(len(items), sets)
	groups := make([][]T, 0, len(indices))
	for _, members := range indices {
		group := make([]T, len(members))
		for i, member := range members {
			group[i] = items[member]
		}
		groups = append(groups, group)
	}
	return groups
}

// groupIndices returns the connected components with more than one member,
// ordered by root, each listing its members in index order.
func groupIndices(n int, sets *disjointSet) [][]int {
	byRoot := make(map[int][]int)
	for i := range n {
		root := sets.find(i)
		byRoot[root] = append(byRoot[root], i)
	}
	roots := make([]int, 0, len(byRoot))
	for root, members := range byRoot {
		if len(members) > 1 {
			roots = append(roots, root)
		}
	}
	sort.Ints(roots)
	groups := make([][]int, 0, len(roots))
	for _, root := range roots {
		groups = append(groups, byRoot[root])
	}
	return groups
}

func selectCanonical(group []Candidate, better func(Candidate, Candidate) bool) Candidate {
	var best Candidate
	for _, candidate := range group {
		if candidate.ID == "" {
			continue
		}
		if best.ID == "" || better(candidate, best) {
			best = candidate
		}
	}
	return best
}

func betterExactCanonical(left, right Candidate) bool {
	if left.AssetScore != right.AssetScore {
		return left.AssetScore > right.AssetScore
	}
	return earlier(left, right)
}

func betterPerceptualCanonical(left, right Candidate) bool {
	if left.Size != right.Size {
		return left.Size > right.Size
	}
	if left.AssetScore != right.AssetScore {
		return left.AssetScore > right.AssetScore
	}
	return earlier(left, right)
}

func earlier(left, right Candidate) bool {
	if !left.CreatedAt.Equal(right.CreatedAt) {
		return left.CreatedAt.Before(right.CreatedAt)
	}
	return left.ID < right.ID
}

func nearTitlePrefilter(left, right nearCandidate) bool {
	if !mediasim.TitleLengthCouldReachThreshold(left.titleKeys, right.titleKeys, mediasim.NearDuplicateTitleThreshold) {
		return false
	}
	return mediasim.QGramContainment(left.titleQGrams, right.titleQGrams) >= 0.45
}

func pairKey(left, right int) uint64 {
	if left > right {
		left, right = right, left
	}
	return uint64(uint32(left))<<32 | uint64(uint32(right))
}
