package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/localpath"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
	"github.com/nianzhibai/91/backend/internal/persistence"
	"github.com/nianzhibai/91/backend/internal/streamhttp"
	"github.com/nianzhibai/91/backend/internal/tasklimit"
)

type Config struct {
	Hardware        *Hardware
	FFmpegPath      string
	FFprobePath     string
	FFmpegThreads   int
	DurationSeconds int // 兼容旧配置；当前预览视频每段固定 3 秒
	Width           int
	Segments        int    // 兼容旧配置；当前 30 秒及以上视频固定使用 4 段
	LocalDir        string // 本地预览视频和封面目录
}

func generationDriveID(drv drives.Drive) string {
	if drv == nil {
		return ""
	}
	return drv.ID()
}

type Generator struct {
	cfg Config
	// Captured once per asset so configuration changes never mix encoders
	// between the segments of an in-flight preview.
	hardware *hardwareBackend

	segmentBudget time.Duration
	finishTimeout time.Duration
}

const (
	defaultTeaserSegmentBudget = 4 * time.Minute
	teaserSegmentTimeout       = 90 * time.Second
	defaultTeaserFinishTimeout = 30 * time.Second
)

type ThumbnailGenerator interface {
	Probe(ctx context.Context, link *drives.StreamLink) (float64, error)
	GenerateThumbnail(ctx context.Context, link *drives.StreamLink, videoID string, duration float64) (string, error)
}

type TeaserGenerator interface {
	Probe(ctx context.Context, link *drives.StreamLink) (float64, error)
	Generate(ctx context.Context, link *drives.StreamLink, duration float64) (string, error)
	MoveToLocal(tmpPath, videoID string) (string, error)
}

type refreshingTeaserGenerator interface {
	GenerateWithLinkRefresh(ctx context.Context, first *drives.StreamLink, duration float64, refreshAfterFailure func(context.Context) (*drives.StreamLink, error)) (string, error)
}

func New(cfg Config) *Generator {
	if cfg.FFmpegThreads < 1 {
		cfg.FFmpegThreads = 1
	}
	if cfg.FFmpegPath == "" {
		cfg.FFmpegPath = "ffmpeg"
	}
	if cfg.FFprobePath == "" {
		cfg.FFprobePath = "ffprobe"
	}
	if cfg.DurationSeconds != 3 {
		cfg.DurationSeconds = 3
	}
	if cfg.Width == 0 {
		cfg.Width = 480
	}
	if cfg.Segments <= 0 {
		cfg.Segments = 3
	}
	return &Generator{
		cfg:           cfg,
		segmentBudget: defaultTeaserSegmentBudget,
		finishTimeout: defaultTeaserFinishTimeout,
	}
}

// --- 选段策略 ---

type teaserPlan struct {
	starts  []float64
	eachSec float64
}

func buildTeaserPlan(cfg Config, duration float64) teaserPlan {
	if cfg.DurationSeconds != 3 {
		cfg.DurationSeconds = 3
	}
	if cfg.Segments <= 0 {
		cfg.Segments = 3
	}

	segs := 1
	if duration > 0 && duration < 30 {
		segs = 3
	} else if duration >= 30 {
		segs = 4
	}

	eachSec := 3.0
	if duration > 0 && duration < eachSec {
		eachSec = duration
	}

	return teaserPlan{
		starts:  pickSegmentStarts(duration, segs, eachSec),
		eachSec: eachSec,
	}
}

// pickSegmentStarts 根据视频总时长选出 N 段起点秒数（按时间升序）
//
// 规则：
//   - duration < 30s → 最多 3 段；不足 3 秒时用完整短视频作为单段
//   - 30s ≤ duration < 10min → 4 段：前段跳过片头、末段避开片尾
//   - duration ≥ 10min → 固定 4 段，按 20% ~ 80% 等距分布
func pickSegmentStarts(duration float64, n int, eachSec float64) []float64 {
	if n <= 0 {
		n = 1
	}
	if duration <= 0 {
		// 未知时长，用保守默认
		return []float64{10}
	}
	if duration < 30 {
		completeSegments := int(math.Floor(duration / eachSec))
		if completeSegments > n {
			completeSegments = n
		}
		if completeSegments <= 0 {
			return nil
		}
		usable := duration - eachSec
		first := math.Min(duration*0.1, usable)
		if completeSegments == 1 {
			return []float64{math.Max(0, first)}
		}
		starts := make([]float64, 0, completeSegments)
		step := (usable - first) / float64(completeSegments-1)
		for i := 0; i < completeSegments; i++ {
			starts = append(starts, first+step*float64(i))
		}
		return starts
	}

	// 余量：保证最后一段结束前留 1 秒，避免切到文件末尾
	usable := duration - eachSec - 1
	if usable < 0 {
		usable = 0
	}

	if duration < 600 {
		// 30s ~ 10min：20% 起，均匀分段
		starts := make([]float64, 0, n)
		// 保证第一段跳过片头（>= 5% 或 3s）
		firstMin := math.Max(3, duration*0.05)
		// 最后一段结束 <= 85%，避开结尾
		lastMax := duration * 0.85
		if lastMax < firstMin {
			lastMax = firstMin
		}
		if n == 1 {
			return []float64{duration * 0.25}
		}
		step := (lastMax - firstMin) / float64(n-1)
		for i := 0; i < n; i++ {
			s := firstMin + step*float64(i)
			if s > usable {
				s = usable
			}
			starts = append(starts, s)
		}
		return starts
	}

	// 长视频：按 20% / 50% / 80% 布置
	if n == 1 {
		return []float64{duration * 0.3}
	}
	starts := make([]float64, 0, n)
	pct := make([]float64, 0, n)
	// 均匀在 [0.2, 0.8] 区间取 N 个点
	lo, hi := 0.2, 0.8
	if n == 1 {
		pct = append(pct, 0.3)
	} else {
		step := (hi - lo) / float64(n-1)
		for i := 0; i < n; i++ {
			pct = append(pct, lo+step*float64(i))
		}
	}
	for _, p := range pct {
		s := duration * p
		if s > usable {
			s = usable
		}
		starts = append(starts, s)
	}
	return starts
}

func teaserCandidateStarts(duration float64, primary []float64, eachSec float64) []float64 {
	out := make([]float64, 0, len(primary)+8)
	for _, s := range primary {
		out = appendUniqueStart(out, s, eachSec)
	}

	if duration <= 0 {
		for _, s := range []float64{0, 3, 30, 60} {
			out = appendUniqueStart(out, s, eachSec)
		}
		return out
	}

	usable := duration - eachSec - 1
	if usable < 0 {
		usable = 0
	}
	for _, pct := range []float64{0.03, 0.08, 0.12, 0.25, 0.40, 0.55, 0.70, 0.90} {
		s := duration * pct
		if s > usable {
			s = usable
		}
		out = appendUniqueStart(out, s, eachSec)
	}
	return out
}

func appendUniqueStart(starts []float64, start, eachSec float64) []float64 {
	if start < 0 {
		start = 0
	}
	minGap := math.Max(1, eachSec*1.5)
	for _, existing := range starts {
		if math.Abs(existing-start) < minGap {
			return starts
		}
	}
	return append(starts, start)
}

// thumbnailOffsets 选封面抽帧的时间点（秒）。独立于预览视频。
// 默认取视频中间帧；时长未知时退回早期帧。
func thumbnailOffsets(duration float64) []float64 {
	if duration <= 0 {
		return []float64{5, 1, 0}
	}
	mid := duration / 2
	out := []float64{mid}
	for _, fallback := range []float64{5, 1, 0} {
		if !containsOffset(out, fallback) {
			out = append(out, fallback)
		}
	}
	return out
}

func containsOffset(offsets []float64, target float64) bool {
	for _, offset := range offsets {
		if math.Abs(offset-target) < 0.01 {
			return true
		}
	}
	return false
}

// --- 封面 ---

// GenerateThumbnail 抽一张 jpg 封面。默认从视频中间抽帧，失败时回退到更早时间点。
func (g *Generator) GenerateThumbnail(ctx context.Context, link *drives.StreamLink, videoID string, duration float64) (string, error) {
	task := g.forTask()
	path, err := task.generateThumbnail(ctx, link, videoID, duration)
	if task.fallbackToCPU(ctx, err) {
		return task.generateThumbnail(ctx, link, videoID, duration)
	}
	return path, err
}

