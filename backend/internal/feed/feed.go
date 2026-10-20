// Package feed turns a per-user signal set into one shorts batch.
//
// Everything here is a pure function of its input: no database, no clock, no
// randomness beyond the caller-supplied source. The API layer loads the signals
// from the catalog, calls BuildBatch, and memoises the result; the scoring
// policy stays testable without a database or an HTTP server.
package feed

import (
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/video-site/backend/internal/natural"
)

const (
	weightTagAffinity = 0.45
	weightDirAffinity = 0.25
	weightFreshness   = 0.15
	weightQuality     = 0.15

	freshnessDecayDays = 14.0

	// A batch reserves at most 40% for the directory the user just watched.
	// Handing over a whole directory turns the shorts feed into a playlist;
	// continuing is the user's decision, not the algorithm's.
	sameDirectoryShare = 0.4
	tagShare           = 0.2
	exploreShare       = 0.2

	// A directory whose most frequent tag covers at least this share of its tag
	// assignments groups one kind of content. Below it the directory is an
	// archive of whatever happened to be downloaded together, where
	// same-directory recommendations are noise rather than signal.
	semanticTagShare = 0.4

	// A directory needs this many videos sharing a filename stem before it is
	// treated as a series.
	seriesMinSiblings = 3
)

// Candidate is the minimal video projection the scoring needs.
type Candidate struct {
	ID              string
	ParentID        string
	FileName        string
	DurationSeconds int
	PublishedAt     int64
	Views           int
	Likes           int
	Tags            []string
}

// TagShare describes how concentrated one directory's tags are.
type TagShare struct {
	Tagged   int
	TopShare float64
}

// DirectoryProfile tells BuildBatch whether a directory may be used as a
// same-content source, and whether its videos form an ordered series.
type DirectoryProfile struct {
	// Semantic is false for archive directories, where "same directory" means
	// "same download batch" instead of "same kind of content".
	Semantic bool
	// Series is true when the directory holds an ordered run of episodes, so
	// recommendations continue with the neighbouring episode instead of a
	// random sibling.
	Series bool
}

// Input is everything one batch decision needs.
type Input struct {
	Now         time.Time
	Count       int
	Pool        []Candidate
	Directories map[string]DirectoryProfile
	DirAffinity map[string]float64
	TagAffinity map[string]float64
	// Exclude holds ids that must not appear in this batch: recently watched
	// videos and everything already served in the current session.
	Exclude map[string]struct{}
	// LastWatched is the video the user just consumed, or nil at session start.
	LastWatched *Candidate
	Rand        *rand.Rand
}

// ProfileDirectories classifies each directory from its tag concentration and
// its filename stems.
//
// A directory with no tags at all carries no evidence either way and is trusted
// as semantic: the user's own folder structure is a better prior than assuming
// every untagged folder is a dumping ground.
func ProfileDirectories(pool []Candidate, shares map[string]TagShare) map[string]DirectoryProfile {
	parents := make(map[string]struct{}, len(shares))
	for parentID := range shares {
		parents[parentID] = struct{}{}
	}
	stems := make(map[string]map[string]int, len(shares))
	for _, candidate := range pool {
		if candidate.ParentID == "" {
			continue
		}
		parents[candidate.ParentID] = struct{}{}
		stem := seriesStem(candidate.FileName)
		if stem == "" {
			continue
		}
		byStem := stems[candidate.ParentID]
		if byStem == nil {
			byStem = map[string]int{}
			stems[candidate.ParentID] = byStem
		}
		byStem[stem]++
	}

	profiles := make(map[string]DirectoryProfile, len(parents))
	for parentID := range parents {
		profile := DirectoryProfile{Semantic: true}
		if share, ok := shares[parentID]; ok && share.Tagged > 0 {
			profile.Semantic = share.TopShare >= semanticTagShare
		}
		for _, count := range stems[parentID] {
			if count >= seriesMinSiblings {
				profile.Series = true
				break
			}
		}
		profiles[parentID] = profile
	}
	return profiles
}

// seriesStem reduces a filename to the part that survives across episodes.
// Digit runs are dropped so "第3集" and "第4集" share a stem, and separators are
// removed so "show_01" and "show 02" do too.
func seriesStem(fileName string) string {
	name := strings.TrimSpace(fileName)
	if name == "" {
		return ""
	}
	if dot := strings.LastIndexByte(name, '.'); dot > 0 {
		name = name[:dot]
	}
	var digitsRemoved strings.Builder
	for _, r := range name {
		if !unicode.IsDigit(r) {
			digitsRemoved.WriteRune(r)
		}
	}
	stem := strings.Map(func(r rune) rune {
		switch r {
		case '_', '-', '.', '(', ')', '[', ']', ' ', '\t', '#':
			return -1
		}
		return r
	}, strings.ToLower(digitsRemoved.String()))
	if len([]rune(stem)) < 2 {
		return ""
	}
	return stem
}

