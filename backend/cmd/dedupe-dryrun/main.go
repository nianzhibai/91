// dedupe-dryrun：预演/执行夜间维护的内容级查重，并对比旧的 teaser 帧通道
// （content）与新的感知签名通道（signature）。分组、canonical 选择和最终删除
// 计划与生产共用 internal/dedupe。
//
// 默认 -channel compare：两个通道都跑，输出逐对判定矩阵和分歧明细，用于校准
// mediasig 的阈值。现有视频的签名从本地 teaser 按主选段映射回源时间。
//
// 默认只读，不写库、不删文件；指定单个通道并加 -apply 后一次性提交该通道的
// 完整计划，再幂等清理本地生成资产。原始媒体源始终不在清理范围内。
//
// 用法：在 backend 目录下运行
//
//	go run ./cmd/dedupe-dryrun -db data/91.db -local-dir data/previews [-channel content|signature [-apply]]
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
	"github.com/nianzhibai/91/backend/internal/mediasim"
	"github.com/nianzhibai/91/backend/internal/preview"
)

const durationToleranceSeconds = mediasim.NearDuplicateDurationToleranceSeconds

const (
	modeCompare   = "compare"
	modeContent   = "content"
	modeSignature = "signature"
)

func main() {
	dbPath := flag.String("db", "data/91.db", "sqlite path")
	localDir := flag.String("local-dir", "data/previews", "本地预览目录(storage.data_dir 下的 previews)")
	ffmpegPath := flag.String("ffmpeg", "ffmpeg", "ffmpeg 路径")
	workers := flag.Int("workers", 8, "签名提取并发数")
	mode := flag.String("channel", modeCompare, "compare：对比两个通道；content / signature：只跑该通道")
	limit := flag.Int("limit", 50, "compare 模式最多列出的分歧对数")
	apply := flag.Bool("apply", false, "真正执行：删除重复项（默认只读预演，须指定单个通道）")
	flag.Parse()
	switch *mode {
	case modeCompare, modeContent, modeSignature:
	default:
		log.Fatalf("unknown -channel %q", *mode)
	}
	if *apply && *mode == modeCompare {
		log.Fatal("-apply 需要 -channel content 或 -channel signature")
	}

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
	involved := pairedCandidates(candidates)
	fmt.Fprintf(os.Stderr, "videos=%d content_candidates=%d involved_in_pairs=%d\n", len(videos), len(candidates), len(involved))

	var frameSignatures map[string]*mediasim.FrameSignature
	if *mode != modeSignature {
		frameSignatures = extractFrameSignatures(ctx, localAbs, *ffmpegPath, *workers, *apply, candidates, involved)
	}
	if *mode != modeContent {
		attachSignatures(ctx, *ffmpegPath, *workers, candidates, involved)
	}
	loadFrameSignature := func(_ context.Context, candidate dedupe.Candidate) (*mediasim.FrameSignature, error) {
		return frameSignatures[candidate.ID], nil
	}

	if *mode == modeCompare {
		comparePairs(candidates, frameSignatures, videosByID, *limit)
	}
	var plan dedupe.Plan
	for _, stage := range stagesFor(*mode) {
		plan, err = dedupe.Build(ctx, candidates, dedupe.Options{Channels: channelOf(stage), LoadContentSignature: loadFrameSignature})
		if err != nil {
			log.Fatalf("build dedupe plan: %v", err)
		}
		printPlan(stage, plan, videosByID)
	}
	if !*apply {
		fmt.Println("\n只读预演；指定 -channel content 或 signature 并加 -apply 执行。")
		return
	}
	if err := applyPlan(ctx, cat, localAbs, plan); err != nil {
		log.Fatalf("apply dedupe plan: %v", err)
	}
	fmt.Printf("\n已合并并移除 %d 个重复视频行。\n", len(plan.Actions))
}

func stagesFor(mode string) []dedupe.Stage {
	switch mode {
	case modeContent:
		return []dedupe.Stage{dedupe.StageContent}
	case modeSignature:
		return []dedupe.Stage{dedupe.StageSignature}
	default:
		return []dedupe.Stage{dedupe.StageContent, dedupe.StageSignature}
	}
}