func (g *Generator) generateThumbnail(ctx context.Context, link *drives.StreamLink, videoID string, duration float64) (string, error) {
	dir := filepath.Join(g.cfg.LocalDir, "thumbs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := mediaasset.ThumbnailPath(g.cfg.LocalDir, videoID)
	temp, err := os.CreateTemp(dir, ".thumbnail-*.part.jpg")
	if err != nil {
		return "", err
	}
	tempPath := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}
	_ = os.Remove(tempPath)
	defer os.Remove(tempPath)

	var lastErr error
	var colorRepair sourceColorRepair
	offsets := thumbnailOffsets(duration)
	for i, offset := range offsets {
		if i > 0 {
			_ = os.Remove(tempPath)
		}
		if err := g.generateThumbnailAtOffset(ctx, link, tempPath, offset, &colorRepair); err != nil {
			lastErr = err
			if g.hardware != nil && hardwareFailure(err) {
				return "", err
			}
			if !thumbnailOffsetFallbackAllowed(err) {
				return "", err
			}
			continue
		}
		if err := replaceFile(tempPath, dst); err != nil {
			return "", err
		}
		return dst, nil
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", errors.New("thumbnail generation did not run")
}

func (g *Generator) generateThumbnailAtOffset(ctx context.Context, link *drives.StreamLink, dst string, offset float64, colorRepair *sourceColorRepair) error {
	ctx2, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	ffmpegLink, cleanup, err := prepareFFmpegLink(ctx2, link)
	if err != nil {
		return err
	}
	defer cleanup()

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-filter_threads", strconv.Itoa(g.cfg.FFmpegThreads),
		"-filter_complex_threads", strconv.Itoa(g.cfg.FFmpegThreads),
		"-threads", strconv.Itoa(g.cfg.FFmpegThreads),
		"-ss", fmt.Sprintf("%.2f", offset),
	}
	args = append(args, g.hardware.inputOptions()...)
	outputArgs := []string{
		"-threads", strconv.Itoa(g.cfg.FFmpegThreads),
		"-frames:v", "1",
		"-vf", g.thumbnailFilter(),
		"-q:v", "3",
		"-y", dst,
	}
	out, err := g.runMediaCommand(ctx2, link, ffmpegLink, args, outputArgs, colorRepair)
	if err != nil {
		os.Remove(dst)
		return g.processingError(ffmpegCommandError("ffmpeg thumb", err, out))
	}
	if info, statErr := os.Stat(dst); statErr != nil || info.Size() == 0 {
		os.Remove(dst)
		return g.processingError(fmt.Errorf("ffmpeg thumb produced empty file, stderr: %s", string(out)))
	}
	return nil
}

func thumbnailVideoFilter(width int) string {
	// FFmpeg 7 rejects non-full-range YUV for MJPEG/JPEG output. Force the
	// scaled frame into a JPEG-friendly full-range pixel format before encode.
	return fmt.Sprintf("scale=%d:-2:out_range=pc,format=yuvj420p", width)
}

func teaserSegmentVideoFilter(width int, fade bool, eachSec float64) string {
	parts := []string{
		fmt.Sprintf("scale=%d:-2", width),
		"setsar=1",
		// Input seeking against remote/VFR media can leave the decoded segment with
		// the source file's original PTS. The sequential generator later concat-copies
		// individually encoded segments, so every segment file must start at zero;
		// otherwise players fill the timestamp gap with black frames.
		"setpts=PTS-STARTPTS",
	}
	if fade {
		fadeOutStart := eachSec - 0.2
		if fadeOutStart < 0 {
			fadeOutStart = 0
		}
		parts = append(parts,
			"fade=t=in:st=0:d=0.20",
			fmt.Sprintf("fade=t=out:st=%.2f:d=0.2", fadeOutStart),
		)
	}
	return strings.Join(parts, ",")
}

func thumbnailOffsetFallbackAllowed(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := drives.RateLimitRetryAfter(err); ok {
		return false
	}
	text := strings.ToLower(err.Error())
	if transientRemoteMediaReadText(text) || strings.Contains(text, "moov atom not found") {
		return true
	}
	if deterministicMediaInputError(text) {
		return false
	}
	return invalidColorMetadataText(text) ||
		strings.Contains(text, "produced empty file") ||
		strings.Contains(text, "context deadline exceeded")
}

// --- 时长 ---

const (
	durationSanityMinFileSize       = int64(1 << 20)
	minimumMediaBytesPerSecond      = 16.0
	maximumAcceptedMediaDurationSec = 10 * 365 * 24 * 60 * 60
)

// plausibleMediaDuration intentionally uses an extremely permissive minimum
// bitrate. Its purpose is to reject unit/corruption mistakes (for example a
// 24 MB file recorded as several million seconds), not to judge media quality.
func plausibleMediaDuration(size int64, duration float64) bool {
	if duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) || duration > maximumAcceptedMediaDurationSec {
		return false
	}
	if size < durationSanityMinFileSize {
		return true
	}
	return float64(size)/duration >= minimumMediaBytesPerSecond
}

func normalizedStoredDuration(ctx context.Context, cat *catalog.Catalog, v *catalog.Video) (float64, error) {
	if v == nil || v.DurationSeconds <= 0 {
		return 0, nil
	}
	duration := float64(v.DurationSeconds)
	if plausibleMediaDuration(v.Size, duration) {
		return duration, nil
	}
	log.Printf("[preview] discard implausible duration video=%s size=%d duration=%d", v.ID, v.Size, v.DurationSeconds)
	if cat != nil {
		if err := cat.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{
			DurationSeconds:    0,
			DurationSecondsSet: true,
		}); err != nil {
			return 0, fmt.Errorf("clear invalid duration: %w", err)
		}
	}
	v.DurationSeconds = 0
	return 0, nil
}

func storeProbedDuration(ctx context.Context, cat *catalog.Catalog, v *catalog.Video, duration float64) (float64, error) {
	if v == nil || !plausibleMediaDuration(v.Size, duration) {
		if v != nil && duration > 0 {
			log.Printf("[preview] ignore implausible probed duration video=%s size=%d duration=%.1f", v.ID, v.Size, duration)
		}
		return 0, errDurationUnavailable
	}
	seconds := max(1, int(duration))
	if cat != nil {
		if err := cat.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{DurationSeconds: seconds}); err != nil {
			return 0, fmt.Errorf("save duration: %w", err)
		}
	}
	v.DurationSeconds = seconds
	return duration, nil
}

// Probe 用 ffprobe 拿视频时长（秒，浮点）
func (g *Generator) Probe(ctx context.Context, link *drives.StreamLink) (float64, error) {
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ffmpegLink, cleanup, err := prepareFFmpegLink(ctx2, link)
	if err != nil {
		return 0, err
	}
	defer cleanup()

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-show_entries", "stream=codec_type,duration:format=duration",
		"-of", "json",
	}
	args = append(args, ffmpegHTTPInputOptions(ffmpegLink)...)
	args = append(args, ffmpegLink.URL)

	cmd := exec.CommandContext(ctx2, g.cfg.FFprobePath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		errOut := stderr.Bytes()
		if len(errOut) == 0 {
			errOut = out
		}
		if ctx2.Err() != nil {
			err = ctx2.Err()
		}
		return 0, ffmpegCommandError("ffprobe", err, errOut)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return 0, nil
	}
	var probe localMediaProbe
	if err := json.Unmarshal(out, &probe); err != nil {
		return 0, fmt.Errorf("ffprobe duration output: %w", err)
	}
	return preferredSourceDuration(probe), nil
}

// preferredSourceDuration avoids trusting a container timeline that was
// stretched by a late auxiliary stream. Stream duration measures the coded
// span itself, so the longest usable video stream is the best lightweight
// representation of the source video. Containers without a usable video
// duration retain the format-level fallback used previously.
func preferredSourceDuration(probe localMediaProbe) float64 {
	var videoDuration float64
	for _, stream := range probe.Streams {
		if !strings.EqualFold(stream.CodecType, "video") {
			continue
		}
		if duration := parseProbeDuration(stream.Duration); duration > videoDuration {
			videoDuration = duration
		}
	}
	if videoDuration > 0 {
		return videoDuration
	}
	formatDuration := parseProbeDuration(probe.Format.Duration)
	if formatDuration > 0 {
		return formatDuration
	}
	return 0
}

// --- 预览视频 ---

// Generate 拉取预览视频到本地临时文件，返回路径。
// 根据 Config.Segments 和视频时长决定是单段还是多段拼接。
func (g *Generator) Generate(ctx context.Context, link *drives.StreamLink, duration float64) (string, error) {
	return g.GenerateWithLinkRefresh(ctx, link, duration, nil)
}

func (g *Generator) GenerateWithLinkRefresh(ctx context.Context, first *drives.StreamLink, duration float64, refreshAfterFailure func(context.Context) (*drives.StreamLink, error)) (string, error) {
	task := g.forTask()
	path, err := task.generateSerialWithRefresh(ctx, duration, first, refreshAfterFailure)
	if task.fallbackToCPU(ctx, err) {
		return task.generateSerialWithRefresh(ctx, duration, first, refreshAfterFailure)
	}
	return path, err
}

type teaserSegmentCandidate struct {
	index int
	start float64
}

type teaserSegmentResult struct {
	teaserSegmentCandidate
	path string
	err  error
}

