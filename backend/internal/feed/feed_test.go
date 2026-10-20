package feed

import (
	"math/rand/v2"
	"testing"
	"time"
)

func candidate(id, parentID, fileName string, publishedAt time.Time) Candidate {
	return Candidate{
		ID:          id,
		ParentID:    parentID,
		FileName:    fileName,
		PublishedAt: publishedAt.UnixMilli(),
	}
}

func seededRand() *rand.Rand {
	return rand.New(rand.NewPCG(1, 2))
}

func TestSeriesStemIgnoresEpisodeNumber(t *testing.T) {
	for _, test := range []struct {
		name  string
		left  string
		right string
		same  bool
	}{
		{name: "underscore episode", left: "show_01.mp4", right: "show_02.mp4", same: true},
		{name: "chinese episode marker", left: "第3集.mp4", right: "第4集.mp4", same: true},
		{name: "season episode marker", left: "S01E03.mkv", right: "S01E04.mkv", same: true},
		{name: "different shows", left: "cats.mp4", right: "dogs.mp4", same: false},
		{name: "too short after stripping", left: "1.mp4", right: "2.mp4", same: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			left, right := seriesStem(test.left), seriesStem(test.right)
			if same := left == right && left != ""; same != test.same {
				t.Fatalf("seriesStem(%q)=%q, seriesStem(%q)=%q, want same=%v",
					test.left, left, test.right, right, test.same)
			}
		})
	}
}

func TestProfileDirectoriesSeparatesArchiveFromContent(t *testing.T) {
	now := time.Now()
	pool := []Candidate{
		candidate("a1", "content", "a1.mp4", now),
		candidate("a2", "content", "a2.mp4", now),
		candidate("a3", "content", "a3.mp4", now),
		candidate("b1", "archive", "b1.mp4", now),
		candidate("b2", "archive", "b2.mp4", now),
		candidate("c1", "untagged", "c1.mp4", now),
	}
	shares := map[string]TagShare{
		// 集中度高：这个目录装的是同一种内容。
		"content": {Tagged: 10, TopShare: 0.8},
		// 集中度低：这个目录只是同批次下载下来的杂项。
		"archive": {Tagged: 10, TopShare: 0.12},
	}

	profiles := ProfileDirectories(pool, shares)
	if !profiles["content"].Semantic {
		t.Fatal("a concentrated directory must be semantic")
	}
	if profiles["archive"].Semantic {
		t.Fatal("a high-entropy directory must not be semantic")
	}
	// 没有标签就没有证据；此时信任用户的目录结构，而不是假定它是垃圾堆。
	if !profiles["untagged"].Semantic {
		t.Fatal("an untagged directory must default to semantic")
	}
}

func TestProfileDirectoriesDetectsSeries(t *testing.T) {
	now := time.Now()
	series := []Candidate{
		candidate("s1", "series", "第1集.mp4", now),
		candidate("s2", "series", "第2集.mp4", now),
		candidate("s3", "series", "第3集.mp4", now),
		candidate("m1", "mixed", "cats.mp4", now),
		candidate("m2", "mixed", "dogs.mp4", now),
		candidate("m3", "mixed", "birds.mp4", now),
	}

	profiles := ProfileDirectories(series, nil)
	if !profiles["series"].Series {
		t.Fatal("three shared stems must be treated as a series")
	}
	if profiles["mixed"].Series {
		t.Fatal("unrelated filenames must not be treated as a series")
	}
}

func TestBuildBatchContinuesTheSeriesFromTheWatchedEpisode(t *testing.T) {
	now := time.Now()
	pool := []Candidate{
		candidate("ep04", "dir", "第4集.mp4", now),
		candidate("ep05", "dir", "第5集.mp4", now),
		candidate("ep06", "dir", "第6集.mp4", now),
		candidate("ep07", "dir", "第7集.mp4", now),
	}
	last := candidate("ep03", "dir", "第3集.mp4", now)
	// ep03 本身不在池子里（它已经被排除），但 last 仍然指向它。
	for index := 0; index < 20; index++ {
		pool = append(pool, candidate("other-"+string(rune('a'+index)), "other", "", now))
	}

	picked := BuildBatch(Input{
		Now:         now,
		Count:       5,
		Pool:        pool,
		Directories: map[string]DirectoryProfile{"dir": {Semantic: true, Series: true}},
		LastWatched: &last,
		Rand:        seededRand(),
	})
	if len(picked) != 5 {
		t.Fatalf("batch size = %d, want 5", len(picked))
	}
	if picked[0] != "ep04" || picked[1] != "ep05" {
		t.Fatalf("series continuation = %v, want ep04 then ep05", picked[:2])
	}
}

