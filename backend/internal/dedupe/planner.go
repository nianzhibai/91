package dedupe

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/video-site/backend/internal/mediasim"
)

type ImageComparator func(leftPath, rightPath string) (float64, error)
type SignatureLoader func(context.Context, Candidate) (*mediasim.FrameSignature, error)

type Options struct {
	Channels             Channels
	CompareImages        ImageComparator
	LoadContentSignature SignatureLoader
}

type plannerState struct {
	candidates map[string]Candidate
	alive      map[string]bool
	redirects  map[string]string
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
	state := plannerState{
		candidates: make(map[string]Candidate, len(candidates)),
		alive:      make(map[string]bool, len(candidates)),
		redirects:  make(map[string]string),
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
	if options.Channels.includes(ChannelContent) {
		if err := state.planContent(ctx, ordered, options.LoadContentSignature); err != nil {
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

type exactKey struct {
	size    int64
	sampled string
}

func (s *plannerState) planExact(ctx context.Context, candidates []Candidate) error {
	groups := make(map[exactKey][]Candidate)
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !s.alive[candidate.ID] {
			continue
		}
		sampled := strings.ToLower(strings.TrimSpace(candidate.SampledSHA256))
		if candidate.Size <= 0 || sampled == "" {
			continue
		}
		key := exactKey{size: candidate.Size, sampled: sampled}
		groups[key] = append(groups[key], candidate)
	}
	keys := make([]exactKey, 0, len(groups))
	for key, group := range groups {
		if len(group) > 1 {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].size != keys[j].size {
			return keys[i].size < keys[j].size
		}
		return keys[i].sampled < keys[j].sampled
	})
	for _, key := range keys {
		group := groups[key]
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
					titleScore := mediasim.TitleSimilarity(left.Title, right.Title)
					if titleScore < mediasim.NearDuplicateTitleThreshold {
						continue
					}
					s.plan.Stats.Near.Comparisons++
					ssim, err := compare(left.ThumbnailPath, right.ThumbnailPath)
					if err != nil {
						s.plan.Issues = append(s.plan.Issues, Issue{Stage: StageNear, LeftID: left.ID, RightID: right.ID, Err: err})
						continue
					}
					if ssim >= mediasim.NearDuplicateThumbSSIMThreshold {
						sets.union(i, j)
						s.plan.Matches = append(s.plan.Matches, Match{Stage: StageNear, LeftID: left.ID, RightID: right.ID, Score: ssim})
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

func (s *plannerState) planContent(ctx context.Context, candidates []Candidate, load SignatureLoader) error {
	content := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if !s.alive[candidate.ID] || candidate.DurationSeconds < mediasim.ContentDuplicateMinDurationSeconds || strings.TrimSpace(candidate.TeaserPath) == "" {
			continue
		}
		content = append(content, candidate)
	}
	sort.Slice(content, func(i, j int) bool {
		if content[i].DurationSeconds != content[j].DurationSeconds {
			return content[i].DurationSeconds < content[j].DurationSeconds
		}
		return earlier(content[i], content[j])
	})
	s.plan.Stats.Content.Candidates = len(content)
	if len(content) < 2 {
		return nil
	}
	if load == nil {
		return fmt.Errorf("dedupe: content channel requires a signature loader")
	}

	sigs := make(map[int]*mediasim.FrameSignature)
	failed := make(map[int]bool)
	ensureSignature := func(i int) *mediasim.FrameSignature {
		if signature, ok := sigs[i]; ok {
			return signature
		}
		if failed[i] {
			return nil
		}
		signature, err := load(ctx, content[i])
		if err != nil || signature == nil || signature.InformativeFrames() < mediasim.ContentDuplicateMinComparisons {
			failed[i] = true
			s.plan.Stats.Content.ExtractFailed++
			if err != nil {
				s.plan.Issues = append(s.plan.Issues, Issue{Stage: StageContent, VideoID: content[i].ID, Err: err})
			}
			return nil
		}
		sigs[i] = signature
		s.plan.Stats.Content.Extracted++
		return signature
	}

	sets := newDisjointSet(len(content))
	windowStart := 0
	for i := range content {
		if err := ctx.Err(); err != nil {
			return err
		}
		for windowStart < i && content[i].DurationSeconds-content[windowStart].DurationSeconds > mediasim.NearDuplicateDurationToleranceSeconds {
			delete(sigs, windowStart)
			delete(failed, windowStart)
			windowStart++
		}
		if windowStart == i {
			continue
		}
		rightSignature := ensureSignature(i)
		if rightSignature == nil {
			continue
		}
		for j := windowStart; j < i; j++ {
			leftSignature := ensureSignature(j)
			if leftSignature == nil {
				continue
			}
			comparison := mediasim.CompareFrameSignatures(leftSignature, rightSignature)
			s.plan.Stats.Content.Comparisons++
			if comparison.IsContentDuplicate() {
				sets.union(i, j)
				s.plan.Matches = append(s.plan.Matches, Match{
					Stage: StageContent, LeftID: content[j].ID, RightID: content[i].ID,
					Score: comparison.MedianSSIM, Comparisons: comparison.Comparisons,
				})
				continue
			}
			if content[j].DurationSeconds != content[i].DurationSeconds {
				continue
			}
			cross := mediasim.CompareFrameSignaturesCross(leftSignature, rightSignature)
			if !cross.IsContentDuplicate() {
				continue
			}
			sets.union(i, j)
			s.plan.Stats.Content.CrossMatched++
			s.plan.Matches = append(s.plan.Matches, Match{
				Stage: StageContent, LeftID: content[j].ID, RightID: content[i].ID,
				Score: cross.MedianBest, Comparisons: min(cross.LeftFrames, cross.RightFrames), Cross: true,
			})
		}
	}

	groups := groupedCandidates(content, sets)
	for _, group := range groups {
		canonical := selectCanonical(group, betterPerceptualCanonical)
		if canonical.ID == "" {
			continue
		}
		s.plan.Stats.Content.Groups++
		s.plan.Stats.Content.Deleted += s.markGroup(StageContent, group, canonical)
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
			ExpectedUpdatedAt: candidate.ExpectedUpdatedAt,
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
		s.plan.Actions[i].CanonicalExpectedUpdatedAt = s.candidates[canonical].ExpectedUpdatedAt
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
	return nil
}

func groupedNearCandidates(candidates []nearCandidate, sets *disjointSet) [][]nearCandidate {
	byRoot := make(map[int][]nearCandidate)
	for i, candidate := range candidates {
		root := sets.find(i)
		byRoot[root] = append(byRoot[root], candidate)
	}
	roots := make([]int, 0, len(byRoot))
	for root, group := range byRoot {
		if len(group) > 1 {
			roots = append(roots, root)
		}
	}
	sort.Ints(roots)
	groups := make([][]nearCandidate, 0, len(roots))
	for _, root := range roots {
		groups = append(groups, byRoot[root])
	}
	return groups
}

func groupedCandidates(candidates []Candidate, sets *disjointSet) [][]Candidate {
	byRoot := make(map[int][]Candidate)
	for i, candidate := range candidates {
		root := sets.find(i)
		byRoot[root] = append(byRoot[root], candidate)
	}
	roots := make([]int, 0, len(byRoot))
	for root, group := range byRoot {
		if len(group) > 1 {
			roots = append(roots, root)
		}
	}
	sort.Ints(roots)
	groups := make([][]Candidate, 0, len(roots))
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
