package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/nianzhibai/91/backend/internal/dedupe"
)

// duplicateLookupScope selects rows that may act as duplicate partners at
// ingress and in maintenance. A hidden row is either a legacy hide awaiting
// migration to the blacklist or a source whose provider reports no usable
// size, and rows without a file cannot be played.
const duplicateLookupScope = `COALESCE(hidden, 0) = 0 AND COALESCE(file_id, '') != ''`

// contentVerifiedWhere excludes rows whose source may have been replaced in
// place. Their thumbnails and teasers can still describe the previous bytes
// until the fingerprint step settles the verification, so perceptual
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

// weakDuplicate reports whether a file name and size match may stand for the
// same content. The match is provisional: exact signals carried by both rows
// must agree. Sampled fingerprints compare across drives; provider hashes only
// within one drive, because drives use different hash algorithms.
func weakDuplicate(left, right *Video) bool {
	if left.FileName == "" || left.FileName != right.FileName || left.Size <= 0 || left.Size != right.Size {
		return false
	}
	ls, rs := normalizeContentHash(left.SampledSHA256), normalizeContentHash(right.SampledSHA256)
	if ls != "" && rs != "" && ls != rs {
		return false
	}
	lh, rh := normalizeContentHash(left.ContentHash), normalizeContentHash(right.ContentHash)
	return left.DriveID != right.DriveID || lh == "" || rh == "" || lh == rh
}

// weakDuplicateCompatibleSQL is the signal check of weakDuplicate for two
// aliased video rows whose file name and size are already known to match.
func weakDuplicateCompatibleSQL(left, right string) string {
	return `(COALESCE(` + left + `.sampled_sha256, '') = '' OR COALESCE(` + right + `.sampled_sha256, '') = ''
			OR ` + left + `.sampled_sha256 = ` + right + `.sampled_sha256)
		AND (` + left + `.drive_id != ` + right + `.drive_id OR COALESCE(` + left + `.content_hash, '') = ''
			OR COALESCE(` + right + `.content_hash, '') = '' OR ` + left + `.content_hash = ` + right + `.content_hash)`
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
