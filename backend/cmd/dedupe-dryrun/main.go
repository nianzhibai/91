// dedupe-dryrun：预演/执行夜间维护的内容级查重通道（Phase 5 content channel）。
// 分组、传递闭包、canonical 选择和最终删除计划与生产共用 internal/dedupe。
//
// 默认只读，不写库、不删文件；加 -apply 后一次性提交完整计划，再幂等清理
// 本地生成资产。原始媒体源始终不在清理范围内。
//
// 用法：在 backend 目录下运行
//
//	go run ./cmd/dedupe-dryrun -db data/video-site.db -local-dir data/previews [-apply]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/dedupe"
	"github.com/video-site/backend/internal/localpath"
	"github.com/video-site/backend/internal/mediaasset"
	"github.com/video-site/backend/internal/mediasim"
)

const durationToleranceSeconds = mediasim.NearDuplicateDurationToleranceSeconds

func main() {
	dbPath := flag.String("db", "data/video-site.db", "sqlite path")
	localDir := flag.String("local-dir", "data/previews", "本地预览目录(config storage.local_preview_dir)")
	ffmpegPath := flag.String("ffmpeg", "ffmpeg", "ffmpeg 路径")
	workers := flag.Int("workers", 8, "签名提取并发数")
	apply := flag.Bool("apply", false, "真正执行：删除重复项（默认只读预演）")
	flag.Parse()

	cat, err := catalog.Open(*dbPath)
	if err != nil {
		log.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	videos, err := cat.ListVideoMaintenanceCandidates(ctx)
	if err != nil {
		log.Fatalf("list videos: %v", err)
	}
	localAbs, err := filepath.Abs(*localDir)
	if err != nil {
		log.Fatalf("local dir: %v", err)
	}

	candidates, videosByID := contentCandidates(localAbs, videos)
	fmt.Fprintf(os.Stderr, "videos=%d content_candidates=%d\n", len(videos), len(candidates))
	signatures := extractSignatures(ctx, localAbs, *ffmpegPath, *workers, *apply, candidates)

	plan, err := dedupe.Build(ctx, candidates, dedupe.Options{
		Channels: dedupe.ChannelContent,
		LoadContentSignature: func(_ context.Context, candidate dedupe.Candidate) (*mediasim.FrameSignature, error) {
			return signatures[candidate.ID], nil
		},
	})
	if err != nil {
		log.Fatalf("build content dedupe plan: %v", err)
	}
	printPlan(plan, videosByID)
	if !*apply {
		fmt.Printf("\n将删除 %d 个视频（只读预演，加 -apply 执行）。\n", len(plan.Actions))
		return
	}
	if err := applyPlan(ctx, cat, localAbs, plan); err != nil {
		log.Fatalf("apply content dedupe plan: %v", err)
	}
	fmt.Printf("\n已删除 %d 个视频。\n", len(plan.Actions))
	if dropped, err := cat.DropLegacyDuplicateReviewTable(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "移除旧复核队列表失败: %v\n", err)
	} else if dropped {
		fmt.Println("旧复核队列表已在自动去重完成后移除。")
	}
}

func contentCandidates(localDir string, videos []*catalog.Video) ([]dedupe.Candidate, map[string]*catalog.Video) {
	candidates := make([]dedupe.Candidate, 0, len(videos))
	videosByID := make(map[string]*catalog.Video, len(videos))
	for _, video := range videos {
		if video == nil || video.DurationSeconds < mediasim.ContentDuplicateMinDurationSeconds || strings.TrimSpace(video.PreviewStatus) != "ready" {
			continue
		}
		teaserPath, ok := existingPreviewPath(localDir, video.PreviewLocal)
		if !ok {
			continue
		}
		candidates = append(candidates, dedupe.Candidate{
			ID:                video.ID,
			Title:             video.Title,
			DurationSeconds:   video.DurationSeconds,
			Size:              video.Size,
			SampledSHA256:     video.SampledSHA256,
			AssetScore:        assetScore(localDir, video),
			CreatedAt:         video.CreatedAt,
			ExpectedUpdatedAt: video.UpdatedAt.UnixMilli(),
			TeaserPath:        teaserPath,
		})
		videosByID[video.ID] = video
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].DurationSeconds != candidates[j].DurationSeconds {
			return candidates[i].DurationSeconds < candidates[j].DurationSeconds
		}
		if !candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
		}
		return candidates[i].ID < candidates[j].ID
	})
	return candidates, videosByID
}

