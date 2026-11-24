package catalog

import (
	"cmp"
	"context"
	"path"
	"slices"
	"strings"
	"time"
)

// Sort keys for ListParams.Sort. An empty key means SortLatest.
//
// SortRelevance is the keyword-search order. Exact title or file name matches
// come first, followed by other title, file name or author matches, with each
// tier ordered by popularity. Videos found through their directory follow:
// those in a directory named exactly like a term, or matching a term only
// their directory path contains. They are grouped by directory so collections
// stay intact: directories are ordered by directory-name match strength (exact
// segment before partial, closer to the video's parent folder first), then by
// group popularity, keeping matched subdirectories right after their matched
// parent, and each directory uses the same natural order as its collection.
// Without a keyword there is nothing to rank, so it falls back to SortHot.
const (
	SortRelevance = "relevance"
	SortLatest    = "latest"
	SortHot       = "hot"
	SortRecent    = "recent"
)

type searchRank int

const (
	searchRankExactName searchRank = iota
	searchRankPartialName
	searchRankPath
)

type directoryMatchQuality int

const (
	dirMatchNone directoryMatchQuality = iota
	dirMatchPartial
	dirMatchExactSegment
)

type searchCandidate struct {
	id           string
	title        string
	author       string
	fileName     string
	driveID      string
	parentID     string
	dirPath      string
	likes        int64
	lastLikedAt  int64
	publishedAt  int64
	createdAt    time.Time
	rank         searchRank
	dirSegments  [][]rune
	natural      naturalVideoKey
	dirGroupKey  searchDirKey
	dirQuality   directoryMatchQuality
	dirMatchDist int // 0 = direct parent folder, 1 = grandparent, ...
}

type searchDirKey struct {
	driveID  string
	parentID string
	dirPath  string
}

type searchDirGroup struct {
	key             searchDirKey
	segments        [][]rune
	quality         directoryMatchQuality
	matchDist       int
	bestLikes       int64
	bestLastLikedAt int64
	bestPublishedAt int64
	rows            []searchCandidate
}

type searchDirNode struct {
	group    searchDirGroup
	children []*searchDirNode
}

