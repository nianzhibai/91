package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/dedupe"
)

const (
	duplicateReviewPending   = "pending"
	duplicateReviewDismissed = "dismissed"

	// DuplicateOriginReview marks merges a person confirmed from the review
	// list.
	DuplicateOriginReview = "review"
)

// ErrDuplicateReviewNotFound reports a pair that is not pending review.
var ErrDuplicateReviewNotFound = errors.New("catalog: duplicate review not found")

// DuplicateReview is a pending pair of visible videos that perceptual
// matching could not settle automatically.
type DuplicateReview struct {
	Left        *Video
	Right       *Video
	Evidence    dedupe.Match
	FirstSeenAt time.Time
}

type DuplicateReviewList struct {
	Items []*DuplicateReview
	Total int
	Page  int
	Size  int
}

func orderedPair(left, right string) (string, string) {
	if left > right {
		return right, left
	}
	return left, right
}

// ReplaceDuplicateReviews makes reviews the complete set of pending pairs.
// Pairs still pending keep their first-seen time, dismissed pairs stay
// dismissed, and dismissals of videos that no longer exist are dropped.
func (c *Catalog) ReplaceDuplicateReviews(ctx context.Context, reviews []dedupe.Review) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	firstSeen := make(map[[2]string]int64)
	rows, err := tx.QueryContext(ctx, `SELECT left_video_id, right_video_id, first_seen_at FROM duplicate_reviews WHERE status = ?`, duplicateReviewPending)
	if err != nil {
		return err
	}
	for rows.Next() {
		var key [2]string
		var seen int64
		if err := rows.Scan(&key[0], &key[1], &seen); err != nil {
			rows.Close()
			return err
		}
		firstSeen[key] = seen
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM duplicate_reviews WHERE status = ?`, duplicateReviewPending); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM duplicate_reviews
 WHERE left_video_id NOT IN (SELECT id FROM videos)
    OR right_video_id NOT IN (SELECT id FROM videos)`); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for _, review := range reviews {
		left, right := orderedPair(review.LeftID, review.RightID)
		if left == "" || left == right {
			return fmt.Errorf("catalog: invalid duplicate review %q/%q", review.LeftID, review.RightID)
		}
		evidence, err := json.Marshal(review.Match)
		if err != nil {
			return err
		}
		seen, ok := firstSeen[[2]string{left, right}]
		if !ok {
			seen = now
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO duplicate_reviews (left_video_id, right_video_id, status, evidence, first_seen_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(left_video_id, right_video_id) DO NOTHING`,
			left, right, duplicateReviewPending, string(evidence), seen, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DismissedDuplicatePairs returns whether a person judged two videos not to
// be duplicates. The result is a snapshot for one planner run.
func (c *Catalog) DismissedDuplicatePairs(ctx context.Context) (func(leftID, rightID string) bool, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT left_video_id, right_video_id FROM duplicate_reviews WHERE status = ?`, duplicateReviewDismissed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	dismissed := make(map[[2]string]struct{})
	for rows.Next() {
		var key [2]string
		if err := rows.Scan(&key[0], &key[1]); err != nil {
			return nil, err
		}
		dismissed[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return func(leftID, rightID string) bool {
		left, right := orderedPair(leftID, rightID)
		_, ok := dismissed[[2]string{left, right}]
		return ok
	}, nil
}

// pendingReviewWhere selects pending pairs whose videos are both visible.
const pendingReviewWhere = `status = 'pending'
   AND EXISTS (SELECT 1 FROM videos WHERE videos.id = duplicate_reviews.left_video_id AND ` + duplicateLookupScope + `)
   AND EXISTS (SELECT 1 FROM videos WHERE videos.id = duplicate_reviews.right_video_id AND ` + duplicateLookupScope + `)`

// ListDuplicateReviews pages through pending pairs, newest first.
func (c *Catalog) ListDuplicateReviews(ctx context.Context, page, size int) (*DuplicateReviewList, error) {
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 100
	}
	list := &DuplicateReviewList{Items: []*DuplicateReview{}, Page: page, Size: size}
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM duplicate_reviews WHERE `+pendingReviewWhere).Scan(&list.Total); err != nil {
		return nil, err
	}
	if list.Total == 0 || page-1 > (list.Total-1)/size {
		return list, nil
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT left_video_id, right_video_id, evidence, first_seen_at FROM duplicate_reviews
 WHERE `+pendingReviewWhere+`
 ORDER BY first_seen_at DESC, left_video_id, right_video_id
 LIMIT ? OFFSET ?`, size, (page-1)*size)
	if err != nil {
		return nil, err
	}
	type pendingRow struct {
		left, right, evidence string
		firstSeen             int64
	}
	var pending []pendingRow
	for rows.Next() {
		var row pendingRow
		if err := rows.Scan(&row.left, &row.right, &row.evidence, &row.firstSeen); err != nil {
			rows.Close()
			return nil, err
		}
		pending = append(pending, row)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, row := range pending {
		review := &DuplicateReview{FirstSeenAt: time.UnixMilli(row.firstSeen)}
		if err := json.Unmarshal([]byte(row.evidence), &review.Evidence); err != nil {
			return nil, fmt.Errorf("catalog: decode review evidence %s/%s: %w", row.left, row.right, err)
		}
		if review.Left, err = c.GetVideo(ctx, row.left); err != nil {
			return nil, err
		}
		if review.Right, err = c.GetVideo(ctx, row.right); err != nil {
			return nil, err
		}
		list.Items = append(list.Items, review)
	}
	return list, nil
}

// DismissDuplicateReview records that two videos are not duplicates. Later
// plans neither merge nor review the pair.
func (c *Catalog) DismissDuplicateReview(ctx context.Context, leftID, rightID string) error {
	left, right := orderedPair(strings.TrimSpace(leftID), strings.TrimSpace(rightID))
	result, err := c.db.ExecContext(ctx, `
UPDATE duplicate_reviews SET status = ?, updated_at = ?
 WHERE left_video_id = ? AND right_video_id = ? AND `+pendingReviewWhere,
		duplicateReviewDismissed, time.Now().UnixMilli(), left, right)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err == nil && count == 0 {
		return ErrDuplicateReviewNotFound
	}
	return err
}

// MergeDuplicateReview merges mergeID into keepID after a person confirmed a
// pending pair, through the same transaction as automatic merges. Generated
// assets of the merged video are left to the duplicate asset cleanup jobs.
func (c *Catalog) MergeDuplicateReview(ctx context.Context, keepID, mergeID string) error {
	keepID, mergeID = strings.TrimSpace(keepID), strings.TrimSpace(mergeID)
	left, right := orderedPair(keepID, mergeID)
	var encoded string
	err := c.db.QueryRowContext(ctx, `
SELECT evidence FROM duplicate_reviews
 WHERE left_video_id = ? AND right_video_id = ? AND `+pendingReviewWhere, left, right).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) || keepID == mergeID {
		return ErrDuplicateReviewNotFound
	}
	if err != nil {
		return err
	}
	var match dedupe.Match
	if err := json.Unmarshal([]byte(encoded), &match); err != nil {
		return fmt.Errorf("catalog: decode review evidence: %w", err)
	}
	keep, err := c.GetVideo(ctx, keepID)
	if err != nil {
		return err
	}
	merged, err := c.GetVideo(ctx, mergeID)
	if err != nil {
		return err
	}
	evidence := dedupe.NewEvidence(match.Reason, keepID, keepID, "manual_review")
	// The observed comparison may involve a video merged earlier; it is
	// evidence for this pair only when it compared exactly these two.
	if orderedLeft, orderedRight := orderedPair(match.LeftID, match.RightID); orderedLeft == left && orderedRight == right {
		evidence.Match = &match
	}
	if err := c.ApplyDuplicateVideoDeletions(ctx, []DuplicateVideoDeletion{{
		VideoID: mergeID, CanonicalVideoID: keepID,
		ExpectedRevision: DuplicateRevision(merged), CanonicalExpectedRevision: DuplicateRevision(keep),
		Origin: DuplicateOriginReview, Evidence: evidence,
	}}); err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `DELETE FROM duplicate_reviews WHERE left_video_id = ? OR right_video_id = ?`, mergeID, mergeID)
	return err
}