// BuildBatch picks up to Count ids, ordered so the most relevant video comes
// first. Callers persist the returned order as the served order.
func BuildBatch(in Input) []string {
	if in.Count <= 0 {
		return nil
	}
	rnd := in.Rand
	if rnd == nil {
		rnd = rand.New(rand.NewPCG(uint64(in.Now.UnixNano()), 0x5f3759df))
	}

	pool := make([]Candidate, 0, len(in.Pool))
	seen := make(map[string]struct{}, len(in.Pool))
	for _, candidate := range in.Pool {
		if candidate.ID == "" {
			continue
		}
		if _, excluded := in.Exclude[candidate.ID]; excluded {
			continue
		}
		if _, duplicate := seen[candidate.ID]; duplicate {
			continue
		}
		seen[candidate.ID] = struct{}{}
		pool = append(pool, candidate)
	}
	if len(pool) == 0 {
		return nil
	}
	if len(pool) <= in.Count {
		// The library is smaller than one batch: everything is a candidate and
		// ordering cannot change what the user sees.
		rnd.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
		ids := make([]string, 0, len(pool))
		for _, candidate := range pool {
			ids = append(ids, candidate.ID)
		}
		return ids
	}

	byID := make(map[string]Candidate, len(pool))
	for _, candidate := range pool {
		byID[candidate.ID] = candidate
	}

	// 同目录配额是整批的上限，不只是「同目录那一步」的上限：后面的打分补齐
	// 同样不能把一批塞满同一个目录，否则配额形同虚设。
	sameDirectoryTarget := quotaFor(in.Count, sameDirectoryShare)
	lastParentID := ""
	sameDirectoryExpands := false
	if in.LastWatched != nil {
		lastParentID = in.LastWatched.ParentID
		if profile, ok := in.Directories[lastParentID]; ok && profile.Semantic {
			sameDirectoryExpands = true
		}
	}
	// 整库都放在一个目录里时，这个上限只会让一批填不满，所以只在库里确实
	// 存在别的目录时才启用。
	enforceSameDirectoryCap := sameDirectoryExpands && hasOtherDirectories(pool, lastParentID)

	picked := make([]string, 0, in.Count)
	taken := make(map[string]struct{}, in.Count)
	sameDirectoryCount := 0
	add := func(id string) bool {
		if len(picked) >= in.Count {
			return false
		}
		if _, ok := taken[id]; ok {
			return false
		}
		if enforceSameDirectoryCap {
			if candidate, ok := byID[id]; ok && candidate.ParentID == lastParentID {
				if sameDirectoryCount >= sameDirectoryTarget {
					return false
				}
				sameDirectoryCount++
			}
		}
		taken[id] = struct{}{}
		picked = append(picked, id)
		return true
	}
	// addForced 只保证不重复、不超量，用于最后的兜底填充。
	addForced := func(id string) bool {
		if len(picked) >= in.Count {
			return false
		}
		if _, ok := taken[id]; ok {
			return false
		}
		taken[id] = struct{}{}
		picked = append(picked, id)
		return true
	}

	scored := newScorer(in, pool)
	ranking := scored.ranked(pool)
	shuffled := make([]Candidate, len(pool))
	copy(shuffled, pool)
	rnd.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	// 1. The directory the user just watched.
	if sameDirectoryExpands {
		profile := in.Directories[lastParentID]
		for _, id := range sameDirectoryPicks(*in.LastWatched, pool, profile, sameDirectoryTarget, rnd) {
			if sameDirectoryCount >= sameDirectoryTarget {
				break
			}
			add(id)
		}
	}

	// 2. Same tags, different directory: the sideways step that keeps the feed
	// from collapsing into one folder.
	tagTarget := quotaFor(in.Count, tagShare)
	added := 0
	for _, candidate := range ranking {
		if added >= tagTarget {
			break
		}
		if scored.tagWeight(candidate.candidate) <= 0 {
			continue
		}
		if in.LastWatched != nil && candidate.candidate.ParentID == in.LastWatched.ParentID {
			continue
		}
		if add(candidate.candidate.ID) {
			added++
		}
	}

	// 3. Exploration. This quota is what keeps the feed from narrowing into a
	// self-reinforcing loop, so it is never traded away for relevance.
	exploreTarget := quotaFor(in.Count, exploreShare)
	added = 0
	for _, candidate := range shuffled {
		if added >= exploreTarget {
			break
		}
		if add(candidate.ID) {
			added++
		}
	}

	// 4. Fill the rest by score.
	for _, candidate := range ranking {
		if len(picked) >= in.Count {
			break
		}
		add(candidate.candidate.ID)
	}

	// 5. Last resort: a pool barely larger than the batch can leave quotas
	// unfilled above.
	for _, candidate := range shuffled {
		if len(picked) >= in.Count {
			break
		}
		addForced(candidate.ID)
	}
	return picked
}