// searchOrderedVideoIDs returns every video matching the query in SortRelevance
// order. Classification and directory grouping run in Go over the SQL-filtered
// candidates so neither natural ordering nor match analysis has to be encoded
// in SQL.
func (c *Catalog) searchOrderedVideoIDs(ctx context.Context, query videoListQuery) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT videos.id, videos.title, COALESCE(videos.author, ''), COALESCE(videos.file_name, ''), videos.drive_id, COALESCE(videos.parent_id, ''),
       videos.dir_path, COALESCE(videos.likes, 0), COALESCE(videos.last_liked_at, 0), videos.published_at, videos.created_at
  FROM videos`+query.whereSQL, query.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []searchCandidate
	for rows.Next() {
		var row searchCandidate
		var createdAt int64
		if err := rows.Scan(&row.id, &row.title, &row.author, &row.fileName, &row.driveID, &row.parentID,
			&row.dirPath, &row.likes, &row.lastLikedAt, &row.publishedAt, &createdAt); err != nil {
			return nil, err
		}
		row.createdAt = time.UnixMilli(createdAt)
		candidates = append(candidates, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return rankSearchCandidates(candidates, query.searchKeyword, query.searchTerms), nil
}

func rankSearchCandidates(candidates []searchCandidate, rawKeyword string, terms []string) []string {
	lowerTerms := make([]string, len(terms))
	for i, term := range terms {
		lowerTerms[i] = strings.ToLower(term)
	}

	var exactRows, partialRows, pathRows []searchCandidate
	for i := range candidates {
		row := candidates[i]
		classifySearchCandidate(&row, rawKeyword, lowerTerms)
		switch row.rank {
		case searchRankExactName:
			exactRows = append(exactRows, row)
		case searchRankPartialName:
			partialRows = append(partialRows, row)
		default:
			pathRows = append(pathRows, row)
		}
	}

	slices.SortFunc(exactRows, compareHotCandidate)
	slices.SortFunc(partialRows, compareHotCandidate)
	orderedPathRows := orderSearchPathCandidates(pathRows)

	ids := make([]string, 0, len(candidates))
	for _, row := range exactRows {
		ids = append(ids, row.id)
	}
	for _, row := range partialRows {
		ids = append(ids, row.id)
	}
	for _, row := range orderedPathRows {
		ids = append(ids, row.id)
	}
	return ids
}

func classifySearchCandidate(row *searchCandidate, rawKeyword string, lowerTerms []string) {
	if exactNameMatch(row.title, row.fileName, rawKeyword) {
		row.rank = searchRankExactName
		return
	}
	// A directory that merely shares a substring with terms the video's own
	// fields already match is incidental; it does not pull the video out of
	// the name tier.
	quality, dist, matched := matchDirectoryPath(row.dirPath, row.fileName, lowerTerms)
	if matched && (quality == dirMatchExactSegment || !nameFieldsMatchAllTerms(row, lowerTerms)) {
		row.rank = searchRankPath
		row.dirSegments = naturalPathSegments(row.dirPath)
		row.natural = newNaturalVideoKey(row.fileName, row.title, row.id, row.createdAt)
		row.dirGroupKey = searchDirKey{driveID: row.driveID, parentID: row.parentID, dirPath: row.dirPath}
		row.dirQuality = quality
		row.dirMatchDist = dist
		return
	}
	row.rank = searchRankPartialName
}

// matchDirectoryPath reports whether the directory path participates in
// matching at least one query term, along with the strongest match quality
// across its path segments and how close that match is to the immediate parent
// folder (0 = direct parent).
func matchDirectoryPath(dirPath, fileName string, lowerTerms []string) (directoryMatchQuality, int, bool) {
	if dirPath == "" || len(lowerTerms) == 0 {
		return dirMatchNone, 0, false
	}
	rawSegments := strings.Split(dirPath, "/")
	lowerSegments := make([]string, len(rawSegments))
	for i, seg := range rawSegments {
		lowerSegments[i] = strings.ToLower(strings.TrimSpace(seg))
	}
	lowerDir := strings.ToLower(dirPath)
	lowerFile := strings.ToLower(fileName)
	lowerFull := lowerDir + "/" + lowerFile

	bestQuality := dirMatchNone
	bestDist := len(rawSegments)
	matched := false

	for _, term := range lowerTerms {
		// Check each directory segment from the direct parent outward so closer
		// matches win ties.
		segMatched := false
		for i := len(lowerSegments) - 1; i >= 0; i-- {
			seg := lowerSegments[i]
			if seg == "" {
				continue
			}
			dist := len(lowerSegments) - 1 - i
			var quality directoryMatchQuality
			switch {
			case seg == term:
				quality = dirMatchExactSegment
			case strings.Contains(seg, term):
				quality = dirMatchPartial
			default:
				continue
			}
			segMatched = true
			matched = true
			if quality > bestQuality || (quality == bestQuality && dist < bestDist) {
				bestQuality = quality
				bestDist = dist
			}
		}
		if segMatched {
			continue
		}
		// Cross-segment matches such as "旅行/2025" or "旅行/片段" also involve
		// the directory path; a term matched in fileName alone does not.
		if strings.Contains(lowerDir, term) || (strings.Contains(lowerFull, term) && !strings.Contains(lowerFile, term)) {
			matched = true
			if bestQuality < dirMatchPartial {
				bestQuality = dirMatchPartial
				bestDist = 0
			}
		}
	}

	return bestQuality, bestDist, matched
}

func orderSearchPathCandidates(rows []searchCandidate) []searchCandidate {
	if len(rows) == 0 {
		return nil
	}

	groupIndex := make(map[searchDirKey]int)
	var groups []searchDirGroup
	for _, row := range rows {
		idx, ok := groupIndex[row.dirGroupKey]
		if !ok {
			idx = len(groups)
			groupIndex[row.dirGroupKey] = idx
			groups = append(groups, searchDirGroup{
				key:             row.dirGroupKey,
				segments:        row.dirSegments,
				quality:         row.dirQuality,
				matchDist:       row.dirMatchDist,
				bestLikes:       row.likes,
				bestLastLikedAt: row.lastLikedAt,
				bestPublishedAt: row.publishedAt,
			})
		}
		g := &groups[idx]
		g.rows = append(g.rows, row)
		if row.dirQuality > g.quality || (row.dirQuality == g.quality && row.dirMatchDist < g.matchDist) {
			g.quality = row.dirQuality
			g.matchDist = row.dirMatchDist
		}
		if compareHotMetrics(row.likes, row.lastLikedAt, row.publishedAt, g.bestLikes, g.bestLastLikedAt, g.bestPublishedAt) < 0 {
			g.bestLikes = row.likes
			g.bestLastLikedAt = row.lastLikedAt
			g.bestPublishedAt = row.publishedAt
		}
	}

	for i := range groups {
		slices.SortFunc(groups[i].rows, func(a, b searchCandidate) int {
			return a.natural.compare(b.natural)
		})
	}

	// Build a shallow parent-child forest among matched directories on the same
	// drive so matched subdirectories stay attached directly beneath their
	// matched ancestor (e.g. "大三/期末" stays right after "大三"), while
	// unrelated directories are ranked by relevance and group popularity.
	nodes := make([]*searchDirNode, len(groups))
	byDriveAndPath := make(map[string][]*searchDirNode, len(groups))
	for i := range groups {
		nodes[i] = &searchDirNode{group: groups[i]}
		k := groups[i].key.driveID + "\x00" + groups[i].key.dirPath
		byDriveAndPath[k] = append(byDriveAndPath[k], nodes[i])
	}
	for _, list := range byDriveAndPath {
		if len(list) > 1 {
			slices.SortFunc(list, compareDirNodes)
		}
	}

	var roots []*searchDirNode
	for _, node := range nodes {
		parent := findMatchedAncestorNode(node, byDriveAndPath)
		if parent != nil {
			parent.children = append(parent.children, node)
		} else {
			roots = append(roots, node)
		}
	}

	slices.SortFunc(roots, compareDirNodes)
	out := make([]searchCandidate, 0, len(rows))
	var appendNode func(node *searchDirNode)
	appendNode = func(node *searchDirNode) {
		out = append(out, node.group.rows...)
		if len(node.children) > 1 {
			slices.SortFunc(node.children, compareSubdirNodes)
		}
		for _, child := range node.children {
			appendNode(child)
		}
	}
	for _, root := range roots {
		appendNode(root)
	}
	return out
}

func findMatchedAncestorNode(node *searchDirNode, byDriveAndPath map[string][]*searchDirNode) *searchDirNode {
	dirPath := node.group.key.dirPath
	for {
		slash := strings.LastIndexByte(dirPath, '/')
		if slash <= 0 {
			return nil
		}
		dirPath = dirPath[:slash]
		if parents := byDriveAndPath[node.group.key.driveID+"\x00"+dirPath]; len(parents) > 0 {
			return parents[0]
		}
	}
}

func compareDirNodes(a, b *searchDirNode) int {
	ga, gb := a.group, b.group
	if ga.quality != gb.quality {
		return cmp.Compare(gb.quality, ga.quality)
	}
	if ga.matchDist != gb.matchDist {
		return cmp.Compare(ga.matchDist, gb.matchDist)
	}
	if c := compareHotMetrics(ga.bestLikes, ga.bestLastLikedAt, ga.bestPublishedAt,
		gb.bestLikes, gb.bestLastLikedAt, gb.bestPublishedAt); c != 0 {
		return c
	}
	return compareDirIdentity(ga, gb)
}

func compareSubdirNodes(a, b *searchDirNode) int {
	return compareDirIdentity(a.group, b.group)
}

func compareDirIdentity(a, b searchDirGroup) int {
	if c := compareNaturalPath(a.segments, b.segments); c != 0 {
		return c
	}
	if c := strings.Compare(a.key.driveID, b.key.driveID); c != 0 {
		return c
	}
	return strings.Compare(a.key.parentID, b.key.parentID)
}

func nameFieldsMatchAllTerms(row *searchCandidate, lowerTerms []string) bool {
	fields := []string{strings.ToLower(row.title), strings.ToLower(row.author), strings.ToLower(row.fileName)}
	for _, term := range lowerTerms {
		if !slices.ContainsFunc(fields, func(field string) bool { return strings.Contains(field, term) }) {
			return false
		}
	}
	return true
}

func exactNameMatch(title, fileName, keyword string) bool {
	trimmedKeyword := strings.TrimSpace(keyword)
	if trimmedKeyword == "" {
		return false
	}
	trimmedFileName := strings.TrimSpace(fileName)
	stem := strings.TrimSpace(strings.TrimSuffix(trimmedFileName, path.Ext(trimmedFileName)))
	return strings.EqualFold(strings.TrimSpace(title), trimmedKeyword) ||
		strings.EqualFold(trimmedFileName, trimmedKeyword) ||
		strings.EqualFold(stem, trimmedKeyword)
}

func naturalPathSegments(dirPath string) [][]rune {
	if dirPath == "" {
		return nil
	}
	parts := strings.Split(dirPath, "/")
	segments := make([][]rune, len(parts))
	for i, part := range parts {
		segments[i] = naturalRunes(part)
	}
	return segments
}

func compareNaturalPath(left, right [][]rune) int {
	for i := 0; i < len(left) && i < len(right); i++ {
		if c := compareNaturalRunes(left[i], right[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(left), len(right))
}

func compareHotCandidate(a, b searchCandidate) int {
	if c := compareHotMetrics(a.likes, a.lastLikedAt, a.publishedAt, b.likes, b.lastLikedAt, b.publishedAt); c != 0 {
		return c
	}
	return strings.Compare(a.id, b.id)
}

// compareHotMetrics orders by SortHot's keys, most popular first.
func compareHotMetrics(likesA, lastLikedA, publishedA, likesB, lastLikedB, publishedB int64) int {
	return cmp.Or(
		cmp.Compare(likesB, likesA),
		cmp.Compare(lastLikedB, lastLikedA),
		cmp.Compare(publishedB, publishedA),
	)
}
