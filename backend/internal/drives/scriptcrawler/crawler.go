package scriptcrawler

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/fingerprint"
	"github.com/video-site/backend/internal/mediaasset"
	"github.com/video-site/backend/internal/persistence"
	"golang.org/x/net/proxy"
)

const (
	DefaultTargetNew           = 10
	defaultUserAgent           = "Mozilla/5.0 (compatible; video-site-91-scriptcrawler/1.0)"
	defaultCandidateMultiplier = 10
	defaultCandidateFloorExtra = 50
	defaultCandidateBudgetMax  = 500
	defaultRunTimeout          = 3 * time.Hour
	defaultCandidateIdle       = 30 * time.Minute
	defaultV2IdleTimeout       = 5 * time.Minute
	defaultProgressInterval    = 60 * time.Second
	defaultDoneGrace           = 5 * time.Second
	defaultMaxStdoutBytes      = 64 * 1024 * 1024
	defaultMaxStderrBytes      = 1024 * 1024
	maxV1StdoutLineBytes       = 4 * 1024 * 1024
	maxV2StdoutLineBytes       = 1024 * 1024
	maxStderrLineBytes         = 8 * 1024
	crawlHistoryKeep           = 5
	crawlPartMaxAge            = 24 * time.Hour
)

type CrawlerConfig struct {
	Driver  *Driver
	Catalog *catalog.Catalog
	// GetDriveConfig can supply the task-generation snapshot while a newer
	// desired configuration is waiting for the current crawl to finish.
	GetDriveConfig func(context.Context, string) (*catalog.Drive, error)
	CrawlerName    string
	// Protocol is the protocol to start from. Every run re-reads it from the
	// script itself, so this is the value used before the first run and
	// whenever SkipProtocolRefresh keeps the script from being consulted.
	Protocol string
	// SkipProtocolRefresh keeps Protocol authoritative instead of re-reading
	// the script's metadata on each run. Test harnesses that point ScriptPath
	// at a stub set this; production crawlers must leave it false so an edited
	// script cannot keep running under its previously declared protocol.
	SkipProtocolRefresh  bool
	PythonPath           string
	FFmpegPath           string
	FFprobePath          string
	ScriptPath           string
	WorkDir              string
	CommonThumbDir       string
	ProxyURL             string
	ConfigJSON           string
	DisablePreview       bool
	HTTPClient           *http.Client
	DownloadTimeout      time.Duration
	RunTimeout           time.Duration
	CandidateIdleTimeout time.Duration
	IdleTimeout          time.Duration
	DoneGrace            time.Duration
	MaxStdoutBytes       int64
	MaxStderrBytes       int64
	OnProgress           func(CrawlProgress)
}

type Crawler struct {
	cfg                          CrawlerConfig
	runTimeoutExplicit           bool
	candidateIdleTimeoutExplicit bool
	hlsCapsOnce                  sync.Once
	hlsCaps                      ffmpegHLSCapabilities

	// protocol is the protocol resolved for the current run. It is refreshed
	// from the script under runMu, so cfg stays immutable after construction.
	protocol string
	runMu    sync.Mutex
}

func NewCrawler(cfg CrawlerConfig) *Crawler {
	runTimeoutExplicit := cfg.RunTimeout > 0
	candidateIdleTimeoutExplicit := cfg.CandidateIdleTimeout > 0
	if strings.TrimSpace(cfg.PythonPath) == "" {
		cfg.PythonPath = "python3"
	}
	if strings.TrimSpace(cfg.FFmpegPath) == "" {
		cfg.FFmpegPath = "ffmpeg"
	}
	if strings.TrimSpace(cfg.FFprobePath) == "" {
		cfg.FFprobePath = "ffprobe"
	}
	if cfg.DownloadTimeout <= 0 {
		cfg.DownloadTimeout = 30 * time.Minute
	}
	if strings.TrimSpace(cfg.Protocol) == "" {
		cfg.Protocol = ProtocolV1
	}
	cfg.Protocol = strings.TrimSpace(cfg.Protocol)
	if cfg.RunTimeout <= 0 {
		cfg.RunTimeout = defaultRunTimeout
	}
	if cfg.CandidateIdleTimeout <= 0 {
		cfg.CandidateIdleTimeout = defaultCandidateIdle
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaultV2IdleTimeout
	}
	if cfg.DoneGrace <= 0 {
		cfg.DoneGrace = defaultDoneGrace
	}
	if cfg.MaxStdoutBytes <= 0 {
		cfg.MaxStdoutBytes = defaultMaxStdoutBytes
	}
	if cfg.MaxStderrBytes <= 0 {
		cfg.MaxStderrBytes = defaultMaxStderrBytes
	}
	if cfg.HTTPClient == nil {
		transport := &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 60 * time.Second,
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
		}
		if err := configureExplicitProxy(transport, cfg.ProxyURL); err != nil {
			log.Printf("[scriptcrawler] invalid configured proxy URL, falling back to env: %v", err)
		}
		cfg.HTTPClient = &http.Client{Transport: transport}
	}
	return &Crawler{
		cfg:                          cfg,
		runTimeoutExplicit:           runTimeoutExplicit,
		candidateIdleTimeoutExplicit: candidateIdleTimeoutExplicit,
		protocol:                     cfg.Protocol,
	}
}

func (c *Crawler) effectiveRunTimeout() time.Duration {
	if c.protocol == ProtocolV2 || c.runTimeoutExplicit {
		return c.cfg.RunTimeout
	}
	return 0
}

func (c *Crawler) effectiveCandidateIdleTimeout() time.Duration {
	if c.protocol == ProtocolV2 || c.candidateIdleTimeoutExplicit {
		return c.cfg.CandidateIdleTimeout
	}
	return 0
}

type CrawlResult struct {
	TargetNew       int
	CandidateBudget int
	TotalEntries    int
	NewVideos       int
	Skipped         int
	Failed          int
	SeenSnapshot    int
	StartedAt       time.Time
	FinishedAt      time.Time
	JobFile         string
	SeenFile        string
}

type CrawlProgress struct {
	TargetNew    int
	TotalEntries int
	NewVideos    int
	Skipped      int
	Failed       int
	SeenSnapshot int
	Checked      int
	Emitted      int
	Message      string
}

type Job struct {
	Protocol          string          `json:"protocol"`
	Mode              string          `json:"mode"`
	RunID             string          `json:"run_id"`
	CrawlerID         string          `json:"crawler_id"`
	TargetNew         int             `json:"target_new"`
	UniqueTarget      int             `json:"unique_target,omitempty"`
	CandidateBudget   int             `json:"candidate_budget,omitempty"`
	SeenSourceIDsFile string          `json:"seen_source_ids_file"`
	OutputDir         string          `json:"output_dir"`
	Config            json.RawMessage `json:"config"`
	Network           JobNetwork      `json:"network"`
	Limits            *JobLimits      `json:"limits,omitempty"`
}

