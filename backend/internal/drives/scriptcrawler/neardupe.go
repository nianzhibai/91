package scriptcrawler

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
	"github.com/nianzhibai/91/backend/internal/mediasig"
	"github.com/nianzhibai/91/backend/internal/mediasim"
	"github.com/nianzhibai/91/backend/internal/preview"
)

// 阈值统一定义在 mediasim（NearDuplicate*）和 mediasig，与夜间维护共用。
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

// findNearDuplicateVideo 在导入前查找库内近重复视频，两条通道：
//  1. 标题相似 + 封面 SSIM（同源重发场景）；
//  2. 感知签名：时长几乎相等时，用本地新视频在预览选段处的签名比对库内
//     已存签名（跨源不同压制场景，标题和封面通常完全对不上）。与媒体任务
//     一样不受预览开关影响。
func (c *Importer) findNearDuplicateVideo(ctx context.Context, source *catalog.Video, sourceThumbPath, sourceVideoPath string) (*nearDuplicateMatch, error) {
	if c == nil || c.cfg.Catalog == nil || source == nil {
		return nil, nil
	}
	if strings.TrimSpace(source.Title) == "" || source.DurationSeconds <= 0 {
		return nil, nil
	}

	candidates, err := c.cfg.Catalog.ListNearDuplicateVideoCandidates(ctx, source, mediasim.NearDuplicateDurationToleranceSeconds, nearDuplicateCandidateLimit)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	if match := c.findTitleThumbDuplicate(source, sourceThumbPath, candidates); match != nil {
		return match, nil
	}
	return c.findSignatureDuplicate(ctx, source, sourceVideoPath, candidates)
}

func (c *Importer) findTitleThumbDuplicate(source *catalog.Video, sourceThumbPath string, candidates []*catalog.Video) *nearDuplicateMatch {
	sourceThumbPath = strings.TrimSpace(sourceThumbPath)
	commonThumbDir := strings.TrimSpace(c.cfg.CommonThumbDir)
	if sourceThumbPath == "" || commonThumbDir == "" {
		return nil
	}
	if _, err := os.Stat(sourceThumbPath); err != nil {
		return nil
	}
	left := dedupe.Candidate{ID: source.ID, Title: source.Title, DurationSeconds: source.DurationSeconds, ThumbnailPath: sourceThumbPath}
	for _, candidate := range candidates {
		if candidate == nil || candidate.ID == source.ID {
			continue
		}
		candidateThumbPath := mediaasset.ThumbnailPathInDir(commonThumbDir, candidate.ID)
		if _, err := os.Stat(candidateThumbPath); err != nil {
			continue
		}
		right := dedupe.Candidate{ID: candidate.ID, Title: candidate.Title, DurationSeconds: candidate.DurationSeconds, ThumbnailPath: candidateThumbPath}
		match, matched, err := dedupe.MatchNear(left, right, mediasim.ImageSSIM)
		if err != nil {
			log.Printf("[scriptcrawler] drive=%s source_id=%s candidate=%s thumbnail ssim failed: %v", c.cfg.Driver.ID(), source.ID, candidate.ID, err)
			continue
		}
		if matched {
			return &nearDuplicateMatch{video: candidate, match: match}
		}
	}
	return nil
}

// findSignatureDuplicate samples the downloaded file at the teaser windows
// and compares it with the stored signatures of library videos of nearly the
// same duration. It catches re-encodes whose titles and covers differ. Only
// strong matches count; probable pairs are imported and left to maintenance.
func (c *Importer) findSignatureDuplicate(ctx context.Context, source *catalog.Video, sourceVideoPath string, candidates []*catalog.Video) (*nearDuplicateMatch, error) {
	left := dedupe.Candidate{ID: source.ID, DurationSeconds: source.DurationSeconds}
	sourceVideoPath = strings.TrimSpace(sourceVideoPath)
	if sourceVideoPath == "" || !dedupe.SignatureEligible(left) {
		return nil, nil
	}
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate != nil && candidate.ID != source.ID {
			ids = append(ids, candidate.ID)
		}
	}
	signatures, err := c.cfg.Catalog.LoadVideoSignatures(ctx, ids)
	if err != nil || len(signatures) == 0 {
		return nil, err
	}

	windows := preview.TeaserSourceWindows(float64(source.DurationSeconds))
	clips := make([]mediasig.Clip, 0, len(windows))
	for _, window := range windows {
		clips = append(clips, mediasig.Clip{Path: sourceVideoPath, Offset: window.Start, Window: window})
	}
	left.Signature, err = mediasig.Extract(ctx, c.cfg.FFmpegPath, time.Duration(source.DurationSeconds)*time.Second, clips)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.Printf("[scriptcrawler] drive=%s source_id=%s signature failed: %v", c.cfg.Driver.ID(), source.ID, err)
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