// generateSerialWithRefresh generates at most one teaser segment at a time.
// A healthy source link is reused for the whole task. When a concrete remote
// read failure occurs, the current segment gets one attempt with a refreshed
// link; a successful replacement then becomes the source for later segments.
func (g *Generator) generateSerialWithRefresh(
	ctx context.Context,
	duration float64,
	initialLink *drives.StreamLink,
	refreshAfterFailure func(context.Context) (*drives.StreamLink, error),
) (string, error) {
	if err := os.MkdirAll(g.cfg.LocalDir, 0o755); err != nil {
		return "", err
	}

	plan := buildTeaserPlan(g.cfg, duration)
	starts := plan.starts
	eachSec := plan.eachSec
	if len(starts) == 0 {
		return "", fmt.Errorf("video too short for %.0fs teaser segment", eachSec)
	}

	segmentCtx, cancelSegments := context.WithTimeout(ctx, g.segmentBudget)
	defer cancelSegments()

	generatedPaths := make([]string, 0, len(starts))
	success := false
	defer func() {
		if success {
			return
		}
		for _, p := range generatedPaths {
			_ = os.Remove(p)
		}
	}()

	candidates := teaserCandidateStarts(duration, starts, eachSec)
	targetSegments := len(starts)
	fadeSegments := targetSegments > 1
	requiredSegments := requiredTeaserSegments(duration, targetSegments, false)
	segmentResults := make([]teaserSegmentResult, 0, targetSegments)
	currentLink := initialLink
	var colorRepair sourceColorRepair
	nextCandidate := 0
	var lastErr error
segmentLoop:
	for nextCandidate < len(candidates) && len(segmentResults) < targetSegments {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if segmentCtx.Err() != nil {
			break
		}
		candidate := teaserSegmentCandidate{
			index: nextCandidate,
			start: candidates[nextCandidate],
		}
		nextCandidate++

		result := teaserSegmentResult{teaserSegmentCandidate: candidate}
		result.path, result.err = g.generateSingleSegment(
			segmentCtx,
			candidate.index,
			candidate.start,
			eachSec,
			fadeSegments,
			currentLink,
			&colorRepair,
		)
		if err := ctx.Err(); err != nil && result.err != nil {
			return "", err
		}
		if g.hardware != nil && hardwareFailure(result.err) {
			return "", result.err
		}
		if segmentCtx.Err() == nil && refreshAfterFailure != nil && directMediaLinkRefreshAllowed(result.err) {
			refreshed, refreshErr := refreshAfterFailure(segmentCtx)
			if refreshErr == nil && (refreshed == nil || strings.TrimSpace(refreshed.URL) == "") {
				refreshErr = fmt.Errorf("%w: empty refreshed direct link", drives.ErrGenerationStreamUnavailable)
			}
			switch {
			case refreshErr == nil:
				currentLink = refreshed
				result.path, result.err = g.generateSingleSegment(
					segmentCtx,
					candidate.index,
					candidate.start,
					eachSec,
					fadeSegments,
					currentLink,
					&colorRepair,
				)
			case !errors.Is(refreshErr, drives.ErrGenerationStreamUnavailable):
				if segmentCtx.Err() != nil && errors.Is(refreshErr, segmentCtx.Err()) {
					lastErr = refreshErr
					break segmentLoop
				}
				return "", refreshErr
			}
		}

		if result.err != nil {
			if g.hardware != nil && hardwareFailure(result.err) {
				return "", result.err
			}
			if !teaserSegmentFallbackAllowed(result.err) {
				return "", result.err
			}
			lastErr = result.err
			continue
		}
		generatedPaths = append(generatedPaths, result.path)
		segmentResults = append(segmentResults, result)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := segmentCtx.Err(); err != nil && len(segmentResults) < targetSegments {
		lastErr = fmt.Errorf("teaser segment budget exhausted: %w", err)
		applog.Warn(ctx, fmt.Sprintf("[preview] teaser segment budget exhausted segments=%d/%d duration=%.1fs",
			len(segmentResults), targetSegments, duration), lastErr, applog.Fields{Component: "preview", Stage: "segments"})
	}
	cancelSegments()
	if len(segmentResults) < requiredSegments {
		degradedRequired := requiredTeaserSegments(duration, targetSegments, true)
		if lastErr == nil || len(segmentResults) < degradedRequired {
			if lastErr != nil {
				return "", fmt.Errorf("only generated %d/%d teaser segments: %w", len(segmentResults), targetSegments, lastErr)
			}
			return "", fmt.Errorf("only generated %d/%d teaser segments", len(segmentResults), targetSegments)
		}
		applog.Warn(ctx, fmt.Sprintf("[preview] degraded teaser accepted segments=%d/%d duration=%.1fs",
			len(segmentResults), targetSegments, duration), lastErr, applog.Fields{Component: "preview", Stage: "segments"})
	}

	// Fallback candidates are attempted after the primary plan and are not
	// necessarily later on the source timeline. Restore timeline order before
	// writing the concat list.
	sort.SliceStable(segmentResults, func(i, j int) bool {
		if segmentResults[i].start == segmentResults[j].start {
			return segmentResults[i].index < segmentResults[j].index
		}
		return segmentResults[i].start < segmentResults[j].start
	})
	segmentPaths := make([]string, len(segmentResults))
	for i, result := range segmentResults {
		segmentPaths[i] = result.path
	}

	if len(segmentPaths) == 1 {
		success = true
		return segmentPaths[0], nil
	}

	// Finishing uses only local files and gets its own budget. Derive it from
	// the task context so exhausted remote reads do not cancel usable segments,
	// while stopping the task still cancels both stages.
	finishCtx, cancelFinish := context.WithTimeout(ctx, g.finishTimeout)
	defer cancelFinish()

	tmp, err := os.CreateTemp(g.cfg.LocalDir, "teaser-*.mp4")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	_ = os.Remove(tmpPath)

	list, err := os.CreateTemp(g.cfg.LocalDir, "teaser-concat-*.txt")
	if err != nil {
		return "", err
	}
	listPath := list.Name()
	for _, p := range segmentPaths {
		if _, err := fmt.Fprintf(list, "file '%s'\n", escapeConcatPath(p)); err != nil {
			list.Close()
			_ = os.Remove(listPath)
			return "", err
		}
	}
	if err := list.Close(); err != nil {
		_ = os.Remove(listPath)
		return "", err
	}
	defer os.Remove(listPath)

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-f", "concat",
		"-safe", "0",
		"-i", listPath,
		"-c", "copy",
		"-movflags", "+faststart",
		"-y", tmpPath,
	}
	out, err := exec.CommandContext(finishCtx, g.cfg.FFmpegPath, args...).CombinedOutput()
	if err != nil {
		_ = os.Remove(tmpPath)
		if finishCtx.Err() != nil {
			err = finishCtx.Err()
		}
		return "", ffmpegCommandError("ffmpeg concat", err, out)
	}
	if info, statErr := os.Stat(tmpPath); statErr != nil || info.Size() == 0 {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("ffmpeg concat produced empty file, stderr: %s", string(out))
	}
	if err := g.validateGeneratedTeaser(finishCtx, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}

	for _, p := range segmentPaths {
		_ = os.Remove(p)
	}
	success = true
	return tmpPath, nil
}

func requiredTeaserSegments(duration float64, targetSegments int, degraded bool) int {
	if targetSegments <= 0 {
		return 0
	}
	if duration > 0 && duration < 30 {
		return 1
	}
	// The normal path must produce the complete plan. Only after eligible
	// per-timestamp retries have failed may a damaged or sparsely encoded
	// source be accepted as an explicitly logged degraded teaser.
	if degraded && targetSegments > 2 {
		return 2
	}
	return targetSegments
}

func (g *Generator) generateSingleSegment(ctx context.Context, index int, start, eachSec float64, fade bool, link *drives.StreamLink, colorRepair *sourceColorRepair) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, teaserSegmentTimeout)
	defer cancel()

	ffmpegLink, cleanup, err := prepareFFmpegLink(ctx, link)
	if err != nil {
		return "", err
	}
	defer cleanup()

	seg, err := os.CreateTemp(g.cfg.LocalDir, fmt.Sprintf("teaser-seg-%d-*.mp4", index))
	if err != nil {
		return "", err
	}
	segPath := seg.Name()
	seg.Close()

	filter := g.segmentFilter(fade, eachSec)

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-filter_threads", strconv.Itoa(g.cfg.FFmpegThreads),
		"-filter_complex_threads", strconv.Itoa(g.cfg.FFmpegThreads),
		"-threads", strconv.Itoa(g.cfg.FFmpegThreads),
	}
	args = append(args,
		"-ss", fmt.Sprintf("%.2f", start),
		"-t", fmt.Sprintf("%.2f", eachSec),
	)
	args = append(args, g.hardware.inputOptions()...)
	outputArgs := []string{
		"-threads", strconv.Itoa(g.cfg.FFmpegThreads),
		"-an",
		"-vf", filter,
	}
	outputArgs = append(outputArgs, g.encoderOptions()...)
	outputArgs = append(outputArgs,
		"-movflags", "+faststart",
		"-y", segPath,
	)
	out, err := g.runMediaCommand(ctx, link, ffmpegLink, args, outputArgs, colorRepair)
	if err != nil {
		_ = os.Remove(segPath)
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return "", g.processingError(ffmpegCommandError("ffmpeg segment", err, out))
	}
	if info, statErr := os.Stat(segPath); statErr != nil || info.Size() == 0 {
		_ = os.Remove(segPath)
		return "", g.processingError(fmt.Errorf("ffmpeg segment produced empty file, stderr: %s", string(out)))
	}
	if err := g.validateGeneratedTeaser(ctx, segPath); err != nil {
		_ = os.Remove(segPath)
		return "", g.processingError(err)
	}
	return segPath, nil
}