func extractSignatures(ctx context.Context, localDir, ffmpegPath string, workers int, storeCache bool, candidates []dedupe.Candidate) map[string]*mediasim.FrameSignature {
	involved := make(map[int]struct{})
	for i := range candidates {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].DurationSeconds-candidates[i].DurationSeconds > durationToleranceSeconds {
				break
			}
			involved[i] = struct{}{}
			involved[j] = struct{}{}
		}
	}
	fmt.Fprintf(os.Stderr, "involved_in_pairs=%d, extracting signatures...\n", len(involved))
	if workers <= 0 {
		workers = 1
	}

	signatures := make(map[string]*mediasim.FrameSignature)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	done := 0
	cacheHits := 0
	for i := range involved {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			candidate := candidates[i]
			cachePath := mediaasset.FrameSignaturePath(localDir, candidate.ID)
			signature, cached := mediasim.LoadCachedTeaserSignature(cachePath, candidate.TeaserPath)
			var err error
			if !cached {
				signature, err = mediasim.ExtractTeaserFrameSignature(ctx, ffmpegPath, candidate.TeaserPath)
				if err == nil && signature != nil && storeCache {
					if storeErr := mediasim.StoreCachedTeaserSignature(cachePath, candidate.TeaserPath, signature); storeErr != nil {
						fmt.Fprintf(os.Stderr, "  cache write failed id=%s: %v\n", candidate.ID, storeErr)
					}
				}
			}

			mu.Lock()
			defer mu.Unlock()
			done++
			if cached {
				cacheHits++
			}
			if done%300 == 0 {
				fmt.Fprintf(os.Stderr, "  %d/%d (cache hits %d)\n", done, len(involved), cacheHits)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "  extract failed id=%s: %v\n", candidate.ID, err)
				return
			}
			if signature == nil || signature.InformativeFrames() < mediasim.ContentDuplicateMinComparisons {
				return
			}
			signatures[candidate.ID] = signature
		}(i)
	}
	wg.Wait()
	fmt.Fprintf(os.Stderr, "signatures=%d cache_hits=%d\n", len(signatures), cacheHits)
	return signatures
}

func printPlan(plan dedupe.Plan, videosByID map[string]*catalog.Video) {
	alignedMatches := 0
	for _, match := range plan.Matches {
		if match.Stage != dedupe.StageContent {
			continue
		}
		if match.Cross {
			left, right := videosByID[match.LeftID], videosByID[match.RightID]
			fmt.Printf("[交叉命中] %s (%q) <-> %s (%q) median_best=%.3f\n", match.LeftID, videoTitle(left), match.RightID, videoTitle(right), match.Score)
			continue
		}
		alignedMatches++
	}
	groups := make([]dedupe.Group, 0)
	for _, group := range plan.Groups {
		if group.Stage == dedupe.StageContent {
			groups = append(groups, group)
		}
	}
	fmt.Printf("\n=== 内容级重复分组：%d 组（对齐命中 %d 次，交叉命中 %d 次）===\n", len(groups), alignedMatches, plan.Stats.Content.CrossMatched)
	for i, group := range groups {
		duration := 0
		if len(group.MemberIDs) > 0 && videosByID[group.MemberIDs[0]] != nil {
			duration = videosByID[group.MemberIDs[0]].DurationSeconds
		}
		fmt.Printf("\n组 %d（时长 %ds）：\n", i+1, duration)
		for _, id := range group.MemberIDs {
			video := videosByID[id]
			if video == nil {
				continue
			}
			marker := "删除"
			if id == group.CanonicalVideoID {
				marker = "保留"
			}
			fmt.Printf("  [%s] %s size=%d drive=%s title=%q\n", marker, video.ID, video.Size, video.DriveID, video.Title)
		}
	}
}

