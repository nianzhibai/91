// dedupe-dryrun：预演/执行夜间维护的感知签名查重通道。分组、canonical 选择
// 和最终删除计划与生产共用 internal/dedupe。
//
// 签名优先读取库内已存的；缺失的从本地 teaser 按主选段临时推导，只在内存中
// 使用，不写回数据库。
//
// 默认只读，不写库、不删文件；加 -apply 后一次性提交完整计划，再幂等清理
// 本地生成资产。原始媒体源始终不在清理范围内。
//
// 用法：在 backend 目录下运行
//
//	go run ./cmd/dedupe-dryrun -db data/91.db -local-dir data/previews [-apply]
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

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/localpath"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
	"github.com/nianzhibai/91/backend/internal/mediasig"
	"github.com/nianzhibai/91/backend/internal/preview"
)

func main() {
	dbPath := flag.String("db", "data/91.db", "sqlite path")
	localDir := flag.String("local-dir", "data/previews", "本地预览目录(storage.data_dir 下的 previews)")
	ffmpegPath := flag.String("ffmpeg", "ffmpeg", "ffmpeg 路径")
	workers := flag.Int("workers", 4, "从 teaser 推导缺失签名的并发数")
	apply := flag.Bool("apply", false, "真正执行：合并重复项（默认只读预演）")
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

	candidates, teasers, videosByID := signatureCandidates(localAbs, videos)
	if err := attachSignatures(ctx, cat, *ffmpegPath, *workers, candidates, teasers); err != nil {
		log.Fatalf("load signatures: %v", err)
	}
	plan, err := dedupe.Build(ctx, candidates, dedupe.Options{Channels: dedupe.ChannelSignature})
	if err != nil {
		log.Fatalf("build dedupe plan: %v", err)
	}
	printPlan(plan, videosByID)
	if !*apply {
		fmt.Printf("\n将合并 %d 个重复视频（只读预演，加 -apply 执行）。\n", len(plan.Actions))
		return
	}
	if err := applyPlan(ctx, cat, localAbs, plan); err != nil {
		log.Fatalf("apply dedupe plan: %v", err)
	}
	fmt.Printf("\n已合并 %d 个重复视频。\n", len(plan.Actions))
}

// signatureCandidates returns the videos long enough for a signature verdict,
// with the local teaser of each that has one.
func signatureCandidates(localDir string, videos []*catalog.Video) ([]dedupe.Candidate, map[string]string, map[string]*catalog.Video) {
	candidates := make([]dedupe.Candidate, 0, len(videos))
	teasers := make(map[string]string)
	videosByID := make(map[string]*catalog.Video, len(videos))
	for _, video := range videos {
		if video == nil {
			continue
		}
		candidate := dedupe.Candidate{
			ID:              video.ID,
			Title:           video.Title,
			DurationSeconds: video.DurationSeconds,
			Size:            video.Size,
			ContentHash:     video.ContentHash,
			SampledSHA256:   video.SampledSHA256,
			AssetScore:      assetScore(localDir, video),
			CreatedAt:       video.CreatedAt,
			Revision:        catalog.DuplicateRevision(video),
		}
		if !dedupe.SignatureEligible(candidate) {
			continue
		}
		if strings.TrimSpace(video.PreviewStatus) == "ready" {
			if teaser, ok := existingPreviewPath(localDir, video.PreviewLocal); ok {
				teasers[video.ID] = teaser
			}
		}
		candidates = append(candidates, candidate)
		videosByID[video.ID] = video
	}
	return candidates, teasers, videosByID
}

// attachSignatures sets the stored signature of every candidate and derives
// the missing ones from local teasers without storing them.
func attachSignatures(ctx context.Context, cat *catalog.Catalog, ffmpegPath string, workers int, candidates []dedupe.Candidate, teasers map[string]string) error {
	ids := make([]string, len(candidates))
	for i := range candidates {
		ids[i] = candidates[i].ID
	}
	stored, err := cat.LoadVideoSignatures(ctx, ids)
	if err != nil {
		return err
	}
	var missing []int
	for i := range candidates {
		if signature := stored[candidates[i].ID]; signature != nil {
			candidates[i].Signature = signature
		} else if teasers[candidates[i].ID] != "" {
			missing = append(missing, i)
		}
	}
	fmt.Fprintf(os.Stderr, "candidates=%d stored_signatures=%d deriving_from_teasers=%d\n", len(candidates), len(stored), len(missing))

	jobs := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	derived, failed := 0, 0
	for range max(1, workers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				candidate := &candidates[i]
				duration := float64(candidate.DurationSeconds)
				signature, err := mediasig.FromTeaser(ctx, ffmpegPath, teasers[candidate.ID],
					time.Duration(candidate.DurationSeconds)*time.Second, preview.TeaserSourceWindows(duration))
				mu.Lock()
				if err != nil {
					failed++
					fmt.Fprintf(os.Stderr, "  derive failed id=%s: %v\n", candidate.ID, err)
				} else {
					candidate.Signature = signature
					derived++
				}
				mu.Unlock()
			}
		}()
	}
	for _, i := range missing {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	fmt.Fprintf(os.Stderr, "derived=%d failed=%d\n", derived, failed)
	return nil
}