func teaserSegmentFallbackAllowed(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := drives.RateLimitRetryAfter(err); ok {
		return false
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "server returned 403") ||
		strings.Contains(text, "403 forbidden") ||
		strings.Contains(text, "server returned 405") ||
		strings.Contains(text, "405 method") ||
		strings.Contains(text, "access denied") ||
		strings.Contains(text, "request has been blocked") ||
		strings.Contains(text, "访问被阻断") {
		return false
	}
	if deterministicMediaInputError(text) && !transientRemoteMediaReadText(text) {
		return false
	}
	return strings.Contains(text, "generated teaser has no video stream") ||
		strings.Contains(text, "generated teaser has invalid duration") ||
		strings.Contains(text, "generated teaser is empty") ||
		strings.Contains(text, "produced empty file") ||
		strings.Contains(text, "ffmpeg segment:") ||
		strings.Contains(text, "ffprobe teaser:")
}

func deterministicMediaInputError(text string) bool {
	text = strings.ToLower(text)
	// Color metadata can change between coded segments. A color failure at
	// one timestamp does not establish that the entire source is unusable.
	return strings.Contains(text, "invalid data found when processing input") ||
		strings.Contains(text, "could not find codec parameters") ||
		strings.Contains(text, "unknown format")
}

func transientRemoteMediaReadText(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "partial file") ||
		strings.Contains(text, "after eof") ||
		strings.Contains(text, "end of file") ||
		strings.Contains(text, "connection reset") ||
		strings.Contains(text, "connection timed out") ||
		strings.Contains(text, "i/o timeout") ||
		strings.Contains(text, "tls handshake timeout") ||
		strings.Contains(text, "context deadline exceeded") ||
		strings.Contains(text, "signal: killed")
}

// directMediaLinkRefreshAllowed identifies failures for which a newly signed
// ordinary download URL can materially change the result. Container/codec
// errors are deliberately excluded: refreshing those links only repeats a
// deterministic failure and increases pressure on the provider API.
func directMediaLinkRefreshAllowed(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	if drives.ErrorMentionsHTTPStatus(err,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusMethodNotAllowed,
		http.StatusGone,
		http.StatusRequestedRangeNotSatisfiable,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	) || transientRemoteMediaReadText(text) || strings.Contains(text, "moov atom not found") {
		return true
	}
	if deterministicMediaInputError(text) {
		return false
	}
	return false
}

type localMediaProbe struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		Duration  string `json:"duration"`
	} `json:"streams"`
	Format struct {
		Duration  string `json:"duration"`
		StartTime string `json:"start_time"`
	} `json:"format"`
}

func (g *Generator) validateGeneratedTeaser(ctx context.Context, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return errors.New("generated teaser is empty")
	}

	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := []string{
		"-v", "error",
		"-show_entries", "stream=codec_type,duration:format=start_time,duration",
		"-of", "json",
		path,
	}
	out, err := exec.CommandContext(ctx2, g.cfg.FFprobePath, args...).CombinedOutput()
	if err != nil {
		if ctx2.Err() != nil {
			err = ctx2.Err()
		}
		return ffmpegCommandError("ffprobe teaser", err, out)
	}

	var probe localMediaProbe
	if err := json.Unmarshal(out, &probe); err != nil {
		return fmt.Errorf("ffprobe teaser output: %w", err)
	}

	startTime := parseProbeDuration(probe.Format.StartTime)
	if startTime > 0.5 {
		return fmt.Errorf("generated teaser starts too late %.3fs", startTime)
	}

	duration := parseProbeDuration(probe.Format.Duration)
	hasVideo := false
	for _, stream := range probe.Streams {
		if stream.CodecType == "video" {
			hasVideo = true
		}
		if d := parseProbeDuration(stream.Duration); d > duration {
			duration = d
		}
	}
	if !hasVideo {
		return errors.New("generated teaser has no video stream")
	}
	if duration <= 0.01 {
		return fmt.Errorf("generated teaser has invalid duration %.3fs", duration)
	}
	return nil
}

func parseProbeDuration(raw string) float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "N/A" {
		return 0
	}
	d, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(d) || math.IsInf(d, 0) {
		return 0
	}
	return d
}

func escapeConcatPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return strings.ReplaceAll(path, "'", "'\\''")
}

func prepareFFmpegLink(ctx context.Context, link *drives.StreamLink) (*drives.StreamLink, func(), error) {
	if link == nil {
		return nil, func() {}, errors.New("missing stream link")
	}
	if !shouldProxyFFmpegLink(link) {
		return link, func() {}, nil
	}
	return startLocalFFmpegProxy(ctx, link)
}

func shouldProxyFFmpegLink(link *drives.StreamLink) bool {
	if link == nil {
		return false
	}
	raw := strings.ToLower(link.URL)
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return false
	}
	// FFmpeg forwards custom Authorization/Cookie headers to redirect targets.
	// Keep protected first-hop links behind our loopback proxy so redirects are
	// followed with the shared cross-origin credential policy instead.
	if link.PassThroughRedirects {
		return true
	}
	if streamhttp.HasSensitiveHeaders(link.Headers) {
		return true
	}
	if strings.Contains(raw, "115cdn") {
		return true
	}
	return strings.Contains(strings.ToLower(link.Headers.Get("User-Agent")), "115")
}

func startLocalFFmpegProxy(ctx context.Context, link *drives.StreamLink) (*drives.StreamLink, func(), error) {
	localURL, cleanup, err := streamhttp.StartLoopbackRelay(ctx, link.URL, link.Headers)
	if err != nil {
		return nil, nil, err
	}

	proxied := *link
	proxied.URL = localURL
	proxied.Headers = nil
	proxied.PassThroughRedirects = false
	return &proxied, cleanup, nil
}

func ffmpegHTTPInputOptions(link *drives.StreamLink) []string {
	if link == nil {
		return nil
	}
	var args []string
	if ua := strings.TrimSpace(link.Headers.Get("User-Agent")); ua != "" {
		args = append(args, "-user_agent", ua)
	}
	if h := buildHeaders(link.Headers); h != "" {
		args = append(args, "-headers", h)
	}
	return args
}

func ffmpegCommandError(tool string, err error, output []byte) error {
	wrapped := fmt.Errorf("%s: %w, stderr: %s", tool, err, redactURLs(string(output)))
	if ffmpegOutputLooksRateLimited(output) || drives.ErrorMentionsHTTPStatus(err, http.StatusTooManyRequests) {
		return &drives.RateLimitError{
			Provider: "media source",
			Err:      wrapped,
		}
	}
	return wrapped
}

func redactURLs(text string) string {
	fields := strings.Fields(text)
	for i, field := range fields {
		if strings.HasPrefix(field, "http://") || strings.HasPrefix(field, "https://") {
			suffix := ""
			for len(field) > 0 {
				last := field[len(field)-1]
				if last != '.' && last != ',' && last != ';' && last != ')' {
					break
				}
				suffix = string(last) + suffix
				field = field[:len(field)-1]
			}
			fields[i] = "https://<redacted>" + suffix
		}
	}
	return strings.Join(fields, " ")
}

func ffmpegOutputLooksRateLimited(output []byte) bool {
	return drives.TextMentionsHTTPStatus(string(output), http.StatusTooManyRequests)
}

// --- 本地落盘 ---

// MoveToLocal 把临时文件改名到稳定位置，返回最终路径
func (g *Generator) MoveToLocal(tmpPath, videoID string) (string, error) {
	if err := os.MkdirAll(g.cfg.LocalDir, 0o755); err != nil {
		return "", err
	}
	dst := mediaasset.PreviewPath(g.cfg.LocalDir, videoID)
	if err := os.Rename(tmpPath, dst); err != nil {
		// 跨盘 rename 可能失败，fallback 到 copy
		partPath := dst + ".part"
		_ = os.Remove(partPath)
		if cerr := copyFile(tmpPath, partPath); cerr != nil {
			return "", cerr
		}
		if cerr := replaceFile(partPath, dst); cerr != nil {
			_ = os.Remove(partPath)
			return "", cerr
		}
		_ = os.Remove(tmpPath)
	}
	return dst, nil
}

