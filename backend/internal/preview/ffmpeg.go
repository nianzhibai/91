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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/mediaasset"
	"github.com/video-site/backend/internal/persistence"
	"github.com/video-site/backend/internal/streamhttp"
)

type Config struct {
	FFmpegPath      string
	FFprobePath     string
	DurationSeconds int // 兼容旧配置；当前预览视频每段固定 3 秒
	Width           int
	Segments        int    // 兼容旧配置；当前 30 秒及以上视频固定使用 4 段
	LocalDir        string // 本地预览视频和封面目录
}

type Generator struct {
	cfg Config
}

const teaserSegmentTimeout = 90 * time.Second

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
	GenerateWithLinkProvider(ctx context.Context, first *drives.StreamLink, duration float64, refresh func(context.Context) (*drives.StreamLink, error)) (string, error)
}

func New(cfg Config) *Generator {
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
	return &Generator{cfg: cfg}
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
	offsets := thumbnailOffsets(duration)
	for i, offset := range offsets {
		if i > 0 {
			_ = os.Remove(tempPath)
		}
		if err := g.generateThumbnailAtOffset(ctx, link, tempPath, offset); err != nil {
			lastErr = err
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

func (g *Generator) generateThumbnailAtOffset(ctx context.Context, link *drives.StreamLink, dst string, offset float64) error {
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
		"-ss", fmt.Sprintf("%.2f", offset),
	}
	args = append(args, ffmpegHTTPInputOptions(ffmpegLink)...)
	args = append(args,
		"-i", ffmpegLink.URL,
		"-frames:v", "1",
		"-vf", thumbnailVideoFilter(g.cfg.Width),
		"-q:v", "3",
		"-y", dst,
	)

	cmd := exec.CommandContext(ctx2, g.cfg.FFmpegPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(dst)
		return ffmpegCommandError("ffmpeg thumb", err, out)
	}
	if info, statErr := os.Stat(dst); statErr != nil || info.Size() == 0 {
		os.Remove(dst)
		return fmt.Errorf("ffmpeg thumb produced empty file, stderr: %s", string(out))
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
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "produced empty file") ||
		strings.Contains(text, "signal: killed") ||
		strings.Contains(text, "context deadline exceeded")
}

// --- 时长 ---

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
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
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
		return 0, ffmpegCommandError("ffprobe", err, errOut)
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" || raw == "N/A" {
		return 0, nil
	}
	return strconv.ParseFloat(raw, 64)
}

// --- 预览视频 ---

// Generate 拉取预览视频到本地临时文件，返回路径。
// 根据 Config.Segments 和视频时长决定是单段还是多段拼接。
func (g *Generator) Generate(ctx context.Context, link *drives.StreamLink, duration float64) (string, error) {
	return g.generate(ctx, duration, func(int) (*drives.StreamLink, error) {
		return link, nil
	})
}

func (g *Generator) GenerateWithLinkProvider(ctx context.Context, first *drives.StreamLink, duration float64, refresh func(context.Context) (*drives.StreamLink, error)) (string, error) {
	return g.generateSequential(ctx, duration, func(index int) (*drives.StreamLink, error) {
		if index == 0 || refresh == nil {
			return first, nil
		}
		return refresh(ctx)
	})
}

