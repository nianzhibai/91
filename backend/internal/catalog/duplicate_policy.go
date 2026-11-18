package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/nianzhibai/91/backend/internal/dedupe"
)

// duplicateLookupScope selects rows that may act as duplicate partners at
// ingress and in maintenance. Hidden rows are retired placeholders (the hide
// action now blacklists instead) and rows without a file cannot be played.
const duplicateLookupScope = `COALESCE(hidden, 0) = 0 AND COALESCE(file_id, '') != ''`

// contentVerifiedWhere excludes rows whose source may have been replaced in
// place. Their thumbnails and teasers can still describe the previous bytes
// until the fingerprint worker settles the verification, so perceptual
// matching must not use them.
const contentVerifiedWhere = `COALESCE(previous_sampled_sha256, '') = ''`

func normalizeContentHash(hash string) string {
	return dedupe.NormalizeHash(hash)
}

// equivalentVideoLookupWhere is the SQL form of dedupe.ExactKeys: a provider
// content hash, or a size-qualified sampled fingerprint.
func equivalentVideoLookupWhere(source *Video) (string, []any, bool) {
	if source == nil {
		return "", nil, false
	}
	var parts []string
	var args []any
	for _, key := range dedupe.ExactKeys(exactCandidate(source)) {
		switch key.Reason {
		case dedupe.ReasonContentHash:
			parts = append(parts, "(COALESCE(content_hash, '') != '' AND content_hash = ?)")
			args = append(args, key.Value)
		case dedupe.ReasonSampledSHA256:
			parts = append(parts, "(size_bytes = ? AND COALESCE(sampled_sha256, '') != '' AND sampled_sha256 = ?)")
			args = append(args, source.Size, normalizeContentHash(source.SampledSHA256))
		}
	}
	if len(parts) == 0 {
		return "", nil, false
	}
	return strings.Join(parts, " OR "), args, true
}

// exactDuplicateReason returns the first exact key shared by two videos, or
// an empty string when they share none.
func exactDuplicateReason(left, right *Video) string {
	rightKeys := dedupe.ExactKeys(exactCandidate(right))
	for _, key := range dedupe.ExactKeys(exactCandidate(left)) {
		for _, other := range rightKeys {
			if key == other {
				return key.Reason
			}
		}
	}
	return ""
}

func exactCandidate(v *Video) dedupe.Candidate {
	if v == nil {
		return dedupe.Candidate{}
	}
	return dedupe.Candidate{ContentHash: v.ContentHash, SampledSHA256: v.SampledSHA256, Size: v.Size}
}

// DuplicateRevision identifies the row state a duplicate decision depends on:
// source identity, matching signals and the generated assets that were
// compared. Asset versions are included because regeneration keeps the same
// path. Counters, reactions and other timestamps are excluded because a merge
// re-reads them inside its transaction, so engagement never invalidates a plan.
func DuplicateRevision(v *Video) string {
	if v == nil {
		return ""
	}
	h := sha256.New()
	for _, field := range []string{
		v.DriveID, v.FileID, v.SourceModTime, normalizeContentHash(v.ContentHash), strconv.FormatInt(v.Size, 10),
		normalizeContentHash(v.SampledSHA256), v.FingerprintStatus,
		v.Title, strconv.Itoa(v.DurationSeconds),
		v.ThumbnailURL, strconv.FormatInt(unixMilliOrZero(v.ThumbnailUpdatedAt), 10),
		v.PreviewStatus, v.PreviewLocal, strconv.FormatInt(unixMilliOrZero(v.PreviewUpdatedAt), 10),
		strconv.FormatBool(v.Hidden), strconv.FormatInt(v.CreatedAt.UnixMilli(), 10),
	} {
		h.Write([]byte(field))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