func TestBuildBatchDoesNotExpandAnArchiveDirectory(t *testing.T) {
	now := time.Now()
	pool := []Candidate{
		candidate("ep04", "dir", "第4集.mp4", now.Add(-10 * time.Hour)),
		candidate("ep05", "dir", "第5集.mp4", now.Add(-10 * time.Hour)),
		candidate("ep06", "dir", "第6集.mp4", now.Add(-10 * time.Hour)),
		candidate("ep07", "dir", "第7集.mp4", now.Add(-10 * time.Hour)),
	}
	last := candidate("ep03", "dir", "第3集.mp4", now)
	for index := 0; index < 20; index++ {
		pool = append(pool, candidate("other-"+string(rune('a'+index)), "other", "", now))
	}

	picked := BuildBatch(Input{
		Now:         now,
		Count:       5,
		Pool:        pool,
		Directories: map[string]DirectoryProfile{"dir": {Semantic: false, Series: true}},
		LastWatched: &last,
		Rand:        seededRand(),
	})
	if picked[0] == "ep04" && picked[1] == "ep05" {
		t.Fatalf("archive directory drove a series continuation: %v", picked)
	}
}

func TestBuildBatchCapsTheSameDirectoryShare(t *testing.T) {
	now := time.Now()
	pool := make([]Candidate, 0, 30)
	for index := 0; index < 10; index++ {
		pool = append(pool, candidate("dir-a-"+string(rune('a'+index)), "dir", "", now))
	}
	for index := 0; index < 20; index++ {
		pool = append(pool, candidate("dir-b-"+string(rune('a'+index)), "other", "", now))
	}
	last := candidate("watched", "dir", "", now)

	picked := BuildBatch(Input{
		Now:         now,
		Count:       5,
		Pool:        pool,
		Directories: map[string]DirectoryProfile{"dir": {Semantic: true}},
		LastWatched: &last,
		Rand:        seededRand(),
	})

	fromDirectory := 0
	for _, id := range picked {
		if len(id) >= 5 && id[:5] == "dir-a" {
			fromDirectory++
		}
	}
	// 一批 5 条最多买 2 条同目录：再多就把推荐流变成了目录连播。
	if fromDirectory > 2 {
		t.Fatalf("same-directory picks = %d, want at most 2 (batch %v)", fromDirectory, picked)
	}
	if fromDirectory == 0 {
		t.Fatalf("no same-directory pick at all: %v", picked)
	}
}

func TestBuildBatchExcludesServedAndWatchedVideos(t *testing.T) {
	now := time.Now()
	pool := []Candidate{
		candidate("keep-1", "dir", "", now),
		candidate("keep-2", "dir", "", now),
		candidate("drop-1", "dir", "", now),
		candidate("drop-2", "dir", "", now),
	}
	picked := BuildBatch(Input{
		Now:     now,
		Count:   5,
		Pool:    pool,
		Exclude: map[string]struct{}{"drop-1": {}, "drop-2": {}},
		Rand:    seededRand(),
	})
	if len(picked) != 2 {
		t.Fatalf("batch = %v, want only the two kept videos", picked)
	}
	for _, id := range picked {
		if id == "drop-1" || id == "drop-2" {
			t.Fatalf("excluded video %q leaked into the batch", id)
		}
	}
}

func TestBuildBatchReturnsNothingForAnEmptyPool(t *testing.T) {
	if picked := BuildBatch(Input{Now: time.Now(), Count: 5}); len(picked) != 0 {
		t.Fatalf("empty pool produced %v", picked)
	}
}

func TestBuildBatchNeverRepeatsWithinOneBatch(t *testing.T) {
	now := time.Now()
	pool := make([]Candidate, 0, 40)
	for index := 0; index < 40; index++ {
		parent := "dir"
		if index%2 == 0 {
			parent = "other"
		}
		pool = append(pool, candidate("video-"+string(rune('a'+index%26))+string(rune('a'+index/26)), parent, "", now))
	}
	picked := BuildBatch(Input{
		Now:         now,
		Count:       10,
		Pool:        pool,
		Directories: map[string]DirectoryProfile{"dir": {Semantic: true}},
		LastWatched: func() *Candidate { value := pool[0]; return &value }(),
		Rand:        seededRand(),
	})
	if len(picked) != 10 {
		t.Fatalf("batch size = %d, want 10", len(picked))
	}
	seen := map[string]struct{}{}
	for _, id := range picked {
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate %q in %v", id, picked)
		}
		seen[id] = struct{}{}
	}
}

func TestBuildBatchPrefersHigherTagAffinity(t *testing.T) {
	now := time.Now()
	pool := []Candidate{
		{ID: "matched", ParentID: "a", Tags: []string{"舞蹈"}, PublishedAt: now.UnixMilli()},
		{ID: "unmatched", ParentID: "b", PublishedAt: now.UnixMilli()},
	}
	// 池子只比一批多一条，配额会被逐级填满，最后按分数补齐。
	pool = append(pool, Candidate{ID: "filler", ParentID: "c", PublishedAt: now.UnixMilli()})
	picked := BuildBatch(Input{
		Now:         now,
		Count:       2,
		Pool:        pool,
		TagAffinity: map[string]float64{"舞蹈": 3},
		Rand:        seededRand(),
	})
	found := false
	for _, id := range picked {
		if id == "matched" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tag-matched candidate was not picked: %v", picked)
	}
}