func (g *Generator) generate(ctx context.Context, duration float64, linkForInput func(int) (*drives.StreamLink, error)) (string, error) {
	if err := os.MkdirAll(g.cfg.LocalDir, 0o755); err != nil {
		return "", err
	}

	plan := buildTeaserPlan(g.cfg, duration)
	starts := plan.starts
	eachSec := plan.eachSec
	if len(starts) == 0 {
		return "", fmt.Errorf("video too short for %.0fs teaser segment", eachSec)
	}

	ctx2, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	// 用 ffmpeg 的 concat 滤镜一次输出：多个 -ss input 再 concat + fade
	tmp, err := os.CreateTemp(g.cfg.LocalDir, "teaser-*.mp4")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	tmp.Close()

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
	}

	// 每段独立 -ss + -i，精确 seek 重新解码保证拼接帧准
	var cleanups []func()
	defer func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}()
	for i, s := range starts {
		link, err := linkForInput(i)
		if err != nil {
			os.Remove(tmpPath)
			return "", err
		}
		ffmpegLink, cleanup, err := prepareFFmpegLink(ctx2, link)
		if err != nil {
			os.Remove(tmpPath)
			return "", err
		}
		cleanups = append(cleanups, cleanup)
		args = append(args, ffmpegHTTPInputOptions(ffmpegLink)...)
		args = append(args,
			"-ss", fmt.Sprintf("%.2f", s),
			"-t", fmt.Sprintf("%.2f", eachSec),
			"-i", ffmpegLink.URL,
		)
	}

	if len(starts) == 1 {
		// 单段：无需 concat，直接缩放 + 无音
		args = append(args,
			"-an",
			"-vf", teaserSegmentVideoFilter(g.cfg.Width, false, eachSec),
			"-c:v", "libx264",
			"-preset", "veryfast",
			"-crf", "28",
			"-movflags", "+faststart",
			"-y", tmpPath,
		)
	} else {
		// 多段：各段缩放 + 0.2s 黑场淡入淡出，concat 拼接
		// filter_complex: [0:v]scale,setpts,fade... [v0]; ...; [v0][v1][v2]concat=n=3:v=1:a=0[v]
		var filter strings.Builder
		for i := range starts {
			if i > 0 {
				filter.WriteString(";")
			}
			fmt.Fprintf(&filter,
				"[%d:v]%s[v%d]",
				i, teaserSegmentVideoFilter(g.cfg.Width, true, eachSec), i)
		}
		filter.WriteString(";")
		for i := range starts {
			fmt.Fprintf(&filter, "[v%d]", i)
		}
		fmt.Fprintf(&filter, "concat=n=%d:v=1:a=0[v]", len(starts))

		args = append(args,
			"-filter_complex", filter.String(),
			"-map", "[v]",
			"-an",
			"-c:v", "libx264",
			"-preset", "veryfast",
			"-crf", "28",
			"-movflags", "+faststart",
			"-y", tmpPath,
		)
	}

	cmd := exec.CommandContext(ctx2, g.cfg.FFmpegPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(tmpPath)
		return "", ffmpegCommandError("ffmpeg", err, out)
	}

	if info, statErr := os.Stat(tmpPath); statErr != nil || info.Size() == 0 {
		os.Remove(tmpPath)
		return "", fmt.Errorf("ffmpeg produced empty file, stderr: %s", string(out))
	}
	if err := g.validateGeneratedTeaser(ctx2, tmpPath); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	return tmpPath, nil
}

