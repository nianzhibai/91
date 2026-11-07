package catalog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nianzhibai/91/backend/internal/dedupe"
)

const (
	DuplicateOriginScan        = "scan"
	DuplicateOriginCrawler     = "crawler"
	DuplicateOriginMaintenance = "maintenance"
	DuplicateOriginCommand     = "command"

	DuplicateOutcomeSkipped  = "skipped_import"
	DuplicateOutcomeExisting = "matched_existing"
	DuplicateOutcomeMerged   = "merged"
	DuplicateOutcomeReplaced = "replaced"
)

// duplicateSnapshot intentionally excludes counters, generated paths and
// observation timestamps. Those change independently of a duplicate decision
// and would make repeated scans produce a new record on every observation.
// ContentHash is the provider's untyped value; its algorithm is not inferred
// from the string length. SizeBytes is the reported size, not a verified size.
type duplicateSnapshot struct {
	ID               string   `json:"id"`
	DriveID          string   `json:"driveId"`
	FileID           string   `json:"fileId"`
	FileName         string   `json:"fileName"`
	ParentID         string   `json:"parentId,omitempty"`
	DirName          string   `json:"dirName,omitempty"`
	AncestorDirIDs   []string `json:"ancestorDirIds,omitempty"`
	AncestorDirNames []string `json:"ancestorDirNames,omitempty"`
	Title            string   `json:"title"`
	SizeBytes        int64    `json:"sizeBytes"`
	DurationSeconds  int      `json:"durationSeconds"`
	ContentHash      string   `json:"contentHash,omitempty"`
	SampledSHA256    string   `json:"sampledSha256,omitempty"`
}

func snapshotDuplicateVideo(v *Video) *duplicateSnapshot {
	if v == nil {
		return nil
	}
	return &duplicateSnapshot{
		ID: v.ID, DriveID: v.DriveID, FileID: v.FileID, FileName: v.FileName,
		ParentID: v.ParentID, DirName: v.DirName,
		AncestorDirIDs:   append([]string(nil), v.AncestorDirIDs...),
		AncestorDirNames: append([]string(nil), v.AncestorDirNames...),
		Title:            v.Title, SizeBytes: v.Size, DurationSeconds: v.DurationSeconds,
		ContentHash: normalizeContentHash(v.ContentHash), SampledSHA256: normalizeContentHash(v.SampledSHA256),
	}
}

// Only stable decision metadata participates in the record's content-derived key.
type duplicateRecord struct {
	Origin    string             `json:"origin"`
	Outcome   string             `json:"outcome"`
	Source    *duplicateSnapshot `json:"source"`
	Canonical *duplicateSnapshot `json:"canonical"`
	Matched   *duplicateSnapshot `json:"matched"`
	Selected  *duplicateSnapshot `json:"selected"`
	Evidence  dedupe.Evidence    `json:"evidence"`
}

// recordDuplicateDecision shares its caller's transaction. It never changes
// admission policy, tombstones or aliases, and it must only describe an outcome
// which will commit in that transaction. Unspecified matching evidence stays unknown.
func recordDuplicateDecision(ctx context.Context, exec videoRowExecer, origin, outcome string, source, canonical, matched, selected *Video, evidence dedupe.Evidence) error {
	if source == nil || canonical == nil || source.ID == "" || canonical.ID == "" || source.ID == canonical.ID {
		return errors.New("catalog: duplicate record requires distinct source and canonical videos")
	}
	if evidence.Reason == "" {
		evidence = dedupe.NewEvidence(dedupe.ReasonUnknown, "", "", "")
	}
	if evidence.Version <= 0 {
		return errors.New("catalog: duplicate evidence requires a version")
	}
	if (matched == nil && evidence.MatchedVideoID != "") || (matched != nil && matched.ID != evidence.MatchedVideoID) {
		return errors.New("catalog: duplicate evidence does not identify its matched snapshot")
	}
	if (selected == nil && evidence.SelectedVideoID != "") || (selected != nil && selected.ID != evidence.SelectedVideoID) {
		return errors.New("catalog: duplicate evidence does not identify its selected snapshot")
	}
	if match := evidence.Match; match != nil {
		if !((match.LeftID == source.ID && match.RightID == evidence.MatchedVideoID) ||
			(match.RightID == source.ID && match.LeftID == evidence.MatchedVideoID)) {
			return errors.New("catalog: duplicate score does not belong to the recorded pair")
		}
	}
	return storeDuplicateRecord(ctx, exec, duplicateRecord{
		Origin: origin, Outcome: outcome,
		Source: snapshotDuplicateVideo(source), Canonical: snapshotDuplicateVideo(canonical),
		Matched: snapshotDuplicateVideo(matched), Selected: snapshotDuplicateVideo(selected),
		Evidence: evidence,
	})
}