func printPlan(plan dedupe.Plan, videosByID map[string]*catalog.Video) {
	stats := plan.Stats.Signature
	fmt.Printf("=== 感知签名通道：候选 %d，比较 %d 次，%d 组，合并 %d 个，待复核 %d 对 ===\n",
		stats.Candidates, stats.Comparisons, stats.Groups, stats.Deleted, len(plan.Reviews))
	groups := append([]dedupe.Group(nil), plan.Groups...)
	sort.Slice(groups, func(i, j int) bool { return groups[i].CanonicalVideoID < groups[j].CanonicalVideoID })
	for i, group := range groups {
		fmt.Printf("\n组 %d（时长 %ds）：\n", i+1, videoDuration(videosByID[group.CanonicalVideoID]))
		for _, id := range group.MemberIDs {
			video := videosByID[id]
			if video == nil {
				continue
			}
			marker := "合并"
			if id == group.CanonicalVideoID {
				marker = "保留"
			}
			fmt.Printf("  [%s] %s size=%d drive=%s title=%q\n", marker, video.ID, video.Size, video.DriveID, video.Title)
		}
	}
	for _, match := range plan.Matches {
		if c := match.Signature; c != nil {
			fmt.Printf("[合并依据 帧 %d/%d 窗 %d/%d 中位距离 %d static=%v] %s <-> %s\n",
				c.MatchedFrames, c.Frames, c.MatchedWindows, c.Windows, c.MedianDistance, c.Static, match.LeftID, match.RightID)
		}
	}
	for _, review := range plan.Reviews {
		c := review.Signature
		fmt.Printf("[待复核 %s 帧 %d/%d 窗 %d/%d 中位距离 %d static=%v] %s %q <-> %s %q\n",
			c.Verdict, c.MatchedFrames, c.Frames, c.MatchedWindows, c.Windows, c.MedianDistance, c.Static,
			review.LeftID, videoTitle(videosByID[review.LeftID]), review.RightID, videoTitle(videosByID[review.RightID]))
	}
}

func applyPlan(ctx context.Context, cat *catalog.Catalog, localDir string, plan dedupe.Plan) error {
	deletions := make([]catalog.DuplicateVideoDeletion, 0, len(plan.Actions))
	for _, action := range plan.Actions {
		deletions = append(deletions, catalog.DuplicateVideoDeletion{
			VideoID:                   action.VideoID,
			CanonicalVideoID:          action.CanonicalVideoID,
			ExpectedRevision:          action.ExpectedRevision,
			CanonicalExpectedRevision: action.CanonicalExpectedRevision,
			Origin:                    catalog.DuplicateOriginCommand,
			Evidence:                  action.Evidence,
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
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 250 * time.Millisecond):
		}
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
		err := cat.CleanupRetiredVideoAssets(ctx, job.VideoID, func(preview string) error {
			return mediaasset.RemoveGeneratedVideoAssets(localDir, job.VideoID, preview)
		})
		if err != nil {
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
	stored, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		return err
	}
	canonical, err := cat.GetVideo(ctx, canonicalID)
	if err != nil {
		return err
	}
	return applyPlan(ctx, cat, localDir, dedupe.Plan{
		Actions: []dedupe.DeleteAction{{
			Stage: dedupe.StageSignature, VideoID: video.ID, CanonicalVideoID: canonicalID,
			ExpectedRevision: catalog.DuplicateRevision(stored), CanonicalExpectedRevision: catalog.DuplicateRevision(canonical),
		}},
		Redirects: map[string]string{video.ID: canonicalID},
	})
}

func existingPreviewPath(localDir, path string) (string, bool) {
	clean, ok := localpath.Managed(localDir, path)
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

func videoDuration(video *catalog.Video) int {
	if video == nil {
		return 0
	}
	return video.DurationSeconds
}

func videoTitle(video *catalog.Video) string {
	if video == nil {
		return ""
	}
	return video.Title
}