func (g *Generator) generateSequential(ctx context.Context, duration float64, linkForInput func(int) (*drives.StreamLink, error)) (string, error) {
	if err := os.MkdirAll(g.cfg.LocalDir, 0o755); err != nil {
		return "", err
	}

	plan := buildTeaserPlan(g.cfg, duration)
	starts := plan.starts
	eachSec := plan.eachSec
	if len(starts) == 0 {
		return "", fmt.Errorf("video too short for %.0fs teaser segment", eachSec)
	}

	ctx2, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	segmentPaths := make([]string, 0, len(starts))
	success := false
	defer func() {
		if success {
			return
		}
		for _, p := range segmentPaths {
			_ = os.Remove(p)
		}
	}()

	candidates := teaserCandidateStarts(duration, starts, eachSec)
	targetSegments := len(starts)
	requiredSegments := requiredTeaserSegments(duration, targetSegments, false)
	var lastErr error
	for i, start := range candidates {
		if len(segmentPaths) >= targetSegments {
			break
		}
		seg, err := g.generateSingleSegment(ctx2, i, start, eachSec, linkForInput)
		if err != nil {
			if !teaserSegmentFallbackAllowed(err) {
				return "", err
			}
			lastErr = err
			continue
		}
		segmentPaths = append(segmentPaths, seg)
	}
	if len(segmentPaths) < requiredSegments {
		degradedRequired := requiredTeaserSegments(duration, targetSegments, true)
		if lastErr == nil || len(segmentPaths) < degradedRequired {
			if lastErr != nil {
				return "", fmt.Errorf("only generated %d/%d teaser segments: %w", len(segmentPaths), targetSegments, lastErr)
			}
			return "", fmt.Errorf("only generated %d/%d teaser segments", len(segmentPaths), targetSegments)
		}
		log.Printf("[preview] degraded teaser accepted segments=%d/%d duration=%.1fs: %v",
			len(segmentPaths), targetSegments, duration, lastErr)
	}

	if len(segmentPaths) == 1 {
		success = true
		return segmentPaths[0], nil
	}

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
	out, err := exec.CommandContext(ctx2, g.cfg.FFmpegPath, args...).CombinedOutput()
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", ffmpegCommandError("ffmpeg concat", err, out)
	}
	if info, statErr := os.Stat(tmpPath); statErr != nil || info.Size() == 0 {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("ffmpeg concat produced empty file, stderr: %s", string(out))
	}
	if err := g.validateGeneratedTeaser(ctx2, tmpPath); err != nil {
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

func (g *Generator) generateSingleSegment(ctx context.Context, index int, start, eachSec float64, linkForInput func(int) (*drives.StreamLink, error)) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, teaserSegmentTimeout)
	defer cancel()

	link, err := linkForInput(index)
	if err != nil {
		return "", err
	}
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

	filter := teaserSegmentVideoFilter(g.cfg.Width, true, eachSec)

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
	}
	args = append(args, ffmpegHTTPInputOptions(ffmpegLink)...)
	args = append(args,
		"-ss", fmt.Sprintf("%.2f", start),
		"-t", fmt.Sprintf("%.2f", eachSec),
		"-i", ffmpegLink.URL,
		"-an",
		"-vf", filter,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "28",
		"-movflags", "+faststart",
		"-y", segPath,
	)
	out, err := exec.CommandContext(ctx, g.cfg.FFmpegPath, args...).CombinedOutput()
	if err != nil {
		_ = os.Remove(segPath)
		return "", ffmpegCommandError("ffmpeg segment", err, out)
	}
	if info, statErr := os.Stat(segPath); statErr != nil || info.Size() == 0 {
		_ = os.Remove(segPath)
		return "", fmt.Errorf("ffmpeg segment produced empty file, stderr: %s", string(out))
	}
	if err := g.validateGeneratedTeaser(ctx, segPath); err != nil {
		_ = os.Remove(segPath)
		return "", err
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
	return strings.Contains(text, "generated teaser has no video stream") ||
		strings.Contains(text, "generated teaser has invalid duration") ||
		strings.Contains(text, "generated teaser is empty") ||
		strings.Contains(text, "produced empty file") ||
		strings.Contains(text, "ffmpeg segment:") ||
		strings.Contains(text, "ffprobe teaser:")
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
	if err != nil {
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
	msg := fmt.Sprintf("%s: %v, stderr: %s", tool, err, redactURLs(string(output)))
	wrapped := errors.New(msg)
	if ffmpegOutputLooksRateLimited(output) {
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
	// TaskGuard holds application-level task admission for the complete provider
	// operation. A nil release means this worker belongs to a retired runtime
	// generation and the queued item must remain pending for its replacement.
	TaskGuard func() func()
	ch        chan *catalog.Video
	queue     videoQueue

	RateLimitCooldown time.Duration
	rateLimit         rateLimitState
	activity          taskActivity
}

func NewWorker(gen TeaserGenerator, cat *catalog.Catalog, drv drives.Drive) *Worker {
	return &Worker{
		Gen:     gen,
		Catalog: cat,
		Drive:   drv,
		ch:      make(chan *catalog.Video, defaultWorkerQueueSize),
	}
}

func (w *Worker) Enqueue(v *catalog.Video) bool {
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

type ThumbWorker struct {
	Gen       ThumbnailGenerator
	Catalog   *catalog.Catalog
	Drive     drives.Drive
	TaskGuard func() func()
	ch        chan *catalog.Video
	queue     videoQueue

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
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.ids)
	if currentID != "" {
		if _, ok := q.ids[currentID]; ok {
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
		Gen:     gen,
		Catalog: cat,
		Drive:   drv,
		ch:      make(chan *catalog.Video, defaultWorkerQueueSize),
	}
}

func (w *ThumbWorker) Enqueue(v *catalog.Video) bool {
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

func (w *Worker) Status() TaskStatus {
	if w == nil {
		return TaskStatus{State: "idle"}
	}
	currentID, _ := w.activity.current()
	return taskStatus(&w.activity, &w.rateLimit, w.queue.lengthExcluding(currentID))
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
	currentID, _ := w.activity.current()
	return taskStatus(&w.activity, &w.rateLimit, w.queue.lengthExcluding(currentID))
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
	return waitQueueIdle(ctx, &w.queue)
}

func waitQueueIdle(ctx context.Context, q *videoQueue) error {
	if q.lengthExcluding("") == 0 {
		return nil
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if q.lengthExcluding("") == 0 {
				return nil
			}
		}
	}
}

func taskStatus(activity *taskActivity, rateLimit *rateLimitState, queueLength int) TaskStatus {
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
	_, title := activity.current()
	if title != "" {
		status.State = "generating"
		status.CurrentTitle = title
		return status
	}
	if queueLength > 0 {
		status.State = "queued"
	}
	return status
}

// Run 阻塞运行直到 ctx 取消
func (w *Worker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case v := <-w.ch:
			w.processQueued(ctx, v)
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
		select {
		case <-ctx.Done():
			return
		case v := <-w.ch:
			w.processQueued(ctx, v)
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

func (w *Worker) processQueued(ctx context.Context, v *catalog.Video) {
	if v == nil {
		return
	}
	if w.Catalog == nil || v.ID == "" {
		w.queue.release(v)
		return
	}
	if w.TaskGuard != nil {
		release := w.TaskGuard()
		if release == nil {
			w.queue.release(v)
			return
		}
		defer release()
	}
	defer w.queue.release(v)
	if err := ctx.Err(); err != nil {
		return
	}
	current, err := w.Catalog.GetVideo(ctx, v.ID)
	if err != nil || current.Hidden {
		return
	}
	w.activity.start(current)
	defer w.activity.done()
	if !waitForRateLimitCooldown(ctx, &w.rateLimit, "preview", w.Drive) {
		return
	}
	w.process(ctx, current)
}

func (w *ThumbWorker) processQueued(ctx context.Context, v *catalog.Video) {
	if w.TaskGuard != nil {
		release := w.TaskGuard()
		if release == nil {
			w.queue.release(v)
			return
		}
		defer release()
	}
	if err := ctx.Err(); err != nil {
		w.queue.release(v)
		return
	}
	w.activity.start(v)
	retry := false
	if waitForRateLimitCooldown(ctx, &w.rateLimit, "thumb", w.Drive) {
		retry = w.process(ctx, v)
	}
	// Keep activity non-idle across release-and-requeue. Configuration admission
	// checks worker status while holding its queue gate; an idle gap here could
	// otherwise let a runtime update start just before this retry is queued.
	w.queue.release(v)
	if retry && ctx.Err() == nil {
		w.EnqueueBlocking(ctx, v)
	}
	w.activity.done()
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

func (w *ThumbWorker) pauseForRateLimit(err error, step, title string) bool {
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
	log.Printf("[thumb] drive=%s rate-limited until=%s step=%s video=%s: %v", w.Drive.ID(), until.Format(time.RFC3339), step, title, err)
	return true
}

func (w *ThumbWorker) pauseForRecoverableError(ctx context.Context, v *catalog.Video, err error, step string) bool {
	title := ""
	videoID := ""
	if v != nil {
		title = v.Title
		videoID = v.ID
	}
	if w.pauseForRateLimit(err, step, title) {
		return true
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
	until := w.rateLimit.pause(time.Now(), w.RateLimitCooldown)
	log.Printf("[thumb] drive=%s transient media source error until=%s failures=%d/%d step=%s video=%s: %v", w.Drive.ID(), until.Format(time.RFC3339), failures, defaultThumbTransientMediaMaxFailures, step, title, err)
	return true
}

func driveErrorShouldCooldown(d drives.Drive, err error) bool {
	if d == nil || err == nil {
		return false
	}
	switch d.Kind() {
	case "p115":
		return drives.ErrorMentionsHTTPStatus(err, http.StatusForbidden, http.StatusMethodNotAllowed, http.StatusTooManyRequests)
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
	queued := v
	loaded, err := w.Catalog.GetVideo(ctx, v.ID)
	if err != nil || loaded.Hidden {
		return false
	}
	if loaded.PreviewLocal == "" {
		loaded.PreviewLocal = queued.PreviewLocal
	}
	current := loaded
	v = loaded
	if loaded.ThumbnailURL != "" && loaded.DurationSeconds > 0 {
		_ = w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{ThumbnailStatus: "ready"})
		return false
	}
	if current.ThumbnailURL != "" {
		durationBackfillFailed := false
		if current.DurationSeconds <= 0 {
			link, _, err := w.streamLink(ctx, current)
			if err != nil {
				if w.pauseForRecoverableError(ctx, current, err, "streamURL") {
					return true
				}
				log.Printf("[thumb] probe streamURL %s: %v", current.Title, err)
				durationBackfillFailed = true
			} else if w.probeDuration(ctx, current, link) {
				return true
			} else if current.DurationSeconds <= 0 {
				durationBackfillFailed = true
			}
		}
		if durationBackfillFailed {
			log.Printf("[thumb] skip duration backfill %s: thumbnail already exists but duration could not be probed", current.Title)
			_ = w.Catalog.UpdateVideoMeta(ctx, current.ID, catalog.VideoMetaPatch{ThumbnailStatus: "skipped"})
			return false
		}
		_ = w.Catalog.UpdateVideoMeta(ctx, current.ID, catalog.VideoMetaPatch{ThumbnailStatus: "ready"})
		return false
	}
	_ = w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{ThumbnailStatus: "pending"})
	link, optimized, err := w.streamLink(ctx, v)
	if err != nil {
		if w.pauseForRecoverableError(ctx, v, err, "streamURL") {
			return true
		}
		log.Printf("[thumb] streamURL %s: %v", v.Title, err)
		_ = w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{ThumbnailStatus: "failed"})
		return false
	}
	if w.probeDuration(ctx, v, link) {
		return true
	}

	if err := w.generateThumbnailWithFallback(ctx, v, link, optimized); err != nil {
		if localLink, ok := localPreviewLink(v); ok && link.URL != localLink.URL {
			if w.probeDuration(ctx, v, localLink) {
				return true
			}
			if localErr := w.generateThumbnailFromLink(ctx, v, localLink); localErr == nil {
				return false
			}
		}
		if w.pauseForRecoverableError(ctx, v, err, "generate") {
			return true
		}
		log.Printf("[thumb] generate %s: %v", v.Title, err)
		_ = w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{ThumbnailStatus: "failed"})
		return false
	}
	return false
}

func (w *ThumbWorker) streamLink(ctx context.Context, v *catalog.Video) (*drives.StreamLink, bool, error) {
	link, optimized, err := preferredGenerationStreamLink(ctx, w.Drive, v.FileID)
	if err == nil {
		return link, optimized, nil
	}
	if localLink, ok := localPreviewLink(v); ok {
		return localLink, false, nil
	}
	return nil, false, err
}

func (w *ThumbWorker) probeDuration(ctx context.Context, v *catalog.Video, link *drives.StreamLink) bool {
	if v.DurationSeconds > 0 {
		return false
	}
	dur, err := w.Gen.Probe(ctx, link)
	if err == nil {
		if dur > 0 {
			v.DurationSeconds = int(dur)
			_ = w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{
				DurationSeconds: int(dur),
			})
		}
		return false
	}
	if w.pauseForRecoverableError(ctx, v, err, "probe") {
		return true
	}
	log.Printf("[thumb] probe %s: %v", v.Title, err)
	return false
}

func (w *ThumbWorker) generateThumbnailFromLink(ctx context.Context, v *catalog.Video, link *drives.StreamLink) error {
	persistence.RLock()
	defer persistence.RUnlock()
	local, err := w.Gen.GenerateThumbnail(ctx, link, v.ID, float64(v.DurationSeconds))
	if err != nil {
		return err
	}
	if err := w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{
		ThumbnailURL:    "/p/thumb/" + v.ID,
		ThumbnailStatus: "ready",
	}); err != nil {
		_ = os.Remove(local)
		log.Printf("[thumb] update %s after generate: %v", v.Title, err)
		return nil
	}
	log.Printf("[thumb] ready %s", v.Title)
	return nil
}

func (w *ThumbWorker) generateThumbnailWithFallback(ctx context.Context, v *catalog.Video, link *drives.StreamLink, optimized bool) error {
	err := w.generateThumbnailFromLink(ctx, v, link)
	if err == nil || !optimized {
		return err
	}
	if generationStreamForbidden(err) {
		refreshed, refreshErr := refreshGenerationStreamLink(ctx, w.Drive, v.FileID)
		switch {
		case refreshErr == nil:
			err = w.generateThumbnailFromLink(ctx, v, refreshed)
			if err == nil || generationStreamForbidden(err) || isRateLimitError(err) {
				return err
			}
		case !errors.Is(refreshErr, drives.ErrGenerationStreamUnavailable):
			return refreshErr
		}
	}
	if isRateLimitError(err) {
		return err
	}
	original, originalErr := w.Drive.StreamURL(ctx, v.FileID)
	if originalErr != nil {
		return originalErr
	}
	return w.generateThumbnailFromLink(ctx, v, original)
}

func localPreviewLink(v *catalog.Video) (*drives.StreamLink, bool) {
	if v.PreviewLocal == "" {
		return nil, false
	}
	clean := filepath.Clean(v.PreviewLocal)
	info, err := os.Stat(clean)
	if err != nil || info.IsDir() || info.Size() == 0 {
		return nil, false
	}
	return &drives.StreamLink{URL: clean}, true
}

func (w *Worker) process(ctx context.Context, v *catalog.Video) {
	if w.skipIfRateLimited(v) {
		return
	}
	link, optimized, err := preferredGenerationStreamLink(ctx, w.Drive, v.FileID)
	if err != nil {
		if w.pauseForRecoverableError(err, "streamURL", v.Title) {
			return
		}
		log.Printf("[preview] streamURL %s: %v", v.Title, err)
		w.Catalog.UpdatePreview(ctx, v.ID, "", "failed")
		return
	}

	// 1) 探时长（失败用 0 继续）
	duration := float64(v.DurationSeconds)
	if duration <= 0 {
		if dur, err := w.Gen.Probe(ctx, link); err == nil && dur > 0 {
			duration = dur
			_ = w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{
				DurationSeconds: int(dur),
			})
		} else if err != nil && w.pauseForRecoverableError(err, "probe", v.Title) {
			return
		}
	}

	// 2) 预览视频
	tmp, err := w.generateTeaser(ctx, v, link, duration, optimized)
	if err != nil {
		if w.pauseForRecoverableError(err, "generate", v.Title) {
			return
		}
		log.Printf("[preview] generate %s: %v", v.Title, err)
		w.Catalog.UpdatePreview(ctx, v.ID, "", "failed")
		return
	}
	persistence.RLock()
	defer persistence.RUnlock()
	local, err := w.Gen.MoveToLocal(tmp, v.ID)
	if err != nil {
		log.Printf("[preview] move %s: %v", v.Title, err)
		w.Catalog.UpdatePreview(ctx, v.ID, "", "failed")
		return
	}

	removePreviousLocalTeaser(v.PreviewLocal, local)
	if err := w.Catalog.UpdatePreview(ctx, v.ID, local, "ready"); err != nil {
		removePreviousLocalTeaser(local, "")
		log.Printf("[preview] update %s after generate: %v", v.Title, err)
		return
	}
	log.Printf("[preview] ready %s (duration=%.1fs)", v.Title, duration)
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
				if err == nil || generationStreamForbidden(err) || isRateLimitError(err) {
					return tmp, err
				}
			case !errors.Is(refreshErr, drives.ErrGenerationStreamUnavailable):
				return "", refreshErr
			}
		}
		if isRateLimitError(err) {
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
	return gen.GenerateWithLinkProvider(ctx, link, duration, func(ctx context.Context) (*drives.StreamLink, error) {
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