func replaceFile(source, destination string) error {
	if err := os.Rename(source, destination); err == nil {
		return nil
	}
	// Windows cannot atomically rename over an existing destination. Removing
	// the old directory entry still preserves any hard-linked backup snapshot.
	if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(source, destination)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// --- Worker ---

type Worker struct {
	Gen     TeaserGenerator
	Catalog *catalog.Catalog
	Drive   drives.Drive
	// Enabled reads the shared runtime policy at admission and task start.
	// Nil enables standalone workers. Already running tasks may finish.
	Enabled func() bool
	// OnPreviewReady lets the application schedule dependent local-asset work
	// without coupling this worker to a concrete thumbnail worker.
	OnPreviewReady func(*catalog.Video)
	// OnStatusChanged runs after a queue, activity or cooldown transition.
	OnStatusChanged func(bool)
	// TaskGuard holds application-level task admission for the complete provider
	// operation. A nil release means this worker belongs to a retired runtime
	// generation and the queued item must remain pending for its replacement.
	TaskGuard func() func()
	// Limiter is shared by all preview workers in the application.
	Limiter *tasklimit.Limiter
	ch      chan *catalog.Video
	queue   videoQueue

	RateLimitCooldown time.Duration
	rateLimit         rateLimitState
	activities        taskActivities
}

func NewWorker(gen TeaserGenerator, cat *catalog.Catalog, drv drives.Drive) *Worker {
	return &Worker{
		Gen:     gen,
		Catalog: cat,
		Drive:   drv,
		// Standalone workers default to one slot. The application injects its
		// shared preview budget before Run starts.
		Limiter: tasklimit.New(1),
		ch:      make(chan *catalog.Video, defaultWorkerQueueSize),
	}
}

func (w *Worker) Enqueue(v *catalog.Video) bool {
	defer w.notifyStatus(false)
	if v == nil || !w.enabled() {
		return false
	}
	if !w.queue.reserve(v) {
		return true
	}
	select {
	case w.ch <- v:
		return true
	default:
		w.queue.release(v)
		return false
	}
}

func (w *Worker) EnqueueBlocking(ctx context.Context, v *catalog.Video) bool {
	defer w.notifyStatus(false)
	if v == nil || !w.enabled() {
		return false
	}
	if !w.queue.reserve(v) {
		return true
	}
	select {
	case w.ch <- v:
		return true
	case <-ctx.Done():
		w.queue.release(v)
		return false
	}
}

func (w *Worker) enabled() bool {
	return w.Enabled == nil || w.Enabled()
}

type ThumbWorker struct {
	OnStatusChanged func(bool)
	Gen             ThumbnailGenerator
	Catalog         *catalog.Catalog
	Drive           drives.Drive
	TaskGuard       func() func()
	// Limiter is shared by all thumbnail workers in the application.
	Limiter    *tasklimit.Limiter
	ch         chan *catalog.Video
	durationCh chan durationTask
	queue      videoQueue
	// Metadata reservations must not suppress a later cover-generation request.
	durationQueue videoQueue

	// followUps preserves a state-change notification that arrives while the
	// same video's thumbnail is already being processed. A plain deduplicating
	// enqueue cannot express this: it would report success while silently folding
	// the notification into the attempt that started before the state changed.
	followUpMu        sync.Mutex
	activeThumbnailID string
	followUps         map[string]*catalog.Video

	RateLimitCooldown time.Duration
	rateLimit         rateLimitState
	activity          taskActivity
}

const (
	defaultTransientMediaCooldown         = 5 * time.Minute
	defaultGenerationRateLimitCooldown    = 5 * time.Minute
	defaultThumbTransientMediaMaxFailures = 3
	defaultWorkerQueueSize                = 10000
)

type rateLimitState struct {
	mu          sync.Mutex
	until       time.Time
	lastSkipLog time.Time
}

type TaskStatus struct {
	State         string
	CurrentTitle  string
	QueueLength   int
	CooldownUntil time.Time
}

type taskActivity struct {
	mu           sync.Mutex
	currentID    string
	currentTitle string
}

type taskActivities struct {
	mu     sync.Mutex
	titles map[string]string
}

type videoQueue struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

func (q *videoQueue) reserve(v *catalog.Video) bool {
	if v == nil {
		return false
	}
	if v.ID == "" {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ids == nil {
		q.ids = make(map[string]struct{})
	}
	if _, ok := q.ids[v.ID]; ok {
		return false
	}
	q.ids[v.ID] = struct{}{}
	return true
}

func (q *videoQueue) release(v *catalog.Video) {
	if v == nil || v.ID == "" {
		return
	}
	q.mu.Lock()
	delete(q.ids, v.ID)
	q.mu.Unlock()
}

func (q *videoQueue) idsSnapshot() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(q.ids))
	for id := range q.ids {
		out = append(out, id)
	}
	return out
}

func (q *videoQueue) lengthExcluding(currentID string) int {
	if currentID == "" {
		return q.lengthExcludingIDs(nil)
	}
	return q.lengthExcludingIDs([]string{currentID})
}

func (q *videoQueue) lengthExcludingIDs(activeIDs []string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.ids)
	for _, activeID := range activeIDs {
		if _, ok := q.ids[activeID]; ok {
			n--
		}
	}
	if n < 0 {
		return 0
	}
	return n
}

func (a *taskActivity) start(v *catalog.Video) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if v == nil {
		a.currentID = ""
		a.currentTitle = ""
		return
	}
	a.currentID = v.ID
	a.currentTitle = v.Title
}

func (a *taskActivity) done() {
	a.mu.Lock()
	a.currentID = ""
	a.currentTitle = ""
	a.mu.Unlock()
}

func (a *taskActivity) current() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.currentID, a.currentTitle
}

func (a *taskActivities) start(v *catalog.Video) {
	if v == nil || v.ID == "" {
		return
	}
	a.mu.Lock()
	if a.titles == nil {
		a.titles = make(map[string]string)
	}
	a.titles[v.ID] = v.Title
	a.mu.Unlock()
}

func (a *taskActivities) done(videoID string) {
	if videoID == "" {
		return
	}
	a.mu.Lock()
	delete(a.titles, videoID)
	a.mu.Unlock()
}

func (a *taskActivities) snapshot() (string, []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.titles) == 0 {
		return "", nil
	}
	activeIDs := make([]string, 0, len(a.titles))
	currentTitle := ""
	for id, title := range a.titles {
		activeIDs = append(activeIDs, id)
		if currentTitle == "" {
			currentTitle = title
		}
	}
	return currentTitle, activeIDs
}

func (s *rateLimitState) active(now time.Time) (time.Time, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.until.IsZero() || !now.Before(s.until) {
		return time.Time{}, false, false
	}
	shouldLog := s.lastSkipLog.IsZero() || now.Sub(s.lastSkipLog) >= 5*time.Minute
	if shouldLog {
		s.lastSkipLog = now
	}
	return s.until, true, shouldLog
}

func (s *rateLimitState) pause(now time.Time, d time.Duration) time.Time {
	if d <= 0 {
		d = defaultTransientMediaCooldown
	}
	until := now.Add(d)
	s.mu.Lock()
	if until.After(s.until) {
		s.until = until
	} else {
		until = s.until
	}
	s.lastSkipLog = time.Time{}
	s.mu.Unlock()
	return until
}

func (s *rateLimitState) coolingUntil(now time.Time) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.until.IsZero() || !now.Before(s.until) {
		return time.Time{}, false
	}
	return s.until, true
}

func NewThumbWorker(gen ThumbnailGenerator, cat *catalog.Catalog, drv drives.Drive) *ThumbWorker {
	return &ThumbWorker{
		Gen:        gen,
		Catalog:    cat,
		Drive:      drv,
		ch:         make(chan *catalog.Video, defaultWorkerQueueSize),
		durationCh: make(chan durationTask, defaultWorkerQueueSize),
	}
}

func (w *ThumbWorker) Enqueue(v *catalog.Video) bool {
	defer w.notifyStatus(false)
	if v == nil {
		return false
	}
	if !w.queue.reserve(v) {
		return true
	}
	select {
	case w.ch <- v:
		return true
	default:
		w.queue.release(v)
		return false
	}
}

func (w *ThumbWorker) EnqueueBlocking(ctx context.Context, v *catalog.Video) bool {
	defer w.notifyStatus(false)
	if v == nil {
		return false
	}
	if !w.queue.reserve(v) {
		return true
	}
	select {
	case w.ch <- v:
		return true
	case <-ctx.Done():
		w.queue.release(v)
		return false
	}
}

// EnqueueFollowUp schedules thumbnail work after the currently running
// attempt when necessary. It is intended for catalog state changes (for
// example a preview becoming ready) that grant a failed thumbnail a new
// attempt. Ordinary duplicate queue submissions should continue to use
// Enqueue or EnqueueBlocking.
func (w *ThumbWorker) EnqueueFollowUp(v *catalog.Video) bool {
	if v == nil {
		return false
	}
	w.followUpMu.Lock()
	if v.ID != "" && w.activeThumbnailID == v.ID {
		if w.followUps == nil {
			w.followUps = make(map[string]*catalog.Video)
		}
		copy := *v
		w.followUps[v.ID] = &copy
		w.followUpMu.Unlock()
		return true
	}
	w.followUpMu.Unlock()
	return w.Enqueue(v)
}