func storeDuplicateRecord(ctx context.Context, exec videoRowExecer, decision duplicateRecord) error {
	encoded, err := json.Marshal(decision)
	if err != nil {
		return fmt.Errorf("encode duplicate decision: %w", err)
	}
	key := sha256.Sum256(encoded)
	// The complete decision has already passed JSON encoding above.
	sourceJSON, _ := json.Marshal(decision.Source)
	canonicalJSON, _ := json.Marshal(decision.Canonical)
	matchedJSON, _ := json.Marshal(decision.Matched)
	selectedJSON, _ := json.Marshal(decision.Selected)
	evidenceJSON, _ := json.Marshal(decision.Evidence)
	source, evidence := decision.Source, decision.Evidence
	now := time.Now().UnixMilli()
	_, err = exec.ExecContext(ctx, `
INSERT INTO duplicate_records (
  record_key, origin, outcome, reason, video_id, drive_id, file_id, file_name, size_bytes,
  canonical_video_id, matched_video_id, selection_reason,
  source_snapshot, canonical_snapshot, matched_snapshot, selected_snapshot, evidence,
  first_seen_at, last_seen_at, occurrences
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
ON CONFLICT(record_key) DO UPDATE SET
  last_seen_at = MAX(duplicate_records.last_seen_at, excluded.last_seen_at),
  occurrences = duplicate_records.occurrences + 1`,
		hex.EncodeToString(key[:]), decision.Origin, decision.Outcome, evidence.Reason, source.ID, source.DriveID, source.FileID, source.FileName, source.SizeBytes,
		decision.Canonical.ID, evidence.MatchedVideoID, evidence.SelectionReason,
		string(sourceJSON), string(canonicalJSON), string(matchedJSON), string(selectedJSON), string(evidenceJSON), now, now)
	return err
}

func recordDuplicateMerge(ctx context.Context, tx *sql.Tx, origin, outcome string, source *Video, canonicalID string, evidence dedupe.Evidence) error {
	load := func(id string) (*Video, error) {
		if id == "" {
			return nil, nil
		}
		return scanVideo(tx.QueryRowContext(ctx, `SELECT `+allVideoCols+` FROM videos WHERE id = ?`, id))
	}
	canonical, err := load(canonicalID)
	if err != nil {
		return err
	}
	matched, err := load(evidence.MatchedVideoID)
	if err != nil {
		return err
	}
	selected, err := load(evidence.SelectedVideoID)
	if err != nil {
		return err
	}
	return recordDuplicateDecision(ctx, tx, origin, outcome, source, canonical, matched, selected, evidence)
}

// RecordCrawlerDuplicate atomically remembers the skipped source and its
// decision. A failed write leaves the source eligible for another crawl.
func (c *Catalog) RecordCrawlerDuplicate(ctx context.Context, source, canonical *Video, seen CrawlerSourceSeen, evidence dedupe.Evidence) error {
	if source == nil || canonical == nil || seen.Status != "duplicate" || seen.Kind == "" || seen.DriveID == "" || seen.SourceID == "" || seen.DriveID != source.DriveID {
		return errors.New("catalog: crawler duplicate requires source, canonical and seen identity")
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := markCrawlerSourceSeen(ctx, tx, seen.Kind, seen.DriveID, seen.SourceID, seen.Status, canonical.ID, seen.SampledSHA256, seen.Size); err != nil {
		return err
	}
	if err := bindCrawlerDiscovery(ctx, tx, seen.DriveID, seen.DiscoveryKey, seen.SourceID); err != nil {
		return err
	}

	// Preserve the snapshots actually compared by ingress. Re-reading metadata
	// here could associate the old score with a subsequently changed file.
	if err := recordDuplicateDecision(ctx, tx, DuplicateOriginCrawler, DuplicateOutcomeSkipped, source, canonical, canonical, canonical, evidence); err != nil {
		return err
	}
	return tx.Commit()
}
