package scriptcrawler

import (
	"context"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/mediasig"
	"github.com/nianzhibai/91/backend/internal/preview"
)

// nearDuplicateCandidateLimit bounds the library videos compared with one
// download. Thresholds are shared with maintenance through dedupe and mediasig.
const nearDuplicateCandidateLimit = 200

type nearDuplicateMatch struct {
	video *catalog.Video
	match dedupe.Match
}

func (m *nearDuplicateMatch) evidence(matchedID, selectedID, selectionReason string) dedupe.Evidence {
	evidence := dedupe.NewEvidence(m.match.Reason, matchedID, selectedID, selectionReason)
	evidence.Match = &m.match
	return evidence
}

func (c *Importer) recordSkippedDuplicate(ctx context.Context, source, canonical *catalog.Video, sourceID, discoveryKey string, evidence dedupe.Evidence) error {
	return c.cfg.Catalog.RecordCrawlerDuplicate(ctx, source, canonical, catalog.CrawlerSourceSeen{
		Kind: Kind, DriveID: c.cfg.Driver.ID(), SourceID: sourceID, DiscoveryKey: discoveryKey,
		Status: "duplicate", SampledSHA256: source.SampledSHA256, Size: source.Size,
	}, evidence)
}

// findNearDuplicateVideo compares the downloaded file with library videos of
// nearly the same duration by perceptual signature. It catches re-encodes,
// letterboxed and watermarked copies whose bytes, titles and covers differ.
// Like media jobs, it runs whatever the preview switch says.
func (c *Importer) findNearDuplicateVideo(ctx context.Context, source *catalog.Video, sourceVideoPath string) (*nearDuplicateMatch, error) {
	if c == nil || c.cfg.Catalog == nil || source == nil {
		return nil, nil
	}
	left := dedupe.Candidate{ID: source.ID, DurationSeconds: source.DurationSeconds}
	sourceVideoPath = strings.TrimSpace(sourceVideoPath)
	if sourceVideoPath == "" || !dedupe.SignatureEligible(left) {
		return nil, nil
	}
	candidates, err := c.cfg.Catalog.ListSignedVideosNearDuration(ctx, source, dedupe.DurationToleranceSeconds, nearDuplicateCandidateLimit)
	if err != nil || len(candidates) == 0 {
		return nil, err
	}
	return c.findSignatureDuplicate(ctx, left, sourceVideoPath, candidates)
}

// findSignatureDuplicate samples the downloaded file at the teaser windows
// and compares it with the stored signatures of the candidates. Only strong
// matches count; probable pairs are imported and left to maintenance review.
func (c *Importer) findSignatureDuplicate(ctx context.Context, left dedupe.Candidate, sourceVideoPath string, candidates []*catalog.Video) (*nearDuplicateMatch, error) {
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.ID)
	}
	signatures, err := c.cfg.Catalog.LoadVideoSignatures(ctx, ids)
	if err != nil || len(signatures) == 0 {
		return nil, err
	}

	windows := preview.TeaserSourceWindows(float64(left.DurationSeconds))
	clips := make([]mediasig.Clip, 0, len(windows))
	for _, window := range windows {
		clips = append(clips, mediasig.Clip{Path: sourceVideoPath, Offset: window.Start, Window: window})
	}
	left.Signature, err = mediasig.Extract(ctx, c.cfg.FFmpegPath, time.Duration(left.DurationSeconds)*time.Second, clips)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		applog.Warn(ctx, "signature extraction failed", err, "component", "scriptcrawler", "driveId", c.cfg.Driver.ID(), "videoId", left.ID, "stage", "dedupe")
		return nil, nil
	}
	for _, candidate := range candidates {
		signature := signatures[candidate.ID]
		if signature == nil {
			continue
		}
		right := dedupe.Candidate{ID: candidate.ID, DurationSeconds: candidate.DurationSeconds, Signature: signature}
		if match, verdict := dedupe.MatchSignature(left, right); verdict == mediasig.VerdictStrong {
			return &nearDuplicateMatch{video: candidate, match: match}, nil
		}
	}
	return nil, nil
}
