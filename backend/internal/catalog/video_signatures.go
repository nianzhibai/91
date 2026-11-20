package catalog

import (
	"context"
	"fmt"
	"strings"

	"github.com/nianzhibai/91/backend/internal/mediasig"
)

// currentSignatureWhere matches rows whose stored signature uses the current
// encoding. Signatures of another format are treated as missing so that they
// are recomputed rather than compared.
var currentSignatureWhere = fmt.Sprintf(`hex(substr(content_signature, 1, 1)) = '%02X'`, mediasig.SignatureFormat)

// SetVideoSignatureForSource stores the perceptual signature sampled from
// source. Like other generated assets, the write is rejected when the row no
// longer refers to the bytes that were sampled.
func (c *Catalog) SetVideoSignatureForSource(ctx context.Context, source *Video, signature *mediasig.Signature) error {
	if source == nil || signature == nil {
		return fmt.Errorf("catalog: set video signature: missing source or signature")
	}
	data, err := signature.MarshalBinary()
	if err != nil {
		return err
	}
	return c.updateVideoRow(ctx, source.ID, source, "content_signature = ?", data)
}

// LoadVideoSignatures returns the current signatures of the given videos.
// Videos without one are absent from the result.
func (c *Catalog) LoadVideoSignatures(ctx context.Context, ids []string) (map[string]*mediasig.Signature, error) {
	out := make(map[string]*mediasig.Signature, len(ids))
	const chunkSize = 500
	for start := 0; start < len(ids); start += chunkSize {
		chunk := ids[start:min(start+chunkSize, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := c.db.QueryContext(ctx,
			`SELECT id, content_signature FROM videos
			  WHERE id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+`)
			    AND `+currentSignatureWhere, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var data []byte
			if err := rows.Scan(&id, &data); err != nil {
				rows.Close()
				return nil, err
			}
			signature := &mediasig.Signature{}
			if err := signature.UnmarshalBinary(data); err != nil {
				rows.Close()
				return nil, fmt.Errorf("catalog: decode signature of %s: %w", id, err)
			}
			out[id] = signature
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ListVideosMissingSignature returns duplicate-eligible videos at least
// minDurationSeconds long that have a ready local teaser but no current
// signature, so a signature can be derived from the teaser.
func (c *Catalog) ListVideosMissingSignature(ctx context.Context, minDurationSeconds int) ([]*Video, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		  WHERE `+duplicateLookupScope+`
		    AND `+contentVerifiedWhere+`
		    AND preview_status = 'ready' AND COALESCE(preview_local, '') != ''
		    AND duration_seconds >= ?
		    AND (content_signature IS NULL OR NOT `+currentSignatureWhere+`)
		  ORDER BY created_at ASC, id ASC`, minDurationSeconds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