func applyPlan(ctx context.Context, cat *catalog.Catalog, localDir string, plan dedupe.Plan) error {
	deletions := make([]catalog.DuplicateVideoDeletion, 0, len(plan.Actions))
	for _, action := range plan.Actions {
		deletions = append(deletions, catalog.DuplicateVideoDeletion{
			VideoID:                    action.VideoID,
			CanonicalVideoID:           action.CanonicalVideoID,
			ExpectedUpdatedAt:          action.ExpectedUpdatedAt,
			CanonicalExpectedUpdatedAt: action.CanonicalExpectedUpdatedAt,
		})
	}
	var applyErr error
	for attempt := 0; attempt < 12; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		applyErr = cat.ApplyDuplicateVideoDeletions(ctx, deletions)
		if applyErr == nil || !sqliteBusy(applyErr) {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 250 * time.Millisecond)
	}
	if applyErr != nil {
		return applyErr
	}
	return cleanupPendingAssets(ctx, cat, localDir)
}

func cleanupPendingAssets(ctx context.Context, cat *catalog.Catalog, localDir string) error {
	jobs, err := cat.ListDuplicateAssetCleanupJobs(ctx, 10000)
	if err != nil {
		return err
	}
	var cleanupErrors []error
	for _, job := range jobs {
		if err := mediaasset.RemoveGeneratedVideoAssets(localDir, job.VideoID, job.PreviewLocal); err != nil {
			_ = cat.FailDuplicateAssetCleanupJob(ctx, job.VideoID, err)
			cleanupErrors = append(cleanupErrors, err)
			continue
		}
		if err := cat.CompleteDuplicateAssetCleanupJob(ctx, job.VideoID); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	return errors.Join(cleanupErrors...)
}

// deleteDuplicateWithAssets remains a small compatibility seam for the command
// test; it uses the same transactional deletion and post-commit cleanup as a
// full plan rather than maintaining a second implementation.
func deleteDuplicateWithAssets(ctx context.Context, cat *catalog.Catalog, localDir string, video *catalog.Video, canonicalID string) error {
	if video == nil {
		return nil
	}
	canonical, err := cat.GetVideo(ctx, canonicalID)
	if err != nil {
		return err
	}
	return applyPlan(ctx, cat, localDir, dedupe.Plan{
		Actions: []dedupe.DeleteAction{{
			Stage: dedupe.StageContent, VideoID: video.ID, CanonicalVideoID: canonicalID,
			ExpectedUpdatedAt: video.UpdatedAt.UnixMilli(), CanonicalExpectedUpdatedAt: canonical.UpdatedAt.UnixMilli(),
		}},
		Redirects: map[string]string{video.ID: canonicalID},
	})
}

func existingPreviewPath(localDir, path string) (string, bool) {
	clean, ok := localpath.Within(localDir, path)
	if !ok {
		return "", false
	}
	info, err := os.Stat(clean)
	return clean, err == nil && info.Mode().IsRegular()
}

func assetScore(localDir string, video *catalog.Video) int {
	if video == nil {
		return 0
	}
	score := 0
	if strings.TrimSpace(video.PreviewStatus) == "ready" {
		if _, ok := existingPreviewPath(localDir, video.PreviewLocal); ok {
			score++
		}
	}
	if strings.TrimSpace(video.ThumbnailURL) == "/p/thumb/"+video.ID {
		for _, path := range mediaasset.ThumbnailPathCandidates(localDir, video.ID) {
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
				score++
				break
			}
		}
	}
	if strings.TrimSpace(video.SampledSHA256) != "" && strings.TrimSpace(video.FingerprintStatus) == "ready" {
		score++
	}
	return score
}

func sqliteBusy(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "busy") || strings.Contains(message, "locked")
}

func videoTitle(video *catalog.Video) string {
	if video == nil {
		return ""
	}
	return video.Title
}