// hasOtherDirectories reports whether the pool holds anything outside parentID.
func hasOtherDirectories(pool []Candidate, parentID string) bool {
	for _, candidate := range pool {
		if candidate.ParentID != parentID {
			return true
		}
	}
	return false
}

// sameDirectoryPicks returns sibling ids from the directory of last.
func sameDirectoryPicks(
	last Candidate,
	pool []Candidate,
	profile DirectoryProfile,
	limit int,
	rnd *rand.Rand,
) []string {
	if limit <= 0 {
		return nil
	}
	siblings := make([]Candidate, 0, 16)
	for _, candidate := range pool {
		if candidate.ID == last.ID || candidate.ParentID != last.ParentID {
			continue
		}
		siblings = append(siblings, candidate)
	}
	if len(siblings) == 0 {
		return nil
	}

	out := make([]string, 0, limit)
	if !profile.Series {
		rnd.Shuffle(len(siblings), func(i, j int) { siblings[i], siblings[j] = siblings[j], siblings[i] })
		for _, candidate := range siblings {
			out = append(out, candidate.ID)
			if len(out) >= limit {
				return out
			}
		}
		return out
	}

	// Series: continue with the neighbouring episode instead of a random
	// sibling, so watching episode 3 leads to 4 and not to 17.
	sort.SliceStable(siblings, func(i, j int) bool {
		if compared := natural.Compare(orderKey(siblings[i]), orderKey(siblings[j])); compared != 0 {
			return compared < 0
		}
		return siblings[i].ID < siblings[j].ID
	})
	lastKey := orderKey(last)
	for _, candidate := range siblings {
		if natural.Compare(lastKey, orderKey(candidate)) < 0 {
			out = append(out, candidate.ID)
			if len(out) >= limit {
				return out
			}
		}
	}
	// Past the end of the run: wrap to the beginning rather than dropping back
	// to a random sibling.
	for _, candidate := range siblings {
		if len(out) >= limit {
			return out
		}
		if natural.Compare(lastKey, orderKey(candidate)) >= 0 {
			out = append(out, candidate.ID)
		}
	}
	return out
}

func orderKey(candidate Candidate) string {
	if trimmed := strings.TrimSpace(candidate.FileName); trimmed != "" {
		return trimmed
	}
	return candidate.ID
}

type scoredCandidate struct {
	candidate Candidate
	score     float64
}

type scorer struct {
	now      time.Time
	maxLikes int
	dirs     map[string]float64
	tags     map[string]float64
}

func newScorer(in Input, pool []Candidate) scorer {
	maxLikes := 0
	for _, candidate := range pool {
		if candidate.Likes > maxLikes {
			maxLikes = candidate.Likes
		}
	}
	return scorer{
		now:      in.Now,
		maxLikes: maxLikes,
		dirs:     in.DirAffinity,
		tags:     in.TagAffinity,
	}
}

// tagWeight sums the user's interest in the candidate's tags without squashing,
// so callers can ask "does this candidate match at all?".
func (s scorer) tagWeight(candidate Candidate) float64 {
	sum := 0.0
	for _, label := range candidate.Tags {
		sum += s.tags[strings.ToLower(strings.TrimSpace(label))]
	}
	return sum
}

func (s scorer) score(candidate Candidate) float64 {
	tag := s.tagWeight(candidate)
	if tag > 0 {
		tag = 1 - math.Exp(-tag)
	}
	dir := clamp01(s.dirs[candidate.ParentID])
	freshness := 1.0
	if candidate.PublishedAt > 0 {
		ageDays := s.now.Sub(time.UnixMilli(candidate.PublishedAt)).Hours() / 24
		if ageDays < 0 {
			ageDays = 0
		}
		freshness = math.Exp(-ageDays / freshnessDecayDays)
	}
	quality := 0.0
	if s.maxLikes > 0 {
		quality = (float64(candidate.Likes) + 0.5) / (float64(s.maxLikes) + 1)
	}
	return weightTagAffinity*tag +
		weightDirAffinity*dir +
		weightFreshness*freshness +
		weightQuality*quality
}

func (s scorer) ranked(pool []Candidate) []scoredCandidate {
	ranked := make([]scoredCandidate, 0, len(pool))
	for _, candidate := range pool {
		ranked = append(ranked, scoredCandidate{candidate: candidate, score: s.score(candidate)})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].candidate.ID < ranked[j].candidate.ID
	})
	return ranked
}

func quotaFor(count int, share float64) int {
	if count <= 0 {
		return 0
	}
	quota := int(math.Round(float64(count) * share))
	if quota < 1 {
		quota = 1
	}
	if quota > count {
		quota = count
	}
	return quota
}

func clamp01(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}