func (w *Worker) Status() TaskStatus {
	if w == nil {
		return TaskStatus{State: "idle"}
	}
	currentTitle, activeIDs := w.activities.snapshot()
	return taskStatus(currentTitle, len(activeIDs) > 0, &w.rateLimit, w.queue.lengthExcludingIDs(activeIDs))
}

func (w *Worker) ActiveVideoIDs() []string {
	if w == nil {
		return nil
	}
	return w.queue.idsSnapshot()
}

func (w *ThumbWorker) Status() TaskStatus {
	if w == nil {
		return TaskStatus{State: "idle"}
	}
	currentID, currentTitle := w.activity.current()
	queued := w.queue.lengthExcluding("") + w.durationQueue.lengthExcluding("")
	if currentID != "" {
		queued = max(0, queued-1)
	}
	return taskStatus(currentTitle, currentID != "", &w.rateLimit, queued)
}

// WaitIdle 阻塞直到 worker 队列为空且当前没有正在处理的任务。
//
// "队列空"的判定基于 videoQueue —— 它在 Enqueue 时 reserve、processQueued
// defer 里 release，因此 lengthExcluding("") == 0 同时覆盖：
//   - channel 中尚未被消费的项
//   - 当前正在 processQueued 的项（哪怕处于 cooldown 等待中）
//
// 调用方应通过 ctx 传入超时 / cancel；ctx 结束时返回 ctx.Err()。
// 200ms 轮询：开销极低，凌晨流水线对几百毫秒级响应延迟不敏感。
func (w *Worker) WaitIdle(ctx context.Context) error {
	if w == nil {
		return nil
	}
	return waitQueueIdle(ctx, &w.queue)
}

// WaitIdle 见 Worker.WaitIdle 注释。
func (w *ThumbWorker) WaitIdle(ctx context.Context) error {
	if w == nil {
		return nil
	}
	return waitQueueIdle(ctx, &w.queue, &w.durationQueue)
}

func waitQueueIdle(ctx context.Context, queues ...*videoQueue) error {
	idle := func() bool {
		for _, queue := range queues {
			if queue.lengthExcluding("") != 0 {
				return false
			}
		}
		return true
	}
	if idle() {
		return nil
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if idle() {
				return nil
			}
		}
	}
}

func taskStatus(currentTitle string, active bool, rateLimit *rateLimitState, queueLength int) TaskStatus {
	if queueLength < 0 {
		queueLength = 0
	}
	status := TaskStatus{
		State:       "idle",
		QueueLength: queueLength,
	}
	if until, ok := rateLimit.coolingUntil(time.Now()); ok {
		status.State = "cooling"
		status.CooldownUntil = until
		return status
	}
	if active {
		status.State = "generating"
		status.CurrentTitle = currentTitle
		return status
	}
	if queueLength > 0 {
		status.State = "queued"
	}
	return status
}

// Run 阻塞运行直到 ctx 取消
func (w *Worker) Run(ctx context.Context) {
	var tasks sync.WaitGroup
	defer tasks.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case v := <-w.ch:
			// Each drive has at most one task waiting for a global slot. FIFO
			// admission lets other drives advance without a per-drive limit or
			// a goroutine for every queued video.
			run := w.prepareQueued(ctx, v)
			if run == nil {
				continue
			}
			tasks.Add(1)
			go func() {
				defer tasks.Done()
				run()
			}()
			// Pace this drive's dispatch without occupying a global slot after
			// processing or accumulating goroutines that only wait on a timer.
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
}