func channelOf(stage dedupe.Stage) dedupe.Channels {
	if stage == dedupe.StageSignature {
		return dedupe.ChannelSignature
	}
	return dedupe.ChannelContent
}

func contentCandidates(localDir string, videos []*catalog.Video) ([]dedupe.Candidate, map[string]*catalog.Video) {
	candidates := make([]dedupe.Candidate, 0, len(videos))
	videosByID := make(map[string]*catalog.Video, len(videos))
	for _, video := range videos {
		// The signature channel accepts shorter videos than the content
		// channel; each channel applies its own minimum when comparing.
		if video == nil || time.Duration(video.DurationSeconds)*time.Second < mediasig.MinDuration || strings.TrimSpace(video.PreviewStatus) != "ready" {
			continue
		}
		teaserPath, ok := existingPreviewPath(localDir, video.PreviewLocal)
		if !ok {
			continue
		}
		candidates = append(candidates, dedupe.Candidate{
			ID:              video.ID,
			Title:           video.Title,
			DurationSeconds: video.DurationSeconds,
			Size:            video.Size,
			ContentHash:     video.ContentHash,
			SampledSHA256:   video.SampledSHA256,
			AssetScore:      assetScore(localDir, video),
			CreatedAt:       video.CreatedAt,
			Revision:        catalog.DuplicateRevision(video),
			TeaserPath:      teaserPath,
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

// pairedCandidates returns the indices of duration-sorted candidates that
// have at least one partner within the duration tolerance.
func pairedCandidates(candidates []dedupe.Candidate) []int {
	involved := make(map[int]struct{})
	forEachPair(candidates, func(i, j int) {
		involved[i] = struct{}{}
		involved[j] = struct{}{}
	})
	out := make([]int, 0, len(involved))
	for i := range involved {
		out = append(out, i)
	}
	sort.Ints(out)
	return out
}

func forEachPair(candidates []dedupe.Candidate, visit func(i, j int)) {
	for i := range candidates {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].DurationSeconds-candidates[i].DurationSeconds > durationToleranceSeconds {
				break
			}
			visit(i, j)
		}
	}
}

// parallel runs work for every index with bounded concurrency and reports
// progress on stderr.
func parallel(label string, workers int, indices []int, work func(int)) {
	if workers <= 0 {
		workers = 1
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, workers)
	done := 0
	for _, i := range indices {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			work(i)
			mu.Lock()
			done++
			if done%300 == 0 {
				fmt.Fprintf(os.Stderr, "  %s %d/%d\n", label, done, len(indices))
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
}

func extractFrameSignatures(ctx context.Context, localDir, ffmpegPath string, workers int, storeCache bool, candidates []dedupe.Candidate, involved []int) map[string]*mediasim.FrameSignature {
	signatures := make(map[string]*mediasim.FrameSignature)
	var mu sync.Mutex
	cacheHits := 0
	parallel("frame signatures", workers, involved, func(i int) {
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
		if cached {
			cacheHits++
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "  frame signature failed id=%s: %v\n", candidate.ID, err)
			return
		}
		if signature != nil && signature.InformativeFrames() >= mediasim.ContentDuplicateMinComparisons {
			signatures[candidate.ID] = signature
		}
	})
	fmt.Fprintf(os.Stderr, "frame_signatures=%d cache_hits=%d\n", len(signatures), cacheHits)
	return signatures
}

// attachSignatures derives perceptual signatures from the existing teasers.
func attachSignatures(ctx context.Context, ffmpegPath string, workers int, candidates []dedupe.Candidate, involved []int) {
	failed := 0
	var mu sync.Mutex
	parallel("perceptual signatures", workers, involved, func(i int) {
		candidate := &candidates[i]
		duration := float64(candidate.DurationSeconds)
		signature, err := mediasig.FromTeaser(ctx, ffmpegPath, candidate.TeaserPath,
			time.Duration(duration*float64(time.Second)), preview.TeaserSourceWindows(duration))
		if err != nil {
			mu.Lock()
			failed++
			mu.Unlock()
			fmt.Fprintf(os.Stderr, "  perceptual signature failed id=%s: %v\n", candidate.ID, err)
			return
		}
		candidate.Signature = signature
	})
	fmt.Fprintf(os.Stderr, "perceptual_signatures=%d failed=%d\n", len(involved)-failed, failed)
}

type pairOutcome struct {
	left, right dedupe.Candidate
	old         string
	oldDetail   string
	verdict     mediasig.Verdict
	comparison  *mediasig.Comparison
}

// comparePairs evaluates both pair rules on every candidate pair within the
// duration tolerance and prints where they disagree.
func comparePairs(candidates []dedupe.Candidate, frameSignatures map[string]*mediasim.FrameSignature, videosByID map[string]*catalog.Video, limit int) {
	matrix := map[string]map[mediasig.Verdict]int{}
	static, staticStrong := 0, 0
	var disagreements []pairOutcome
	forEachPair(candidates, func(i, j int) {
		left, right := candidates[i], candidates[j]
		outcome := pairOutcome{left: left, right: right, old: "none"}
		if match, matched := dedupe.MatchContent(left, right, frameSignatures[left.ID], frameSignatures[right.ID]); matched {
			outcome.old = "aligned"
			if match.Cross {
				outcome.old = "cross"
			}
			outcome.oldDetail = fmt.Sprintf("score=%.3f n=%d", match.Score, match.Comparisons)
		}
		match, verdict := dedupe.MatchSignature(left, right)
		outcome.verdict, outcome.comparison = verdict, match.Signature
		if match.Signature != nil && match.Signature.Static {
			static++
			if verdict == mediasig.VerdictStrong {
				staticStrong++
			}
		}
		if matrix[outcome.old] == nil {
			matrix[outcome.old] = map[mediasig.Verdict]int{}
		}
		matrix[outcome.old][verdict]++
		oldDuplicate := outcome.old != "none"
		if oldDuplicate != (verdict == mediasig.VerdictStrong) {
			disagreements = append(disagreements, outcome)
		}
	})

	fmt.Printf("\n=== 逐对判定（旧 content 通道 × 新 signature 通道）===\n")
	fmt.Printf("%-10s %8s %8s %8s\n", "旧\\新", "strong", "probable", "none")
	for _, old := range []string{"aligned", "cross", "none"} {
		row := matrix[old]
		fmt.Printf("%-10s %8d %8d %8d\n", old, row[mediasig.VerdictStrong], row[mediasig.VerdictProbable], row[mediasig.VerdictNone])
	}
	fmt.Printf("涉及静态画面签名的对数：%d，其中按更严帧距离判为 strong：%d\n", static, staticStrong)

	fmt.Printf("\n=== 分歧明细（%d 对，最多列出 %d 对）===\n", len(disagreements), limit)
	for i, outcome := range disagreements {
		if i >= limit {
			break
		}
		detail := "无签名"
		if c := outcome.comparison; c != nil {
			detail = fmt.Sprintf("%s 帧 %d/%d 窗 %d/%d 中位距离 %d static=%v",
				c.Verdict, c.MatchedFrames, c.Frames, c.MatchedWindows, c.Windows, c.MedianDistance, c.Static)
		}
		fmt.Printf("[旧 %s %s | 新 %s]\n  %s %ds %q\n  %s %ds %q\n", outcome.old, outcome.oldDetail, detail,
			outcome.left.ID, outcome.left.DurationSeconds, videoTitle(videosByID[outcome.left.ID]),
			outcome.right.ID, outcome.right.DurationSeconds, videoTitle(videosByID[outcome.right.ID]))
	}
}

func printPlan(stage dedupe.Stage, plan dedupe.Plan, videosByID map[string]*catalog.Video) {
	stats := plan.Stats.Content
	if stage == dedupe.StageSignature {
		stats = plan.Stats.Signature
	}
	fmt.Printf("\n=== %s 通道：%d 组，合并 %d 个，比较 %d 次，待复核 %d 对 ===\n",
		stage, stats.Groups, stats.Deleted, stats.Comparisons, len(plan.Reviews))
	for i, group := range plan.Groups {
		if group.Stage != stage {
			continue
		}
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
			Stage: dedupe.StageContent, VideoID: video.ID, CanonicalVideoID: canonicalID,
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

func videoTitle(video *catalog.Video) string {
	if video == nil {
		return ""
	}
	return video.Title
}
