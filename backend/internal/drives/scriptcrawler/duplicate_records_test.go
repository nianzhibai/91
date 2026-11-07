package scriptcrawler

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/nianzhibai/91/backend/internal/dedupe"
)

// Verify actual crawler ingress persists the decision, including its pair
// orientation when the newly downloaded item replaces the previous winner.
func assertCrawlerDuplicateRecord(t *testing.T, path, sourceID, canonicalID, reason, outcome string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM duplicate_records WHERE video_id=?`, sourceID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("crawler duplicate records=%d err=%v, want 1", count, err)
	}
	var gotReason, gotOutcome, gotCanonical, encoded string
	if err := db.QueryRow(`SELECT reason,outcome,canonical_video_id,evidence FROM duplicate_records WHERE video_id=?`, sourceID).Scan(&gotReason, &gotOutcome, &gotCanonical, &encoded); err != nil {
		t.Fatal(err)
	}
	if gotReason != reason || gotOutcome != outcome || gotCanonical != canonicalID {
		t.Fatalf("crawler decision=%s/%s/%s, want %s/%s/%s", gotReason, gotOutcome, gotCanonical, reason, outcome, canonicalID)
	}
	var evidence dedupe.Evidence
	if err := json.Unmarshal([]byte(encoded), &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.MatchedVideoID != canonicalID {
		t.Fatalf("matched video=%s, want %s", evidence.MatchedVideoID, canonicalID)
	}
	if reason == dedupe.ReasonTitleThumb && (evidence.Match == nil || evidence.Match.TitleScore < 0.9 || evidence.Match.Score < 0.95) {
		t.Fatalf("crawler dropped thumbnail/title evidence: %+v", evidence)
	}
}
