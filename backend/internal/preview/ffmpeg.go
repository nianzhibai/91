package preview

import (
	"bytes"
	"context"
	"database/sql"
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
	"slices"
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
	"github.com/nianzhibai/91/backend/internal/mediasig"
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

// MediaRequest selects what a media job produces besides the perceptual
// signature, which is always sampled from the segments.
type MediaRequest struct {
	Teaser    bool
	Thumbnail bool
}

// Media is the result of one pass over a video's teaser segments. Paths are
// temporary files the caller moves into place; each is empty when it was not
// requested or could not be produced.
type Media struct {
	TeaserPath    string
	ThumbnailPath string
	// Signature is nil when the windows could not be sampled or the source is
	// too short to compare.
	Signature *mediasig.Signature
	// SignatureComplete reports whether every planned window contributed to
	// the signature. A planned segment that fell back to another start
	// leaves its window out.
	SignatureComplete bool
}

// MediaGenerator produces a video's teaser, thumbnail and signature from one
// set of segment reads.
type MediaGenerator interface {
	Probe(ctx context.Context, link *drives.StreamLink) (float64, error)
	Generate(ctx context.Context, link *drives.StreamLink, duration float64, request MediaRequest) (Media, error)
	MoveToLocal(tmpPath, videoID string) (string, error)
	MoveThumbnailToLocal(tmpPath, videoID string) (string, error)
}

type refreshingMediaGenerator interface {
	GenerateWithLinkRefresh(ctx context.Context, first *drives.StreamLink, duration float64, request MediaRequest, refreshAfterFailure func(context.Context) (*drives.StreamLink, error)) (Media, error)
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

// longVideoSegmentFractions place the four segments of a video at least 30
// seconds long. The positions are fixed fractions of the duration, so videos
// of nearly equal duration are sampled at nearly the same moments: the
// perceptual signatures of re-encoded copies depend on it. Changing them
// requires renewing every stored signature (bump mediasig.SignatureFormat).
var longVideoSegmentFractions = []float64{0.15, 0.38, 0.62, 0.85}

// pickSegmentStarts 根据视频总时长选出 N 段起点秒数（按时间升序）
//
// 规则：
//   - duration < 30s → 最多 3 段，从 10% 起；不足 3 秒时用完整短视频作为单段
//   - duration ≥ 30s → 固定 4 段，起点在时长的 15% / 38% / 62% / 85%
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
	usable := math.Max(0, duration-eachSec-1)
	starts := make([]float64, 0, len(longVideoSegmentFractions))
	for _, fraction := range longVideoSegmentFractions {
		starts = append(starts, math.Min(duration*fraction, usable))
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
		path, err = task.generateThumbnail(ctx, link, videoID, duration)
	}
	return path, mediaInputError(ctx, err)
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
			ExpectedSource:     v,
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
		if err := cat.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{ExpectedSource: v, DurationSeconds: seconds}); err != nil {
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
	args = append(args, mediaInputOptions(ctx2, g.cfg.FFprobePath, ffmpegLink)...)
	args = append(args, ffmpegLink.URL)

	cmd := mediaInputCommand(ctx2, g.cfg.FFprobePath, ffmpegLink, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	err = mediaInputError(ctx2, err)
	if err != nil {
		errOut := stderr.Bytes()
		if len(errOut) == 0 {
			errOut = out
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

// Generate reads the teaser segments of a video once and produces the
// requested teaser and thumbnail together with the perceptual signature.
func (g *Generator) Generate(ctx context.Context, link *drives.StreamLink, duration float64, request MediaRequest) (Media, error) {
	return g.GenerateWithLinkRefresh(ctx, link, duration, request, nil)
}

func (g *Generator) GenerateWithLinkRefresh(ctx context.Context, first *drives.StreamLink, duration float64, request MediaRequest, refreshAfterFailure func(context.Context) (*drives.StreamLink, error)) (Media, error) {
	task := g.forTask()
	media, err := task.generateSerialWithRefresh(ctx, duration, request, first, refreshAfterFailure)
	if task.fallbackToCPU(ctx, err) {
		media, err = task.generateSerialWithRefresh(ctx, duration, request, first, refreshAfterFailure)
	}
	return media, mediaInputError(ctx, err)
}

type teaserSegmentCandidate struct {
	index int
	start float64
}

type teaserSegmentResult struct {
	teaserSegmentCandidate
	path      string
	thumbnail string
	err       error
}

// generateSerialWithRefresh generates at most one teaser segment at a time.
// A healthy source link is reused for the whole task. When a concrete remote
// read failure occurs, the current segment gets one attempt with a refreshed
// link; a successful replacement then becomes the source for later segments.
func (g *Generator) generateSerialWithRefresh(
	ctx context.Context,
	duration float64,
	request MediaRequest,
	initialLink *drives.StreamLink,
	refreshAfterFailure func(context.Context) (*drives.StreamLink, error),
) (Media, error) {
	if err := os.MkdirAll(g.cfg.LocalDir, 0o755); err != nil {
		return Media{}, err
	}

	plan := buildTeaserPlan(g.cfg, duration)
	starts := plan.starts
	eachSec := plan.eachSec
	if len(starts) == 0 {
		return Media{}, fmt.Errorf("video too short for %.0fs teaser segment", eachSec)
	}

	segmentCtx, cancelSegments := context.WithTimeout(ctx, g.segmentBudget)
	defer cancelSegments()

	// Segment files never outlive the job; the teaser and thumbnail returned
	// to the caller are removed from this list once they are handed over.
	generatedPaths := make([]string, 0, len(starts)+1)
	defer func() {
		for _, p := range generatedPaths {
			_ = os.Remove(p)
		}
	}()
	keep := func(path string) {
		for i, p := range generatedPaths {
			if p == path {
				generatedPaths = append(generatedPaths[:i], generatedPaths[i+1:]...)
				return
			}
		}
	}
	thumbnailIndex := -1
	if request.Thumbnail {
		thumbnailIndex = middleSegment(duration, starts, eachSec)
	}

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
			return Media{}, err
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
		withThumbnail := candidate.index == thumbnailIndex
		result.path, result.thumbnail, result.err = g.generateSingleSegment(
			segmentCtx,
			candidate.index,
			candidate.start,
			eachSec,
			fadeSegments,
			withThumbnail,
			currentLink,
			&colorRepair,
		)
		if err := ctx.Err(); err != nil && result.err != nil {
			return Media{}, err
		}
		if g.hardware != nil && hardwareFailure(result.err) {
			return Media{}, result.err
		}
		if segmentCtx.Err() == nil && refreshAfterFailure != nil && directMediaLinkRefreshAllowed(result.err) {
			refreshed, refreshErr := refreshAfterFailure(segmentCtx)
			if refreshErr == nil && (refreshed == nil || strings.TrimSpace(refreshed.URL) == "") {
				refreshErr = fmt.Errorf("%w: empty refreshed direct link", drives.ErrGenerationStreamUnavailable)
			}
			switch {
			case refreshErr == nil:
				currentLink = refreshed
				result.path, result.thumbnail, result.err = g.generateSingleSegment(
					segmentCtx,
					candidate.index,
					candidate.start,
					eachSec,
					fadeSegments,
					withThumbnail,
					currentLink,
					&colorRepair,
				)
			case !errors.Is(refreshErr, drives.ErrGenerationStreamUnavailable):
				if segmentCtx.Err() != nil && errors.Is(refreshErr, segmentCtx.Err()) {
					lastErr = refreshErr
					break segmentLoop
				}
				return Media{}, refreshErr
			}
		}

		if result.err != nil {
			if g.hardware != nil && hardwareFailure(result.err) {
				return Media{}, result.err
			}
			if !teaserSegmentFallbackAllowed(result.err) {
				return Media{}, result.err
			}
			lastErr = result.err
			continue
		}
		generatedPaths = append(generatedPaths, result.path)
		if result.thumbnail != "" {
			generatedPaths = append(generatedPaths, result.thumbnail)
		}
		segmentResults = append(segmentResults, result)
	}
	if err := ctx.Err(); err != nil {
		return Media{}, err
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
				return Media{}, fmt.Errorf("only generated %d/%d teaser segments: %w", len(segmentResults), targetSegments, lastErr)
			}
			return Media{}, fmt.Errorf("only generated %d/%d teaser segments", len(segmentResults), targetSegments)
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
	media := Media{}
	media.Signature, media.SignatureComplete = g.sampleSignature(ctx, duration, starts, segmentResults, eachSec)
	for _, result := range segmentResults {
		if result.thumbnail != "" {
			media.ThumbnailPath = result.thumbnail
			keep(result.thumbnail)
		}
	}
	if !request.Teaser {
		return media, nil
	}

	if len(segmentPaths) == 1 {
		media.TeaserPath = segmentPaths[0]
		keep(media.TeaserPath)
		return media, nil
	}

	// Finishing uses only local files and gets its own budget. Derive it from
	// the task context so exhausted remote reads do not cancel usable segments,
	// while stopping the task still cancels both stages.
	finishCtx, cancelFinish := context.WithTimeout(ctx, g.finishTimeout)
	defer cancelFinish()

	tmp, err := os.CreateTemp(g.cfg.LocalDir, "teaser-*.mp4")
	if err != nil {
		return Media{}, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	_ = os.Remove(tmpPath)

	list, err := os.CreateTemp(g.cfg.LocalDir, "teaser-concat-*.txt")
	if err != nil {
		return Media{}, err
	}
	listPath := list.Name()
	for _, p := range segmentPaths {
		if _, err := fmt.Fprintf(list, "file '%s'\n", escapeConcatPath(p)); err != nil {
			list.Close()
			_ = os.Remove(listPath)
			return Media{}, err
		}
	}
	if err := list.Close(); err != nil {
		_ = os.Remove(listPath)
		return Media{}, err
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
		return Media{}, ffmpegCommandError("ffmpeg concat", err, out)
	}
	if info, statErr := os.Stat(tmpPath); statErr != nil || info.Size() == 0 {
		_ = os.Remove(tmpPath)
		return Media{}, fmt.Errorf("ffmpeg concat produced empty file, stderr: %s", string(out))
	}
	if err := g.validateGeneratedTeaser(finishCtx, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return Media{}, err
	}

	media.TeaserPath = tmpPath
	return media, nil
}

// middleSegment returns the index of the planned segment closest to the
// middle of the video, which supplies the thumbnail.
func middleSegment(duration float64, starts []float64, eachSec float64) int {
	best := -1
	for i, start := range starts {
		if best < 0 || math.Abs(start+eachSec/2-duration/2) < math.Abs(starts[best]+eachSec/2-duration/2) {
			best = i
		}
	}
	return best
}

// sampleSignature samples the segments read at the planned starts. A segment
// that fell back to another start shows moments other videos do not sample,
// so it fills the teaser but stays out of the signature. complete reports
// whether every planned window contributed frames. The signature is
// optional: a failure is logged and the teaser stays usable.
func (g *Generator) sampleSignature(ctx context.Context, duration float64, planned []float64, segments []teaserSegmentResult, eachSec float64) (signature *mediasig.Signature, complete bool) {
	source := secondsDuration(duration)
	if source < mediasig.MinDuration {
		return nil, false
	}
	clips := make([]mediasig.Clip, 0, len(segments))
	for _, segment := range segments {
		if !slices.Contains(planned, segment.start) {
			continue
		}
		clips = append(clips, mediasig.Clip{
			Path:   segment.path,
			Window: mediasig.Window{Start: secondsDuration(segment.start), Duration: secondsDuration(eachSec)},
		})
	}
	if len(clips) == 0 {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, g.finishTimeout)
	defer cancel()
	signature, err := mediasig.Extract(ctx, g.cfg.FFmpegPath, source, clips)
	if err != nil {
		applog.Warn(ctx, "[preview] teaser signature sampling failed", err, applog.Fields{Component: "preview", Stage: "signature"})
		return nil, false
	}
	return signature, signature.SampledWindows() == len(planned)
}

func secondsDuration(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
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

// generateSingleSegment encodes one teaser segment. withThumbnail also writes
// a thumbnail from the middle of the segment in the same read; a thumbnail
// that could not be written is returned empty without failing the segment.
func (g *Generator) generateSingleSegment(ctx context.Context, index int, start, eachSec float64, fade, withThumbnail bool, link *drives.StreamLink, colorRepair *sourceColorRepair) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, teaserSegmentTimeout)
	defer cancel()

	ffmpegLink, cleanup, err := prepareFFmpegLink(ctx, link)
	if err != nil {
		return "", "", err
	}
	defer cleanup()

	segPath, err := reserveTemp(g.cfg.LocalDir, fmt.Sprintf("teaser-seg-%d-*.mp4", index))
	if err != nil {
		return "", "", err
	}
	thumbPath := ""
	if withThumbnail {
		if thumbPath, err = reserveTemp(g.cfg.LocalDir, fmt.Sprintf("thumb-seg-%d-*.jpg", index)); err != nil {
			_ = os.Remove(segPath)
			return "", "", err
		}
	}
	removeOutputs := func() {
		_ = os.Remove(segPath)
		if thumbPath != "" {
			_ = os.Remove(thumbPath)
		}
	}

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
	outputArgs := []string{"-threads", strconv.Itoa(g.cfg.FFmpegThreads)}
	if thumbPath != "" {
		// The segment stays the last output, after the thumbnail.
		outputArgs = append(outputArgs,
			"-filter_complex", g.segmentThumbnailGraph(fade, eachSec),
			"-map", "[thumb]", "-frames:v", "1", "-q:v", "3", "-y", thumbPath,
			"-map", "[segment]",
		)
	} else {
		outputArgs = append(outputArgs, "-vf", g.segmentFilter(fade, eachSec))
	}
	outputArgs = append(outputArgs, "-an")
	outputArgs = append(outputArgs, g.encoderOptions()...)
	outputArgs = append(outputArgs,
		"-movflags", "+faststart",
		"-y", segPath,
	)
	out, err := g.runMediaCommand(ctx, link, ffmpegLink, args, outputArgs, colorRepair)
	if err != nil {
		removeOutputs()
		return "", "", g.processingError(ffmpegCommandError("ffmpeg segment", err, out))
	}
	if info, statErr := os.Stat(segPath); statErr != nil || info.Size() == 0 {
		removeOutputs()
		return "", "", g.processingError(fmt.Errorf("ffmpeg segment produced empty file, stderr: %s", string(out)))
	}
	if err := g.validateGeneratedTeaser(ctx, segPath); err != nil {
		removeOutputs()
		return "", "", g.processingError(err)
	}
	if thumbPath != "" {
		if info, statErr := os.Stat(thumbPath); statErr != nil || info.Size() == 0 {
			_ = os.Remove(thumbPath)
			thumbPath = ""
		}
	}
	return segPath, thumbPath, nil
}

// reserveTemp returns the name of a new empty file in dir.
func reserveTemp(dir, pattern string) (string, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

func teaserSegmentFallbackAllowed(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := drives.RateLimitRetryAfter(err); ok {
		return false
	}
	// Authentication and precondition failures cannot be repaired by seeking to
	// another segment. Return them to the generation source refresh policy.
	if drives.ErrorMentionsHTTPStatus(err, http.StatusUnauthorized, http.StatusPreconditionFailed) {
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
	// A relay already supplied structured retry metadata. Do not shadow it
	// with the default rate-limit wrapper inferred from subprocess text.
	if _, limited := drives.RateLimitRetryAfter(err); limited {
		return wrapped
	}
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

// MoveThumbnailToLocal moves a generated thumbnail to the video's thumbnail
// path and returns it.
func (g *Generator) MoveThumbnailToLocal(tmpPath, videoID string) (string, error) {
	dst := mediaasset.ThumbnailPath(g.cfg.LocalDir, videoID)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := replaceFile(tmpPath, dst); err != nil {
		return "", err
	}
	return dst, nil
}

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

// Worker runs a drive's media jobs: each reads a video's teaser segments once
// for the signature, the teaser and a missing thumbnail.
type Worker struct {
	Gen     MediaGenerator
	Catalog *catalog.Catalog
	Drive   drives.Drive
	// Gate is the shared preview switch. Jobs run either way; it decides
	// whether they produce teasers. Nil enables standalone workers.
	Gate *Gate
	// OnThumbnailMissing receives videos a job left without a thumbnail, so
	// the application can hand them to the thumbnail worker.
	OnThumbnailMissing func(*catalog.Video)
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

	// RateLimitCooldown is the pause when a throttling error gives no retry time.
	RateLimitCooldown time.Duration
	// Cooldown is shared by all workers of the drive. The application injects
	// it before Run starts; standalone workers get their own.
	Cooldown   *tasklimit.Cooldown
	activities taskActivities
}

func NewWorker(gen MediaGenerator, cat *catalog.Catalog, drv drives.Drive) *Worker {
	return &Worker{
		Gen:     gen,
		Catalog: cat,
		Drive:   drv,
		// Standalone workers default to one slot. The application injects its
		// shared preview budget before Run starts.
		Limiter:  tasklimit.New(1),
		Cooldown: &tasklimit.Cooldown{},
		ch:       make(chan *catalog.Video, defaultWorkerQueueSize),
	}
}

func (w *Worker) Enqueue(v *catalog.Video) bool {
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

func (w *Worker) EnqueueBlocking(ctx context.Context, v *catalog.Video) bool {
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

func (w *Worker) enabled() bool {
	return w.Gate.Enabled()
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

	// RateLimitCooldown is the pause when a throttling error gives no retry time.
	RateLimitCooldown time.Duration
	// Cooldown is shared by all workers of the drive; see Worker.Cooldown.
	Cooldown *tasklimit.Cooldown
	activity taskActivity
}

const (
	defaultThumbTransientMediaMaxFailures = 3
	defaultWorkerQueueSize                = 10000
)

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

func NewThumbWorker(gen ThumbnailGenerator, cat *catalog.Catalog, drv drives.Drive) *ThumbWorker {
	return &ThumbWorker{
		Gen:        gen,
		Catalog:    cat,
		Drive:      drv,
		Cooldown:   &tasklimit.Cooldown{},
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
	return taskStatus(currentTitle, len(activeIDs) > 0, w.Cooldown, w.queue.lengthExcludingIDs(activeIDs))
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
	return taskStatus(currentTitle, currentID != "", w.Cooldown, queued)
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

func taskStatus(currentTitle string, active bool, cooldown *tasklimit.Cooldown, queueLength int) TaskStatus {
	if queueLength < 0 {
		queueLength = 0
	}
	status := TaskStatus{
		State:       "idle",
		QueueLength: queueLength,
	}
	if until, ok := cooldown.Until(time.Now()); ok {
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
// succeeds, even when a drive has thousands of queued videos. Without a task
// the queue reservation is released.
func (w *Worker) prepareQueued(ctx context.Context, v *catalog.Video) func() {
	if v == nil {
		return nil
	}
	prepared := false
	taskRelease := func() {}
	defer func() {
		if !prepared {
			taskRelease()
			w.releaseQueued(v)
		}
	}()
	if w.Catalog == nil || v.ID == "" || ctx.Err() != nil {
		return nil
	}
	taskCtx := applog.WithFields(applog.NewTask(ctx, "preview", generationDriveID(w.Drive)), applog.Fields{VideoID: v.ID, FileID: v.FileID})
	if w.TaskGuard != nil {
		release := w.TaskGuard()
		if release == nil {
			return nil
		}
		taskRelease = release
	}
	current, err := w.Catalog.GetVideo(taskCtx, v.ID)
	if err != nil {
		if taskCtx.Err() == nil {
			applog.Error(taskCtx, "Read queued video failed", err, applog.Fields{Stage: "lookup"})
		}
		return nil
	}
	if current.Hidden || current.DriveID != generationDriveID(w.Drive) {
		return nil
	}
	release, err := w.Limiter.AcquireAfter(taskCtx, w.Cooldown)
	if err != nil {
		return nil
	}
	prepared = true
	return func() {
		defer taskRelease()
		defer release()
		w.activities.start(current)
		w.notifyStatus(true)
		retry := w.process(taskCtx, current)
		release()
		// Release before requeueing because videoQueue deduplicates reserved
		// IDs. Keep activity visible throughout the transition for admission.
		w.queue.release(v)
		if retry && ctx.Err() == nil {
			w.EnqueueBlocking(ctx, current)
		}
		w.activities.done(current.ID)
		w.notifyStatus(true)
	}
}

func (w *Worker) releaseQueued(v *catalog.Video) {
	w.queue.release(v)
	w.notifyStatus(true)
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
	if release, err := w.Limiter.AcquireAfter(ctx, w.Cooldown); err == nil {
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

func (w *Worker) skipIfRateLimited(v *catalog.Video) bool {
	until, ok := w.Cooldown.Until(time.Now())
	if ok {
		log.Printf("[preview] drive=%s cooling down until=%s; keep queued video pending", generationDriveID(w.Drive), until.Format(time.RFC3339))
	}
	return ok
}

func (w *Worker) pauseForRateLimit(err error, step, title string) bool {
	wait, ok := drives.RateLimitRetryAfter(err)
	if !ok {
		return false
	}
	until := pauseDrive(w.Cooldown, wait, w.RateLimitCooldown)
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
	until := pauseDrive(w.Cooldown, 0, w.RateLimitCooldown)
	w.notifyStatus(true)
	log.Printf("[preview] drive=%s transient media source error until=%s step=%s video=%s: %v", w.Drive.ID(), until.Format(time.RFC3339), step, title, err)
	return true
}

func (w *ThumbWorker) skipIfRateLimited(v *catalog.Video) bool {
	until, ok := w.Cooldown.Until(time.Now())
	if ok {
		log.Printf("[thumb] drive=%s cooling down until=%s; keep queued thumbnail pending", generationDriveID(w.Drive), until.Format(time.RFC3339))
	}
	return ok
}

// pauseForProviderError shares provider cooldowns with metadata work without
// consuming thumbnail retries or deciding whether to requeue the current task.
func (w *ThumbWorker) pauseForProviderError(err error, step, title string) bool {
	wait, limited := drives.RateLimitRetryAfter(err)
	if !limited && !driveErrorShouldCooldown(w.Drive, err) {
		return false
	}
	until := pauseDrive(w.Cooldown, wait, w.RateLimitCooldown)
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
		count, countErr := w.Catalog.IncrementThumbnailFailuresForSource(ctx, v)
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

// pauseDrive pauses the drive for the provider's retry time, or for the
// worker's configured cooldown when the provider gave none.
func pauseDrive(cooldown *tasklimit.Cooldown, retryAfter, configured time.Duration) time.Time {
	wait := retryAfter
	if wait <= 0 {
		wait = configured
	}
	if wait <= 0 {
		wait = drives.DefaultCooldown
	}
	return cooldown.Pause(time.Now(), wait)
}

func driveErrorShouldCooldown(d drives.Drive, err error) bool {
	return d != nil && drives.ShouldCooldown(d.Kind(), err)
}

func (w *ThumbWorker) process(ctx context.Context, v *catalog.Video) bool {
	if w.skipIfRateLimited(v) {
		return false
	}
	if w.Catalog == nil || v == nil || v.ID == "" {
		return false
	}
	loaded, err := w.Catalog.GetVideo(ctx, v.ID)
	if err != nil || loaded.Hidden || loaded.DriveID != generationDriveID(w.Drive) {
		return false
	}
	v = loaded
	if v.ThumbnailURL != "" {
		return false
	}
	_ = w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{ExpectedSource: v, ThumbnailStatus: "pending"})
	input, err := resolveGenerationInput(ctx, w.Drive, v.FileID)
	if err != nil {
		if w.pauseForRecoverableError(ctx, v, err, "streamURL") {
			return true
		}
		applog.Error(ctx, "Thumbnail stream link failed: "+v.Title, err, applog.Fields{Component: "thumb", DriveID: generationDriveID(w.Drive), VideoID: v.ID, FileID: v.FileID, Stage: "streamURL"})
		_ = w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{ExpectedSource: v, ThumbnailStatus: "failed"})
		return false
	}
	defer input.Close()
	if w.probeDuration(ctx, v, input) {
		return true
	}

	if err := w.generateThumbnailWithFallback(ctx, v, input); err != nil {
		if w.pauseForRecoverableError(ctx, v, err, "generate") {
			return true
		}
		applog.Error(ctx, "Thumbnail generation failed: "+v.Title, err, applog.Fields{Component: "thumb", DriveID: generationDriveID(w.Drive), VideoID: v.ID, FileID: v.FileID, Stage: "generate"})
		_ = w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{ExpectedSource: v, ThumbnailStatus: "failed"})
		return false
	}
	return false
}

func (w *ThumbWorker) generateThumbnailFromLink(ctx context.Context, v *catalog.Video, link *drives.StreamLink, duration float64) error {
	persistence.RLock()
	defer persistence.RUnlock()
	local, err := w.Gen.GenerateThumbnail(ctx, link, v.ID, duration)
	if err != nil {
		return err
	}
	if err := w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{
		ExpectedSource:  v,
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

func (w *ThumbWorker) generateThumbnailWithFallback(ctx context.Context, v *catalog.Video, input *generationInput) error {
	duration := float64(v.DurationSeconds)
	return input.run(ctx, func(ctx context.Context, link *drives.StreamLink) error {
		err := w.generateThumbnailFromLink(ctx, v, link, duration)
		if input.source == nil {
			return w.retryThumbnailWithFreshOriginal(ctx, v, err, duration)
		}
		return err
	})
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

// process runs the media job for v: one pass over the teaser segments yields
// the perceptual signature, the teaser while previews are enabled, and a
// missing thumbnail. It returns true when v must be retried after the shared
// drive cooldown or an interruption; settled outcomes return false.
func (w *Worker) process(ctx context.Context, v *catalog.Video) bool {
	if w.skipIfRateLimited(v) {
		return true
	}
	previews := w.enabled()
	if !v.MediaWants(previews).Any() {
		w.handOffThumbnail(v)
		return false
	}
	// An interrupted task is neither a media failure nor a provider cooldown:
	// the video stays pending and is retried once its context allows it.
	input, err := resolveGenerationInput(ctx, w.Drive, v.FileID)
	if err != nil {
		if ctx.Err() != nil {
			return true
		}
		if w.pauseForRecoverableError(err, "streamURL", v.Title) {
			return true
		}
		applog.Error(ctx, "Media stream link failed: "+v.Title, err, w.logFields(v, "streamURL"))
		w.failMedia(ctx, v, v.MediaWants(previews))
		return false
	}
	defer input.Close()

	// Metadata failures do not prevent a teaser with the unknown-duration
	// plan, but a signature needs the duration.
	duration, probeErr := input.probe(ctx, func(ctx context.Context, link *drives.StreamLink) (float64, error) {
		return probeSourceDuration(ctx, w.Catalog, v, w.Gen, link)
	})
	if probeErr != nil {
		if ctx.Err() != nil {
			return true
		}
		if w.pauseForRecoverableError(probeErr, "probe", v.Title) {
			return true
		}
		applog.Error(ctx, "Media duration probe failed: "+v.Title, probeErr, w.logFields(v, "probe"))
	}
	// The probe stores the duration on v, which decides the signature.
	wants := v.MediaWants(previews)
	if !wants.Any() {
		// A pending job for a video too short to sample has nothing to do
		// until previews are enabled.
		if v.MediaStatus == "pending" {
			_ = w.Catalog.CompleteMediaForSource(ctx, v, nil)
		}
		w.handOffThumbnail(v)
		return false
	}

	request := MediaRequest{Teaser: wants.Teaser, Thumbnail: v.ThumbnailURL == ""}
	media, err := w.generateMedia(ctx, v, input, duration, request)
	if err != nil {
		if ctx.Err() != nil {
			return true
		}
		if w.pauseForRecoverableError(err, "generate", v.Title) {
			return true
		}
		applog.Error(ctx, "Media generation failed: "+v.Title, err, w.logFields(v, "generate"))
		w.failMedia(ctx, v, wants)
		w.handOffThumbnail(v)
		return false
	}

	// The outputs exist; saving them is cheaper than discarding finished work.
	ctx = context.WithoutCancel(ctx)
	persistence.RLock()
	defer persistence.RUnlock()
	if media.TeaserPath != "" {
		w.saveTeaser(ctx, v, media.TeaserPath)
	}
	w.completeMedia(ctx, v, wants, duration, media)
	if media.ThumbnailPath != "" {
		w.saveThumbnail(ctx, v, media.ThumbnailPath)
	}
	w.handOffThumbnail(v)
	applog.Info(ctx, fmt.Sprintf("Media ready: %s (duration=%.1fs teaser=%v sampled=%v thumbnail=%v)",
		v.Title, duration, media.TeaserPath != "", media.Signature != nil, media.ThumbnailPath != ""),
		applog.Fields{Component: "preview", DriveID: generationDriveID(w.Drive), VideoID: v.ID})
	return false
}

func (w *Worker) logFields(v *catalog.Video, stage string) applog.Fields {
	return applog.Fields{Component: "preview", DriveID: generationDriveID(w.Drive), VideoID: v.ID, FileID: v.FileID, Stage: stage}
}

// completeMedia records the job's signature. A signature missing planned
// windows is kept and read again by maintenance until it is complete, since
// a fallback window cannot be compared with other videos. A long video whose segments could not be
// sampled fails the job so a retry admits it again; a job that only renewed
// the teaser keeps an existing signature when sampling failed.
func (w *Worker) completeMedia(ctx context.Context, v *catalog.Video, wants catalog.MediaWants, duration float64, media Media) {
	var err error
	switch signature := media.Signature; {
	case signature != nil && !media.SignatureComplete:
		err = w.Catalog.CompletePartialMediaForSource(ctx, v, signature)
	case signature != nil || (wants.Signature && secondsDuration(duration) < mediasig.MinDuration):
		err = w.Catalog.CompleteMediaForSource(ctx, v, signature)
	case wants.Signature:
		err = w.Catalog.MarkMediaFailedForSource(ctx, v)
	}
	if err != nil {
		applog.Warn(ctx, "Save media signature failed: "+v.Title, err, w.logFields(v, "signature"))
	}
}

// failMedia settles a failed media job so the video is not queued again until
// a retry resets it. A failed renewal keeps the teaser the video already has.
func (w *Worker) failMedia(ctx context.Context, v *catalog.Video, wants catalog.MediaWants) {
	if wants.Teaser && v.PreviewStatus != "ready" {
		_ = w.Catalog.UpdatePreviewForSource(ctx, v, "", "failed")
	}
	if v.MediaStatus == "pending" {
		_ = w.Catalog.MarkMediaFailedForSource(ctx, v)
	}
}

func (w *Worker) saveTeaser(ctx context.Context, v *catalog.Video, path string) {
	local, err := w.Gen.MoveToLocal(path, v.ID)
	if err != nil {
		_ = os.Remove(path)
		applog.Error(ctx, "Preview file move failed: "+v.Title, err, w.logFields(v, "move"))
		if v.PreviewStatus != "ready" {
			_ = w.Catalog.UpdatePreviewForSource(ctx, v, "", "failed")
		}
		return
	}
	reference := filepath.Base(local)
	if err := w.Catalog.UpdatePreviewForSource(ctx, v, reference, "ready"); err != nil {
		w.discardUnsavedTeaser(ctx, v.ID, local)
		applog.Error(ctx, "Save preview metadata failed: "+v.Title, err, w.logFields(v, "save"))
		return
	}
	if previous, ok := localpath.Managed(filepath.Dir(local), v.PreviewLocal); ok {
		removePreviousLocalTeaser(previous, local)
	}
	v.PreviewLocal, v.PreviewStatus = reference, "ready"
}

func (w *Worker) saveThumbnail(ctx context.Context, v *catalog.Video, path string) {
	local, err := w.Gen.MoveThumbnailToLocal(path, v.ID)
	if err != nil {
		_ = os.Remove(path)
		applog.Error(ctx, "Thumbnail file move failed: "+v.Title, err, w.logFields(v, "thumbnail"))
		return
	}
	url := "/p/thumb/" + v.ID
	if err := w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{
		ExpectedSource: v, ThumbnailURL: url, ThumbnailStatus: "ready",
	}); err != nil {
		_ = os.Remove(local)
		applog.Error(ctx, "Save thumbnail metadata failed: "+v.Title, err, w.logFields(v, "thumbnail"))
		return
	}
	v.ThumbnailURL = url
}

// handOffThumbnail passes a video the media job left without a thumbnail to
// the thumbnail worker.
func (w *Worker) handOffThumbnail(v *catalog.Video) {
	if v != nil && v.ThumbnailURL == "" && w.OnThumbnailMissing != nil {
		w.OnThumbnailMissing(v)
	}
}

func (w *Worker) generateMedia(ctx context.Context, v *catalog.Video, input *generationInput, duration float64, request MediaRequest) (Media, error) {
	var media Media
	err := input.run(ctx, func(ctx context.Context, link *drives.StreamLink) error {
		var err error
		if input.source == nil {
			media, err = w.generateMediaFromOriginal(ctx, v, link, duration, request)
		} else {
			media, err = w.Gen.Generate(ctx, link, duration, request)
		}
		return err
	})
	return media, err
}

func (w *Worker) generateMediaFromOriginal(ctx context.Context, v *catalog.Video, link *drives.StreamLink, duration float64, request MediaRequest) (Media, error) {
	gen, ok := w.Gen.(refreshingMediaGenerator)
	if !ok || w.Drive == nil || w.Drive.Kind() != "p115" {
		return w.Gen.Generate(ctx, link, duration, request)
	}
	return gen.GenerateWithLinkRefresh(ctx, link, duration, request, func(ctx context.Context) (*drives.StreamLink, error) {
		return w.Drive.StreamURL(ctx, v.FileID)
	})
}

func isRateLimitError(err error) bool {
	_, ok := drives.RateLimitRetryAfter(err)
	return ok
}

// discardUnsavedTeaser removes a teaser whose metadata write was rejected.
// MoveToLocal writes the stable per-video path, so the row may still reference
// that file. A source update that keeps the reference keeps the same bytes;
// one that changes the content clears the reference first.
func (w *Worker) discardUnsavedTeaser(ctx context.Context, videoID, local string) {
	current, err := w.Catalog.GetVideo(ctx, videoID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err == nil {
		if referenced, ok := localpath.Managed(filepath.Dir(local), current.PreviewLocal); ok &&
			filepath.Clean(referenced) == filepath.Clean(local) {
			return
		}
	}
	removePreviousLocalTeaser(local, "")
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