// Run 阻塞运行直到 ctx 取消
func (w *ThumbWorker) Run(ctx context.Context) {
	for {
		var video *catalog.Video
		durationOnly := false
		var durationContext context.Context
		// Drain missing thumbnails before dispatching metadata-only work.
		select {
		case <-ctx.Done():
			return
		case video = <-w.ch:
		default:
			select {
			case <-ctx.Done():
				return
			case video = <-w.ch:
			case task := <-w.durationCh:
				video = task.video
				durationOnly = true
				durationContext = task.ctx
			}
		}
		if durationOnly {
			taskCtx, cancel := context.WithCancel(durationContext)
			stop := context.AfterFunc(ctx, cancel)
			w.processQueuedTask(taskCtx, video, true)
			canceled := taskCtx.Err() != nil
			stop()
			cancel()
			if canceled {
				continue
			}
		} else {
			w.processQueuedTask(ctx, video, false)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (w *Worker) processQueued(ctx context.Context, v *catalog.Video) {
	if run := w.prepareQueued(ctx, v); run != nil {
		run()
	}
}

// prepareQueued waits in the dispatcher and transfers admission/resource
// ownership to the returned task. No processing goroutine exists before this
// succeeds, even when a drive has thousands of queued videos.
func (w *Worker) prepareQueued(ctx context.Context, v *catalog.Video) func() {
	if v == nil {
		return nil
	}
	prepared := false
	taskRelease := func() {}
	defer func() {
		if !prepared {
			w.queue.release(v)
			taskRelease()
			w.notifyStatus(true)
		}
	}()
	if w.Catalog == nil || v.ID == "" || ctx.Err() != nil || !w.enabled() {
		return nil
	}
	ctx = applog.WithFields(applog.NewTask(ctx, "preview", generationDriveID(w.Drive)), applog.Fields{VideoID: v.ID, FileID: v.FileID})
	if w.TaskGuard != nil {
		release := w.TaskGuard()
		if release == nil {
			return nil
		}
		taskRelease = release
	}
	current, err := w.Catalog.GetVideo(ctx, v.ID)
	if err != nil {
		if ctx.Err() == nil {
			applog.Error(ctx, "Read queued video failed", err, applog.Fields{Stage: "lookup"})
		}
		return nil
	}
	if current.Hidden {
		return nil
	}
	release, ok := acquireGenerationSlot(ctx, w.Limiter, &w.rateLimit, "preview", w.Drive)
	if !ok {
		return nil
	}
	prepared = true
	return func() {
		defer taskRelease()
		defer release()
		if ctx.Err() != nil || !w.enabled() {
			w.queue.release(v)
			return
		}
		w.activities.start(current)
		w.notifyStatus(true)
		defer func() { w.activities.done(current.ID); w.notifyStatus(true) }()
		retry := w.process(ctx, current)
		release()
		// Release before requeueing because videoQueue deduplicates reserved
		// IDs. Keep activity visible throughout the transition for admission.
		w.queue.release(v)
		if retry && ctx.Err() == nil {
			w.EnqueueBlocking(ctx, current)
		}
	}
}

func (w *ThumbWorker) processQueued(ctx context.Context, v *catalog.Video) {
	w.processQueuedTask(ctx, v, false)
}

func (w *ThumbWorker) processQueuedTask(ctx context.Context, v *catalog.Video, durationOnly bool) {
	if v == nil {
		return
	}
	ctx = applog.WithFields(applog.NewTask(ctx, "thumb", generationDriveID(w.Drive)), applog.Fields{VideoID: v.ID, FileID: v.FileID})
	if !durationOnly {
		w.followUpMu.Lock()
		w.activeThumbnailID = v.ID
		w.followUpMu.Unlock()
	}

	retry := false
	activityStarted := false
	var taskRelease func()
	defer func() {
		w.finishQueued(ctx, v, retry, durationOnly)
		if activityStarted {
			w.activity.done()
		}
		if taskRelease != nil {
			taskRelease()
		}
		w.notifyStatus(true)
	}()

	if w.TaskGuard != nil {
		taskRelease = w.TaskGuard()
		if taskRelease == nil {
			return
		}
	}
	if err := ctx.Err(); err != nil {
		return
	}
	if release, ok := acquireGenerationSlot(ctx, w.Limiter, &w.rateLimit, "thumb", w.Drive); ok {
		w.activity.start(v)
		w.notifyStatus(true)
		activityStarted = true
		if durationOnly {
			w.processDuration(ctx, v)
		} else {
			retry = w.process(ctx, v)
		}
		release()
	}
}

func (w *ThumbWorker) finishQueued(ctx context.Context, v *catalog.Video, retry, durationOnly bool) {
	if durationOnly {
		// Cover notifications enter the independent thumbnail queue immediately,
		// so ending the metadata phase cannot discard them.
		w.durationQueue.release(v)
		return
	}
	// Keep activity non-idle across release-and-requeue. Configuration admission
	// checks worker status while holding its queue gate; an idle gap here could
	// otherwise let a runtime update start just before this retry is queued.
	w.followUpMu.Lock()
	followUp := w.followUps[v.ID]
	delete(w.followUps, v.ID)
	if w.activeThumbnailID == v.ID {
		w.activeThumbnailID = ""
	}
	w.queue.release(v)
	if ctx.Err() == nil && (retry || followUp != nil) {
		if followUp == nil {
			followUp = v
		}
		w.EnqueueBlocking(ctx, followUp)
	}
	w.followUpMu.Unlock()
}

// Cooldown can begin while another task waits for the global budget. Recheck
// after acquisition and release immediately if this drive must still wait.
func acquireGenerationSlot(ctx context.Context, limiter *tasklimit.Limiter, state *rateLimitState, label string, drive drives.Drive) (func(), bool) {
	for waitForRateLimitCooldown(ctx, state, label, drive) {
		release, err := limiter.Acquire(ctx)
		if err != nil {
			return nil, false
		}
		if _, cooling := state.coolingUntil(time.Now()); !cooling {
			return release, true
		}
		release()
	}
	return nil, false
}

func waitForRateLimitCooldown(ctx context.Context, state *rateLimitState, label string, drive drives.Drive) bool {
	driveID := ""
	if drive != nil {
		driveID = drive.ID()
	}
	for {
		until, ok := state.coolingUntil(time.Now())
		if !ok {
			return true
		}
		wait := time.Until(until)
		if wait <= 0 {
			continue
		}
		log.Printf("[%s] drive=%s cooling down until=%s; wait before next task", label, driveID, until.Format(time.RFC3339))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

func (w *Worker) skipIfRateLimited(v *catalog.Video) bool {
	until, ok, shouldLog := w.rateLimit.active(time.Now())
	if !ok {
		return false
	}
	if shouldLog {
		log.Printf("[preview] drive=%s rate-limited until=%s; skip queued videos and keep them pending", w.Drive.ID(), until.Format(time.RFC3339))
	}
	return true
}

func (w *Worker) pauseForRateLimit(err error, step, title string) bool {
	wait, ok := drives.RateLimitRetryAfter(err)
	if !ok {
		return false
	}
	if wait <= 0 {
		wait = w.RateLimitCooldown
		if wait <= 0 {
			wait = defaultGenerationRateLimitCooldown
		}
	}
	until := w.rateLimit.pause(time.Now(), wait)
	w.notifyStatus(true)
	log.Printf("[preview] drive=%s rate-limited until=%s step=%s video=%s: %v", w.Drive.ID(), until.Format(time.RFC3339), step, title, err)
	return true
}

func (w *Worker) pauseForRecoverableError(err error, step, title string) bool {
	if w.pauseForRateLimit(err, step, title) {
		return true
	}
	if !driveErrorShouldCooldown(w.Drive, err) {
		return false
	}
	until := w.rateLimit.pause(time.Now(), w.RateLimitCooldown)
	w.notifyStatus(true)
	log.Printf("[preview] drive=%s transient media source error until=%s step=%s video=%s: %v", w.Drive.ID(), until.Format(time.RFC3339), step, title, err)
	return true
}

func (w *ThumbWorker) skipIfRateLimited(v *catalog.Video) bool {
	until, ok, shouldLog := w.rateLimit.active(time.Now())
	if !ok {
		return false
	}
	if shouldLog {
		log.Printf("[thumb] drive=%s rate-limited until=%s; skip queued thumbnails and keep them pending", w.Drive.ID(), until.Format(time.RFC3339))
	}
	return true
}

// pauseForProviderError shares provider cooldowns with metadata work without
// consuming thumbnail retries or deciding whether to requeue the current task.
func (w *ThumbWorker) pauseForProviderError(err error, step, title string) bool {
	wait, limited := drives.RateLimitRetryAfter(err)
	if !limited && !driveErrorShouldCooldown(w.Drive, err) {
		return false
	}
	if wait <= 0 {
		wait = w.RateLimitCooldown
		if wait <= 0 {
			wait = defaultGenerationRateLimitCooldown
		}
	}
	until := w.rateLimit.pause(time.Now(), wait)
	w.notifyStatus(true)
	log.Printf("[thumb] drive=%s provider error cooldown until=%s step=%s video=%s: %v", generationDriveID(w.Drive), until.Format(time.RFC3339), step, title, err)
	return true
}

func (w *ThumbWorker) pauseForRecoverableError(ctx context.Context, v *catalog.Video, err error, step string) bool {
	title := ""
	videoID := ""
	if v != nil {
		title = v.Title
		videoID = v.ID
	}
	if isRateLimitError(err) {
		return w.pauseForProviderError(err, step, title)
	}
	if !driveErrorShouldCooldown(w.Drive, err) {
		return false
	}
	failures := 1
	if w.Catalog != nil && videoID != "" {
		count, countErr := w.Catalog.IncrementThumbnailFailures(ctx, videoID)
		if countErr != nil {
			log.Printf("[thumb] drive=%s transient media source error count failed step=%s video=%s: %v", w.Drive.ID(), step, title, countErr)
		} else {
			failures = count
		}
	}
	if failures >= defaultThumbTransientMediaMaxFailures {
		log.Printf("[thumb] drive=%s transient media source error reached retry limit failures=%d/%d step=%s video=%s: %v", w.Drive.ID(), failures, defaultThumbTransientMediaMaxFailures, step, title, err)
		return false
	}
	log.Printf("[thumb] drive=%s retry transient media source error failures=%d/%d step=%s video=%s", w.Drive.ID(), failures, defaultThumbTransientMediaMaxFailures, step, title)
	return w.pauseForProviderError(err, step, title)
}

func driveErrorShouldCooldown(d drives.Drive, err error) bool {
	if d == nil || err == nil {
		return false
	}
	switch d.Kind() {
	case "p115":
		return drives.ErrorMentionsHTTPStatus(err, http.StatusMethodNotAllowed, http.StatusTooManyRequests)
	case "pikpak":
		return drives.ErrorMentionsHTTPStatus(err, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 509)
	case "p123":
		return drives.ErrorMentionsHTTPStatus(err, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout)
	case "wopan":
		return drives.ErrorMentionsHTTPStatus(err, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 509)
	case "guangyapan":
		return drives.ErrorMentionsHTTPStatus(err, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 509)
	case "googledrive":
		return drives.ErrorMentionsHTTPStatus(err, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout)
	}
	return false
}

func (w *ThumbWorker) process(ctx context.Context, v *catalog.Video) bool {
	if w.skipIfRateLimited(v) {
		return false
	}
	if w.Catalog == nil || v == nil || v.ID == "" {
		return false
	}
	loaded, err := w.Catalog.GetVideo(ctx, v.ID)
	if err != nil || loaded.Hidden {
		return false
	}
	v = loaded
	if v.ThumbnailURL != "" {
		return false
	}
	_ = w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{ThumbnailStatus: "pending"})
	link, optimized, err := w.streamLink(ctx, v)
	if err != nil {
		if w.pauseForRecoverableError(ctx, v, err, "streamURL") {
			return true
		}
		applog.Error(ctx, "Thumbnail stream link failed: "+v.Title, err, applog.Fields{Component: "thumb", DriveID: generationDriveID(w.Drive), VideoID: v.ID, FileID: v.FileID, Stage: "streamURL"})
		_ = w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{ThumbnailStatus: "failed"})
		return false
	}
	if w.probeDuration(ctx, v, link) {
		return true
	}

	if err := w.generateThumbnailWithFallback(ctx, v, link, optimized); err != nil {
		if w.pauseForRecoverableError(ctx, v, err, "generate") {
			return true
		}
		applog.Error(ctx, "Thumbnail generation failed: "+v.Title, err, applog.Fields{Component: "thumb", DriveID: generationDriveID(w.Drive), VideoID: v.ID, FileID: v.FileID, Stage: "generate"})
		_ = w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{ThumbnailStatus: "failed"})
		return false
	}
	return false
}

func (w *ThumbWorker) streamLink(ctx context.Context, v *catalog.Video) (*drives.StreamLink, bool, error) {
	return preferredGenerationStreamLink(ctx, w.Drive, v.FileID)
}

func (w *ThumbWorker) generateThumbnailFromLink(ctx context.Context, v *catalog.Video, link *drives.StreamLink, duration float64) error {
	persistence.RLock()
	defer persistence.RUnlock()
	local, err := w.Gen.GenerateThumbnail(ctx, link, v.ID, duration)
	if err != nil {
		return err
	}
	if err := w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{
		ThumbnailURL:    "/p/thumb/" + v.ID,
		ThumbnailStatus: "ready",
	}); err != nil {
		_ = os.Remove(local)
		applog.Error(ctx, "Save thumbnail metadata failed: "+v.Title, err, applog.Fields{Component: "thumb", DriveID: generationDriveID(w.Drive), VideoID: v.ID, Stage: "save"})
		return nil
	}
	applog.Info(ctx, "Thumbnail ready: "+v.Title, applog.Fields{Component: "thumb", DriveID: generationDriveID(w.Drive), VideoID: v.ID})
	return nil
}

func (w *ThumbWorker) generateThumbnailWithFallback(ctx context.Context, v *catalog.Video, link *drives.StreamLink, optimized bool) error {
	duration := float64(v.DurationSeconds)
	err := w.generateThumbnailFromLink(ctx, v, link, duration)
	if err == nil {
		return nil
	}
	if !optimized {
		return w.retryThumbnailWithFreshOriginal(ctx, v, err, duration)
	}
	if generationStreamForbidden(err) {
		refreshed, refreshErr := refreshGenerationStreamLink(ctx, w.Drive, v.FileID)
		switch {
		case refreshErr == nil:
			err = w.generateThumbnailFromLink(ctx, v, refreshed, duration)
			if err == nil || (isRateLimitError(err) && !generationStreamForbidden(err)) {
				return err
			}
		case !errors.Is(refreshErr, drives.ErrGenerationStreamUnavailable):
			return refreshErr
		}
	}
	if isRateLimitError(err) && !generationStreamForbidden(err) {
		return err
	}
	original, originalErr := w.Drive.StreamURL(ctx, v.FileID)
	if originalErr != nil {
		return originalErr
	}
	err = w.generateThumbnailFromLink(ctx, v, original, duration)
	return w.retryThumbnailWithFreshOriginal(ctx, v, err, duration)
}

func (w *ThumbWorker) retryThumbnailWithFreshOriginal(ctx context.Context, v *catalog.Video, cause error, duration float64) error {
	if cause == nil || w.Drive == nil || w.Drive.Kind() != "p115" || !directMediaLinkRefreshAllowed(cause) {
		return cause
	}
	refreshed, err := w.Drive.StreamURL(ctx, v.FileID)
	if err != nil {
		return err
	}
	return w.generateThumbnailFromLink(ctx, v, refreshed, duration)
}

// process returns true when the current video must be retried after the shared
// drive cooldown. Permanent media errors transition to failed and return
// false; successful work also returns false.
func (w *Worker) process(ctx context.Context, v *catalog.Video) bool {
	if w.skipIfRateLimited(v) {
		return true
	}
	link, optimized, err := preferredGenerationStreamLink(ctx, w.Drive, v.FileID)
	if err != nil {
		if w.pauseForRecoverableError(err, "streamURL", v.Title) {
			return true
		}
		applog.Error(ctx, "Preview stream link failed: "+v.Title, err, applog.Fields{Component: "preview", DriveID: generationDriveID(w.Drive), VideoID: v.ID, FileID: v.FileID, Stage: "streamURL"})
		w.Catalog.UpdatePreview(ctx, v.ID, "", "failed")
		return false
	}

	// Metadata failures do not prevent generation with the unknown-duration plan.
	duration, probeErr := probeSourceDuration(ctx, w.Catalog, v, w.Gen, link)
	if probeErr != nil {
		if w.pauseForRecoverableError(probeErr, "probe", v.Title) {
			return true
		}
		applog.Error(ctx, "Preview duration probe failed: "+v.Title, probeErr, applog.Fields{Component: "preview", DriveID: generationDriveID(w.Drive), VideoID: v.ID, Stage: "probe"})
	}

	// 2) 预览视频
	tmp, err := w.generateTeaser(ctx, v, link, duration, optimized)
	if err != nil {
		if w.pauseForRecoverableError(err, "generate", v.Title) {
			return true
		}
		applog.Error(ctx, "Preview generation failed: "+v.Title, err, applog.Fields{Component: "preview", DriveID: generationDriveID(w.Drive), VideoID: v.ID, FileID: v.FileID, Stage: "generate"})
		w.Catalog.UpdatePreview(ctx, v.ID, "", "failed")
		return false
	}
	persistence.RLock()
	defer persistence.RUnlock()
	local, err := w.Gen.MoveToLocal(tmp, v.ID)
	if err != nil {
		applog.Error(ctx, "Preview file move failed: "+v.Title, err, applog.Fields{Component: "preview", DriveID: generationDriveID(w.Drive), VideoID: v.ID, Stage: "move"})
		w.Catalog.UpdatePreview(ctx, v.ID, "", "failed")
		return false
	}

	if previous, ok := localpath.Managed(filepath.Dir(local), v.PreviewLocal); ok {
		removePreviousLocalTeaser(previous, local)
	}
	reference := filepath.Base(local)
	if err := w.Catalog.UpdatePreview(ctx, v.ID, reference, "ready"); err != nil {
		removePreviousLocalTeaser(local, "")
		applog.Error(ctx, "Save preview metadata failed: "+v.Title, err, applog.Fields{Component: "preview", DriveID: generationDriveID(w.Drive), VideoID: v.ID, Stage: "save"})
		return false
	}
	if w.OnPreviewReady != nil && v.ThumbnailURL == "" {
		ready := *v
		ready.PreviewLocal = reference
		ready.PreviewStatus = "ready"
		w.OnPreviewReady(&ready)
	}
	applog.Info(ctx, fmt.Sprintf("Preview ready: %s (duration=%.1fs)", v.Title, duration), applog.Fields{Component: "preview", DriveID: generationDriveID(w.Drive), VideoID: v.ID})
	return false
}

func (w *Worker) generateTeaser(ctx context.Context, v *catalog.Video, link *drives.StreamLink, duration float64, optimized bool) (string, error) {
	if optimized {
		tmp, err := w.Gen.Generate(ctx, link, duration)
		if err == nil {
			return tmp, nil
		}
		if generationStreamForbidden(err) {
			refreshed, refreshErr := refreshGenerationStreamLink(ctx, w.Drive, v.FileID)
			switch {
			case refreshErr == nil:
				tmp, err = w.Gen.Generate(ctx, refreshed, duration)
				if err == nil || (isRateLimitError(err) && !generationStreamForbidden(err)) {
					return tmp, err
				}
			case !errors.Is(refreshErr, drives.ErrGenerationStreamUnavailable):
				return "", refreshErr
			}
		}
		if isRateLimitError(err) && !generationStreamForbidden(err) {
			return "", err
		}
		original, originalErr := w.Drive.StreamURL(ctx, v.FileID)
		if originalErr != nil {
			return "", originalErr
		}
		return w.generateTeaserFromOriginal(ctx, v, original, duration)
	}
	return w.generateTeaserFromOriginal(ctx, v, link, duration)
}

func (w *Worker) generateTeaserFromOriginal(ctx context.Context, v *catalog.Video, link *drives.StreamLink, duration float64) (string, error) {
	gen, ok := w.Gen.(refreshingTeaserGenerator)
	if !ok || w.Drive == nil || w.Drive.Kind() != "p115" {
		return w.Gen.Generate(ctx, link, duration)
	}
	return gen.GenerateWithLinkRefresh(ctx, link, duration, func(ctx context.Context) (*drives.StreamLink, error) {
		return w.Drive.StreamURL(ctx, v.FileID)
	})
}

func preferredGenerationStreamLink(ctx context.Context, drive drives.Drive, fileID string) (*drives.StreamLink, bool, error) {
	if drive == nil {
		return nil, false, errors.New("missing drive")
	}
	if provider, ok := drive.(drives.GenerationStreamProvider); ok {
		link, err := provider.GenerationStreamURL(ctx, fileID, false)
		if err == nil && link != nil && strings.TrimSpace(link.URL) != "" {
			return link, true, nil
		}
		if err != nil && !errors.Is(err, drives.ErrGenerationStreamUnavailable) {
			return nil, false, err
		}
	}
	link, err := drive.StreamURL(ctx, fileID)
	return link, false, err
}

func refreshGenerationStreamLink(ctx context.Context, drive drives.Drive, fileID string) (*drives.StreamLink, error) {
	provider, ok := drive.(drives.GenerationStreamProvider)
	if !ok {
		return nil, drives.ErrGenerationStreamUnavailable
	}
	link, err := provider.GenerationStreamURL(ctx, fileID, true)
	if err != nil {
		return nil, err
	}
	if link == nil || strings.TrimSpace(link.URL) == "" {
		return nil, fmt.Errorf("%w: empty refreshed link", drives.ErrGenerationStreamUnavailable)
	}
	return link, nil
}

func generationStreamForbidden(err error) bool {
	return drives.ErrorMentionsHTTPStatus(err, http.StatusForbidden)
}

func isRateLimitError(err error) bool {
	_, ok := drives.RateLimitRetryAfter(err)
	return ok
}

func removePreviousLocalTeaser(previous, current string) {
	if previous == "" {
		return
	}
	if filepath.Clean(previous) == filepath.Clean(current) {
		return
	}
	if err := os.Remove(previous); err != nil && !os.IsNotExist(err) {
		log.Printf("[preview] remove old local teaser %s: %v", previous, err)
	}
}

// --- utils ---

func buildHeaders(h map[string][]string) string {
	if len(h) == 0 {
		return ""
	}
	var sb strings.Builder
	for k, vs := range h {
		if strings.EqualFold(k, "User-Agent") ||
			strings.EqualFold(k, "Authorization") ||
			strings.EqualFold(k, "Proxy-Authorization") ||
			strings.EqualFold(k, "Cookie") {
			continue
		}
		for _, v := range vs {
			sb.WriteString(k)
			sb.WriteString(": ")
			sb.WriteString(v)
			sb.WriteString("\r\n")
		}
	}
	return sb.String()
}

func (w *Worker) notifyStatus(immediate bool) {
	if w.OnStatusChanged != nil {
		w.OnStatusChanged(immediate)
	}
}
func (w *ThumbWorker) notifyStatus(immediate bool) {
	if w.OnStatusChanged != nil {
		w.OnStatusChanged(immediate)
	}
}