type JobNetwork struct {
	ProxyURL string `json:"proxy_url,omitempty"`
}

type JobLimits struct {
	MaxRuntimeSeconds           int    `json:"max_runtime_seconds"`
	DeadlineAt                  string `json:"deadline_at"`
	ProgressIntervalSeconds     int    `json:"progress_interval_seconds"`
	IdleTimeoutSeconds          int    `json:"idle_timeout_seconds"`
	CandidateIdleTimeoutSeconds int    `json:"candidate_idle_timeout_seconds"`
}

type Event struct {
	Type               string            `json:"type"`
	Item               Item              `json:"item"`
	SourceID           string            `json:"source_id,omitempty"`
	Title              string            `json:"title,omitempty"`
	MediaURL           string            `json:"media_url,omitempty"`
	MediaLocalFile     string            `json:"media_local_file,omitempty"`
	ThumbnailURL       string            `json:"thumbnail_url,omitempty"`
	ThumbnailLocalFile string            `json:"thumbnail_local_file,omitempty"`
	DetailURL          string            `json:"detail_url,omitempty"`
	Author             string            `json:"author,omitempty"`
	Tags               []string          `json:"tags,omitempty"`
	DurationSeconds    int               `json:"duration_seconds,omitempty"`
	Description        string            `json:"description,omitempty"`
	Headers            map[string]string `json:"headers,omitempty"`
	MediaHeaders       map[string]string `json:"media_headers,omitempty"`
	ThumbnailHeaders   map[string]string `json:"thumbnail_headers,omitempty"`
	Checked            int               `json:"checked,omitempty"`
	Emitted            int               `json:"emitted,omitempty"`
	Message            string            `json:"message,omitempty"`
	Stats              json.RawMessage   `json:"stats,omitempty"`
}

type Item struct {
	SourceID           string            `json:"source_id,omitempty"`
	Title              string            `json:"title"`
	MediaURL           string            `json:"media_url,omitempty"`
	MediaLocalFile     string            `json:"media_local_file,omitempty"`
	ThumbnailURL       string            `json:"thumbnail_url,omitempty"`
	ThumbnailLocalFile string            `json:"thumbnail_local_file,omitempty"`
	DetailURL          string            `json:"detail_url,omitempty"`
	Author             string            `json:"author,omitempty"`
	Tags               []string          `json:"tags,omitempty"`
	DurationSeconds    int               `json:"duration_seconds,omitempty"`
	Description        string            `json:"description,omitempty"`
	Headers            map[string]string `json:"headers,omitempty"`
	MediaHeaders       map[string]string `json:"media_headers,omitempty"`
	ThumbnailHeaders   map[string]string `json:"thumbnail_headers,omitempty"`
	Media              MediaRef          `json:"media,omitempty"`
	Thumbnail          MediaRef          `json:"thumbnail,omitempty"`
}

