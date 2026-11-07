package scriptcrawler

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/localpath"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
	"github.com/nianzhibai/91/backend/internal/mediasim"
	"github.com/nianzhibai/91/backend/internal/preview"
)

// 阈值统一定义在 mediasim（NearDuplicate* / ContentDuplicate*），与夜间维护共用。
const nearDuplicateCandidateLimit = 200

type nearDuplicateMatch struct {
	video           *catalog.Video
	titleSimilarity float64
	thumbnailSSIM   float64
	contentSSIM     float64 // >0 时表示由内容通道命中
	match           dedupe.Match
}

func (m *nearDuplicateMatch) evidence(matchedID, selectedID, selectionReason string) dedupe.Evidence {
	evidence := dedupe.NewEvidence(m.match.Reason(), matchedID, selectedID, selectionReason)
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
//  2. 内容级：时长几乎相等时比较候选 teaser 与本地新视频的对齐帧
//     （跨源不同压制场景，标题和封面通常完全对不上）。
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
	return c.findContentDuplicate(ctx, source, sourceVideoPath, candidates)
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
	for _, candidate := range candidates {
		if candidate == nil || candidate.ID == source.ID {
			continue
		}
		titleScore := mediasim.TitleSimilarity(source.Title, candidate.Title)
		if titleScore < mediasim.NearDuplicateTitleThreshold {
			continue
		}
		candidateThumbPath := mediaasset.ThumbnailPathInDir(commonThumbDir, candidate.ID)
		if _, err := os.Stat(candidateThumbPath); err != nil {
			continue
		}
		ssimScore, err := mediasim.ImageSSIM(sourceThumbPath, candidateThumbPath)
		if err != nil {
			log.Printf("[scriptcrawler] drive=%s source_id=%s candidate=%s thumbnail ssim failed: %v", c.cfg.Driver.ID(), source.ID, candidate.ID, err)
			continue
		}
		if ssimScore >= mediasim.NearDuplicateThumbSSIMThreshold {
			return &nearDuplicateMatch{
				video:           candidate,
				titleSimilarity: titleScore,
				thumbnailSSIM:   ssimScore,
				match: dedupe.Match{Stage: dedupe.StageNear, LeftID: source.ID, RightID: candidate.ID,
					Score: ssimScore, TitleScore: titleScore},
			}
		}
	}
	return nil
}

func (c *Importer) findContentDuplicate(ctx context.Context, source *catalog.Video, sourceVideoPath string, candidates []*catalog.Video) (*nearDuplicateMatch, error) {
	sourceVideoPath = strings.TrimSpace(sourceVideoPath)
	if sourceVideoPath == "" || source.DurationSeconds < mediasim.ContentDuplicateMinDurationSeconds {
		return nil, nil
	}

	var sourceSig *mediasim.FrameSignature
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if candidate == nil || candidate.ID == source.ID {
			continue
		}
		if candidate.DurationSeconds < mediasim.ContentDuplicateMinDurationSeconds {
			continue
		}
		teaserPath, validPath := localpath.Managed(c.cfg.LocalPreviewDir, candidate.PreviewLocal)
		if strings.TrimSpace(candidate.PreviewStatus) != "ready" || !validPath {
			continue
		}
		if info, err := os.Stat(teaserPath); err != nil || !info.Mode().IsRegular() {
			continue
		}

		if sourceSig == nil {
			times := preview.TeaserFrameSourceTimes(float64(source.DurationSeconds), mediasim.FrameSignatureMaxFrames)
			sig, err := mediasim.ExtractFrameSignatureAtTimes(ctx, c.cfg.FFmpegPath, sourceVideoPath, times)
			if err != nil {
				log.Printf("[scriptcrawler] drive=%s source_id=%s content signature failed: %v", c.cfg.Driver.ID(), source.ID, err)
				return nil, nil
			}
			if sig.InformativeFrames() < mediasim.ContentDuplicateMinComparisons {
				return nil, nil
			}
			sourceSig = sig
		}

		candidateSig, err := c.loadCandidateTeaserSignature(ctx, candidate.ID, teaserPath)
		if err != nil {
			log.Printf("[scriptcrawler] drive=%s source_id=%s candidate=%s teaser signature failed: %v", c.cfg.Driver.ID(), source.ID, candidate.ID, err)
			continue
		}
		cmp := mediasim.CompareFrameSignatures(sourceSig, candidateSig)
		if cmp.IsContentDuplicate() {
			return &nearDuplicateMatch{
				video:       candidate,
				contentSSIM: cmp.MedianSSIM,
				match: dedupe.Match{Stage: dedupe.StageContent, LeftID: source.ID, RightID: candidate.ID,
					Score: cmp.MedianSSIM, Comparisons: cmp.Comparisons},
			}, nil
		}
		// 候选 teaser 含兜底段时对齐帧整段错位，时长精确相等时用交叉匹配兜底。
		if candidate.DurationSeconds == source.DurationSeconds {
			if cross := mediasim.CompareFrameSignaturesCross(sourceSig, candidateSig); cross.IsContentDuplicate() {
				log.Printf("[scriptcrawler] drive=%s source_id=%s content duplicate (cross) candidate=%s strong=%d/%d,%d/%d median_best=%.3f",
					c.cfg.Driver.ID(), source.ID, candidate.ID, cross.LeftStrong, cross.LeftFrames, cross.RightStrong, cross.RightFrames, cross.MedianBest)
				return &nearDuplicateMatch{
					video:       candidate,
					contentSSIM: cross.MedianBest,
					match: dedupe.Match{Stage: dedupe.StageContent, LeftID: source.ID, RightID: candidate.ID,
						Score: cross.MedianBest, Comparisons: min(cross.LeftFrames, cross.RightFrames),
						Cross: true, CrossDetails: &cross},
				}, nil
			}
		}
	}
	return nil, nil
}

func (c *Importer) loadCandidateTeaserSignature(ctx context.Context, videoID, teaserPath string) (*mediasim.FrameSignature, error) {
	localDir := strings.TrimSpace(c.cfg.LocalPreviewDir)
	cachePath := ""
	if localDir != "" {
		cachePath = mediaasset.FrameSignaturePath(localDir, videoID)
		if signature, cached := mediasim.LoadCachedTeaserSignature(cachePath, teaserPath); cached {
			return signature, nil
		}
	}

	signature, err := mediasim.ExtractTeaserFrameSignature(ctx, c.cfg.FFmpegPath, teaserPath)
	if err != nil {
		return nil, err
	}
	if cachePath != "" && signature != nil && signature.InformativeFrames() >= mediasim.ContentDuplicateMinComparisons {
		if err := mediasim.StoreCachedTeaserSignature(cachePath, teaserPath, signature); err != nil {
			log.Printf("[scriptcrawler] candidate=%s content signature cache write: %v", videoID, err)
		}
	}
	return signature, nil
}