type MediaRef struct {
	URL       string            `json:"url,omitempty"`
	LocalFile string            `json:"local_file,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

func (e Event) normalizedItem() Item {
	item := e.Item
	if strings.TrimSpace(item.SourceID) == "" {
		item.SourceID = e.SourceID
	}
	if strings.TrimSpace(item.Title) == "" {
		item.Title = e.Title
	}
	if strings.TrimSpace(item.MediaURL) == "" {
		item.MediaURL = e.MediaURL
	}
	if strings.TrimSpace(item.MediaLocalFile) == "" {
		item.MediaLocalFile = e.MediaLocalFile
	}
	if strings.TrimSpace(item.ThumbnailURL) == "" {
		item.ThumbnailURL = e.ThumbnailURL
	}
	if strings.TrimSpace(item.ThumbnailLocalFile) == "" {
		item.ThumbnailLocalFile = e.ThumbnailLocalFile
	}
	if strings.TrimSpace(item.DetailURL) == "" {
		item.DetailURL = e.DetailURL
	}
	if strings.TrimSpace(item.Author) == "" {
		item.Author = e.Author
	}
	if len(item.Tags) == 0 && len(e.Tags) > 0 {
		item.Tags = e.Tags
	}
	if item.DurationSeconds == 0 {
		item.DurationSeconds = e.DurationSeconds
	}
	if strings.TrimSpace(item.Description) == "" {
		item.Description = e.Description
	}
	if len(item.Headers) == 0 && len(e.Headers) > 0 {
		item.Headers = e.Headers
	}
	if len(item.MediaHeaders) == 0 && len(e.MediaHeaders) > 0 {
		item.MediaHeaders = e.MediaHeaders
	}
	if len(item.ThumbnailHeaders) == 0 && len(e.ThumbnailHeaders) > 0 {
		item.ThumbnailHeaders = e.ThumbnailHeaders
	}
	return item
}

func (item Item) hasPayload() bool {
	return strings.TrimSpace(item.Title) != "" ||
		strings.TrimSpace(item.SourceID) != "" ||
		strings.TrimSpace(item.MediaURL) != "" ||
		strings.TrimSpace(item.MediaLocalFile) != "" ||
		strings.TrimSpace(item.Media.URL) != "" ||
		strings.TrimSpace(item.Media.LocalFile) != ""
}

func (c *Crawler) RunOnce(ctx context.Context, targetNew int) (*CrawlResult, error) {
	c.runMu.Lock()
	defer c.runMu.Unlock()

	if c.cfg.Driver == nil {
		return nil, errors.New("scriptcrawler: driver not set")
	}
	if c.cfg.Catalog == nil {
		return nil, errors.New("scriptcrawler: catalog not set")
	}
	if strings.TrimSpace(c.cfg.ScriptPath) == "" {
		return nil, errors.New("scriptcrawler: script_path is required")
	}
	if _, err := os.Stat(c.cfg.ScriptPath); err != nil {
		return nil, fmt.Errorf("scriptcrawler: script not found: %w", err)
	}
	// A crawler script can be replaced in place between runs. Refresh the
	// protocol from the file itself so a scheduled run validates exactly what
	// dry-run just validated, instead of the protocol seen when the drive was
	// attached.
	if !c.cfg.SkipProtocolRefresh {
		metadata, err := ReadMetadata(c.cfg.ScriptPath)
		if err != nil {
			return nil, fmt.Errorf("scriptcrawler: script metadata: %w", err)
		}
		c.protocol = metadata.Protocol
	}
	if c.protocol != ProtocolV1 && c.protocol != ProtocolV2 {
		return nil, fmt.Errorf("scriptcrawler: unsupported protocol %q", c.protocol)
	}
	if targetNew <= 0 {
		targetNew = DefaultTargetNew
	}
	candidateBudget := candidateBudgetForTarget(targetNew)
	if err := c.cfg.Driver.Init(ctx); err != nil {
		return nil, fmt.Errorf("scriptcrawler: driver init: %w", err)
	}

	result := &CrawlResult{TargetNew: targetNew, CandidateBudget: candidateBudget, StartedAt: time.Now()}
	defer func() { result.FinishedAt = time.Now() }()
	emit := func(p CrawlProgress) {
		if c.cfg.OnProgress == nil {
			return
		}
		p.TargetNew = result.TargetNew
		p.TotalEntries = result.TotalEntries
		p.NewVideos = result.NewVideos
		p.Skipped = result.Skipped
		p.Failed = result.Failed
		p.SeenSnapshot = result.SeenSnapshot
		c.cfg.OnProgress(p)
	}
	emit(CrawlProgress{})

	crawlDir, err := filepath.Abs(c.cfg.Driver.CrawlDir())
	if err != nil {
		return result, fmt.Errorf("scriptcrawler: resolve crawl dir: %w", err)
	}
	if err := os.MkdirAll(crawlDir, 0o755); err != nil {
		return result, err
	}
	pruneCrawlDir(crawlDir)
	runID := time.Now().UTC().Format("20060102T150405Z")
	seenPath := filepath.Join(crawlDir, "seen-"+runID+".txt")
	jobPath := filepath.Join(crawlDir, "job-"+runID+".json")
	result.SeenFile = seenPath
	result.JobFile = jobPath

	seenCount, err := c.writeSeenSourceIDs(ctx, seenPath)
	if err != nil {
		return result, fmt.Errorf("scriptcrawler: build seen list: %w", err)
	}
	result.SeenSnapshot = seenCount
	emit(CrawlProgress{})

	runTimeout := c.effectiveRunTimeout()
	var deadline time.Time
	if runTimeout > 0 {
		deadline = time.Now().Add(runTimeout).UTC()
	}
	if err := c.writeJobFile(jobPath, runID, targetNew, candidateBudget, seenPath, deadline); err != nil {
		return result, fmt.Errorf("scriptcrawler: write job: %w", err)
	}

	runCtx := ctx
	cancel := func() {}
	if !deadline.IsZero() {
		runCtx, cancel = context.WithDeadline(ctx, deadline)
	}
	defer cancel()
	if err := c.executeScript(runCtx, ctx, jobPath, targetNew, candidateBudget, result, emit); err != nil {
		return result, err
	}
	return result, nil
}

func (c *Crawler) writeSeenSourceIDs(ctx context.Context, path string) (int, error) {
	seenIDs, err := c.cfg.Catalog.ListCrawlerSourceIDs(ctx, Kind, c.cfg.Driver.ID())
	if err != nil {
		return 0, err
	}
	seen := make(map[string]struct{}, len(seenIDs))
	for _, id := range seenIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			seen[id] = struct{}{}
		}
	}
	tmp := path + ".part"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	for id := range seen {
		if _, err := f.WriteString(id + "\n"); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return 0, err
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return len(seen), nil
}

// pruneCrawlDir bounds the .crawl history. Every run writes a fresh
// seen-/job- file pair, so a scheduled crawler would otherwise accumulate
// them forever. The newest crawlHistoryKeep of each kind survive for
// post-mortem debugging. Stale .part leftovers from interrupted writes are
// removed once they are old enough to not belong to a live run.
func pruneCrawlDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var seenFiles, jobFiles []string
	now := time.Now()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		switch {
		case strings.HasSuffix(name, ".part"):
			if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > crawlPartMaxAge {
				_ = os.Remove(filepath.Join(dir, name))
			}
		case strings.HasPrefix(name, "seen-") && strings.HasSuffix(name, ".txt"):
			seenFiles = append(seenFiles, name)
		case strings.HasPrefix(name, "job-") && strings.HasSuffix(name, ".json"):
			jobFiles = append(jobFiles, name)
		}
	}
	for _, group := range [][]string{seenFiles, jobFiles} {
		if len(group) <= crawlHistoryKeep {
			continue
		}
		// Run IDs are UTC timestamps, so lexical order is chronological.
		sort.Strings(group)
		for _, name := range group[:len(group)-crawlHistoryKeep] {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

func (c *Crawler) writeJobFile(path, runID string, targetNew, candidateBudget int, seenPath string, deadline time.Time) error {
	cfg := json.RawMessage([]byte("{}"))
	if raw := strings.TrimSpace(c.cfg.ConfigJSON); raw != "" {
		if !json.Valid([]byte(raw)) {
			return errors.New("config_json must be valid JSON")
		}
		cfg = json.RawMessage(raw)
	}
	outputDir, err := filepath.Abs(c.cfg.Driver.OutputDir())
	if err != nil {
		return fmt.Errorf("resolve output dir: %w", err)
	}
	job := Job{
		Protocol:          c.protocol,
		Mode:              "crawl",
		RunID:             runID,
		CrawlerID:         c.cfg.Driver.ID(),
		TargetNew:         candidateBudget,
		UniqueTarget:      targetNew,
		CandidateBudget:   candidateBudget,
		SeenSourceIDsFile: seenPath,
		OutputDir:         outputDir,
		Config:            cfg,
		Network:           JobNetwork{ProxyURL: strings.TrimSpace(c.cfg.ProxyURL)},
	}
	if c.protocol == ProtocolV2 {
		job.Limits = &JobLimits{
			MaxRuntimeSeconds:           durationSeconds(c.cfg.RunTimeout),
			DeadlineAt:                  deadline.UTC().Format(time.RFC3339),
			ProgressIntervalSeconds:     durationSeconds(defaultProgressInterval),
			IdleTimeoutSeconds:          durationSeconds(c.cfg.IdleTimeout),
			CandidateIdleTimeoutSeconds: durationSeconds(c.cfg.CandidateIdleTimeout),
		}
	}
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".part"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *Crawler) startScript(ctx context.Context, jobPath string, targetNew, candidateBudget int) (*exec.Cmd, io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, c.cfg.PythonPath, c.cfg.ScriptPath, "--job", jobPath)
	setCrawlerProcAttr(cmd)
	cmd.Cancel = func() error {
		return killCrawlerProcess(cmd)
	}
	cmd.WaitDelay = 3 * time.Second
	if strings.TrimSpace(c.cfg.WorkDir) != "" {
		cmd.Dir = c.cfg.WorkDir
	}
	if proxyURL := strings.TrimSpace(c.cfg.ProxyURL); proxyURL != "" {
		cmd.Env = append(os.Environ(),
			"HTTP_PROXY="+proxyURL,
			"HTTPS_PROXY="+proxyURL,
			"http_proxy="+proxyURL,
			"https_proxy="+proxyURL,
			"NO_PROXY=",
			"no_proxy=",
		)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, nil, err
	}
	log.Printf("[scriptcrawler] drive=%s exec %s --job=%s unique_target=%d candidate_budget=%d", c.cfg.Driver.ID(), c.cfg.ScriptPath, jobPath, targetNew, candidateBudget)
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, nil, err
	}
	go forwardScriptLog(c.cfg.Driver.ID(), stderr, c.cfg.MaxStderrBytes)
	return cmd, stdout, nil
}

func forwardScriptLog(driveID string, r io.Reader, maxBytes int64) {
	reader := bufio.NewReaderSize(r, maxStderrLineBytes)
	line := make([]byte, 0, maxStderrLineBytes)
	var consumed int64
	var suppressed int64
	lineTruncated := false
	flushLine := func() {
		trimmed := strings.TrimSpace(string(line))
		if trimmed != "" {
			if lineTruncated {
				trimmed += "…"
			}
			log.Printf("[scriptcrawler:script] drive=%s %s", driveID, trimmed)
		}
		line = line[:0]
		lineTruncated = false
	}
	for {
		fragment, err := reader.ReadSlice('\n')
		accepted := fragment
		if remaining := maxBytes - consumed; remaining <= 0 {
			accepted = nil
			suppressed += int64(len(fragment))
		} else if int64(len(accepted)) > remaining {
			accepted = accepted[:remaining]
			consumed += int64(len(accepted))
			suppressed += int64(len(fragment) - len(accepted))
		} else {
			consumed += int64(len(accepted))
		}
		if len(accepted) > 0 {
			content := accepted
			if content[len(content)-1] == '\n' {
				content = content[:len(content)-1]
			}
			if remaining := maxStderrLineBytes - len(line); remaining > 0 {
				toKeep := content
				if len(toKeep) > remaining {
					toKeep = toKeep[:remaining]
				}
				line = append(line, toKeep...)
				if len(toKeep) < len(content) {
					lineTruncated = true
					suppressed += int64(len(content) - len(toKeep))
				}
			} else if len(content) > 0 {
				lineTruncated = true
				suppressed += int64(len(content))
			}
		}
		if len(fragment) > 0 && fragment[len(fragment)-1] == '\n' {
			flushLine()
		}
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != io.EOF && !errors.Is(err, os.ErrClosed) {
				log.Printf("[scriptcrawler:script] drive=%s stderr read: %v", driveID, err)
			}
			if len(line) > 0 || lineTruncated {
				flushLine()
			}
			break
		}
	}
	if suppressed > 0 {
		log.Printf("[scriptcrawler:script] drive=%s suppressed %d stderr bytes after limit", driveID, suppressed)
	}
}

func durationSeconds(value time.Duration) int {
	seconds := int(value / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

func (c *Crawler) processItem(ctx context.Context, item Item) (bool, error) {
	item, sourceID, err := normalizeItemForImport(item)
	if err != nil {
		return false, err
	}
	videoID := BuildVideoID(c.cfg.Driver.ID(), sourceID)
	if deleted, err := c.cfg.Catalog.IsVideoDeleted(ctx, videoID); err != nil {
		return false, err
	} else if deleted {
		return false, nil
	}
	if existing, _ := c.cfg.Catalog.GetVideo(ctx, videoID); existing != nil {
		return false, nil
	}
	videoExt := detectVideoExt(item.Media.URL, item.Media.LocalFile)
	videoFile := sourceID + videoExt
	videoPath, err := c.cfg.Driver.VideoPath(videoFile)
	if err != nil {
		return false, err
	}
	size, err := c.materializeMedia(ctx, item.Media, videoPath, item.DetailURL, true)
	if err != nil {
		return false, fmt.Errorf("video: %w", err)
	}
	if err := c.validateDownloadedVideo(ctx, videoPath); err != nil {
		_ = os.Remove(videoPath)
		return false, fmt.Errorf("video invalid: %w", err)
	}

	now := time.Now()
	title := strings.TrimSpace(item.Title)
	if title == "" {
		title = sourceID
	}
	author := strings.TrimSpace(item.Author)
	// 标签策略：
	//   1. 脚本返回的 tags 只挂已存在的标签，不自动创建新标签 → source=crawler；
	//   2. 规则引擎按标题/文件名/作者匹配已有标签池 → source=auto；
	//   3. 视频成功入库后才确保并强制挂载爬虫名标签（不受人工锁定影响）。
	var tagAssignments []catalog.TagAssignment
	tagLabelSeen := map[string]bool{}
	crawlerTagLabel := ""
	appendAssignment := func(a catalog.TagAssignment) {
		key := strings.ToLower(strings.TrimSpace(a.Label))
		if key == "" || tagLabelSeen[key] {
			return
		}
		tagLabelSeen[key] = true
		tagAssignments = append(tagAssignments, a)
	}
	for _, scriptTag := range cleanStringList(item.Tags) {
		label, ok, err := c.cfg.Catalog.LookupTagLabel(ctx, scriptTag)
		if err != nil || !ok {
			continue
		}
		appendAssignment(catalog.TagAssignment{Label: label, Source: "crawler", Evidence: "脚本标签"})
	}
	if matched, err := c.cfg.Catalog.MatchTagAssignments(ctx, title, videoFile, author, ""); err == nil {
		for _, a := range matched {
			appendAssignment(a)
		}
	}
	crawlerTagLabel = c.crawlerTagName()
	previewStatus := "pending"
	if c.previewDisabled(ctx) {
		previewStatus = "disabled"
	}
	v := &catalog.Video{
		ID:              videoID,
		DriveID:         c.cfg.Driver.ID(),
		FileID:          videoFile,
		FileName:        videoFile,
		Title:           title,
		Author:          author,
		DurationSeconds: item.DurationSeconds,
		Size:            size,
		Ext:             strings.TrimPrefix(videoExt, "."),
		Description:     strings.TrimSpace(item.Description),
		PreviewStatus:   previewStatus,
		PublishedAt:     now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	sampled, err := fingerprint.Compute(ctx, c.cfg.Driver, v, fingerprint.Config{}, c.cfg.HTTPClient)
	if err != nil {
		_ = os.Remove(videoPath)
		return false, fmt.Errorf("fingerprint: %w", err)
	}
	v.SampledSHA256 = sampled
	v.FingerprintStatus = "ready"
	if duplicate, err := c.cfg.Catalog.FindVideoBySampledFingerprint(ctx, v); err == nil && duplicate != nil {
		_ = os.Remove(videoPath)
		if markErr := c.cfg.Catalog.MarkCrawlerSourceSeen(ctx, Kind, c.cfg.Driver.ID(), sourceID, "duplicate", duplicate.ID, sampled, size); markErr != nil {
			log.Printf("[scriptcrawler] drive=%s source_id=%s mark duplicate seen: %v", c.cfg.Driver.ID(), sourceID, markErr)
		}
		log.Printf("[scriptcrawler] drive=%s source_id=%s duplicate_of=%s title=%q size=%d", c.cfg.Driver.ID(), sourceID, duplicate.ID, title, size)
		return false, nil
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		_ = os.Remove(videoPath)
		return false, fmt.Errorf("duplicate lookup: %w", err)
	}

	thumbReady := false
	thumbPath := ""
	commonThumbPath := ""
	if item.Thumbnail.URL != "" || item.Thumbnail.LocalFile != "" {
		thumbFile := sourceID + detectThumbExt(item.Thumbnail.URL, item.Thumbnail.LocalFile)
		thumbPath, err = c.cfg.Driver.ThumbPath(thumbFile)
		if err == nil {
			if _, err := c.materializeMedia(ctx, item.Thumbnail, thumbPath, item.DetailURL, false); err != nil {
				log.Printf("[scriptcrawler] drive=%s source_id=%s thumbnail failed: %v", c.cfg.Driver.ID(), sourceID, err)
			} else if c.cfg.CommonThumbDir != "" {
				if err := os.MkdirAll(c.cfg.CommonThumbDir, 0o755); err != nil {
					log.Printf("[scriptcrawler] drive=%s common thumbs mkdir: %v", c.cfg.Driver.ID(), err)
				} else {
					dst := mediaasset.ThumbnailPathInDir(c.cfg.CommonThumbDir, videoID)
					if err := mediaasset.NormalizeThumbnailJPEG(thumbPath, dst); err != nil {
						log.Printf("[scriptcrawler] drive=%s source_id=%s normalize thumbnail: %v", c.cfg.Driver.ID(), sourceID, err)
					} else {
						commonThumbPath = dst
						thumbReady = true
					}
				}
			}
		}
	}
	if thumbReady {
		v.ThumbnailURL = "/p/thumb/" + v.ID
	}
	duplicate, err := c.findNearDuplicateVideo(ctx, v, commonThumbPath, videoPath)
	if err != nil {
		_ = os.Remove(videoPath)
		if thumbPath != "" {
			_ = os.Remove(thumbPath)
		}
		if commonThumbPath != "" {
			_ = os.Remove(commonThumbPath)
		}
		return false, fmt.Errorf("near duplicate lookup: %w", err)
	}
	// Media and thumbnail downloads above are written through .part files.
	// Coordinate only the final file/catalog cleanup and publication.
	persistence.RLock()
	defer persistence.RUnlock()
	publishedByReplacement := false
	if duplicate != nil && duplicate.video != nil {
		if v.Size > duplicate.video.Size {
			if err := c.cfg.Catalog.ReplaceDuplicateVideo(ctx, catalog.DuplicateVideoReplacement{
				NewVideo:                  v,
				ReplacedVideoID:           duplicate.video.ID,
				ExpectedReplacedUpdatedAt: duplicate.video.UpdatedAt.UnixMilli(),
				CrawlerSource: &catalog.CrawlerSourceSeen{
					Kind: Kind, DriveID: c.cfg.Driver.ID(), SourceID: sourceID,
					Status: "imported", SampledSHA256: sampled, Size: size,
				},
			}); err != nil {
				_ = os.Remove(videoPath)
				if thumbPath != "" {
					_ = os.Remove(thumbPath)
				}
				if commonThumbPath != "" {
					_ = os.Remove(commonThumbPath)
				}
				return false, fmt.Errorf("replace smaller near duplicate %s: %w", duplicate.video.ID, err)
			}
			publishedByReplacement = true
			c.cleanupReplacedDuplicateAssets(ctx, duplicate.video)
			log.Printf("[scriptcrawler] drive=%s source_id=%s replacing_smaller_near_duplicate=%s old_size=%d new_size=%d title_similarity=%.3f thumbnail_ssim=%.3f content_ssim=%.3f title=%q duration=%d", c.cfg.Driver.ID(), sourceID, duplicate.video.ID, duplicate.video.Size, v.Size, duplicate.titleSimilarity, duplicate.thumbnailSSIM, duplicate.contentSSIM, title, v.DurationSeconds)
		} else {
			_ = os.Remove(videoPath)
			if thumbPath != "" {
				_ = os.Remove(thumbPath)
			}
			if commonThumbPath != "" {
				_ = os.Remove(commonThumbPath)
			}
			if markErr := c.cfg.Catalog.MarkCrawlerSourceSeen(ctx, Kind, c.cfg.Driver.ID(), sourceID, "duplicate", duplicate.video.ID, sampled, size); markErr != nil {
				log.Printf("[scriptcrawler] drive=%s source_id=%s mark near duplicate seen: %v", c.cfg.Driver.ID(), sourceID, markErr)
			}
			log.Printf("[scriptcrawler] drive=%s source_id=%s near_duplicate_of=%s old_size=%d new_size=%d title_similarity=%.3f thumbnail_ssim=%.3f content_ssim=%.3f title=%q duration=%d", c.cfg.Driver.ID(), sourceID, duplicate.video.ID, duplicate.video.Size, v.Size, duplicate.titleSimilarity, duplicate.thumbnailSSIM, duplicate.contentSSIM, title, v.DurationSeconds)
			return false, nil
		}
	}
	if !publishedByReplacement {
		if err := c.cfg.Catalog.UpsertVideo(ctx, v); err != nil {
			_ = os.Remove(videoPath)
			return false, err
		}
	}
	if len(tagAssignments) > 0 {
		if _, err := c.cfg.Catalog.AddVideoTagAssignments(ctx, v.ID, tagAssignments); err != nil {
			log.Printf("[scriptcrawler] drive=%s source_id=%s attach tags: %v", c.cfg.Driver.ID(), sourceID, err)
		} else {
			for _, a := range tagAssignments {
				v.Tags = append(v.Tags, a.Label)
			}
		}
	}
	if crawlerTagLabel != "" {
		if _, err := c.cfg.Catalog.EnsureCrawlerTagForVideo(ctx, v.ID, crawlerTagLabel); err != nil {
			log.Printf("[scriptcrawler] drive=%s source_id=%s attach crawler tag %q: %v", c.cfg.Driver.ID(), sourceID, crawlerTagLabel, err)
		} else {
			seen := false
			for _, label := range v.Tags {
				if strings.EqualFold(label, crawlerTagLabel) {
					seen = true
					break
				}
			}
			if !seen {
				v.Tags = append(v.Tags, crawlerTagLabel)
			}
		}
	}
	if !publishedByReplacement {
		if err := c.cfg.Catalog.MarkCrawlerSourceSeen(ctx, Kind, c.cfg.Driver.ID(), sourceID, "imported", v.ID, sampled, size); err != nil {
			log.Printf("[scriptcrawler] drive=%s source_id=%s mark imported seen: %v", c.cfg.Driver.ID(), sourceID, err)
		}
	}
	log.Printf("[scriptcrawler] drive=%s source_id=%s ok title=%q size=%d", c.cfg.Driver.ID(), sourceID, title, size)
	return true, nil
}

func (c *Crawler) cleanupReplacedDuplicateAssets(ctx context.Context, video *catalog.Video) {
	if c == nil || c.cfg.Catalog == nil || video == nil || strings.TrimSpace(c.cfg.CommonThumbDir) == "" {
		return
	}
	localDir := filepath.Dir(strings.TrimSpace(c.cfg.CommonThumbDir))
	if err := mediaasset.RemoveGeneratedVideoAssets(localDir, video.ID, video.PreviewLocal); err != nil {
		if markErr := c.cfg.Catalog.FailDuplicateAssetCleanupJob(ctx, video.ID, err); markErr != nil {
			log.Printf("[scriptcrawler] record duplicate asset cleanup failure video=%s: %v", video.ID, markErr)
		}
		log.Printf("[scriptcrawler] duplicate asset cleanup video=%s: %v", video.ID, err)
		return
	}
	if err := c.cfg.Catalog.CompleteDuplicateAssetCleanupJob(ctx, video.ID); err != nil {
		log.Printf("[scriptcrawler] complete duplicate asset cleanup video=%s: %v", video.ID, err)
	}
}

// RestoreRequestedVideos scans the crawler's retained local video directory
// after the crawl/generation/upload pipeline has finished. Only tombstones that
// the user explicitly removed from the blacklist are eligible.
func (c *Crawler) RestoreRequestedVideos(ctx context.Context) (int, error) {
	if c == nil || c.cfg.Driver == nil || c.cfg.Catalog == nil {
		return 0, errors.New("scriptcrawler: restore dependencies not set")
	}
	if err := c.cfg.Driver.Init(ctx); err != nil {
		return 0, fmt.Errorf("scriptcrawler: restore driver init: %w", err)
	}
	requests, err := c.cfg.Catalog.ListCrawlerRestoreRequests(ctx, c.cfg.Driver.ID())
	if err != nil || len(requests) == 0 {
		return 0, err
	}
	entries, err := c.cfg.Driver.List(ctx, c.cfg.Driver.RootID())
	if err != nil {
		return 0, fmt.Errorf("scriptcrawler: scan retained videos: %w", err)
	}
	files := make(map[string]struct {
		size    int64
		modTime time.Time
	}, len(entries))
	for _, entry := range entries {
		if entry.IsDir || entry.Size <= 0 {
			continue
		}
		files[entry.ID] = struct {
			size    int64
			modTime time.Time
		}{size: entry.Size, modTime: entry.ModTime}
	}

	restored := 0
	var restoreErrors []error
	for _, request := range requests {
		if err := ctx.Err(); err != nil {
			return restored, err
		}
		fileID := strings.TrimSpace(request.FileID)
		file, ok := files[fileID]
		if !ok {
			continue
		}

		video := &catalog.Video{}
		if request.Video != nil {
			copy := *request.Video
			video = &copy
		}
		video.ID = request.ID
		video.DriveID = c.cfg.Driver.ID()
		video.FileID = fileID
		if strings.TrimSpace(video.FileName) == "" {
			video.FileName = strings.TrimSpace(request.FileName)
		}
		if strings.TrimSpace(video.FileName) == "" {
			video.FileName = fileID
		}
		if strings.TrimSpace(video.Title) == "" {
			video.Title = strings.TrimSuffix(video.FileName, filepath.Ext(video.FileName))
		}
		video.Ext = strings.TrimPrefix(strings.ToLower(filepath.Ext(fileID)), ".")
		if request.Size != file.size {
			video.SampledSHA256 = ""
			video.FingerprintStatus = "pending"
			video.FingerprintError = ""
		}
		video.Size = file.size
		video.ThumbnailURL = ""
		video.PreviewFileID = ""
		video.PreviewLocal = ""
		video.PreviewStatus = "pending"
		if c.previewDisabled(ctx) {
			video.PreviewStatus = "disabled"
		}
		video.TranscodeStatus = ""
		video.TranscodeError = ""
		video.TranscodedFileID = ""
		video.TranscodedSize = 0
		if video.CreatedAt.IsZero() {
			video.CreatedAt = file.modTime
		}
		// Crawler timestamps are backend-owned. Restoring an older crawler
		// tombstone must not reintroduce a source-supplied publication date.
		video.PublishedAt = video.CreatedAt
		if c.restoreCrawlerThumbnail(video, fileID) {
			video.ThumbnailURL = "/p/thumb/" + video.ID
		}

		if err := c.cfg.Catalog.UpsertVideo(ctx, video); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore %s: %w", request.ID, err))
			continue
		}
		sourceID := strings.TrimPrefix(request.ID, BuildVideoID(c.cfg.Driver.ID(), ""))
		if sourceID != "" {
			if err := c.cfg.Catalog.MarkCrawlerSourceSeen(ctx, Kind, c.cfg.Driver.ID(), sourceID, "imported", video.ID, video.SampledSHA256, video.Size); err != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf("restore %s seen source: %w", request.ID, err))
				continue
			}
		}
		if err := c.cfg.Catalog.CompleteCrawlerRestore(ctx, request.ID); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("complete restore %s: %w", request.ID, err))
			continue
		}
		restored++
		log.Printf("[scriptcrawler] drive=%s restored retained video=%s file=%s size=%d", c.cfg.Driver.ID(), request.ID, fileID, file.size)
	}
	return restored, errors.Join(restoreErrors...)
}

func (c *Crawler) restoreCrawlerThumbnail(video *catalog.Video, fileID string) bool {
	if video == nil || strings.TrimSpace(c.cfg.CommonThumbDir) == "" {
		return false
	}
	stem := strings.TrimSuffix(fileID, filepath.Ext(fileID))
	for _, ext := range []string{".jpg", ".jpeg", ".png", ".webp"} {
		source, err := c.cfg.Driver.ThumbPath(stem + ext)
		if err != nil {
			continue
		}
		info, err := os.Stat(source)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			continue
		}
		if err := os.MkdirAll(c.cfg.CommonThumbDir, 0o755); err != nil {
			return false
		}
		if err := mediaasset.NormalizeThumbnailJPEG(source, mediaasset.ThumbnailPathInDir(c.cfg.CommonThumbDir, video.ID)); err != nil {
			log.Printf("[scriptcrawler] drive=%s restore thumbnail video=%s: %v", c.cfg.Driver.ID(), video.ID, err)
			return false
		}
		return true
	}
	return false
}

func (c *Crawler) previewDisabled(ctx context.Context) bool {
	if c == nil {
		return false
	}
	if c.cfg.Driver != nil {
		if c.cfg.GetDriveConfig != nil {
			if d, err := c.cfg.GetDriveConfig(ctx, c.cfg.Driver.ID()); err == nil && d != nil {
				return !d.TeaserEnabled
			}
		} else if c.cfg.Catalog != nil {
			if d, err := c.cfg.Catalog.GetDrive(ctx, c.cfg.Driver.ID()); err == nil && d != nil {
				return !d.TeaserEnabled
			}
		}
	}
	return c.cfg.DisablePreview
}

func (c *Crawler) materializeMedia(ctx context.Context, ref MediaRef, dst, referer string, required bool) (int64, error) {
	if local := strings.TrimSpace(ref.LocalFile); local != "" {
		return c.copyLocalOutput(local, dst)
	}
	if rawURL := strings.TrimSpace(ref.URL); rawURL != "" {
		attemptCtx, cancel := c.downloadAttemptContext(ctx)
		defer cancel()
		return c.downloadAtomic(attemptCtx, ref, dst, referer)
	}
	if required {
		return 0, errors.New("missing url or local_file")
	}
	return 0, nil
}

func (c *Crawler) validateDownloadedVideo(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_type",
		"-of", "csv=p=0",
		path,
	}
	out, err := exec.CommandContext(ctx, c.cfg.FFprobePath, args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("ffprobe: %s", msg)
	}
	if !strings.Contains(strings.ToLower(string(out)), "video") {
		return errors.New("ffprobe: no video stream")
	}
	return nil
}

func (c *Crawler) copyLocalOutput(src, dst string) (int64, error) {
	outputRoot, err := filepath.Abs(c.cfg.Driver.OutputDir())
	if err != nil {
		return 0, err
	}
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return 0, err
	}
	if srcAbs != outputRoot && !strings.HasPrefix(srcAbs, outputRoot+string(os.PathSeparator)) {
		return 0, errors.New("local_file must be inside job output_dir")
	}
	info, err := os.Stat(srcAbs)
	if err != nil {
		return 0, err
	}
	if info.IsDir() || info.Size() == 0 {
		return 0, errors.New("local_file is empty or directory")
	}
	return info.Size(), copyFileAtomic(srcAbs, dst)
}

func (c *Crawler) downloadAttemptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.cfg.DownloadTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.cfg.DownloadTimeout)
}

func (c *Crawler) downloadAtomic(ctx context.Context, ref MediaRef, dst, referer string) (int64, error) {
	src := strings.TrimSpace(ref.URL)
	if src == "" {
		return 0, errors.New("empty url")
	}
	if _, err := url.Parse(src); err != nil {
		return 0, fmt.Errorf("parse url: %w", err)
	}
	if looksLikeHLSURL(src) {
		return c.downloadHLSAtomic(ctx, ref, dst, referer)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	for k, v := range ref.Headers {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("http %d", resp.StatusCode)
	}
	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	written, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return 0, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return 0, closeErr
	}
	if written <= 0 {
		_ = os.Remove(tmp)
		return 0, errors.New("empty body")
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return written, nil
}

func (c *Crawler) downloadHLSAtomic(ctx context.Context, ref MediaRef, dst, referer string) (int64, error) {
	src := strings.TrimSpace(ref.URL)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	tmp := dst + ".part"
	_ = os.Remove(tmp)
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-y",
	}
	headers := mediaRequestHeaders(ref, referer)
	if ua := strings.TrimSpace(headers.Get("User-Agent")); ua != "" {
		args = append(args, "-user_agent", ua)
	}
	if h := ffmpegHeaderBlock(headers); h != "" {
		args = append(args, "-headers", h)
	}
	args = append(args, c.ffmpegHLSInputOptions(ctx)...)
	args = append(args,
		"-i", src,
		"-c", "copy",
		"-bsf:a", "aac_adtstoasc",
		"-movflags", "+faststart",
		"-f", "mp4",
		tmp,
	)
	out, err := exec.CommandContext(ctx, c.cfg.FFmpegPath, args...).CombinedOutput()
	if err != nil {
		_ = os.Remove(tmp)
		return 0, mediaCommandError("ffmpeg hls", err, out)
	}
	info, err := os.Stat(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if info.IsDir() || info.Size() <= 0 {
		_ = os.Remove(tmp)
		return 0, errors.New("empty hls output")
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return info.Size(), nil
}

func looksLikeHLSURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err == nil && u != nil && strings.EqualFold(path.Ext(u.Path), ".m3u8") {
		return true
	}
	return strings.Contains(strings.ToLower(raw), ".m3u8")
}

func mediaRequestHeaders(ref MediaRef, referer string) http.Header {
	headers := make(http.Header)
	headers.Set("User-Agent", defaultUserAgent)
	if referer != "" {
		headers.Set("Referer", referer)
	}
	for k, v := range ref.Headers {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		headers.Set(k, v)
	}
	return headers
}

func ffmpegHeaderBlock(headers http.Header) string {
	var b strings.Builder
	for k, values := range headers {
		k = strings.TrimSpace(k)
		if k == "" || strings.EqualFold(k, "User-Agent") {
			continue
		}
		for _, v := range values {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteString("\r\n")
		}
	}
	return b.String()
}

func mediaCommandError(tool string, err error, output []byte) error {
	msg := strings.TrimSpace(redactMediaURLs(string(output)))
	if msg == "" {
		return fmt.Errorf("%s: %w", tool, err)
	}
	return fmt.Errorf("%s: %w: %s", tool, err, msg)
}

func redactMediaURLs(text string) string {
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

func configureExplicitProxy(transport *http.Transport, raw string) error {
	proxyURL := strings.TrimSpace(raw)
	if proxyURL == "" {
		return nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid proxy URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(u)
		transport.DialContext = nil
		return nil
	case "socks5", "socks5h":
		dialContext, err := socksProxyDialContext(u)
		if err != nil {
			return err
		}
		transport.Proxy = nil
		transport.DialContext = dialContext
		return nil
	default:
		return fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
}

func socksProxyDialContext(proxyURL *url.URL) (func(context.Context, string, string) (net.Conn, error), error) {
	var auth *proxy.Auth
	if proxyURL.User != nil {
		username := proxyURL.User.Username()
		password, _ := proxyURL.User.Password()
		auth = &proxy.Auth{User: username, Password: password}
	}
	dialer, err := proxy.SOCKS5("tcp", proxyURL.Host, auth, &net.Dialer{Timeout: 60 * time.Second})
	if err != nil {
		return nil, err
	}
	remoteDNS := strings.EqualFold(proxyURL.Scheme, "socks5h")
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		target := addr
		if !remoteDNS {
			resolved, err := resolveSocksTarget(ctx, addr)
			if err != nil {
				return nil, err
			}
			target = resolved
		}
		if ctxDialer, ok := dialer.(proxy.ContextDialer); ok {
			return ctxDialer.DialContext(ctx, network, target)
		}
		type result struct {
			conn net.Conn
			err  error
		}
		ch := make(chan result, 1)
		go func() {
			conn, err := dialer.Dial(network, target)
			ch <- result{conn: conn, err: err}
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case res := <-ch:
			return res.conn, res.err
		}
	}, nil
}

func resolveSocksTarget(ctx context.Context, addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || net.ParseIP(host) != nil {
		return addr, nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", err
	}
	for _, addr := range ips {
		if ip4 := addr.IP.To4(); ip4 != nil {
			return net.JoinHostPort(ip4.String(), port), nil
		}
	}
	if len(ips) > 0 && ips[0].IP != nil {
		return net.JoinHostPort(ips[0].IP.String(), port), nil
	}
	return "", fmt.Errorf("resolve %s: no address", host)
}

func normalizeItemForImport(item Item) (Item, string, error) {
	item.Title = strings.TrimSpace(item.Title)
	if item.Title == "" {
		return item, "", errors.New("title is required")
	}
	item.DetailURL = strings.TrimSpace(item.DetailURL)
	item.Author = strings.TrimSpace(item.Author)
	item.Description = strings.TrimSpace(item.Description)
	item.MediaURL = strings.TrimSpace(item.MediaURL)
	item.MediaLocalFile = strings.TrimSpace(item.MediaLocalFile)
	item.ThumbnailURL = strings.TrimSpace(item.ThumbnailURL)
	item.ThumbnailLocalFile = strings.TrimSpace(item.ThumbnailLocalFile)

	if strings.TrimSpace(item.Media.URL) == "" {
		item.Media.URL = item.MediaURL
	}
	if strings.TrimSpace(item.Media.LocalFile) == "" {
		item.Media.LocalFile = item.MediaLocalFile
	}
	if len(item.Media.Headers) == 0 {
		if len(item.MediaHeaders) > 0 {
			item.Media.Headers = item.MediaHeaders
		} else if len(item.Headers) > 0 {
			item.Media.Headers = item.Headers
		}
	}
	if strings.TrimSpace(item.Thumbnail.URL) == "" {
		item.Thumbnail.URL = item.ThumbnailURL
	}
	if strings.TrimSpace(item.Thumbnail.LocalFile) == "" {
		item.Thumbnail.LocalFile = item.ThumbnailLocalFile
	}
	if len(item.Thumbnail.Headers) == 0 {
		if len(item.ThumbnailHeaders) > 0 {
			item.Thumbnail.Headers = item.ThumbnailHeaders
		} else if len(item.Headers) > 0 {
			item.Thumbnail.Headers = item.Headers
		}
	}

	item.Media.URL = strings.TrimSpace(item.Media.URL)
	item.Media.LocalFile = strings.TrimSpace(item.Media.LocalFile)
	item.Thumbnail.URL = strings.TrimSpace(item.Thumbnail.URL)
	item.Thumbnail.LocalFile = strings.TrimSpace(item.Thumbnail.LocalFile)
	if item.Media.URL == "" && item.Media.LocalFile == "" {
		return item, "", errors.New("media_url is required")
	}

	sourceID := normalizeSourceID(item.SourceID)
	if sourceID == "" {
		sourceID = generatedSourceID(item)
	}
	return item, sourceID, nil
}

func normalizeSourceID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var b strings.Builder
	lastUnderscore := false
	for _, r := range raw {
		allowed := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '_' || r == '-' || r == '.'
		if allowed {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	id := strings.Trim(b.String(), "._-")
	if len(id) > 160 {
		id = strings.Trim(id[:160], "._-")
	}
	return id
}

func generatedSourceID(item Item) string {
	signature := strings.Join([]string{
		item.Title,
		stableURLKey(item.DetailURL),
		stableURLKey(item.Media.URL),
		strings.TrimSpace(item.Media.LocalFile),
	}, "\n")
	sum := sha256.Sum256([]byte(signature))
	return "auto-" + hex.EncodeToString(sum[:])[:24]
}

func stableURLKey(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	u.Fragment = ""
	if u.RawQuery != "" && strings.TrimSpace(u.Path) != "" && !strings.Contains(strings.ToLower(u.RawQuery), "viewkey=") {
		u.RawQuery = ""
	}
	return u.String()
}

func (c *Crawler) crawlerTagName() string {
	if c == nil {
		return ""
	}
	if v := strings.TrimSpace(c.cfg.CrawlerName); v != "" {
		return v
	}
	if c.cfg.Driver != nil {
		return strings.TrimSpace(c.cfg.Driver.ID())
	}
	return ""
}

func candidateBudgetForTarget(targetNew int) int {
	if targetNew <= 0 {
		targetNew = DefaultTargetNew
	}
	budget := targetNew * defaultCandidateMultiplier
	if floor := targetNew + defaultCandidateFloorExtra; budget < floor {
		budget = floor
	}
	if budget > defaultCandidateBudgetMax {
		budget = defaultCandidateBudgetMax
	}
	if budget < targetNew {
		return targetNew
	}
	return budget
}

func BuildVideoID(driveID, sourceID string) string {
	return Kind + "-" + driveID + "-" + sourceID
}

func detectVideoExt(rawURL, localFile string) string {
	if ext := mediaExt(localFile, true); ext != "" {
		return ext
	}
	if ext := mediaExt(rawURL, true); ext != "" {
		return ext
	}
	return ".mp4"
}

func detectThumbExt(rawURL, localFile string) string {
	if ext := mediaExt(localFile, false); ext != "" {
		return ext
	}
	if ext := mediaExt(rawURL, false); ext != "" {
		return ext
	}
	return ".jpg"
}

func mediaExt(raw string, video bool) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	value := raw
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && u != nil && u.Path != "" {
		value = u.Path
	}
	ext := strings.ToLower(path.Ext(value))
	if video {
		switch ext {
		case ".mp4", ".webm", ".mkv", ".mov", ".m4v", ".flv", ".avi":
			return ext
		}
		return ""
	}
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif":
		return ext
	}
	return ""
}

func cleanStringList(in []string) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		key := strings.ToLower(s)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	return out
}

func mergeStringLists(lists ...[]string) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, list := range lists {
		for _, s := range list {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			key := strings.ToLower(s)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

func copyFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
