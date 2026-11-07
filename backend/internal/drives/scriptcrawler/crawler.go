package scriptcrawler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/crawljob"
	"github.com/nianzhibai/91/backend/internal/persistence"
	"github.com/nianzhibai/91/backend/internal/tasklimit"
)

const DefaultTargetNew = 10

var errCrawlTimeLimit = errors.New("crawler time limit reached")

type CrawlerConfig struct {
	FingerprintLimiter *tasklimit.Limiter
	Driver             *Driver
	Catalog            *catalog.Catalog
	CrawlerName        string
	PythonPath         string
	FFmpegPath         string
	FFprobePath        string
	ScriptPath         string
	WorkDir            string
	CommonThumbDir     string
	LocalPreviewDir    string
	ProxyURL           string
	ConfigJSON         string
	FeedID             string
	HTTPClient         *http.Client
	DownloadTimeout    time.Duration
	// RunTimeout bounds discovery and downloads, not processing completed media.
	RunTimeout       time.Duration
	OperationTimeout time.Duration
	StopGrace        time.Duration
	MaxStdoutBytes   int64
	MaxStderrBytes   int64
	// Zero uses the default retry budget; a negative value disables retries.
	MaxRetries int
	PageSize   int
	OnProgress func(crawljob.Result)
}
type Crawler struct {
	*Importer
	cfg     CrawlerConfig
	runMu   sync.Mutex
	tasksMu sync.Mutex
	tasks   map[string]context.CancelFunc
}

func NewCrawler(cfg CrawlerConfig) *Crawler {
	if cfg.PythonPath == "" {
		cfg.PythonPath = "python3"
	}
	if cfg.FFmpegPath == "" {
		cfg.FFmpegPath = "ffmpeg"
	}
	if cfg.FFprobePath == "" {
		cfg.FFprobePath = "ffprobe"
	}
	if cfg.DownloadTimeout <= 0 {
		cfg.DownloadTimeout = 30 * time.Minute
	}
	if cfg.RunTimeout <= 0 {
		cfg.RunTimeout = 3 * time.Hour
	}
	if cfg.OperationTimeout <= 0 {
		cfg.OperationTimeout = defaultOperationTimeout
	}
	if cfg.StopGrace <= 0 {
		cfg.StopGrace = time.Second
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	} else if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.PageSize <= 0 || cfg.PageSize > maxPageItems {
		cfg.PageSize = 24
	}
	if cfg.HTTPClient == nil {
		transport := &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 60 * time.Second, MaxIdleConns: 10, IdleConnTimeout: 90 * time.Second}
		_ = configureExplicitProxy(transport, cfg.ProxyURL)
		cfg.HTTPClient = &http.Client{Transport: transport}
	}
	importer := &Importer{cfg: ImporterConfig{Driver: cfg.Driver, Catalog: cfg.Catalog, FingerprintLimiter: cfg.FingerprintLimiter, CrawlerName: cfg.CrawlerName, FFmpegPath: cfg.FFmpegPath, FFprobePath: cfg.FFprobePath, CommonThumbDir: cfg.CommonThumbDir, LocalPreviewDir: cfg.LocalPreviewDir, HTTPClient: cfg.HTTPClient, DownloadTimeout: cfg.DownloadTimeout}}
	return &Crawler{cfg: cfg, Importer: importer}
}

// Task is an accepted configuration and script snapshot. Callers own drive
// admission until RunTask returns; the durable result is saved before release.
type Task struct {
	Result                   crawljob.Result
	dir, scriptPath, jobPath string
	crawler                  *Crawler
	stageStarted             time.Time
	executed                 bool
	context                  context.Context
	cancel                   context.CancelFunc
}

func (c *Crawler) Prepare(ctx context.Context, targetNew int, parentTaskID string) (_ *Task, err error) {
	if c.cfg.Driver == nil || c.cfg.Catalog == nil {
		return nil, errors.New("crawler dependencies not configured")
	}
	if err := configureExplicitProxy(&http.Transport{}, c.cfg.ProxyURL); err != nil {
		return nil, err
	}
	source, err := os.ReadFile(c.cfg.ScriptPath)
	if err != nil {
		return nil, err
	}
	meta, err := ExtractMetadata(string(source))
	if err != nil {
		return nil, err
	}
	feed, err := meta.ResolveFeed(c.cfg.FeedID)
	if err != nil {
		return nil, err
	}
	config := json.RawMessage(c.cfg.ConfigJSON)
	if len(config) == 0 {
		config = json.RawMessage(`{}`)
	}
	if !json.Valid(config) || !strings.HasPrefix(strings.TrimSpace(string(config)), "{") {
		return nil, errors.New("config_json must be a JSON object")
	}
	if targetNew <= 0 {
		targetNew = DefaultTargetNew
	}
	if err = c.cfg.Driver.Init(ctx); err != nil {
		return nil, err
	}
	root, err := filepath.Abs(c.cfg.Driver.CrawlDir())
	if err != nil {
		return nil, err
	}
	id := uuid.NewString()
	dir := filepath.Join(root, id)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	workDir := filepath.Join(dir, "work")
	if err = os.Mkdir(workDir, 0o700); err != nil {
		return nil, err
	}
	hash := sha256.Sum256(source)
	task := &Task{crawler: c, dir: dir, scriptPath: filepath.Join(dir, "crawler.py"), jobPath: filepath.Join(dir, "job.json"), Result: crawljob.Result{
		FeedID: feed.ID, FeedLabel: feed.Label,
		TaskID: id, ParentTaskID: parentTaskID, DriveID: c.cfg.Driver.ID(), ScriptVersion: hex.EncodeToString(hash[:]), State: "queued", Stage: "queued", AcceptedAt: time.Now(), TargetNew: targetNew, StageMillis: map[string]int64{},
		Budget: crawljob.Budget{Retries: c.cfg.MaxRetries, RuntimeSeconds: int(c.cfg.RunTimeout / time.Second)},
	}}
	if err = os.WriteFile(task.scriptPath, source, 0o600); err != nil {
		return nil, err
	}
	job := Job{Protocol: ProtocolV3, TaskID: id, CrawlerID: c.cfg.Driver.ID(), FeedID: feed.ID, WorkDir: workDir, Config: config, Network: JobNetwork{ProxyURL: c.cfg.ProxyURL}}
	data, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(task.jobPath, data, 0o600); err != nil {
		return nil, err
	}
	if err = persistence.RLockContext(ctx); err != nil {
		return nil, err
	}
	err = c.cfg.Catalog.CreateCrawlerTask(ctx, task.Result)
	persistence.RUnlock()
	if err != nil {
		return nil, err
	}
	task.context, task.cancel = context.WithCancel(ctx)
	c.tasksMu.Lock()
	if c.tasks == nil {
		c.tasks = map[string]context.CancelFunc{}
	}
	c.tasks[task.Result.TaskID] = task.cancel
	c.tasksMu.Unlock()
	return task, nil
}

// CancelTask stops discovery and incomplete downloads of an accepted task.
// Completed media still finishes processing under the caller's task lifetime.
// Addressing the task ID keeps delayed requests from stopping a newer run.
func (c *Crawler) CancelTask(taskID string) bool {
	c.tasksMu.Lock()
	defer c.tasksMu.Unlock()
	cancel, ok := c.tasks[taskID]
	if ok {
		cancel()
	}
	return ok
}

// StopTasks requests graceful stops without releasing active task ownership.
// Application-wide cancellation and deletion use the parent task context.
func (c *Crawler) StopTasks() bool {
	c.tasksMu.Lock()
	defer c.tasksMu.Unlock()
	for _, cancel := range c.tasks {
		cancel()
	}
	return len(c.tasks) > 0
}

func (t *Task) save(ctx context.Context) error {
	t.Result.TargetReached = t.Result.NewVideos >= t.Result.TargetNew
	t.Result.StopRequested = t.context != nil && t.context.Err() != nil
	if err := persistence.RLockContext(ctx); err != nil {
		return err
	}
	defer persistence.RUnlock()
	if err := t.crawler.cfg.Catalog.SaveCrawlerTask(ctx, t.Result); err != nil {
		return err
	}
	if fn := t.crawler.cfg.OnProgress; fn != nil {
		fn(t.Result)
	}
	return nil
}

// Terminal persistence is part of task execution. Keep its workspace and
// admission until the commit succeeds, including after business cancellation.
// Each attempt has its own deadline so a blocked snapshot or SQLite write can
// recover without leaving an abandoned active-task row behind.
func (t *Task) saveTerminal(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	delay := 100 * time.Millisecond
	for {
		saveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := t.save(saveCtx)
		cancel()
		if err == nil {
			return
		}
		applog.Error(ctx, "保存爬虫任务结果失败，将重试", err, applog.Fields{Stage: "save_result"})
		time.Sleep(delay)
		delay = min(delay*2, 5*time.Second)
	}
}

func (t *Task) stage(ctx context.Context, stage string) error {
	now := time.Now()
	if !t.stageStarted.IsZero() {
		t.Result.StageMillis[t.Result.Stage] += now.Sub(t.stageStarted).Milliseconds()
	}
	t.Result.Stage = stage
	t.stageStarted = now
	return t.save(ctx)
}

// RunStage lets application wiring supply resource generation/upload while the
// coordinator retains timing and persistence ownership.
func (t *Task) RunStage(ctx context.Context, stage string, run func(context.Context) error) error {
	if err := t.stage(ctx, stage); err != nil {
		return err
	}
	if err := run(ctx); err != nil {
		return err
	}
	if stage == "generation" {
		issues, err := t.crawler.cfg.Catalog.CrawlerGenerationFailures(ctx, t.Result.ImportedVideoIDs)
		if err != nil {
			return err
		}
		for _, issue := range issues {
			t.Result.AddIssue(issue)
		}
		return t.save(ctx)
	}
	return nil
}
func (c *Crawler) RunOnce(ctx context.Context, targetNew int) (*crawljob.Result, error) {
	t, err := c.Prepare(ctx, targetNew, applog.ContextFields(ctx).TaskID)
	if err != nil {
		return nil, err
	}
	return c.RunTask(ctx, t, nil)
}
func (c *Crawler) RunTask(ctx context.Context, t *Task, followUp func(context.Context, *Task) error) (_ *crawljob.Result, runErr error) {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	if t == nil || t.crawler != c || t.executed {
		return nil, errors.New("invalid or already executed crawler task")
	}
	t.executed = true
	ctx = applog.WithFields(ctx, applog.Fields{TaskID: t.Result.TaskID, DriveID: t.Result.DriveID, Component: "scriptcrawler"})
	runCtx, cancel := context.WithTimeoutCause(ctx, c.cfg.RunTimeout, errCrawlTimeLimit)
	// User stops must reach acquisition before CancelTask/StopTasks return.
	// AfterFunc still handles cancellation inherited from the prepare context.
	c.tasksMu.Lock()
	c.tasks[t.Result.TaskID] = func() {
		t.cancel()
		cancel()
	}
	c.tasksMu.Unlock()
	stopCancel := context.AfterFunc(t.context, cancel)
	defer func() {
		stopCancel()
		cancel()
		t.cancel()
		c.tasksMu.Lock()
		delete(c.tasks, t.Result.TaskID)
		c.tasksMu.Unlock()
	}()
	if t.context.Err() != nil {
		cancel()
	}
	t.Result.State = "running"
	t.Result.StartedAt = time.Now()
	t.Result.StageMillis["queued"] = t.Result.StartedAt.Sub(t.Result.AcceptedAt).Milliseconds()
	t.stageStarted = time.Now()
	failureStage := ""
	var crawlElapsed time.Duration
	defer func() {
		now := time.Now()
		t.Result.StageMillis[t.Result.Stage] += now.Sub(t.stageStarted).Milliseconds()
		t.Result.FinishedAt = now
		t.Result.TargetReached = t.Result.NewVideos >= t.Result.TargetNew
		if ctx.Err() != nil && !errors.Is(runErr, ctx.Err()) {
			runErr = errors.Join(runErr, ctx.Err())
		} else if t.context.Err() != nil && !errors.Is(runErr, t.context.Err()) {
			runErr = errors.Join(runErr, t.context.Err())
		}
		if runErr != nil {
			var protocol *ProtocolError
			var source *ScriptError
			// Stopping acquisition can add context.Canceled while the parent is
			// expiring. Preserve that deadline unless a caller actually canceled.
			userCanceled := errors.Is(ctx.Err(), context.Canceled) || errors.Is(t.context.Err(), context.Canceled)
			if !userCanceled && !errors.Is(runErr, context.DeadlineExceeded) {
				userCanceled = errors.Is(runErr, context.Canceled)
			}
			switch {
			case userCanceled:
				t.Result.State = "canceled"
				t.Result.StopReason = "user_canceled"
			case errors.Is(runErr, errOperationTimeout):
				t.Result.State = "failed"
				t.Result.StopReason = "operation_timeout"
			case errors.Is(runErr, context.DeadlineExceeded):
				t.Result.State = "failed"
				t.Result.StopReason = "timeout"
			case errors.As(runErr, &protocol):
				t.Result.State = "failed"
				t.Result.StopReason = "protocol_error"
			case errors.As(runErr, &source):
				t.Result.State = "failed"
				t.Result.StopReason = "source_error"
			default:
				t.Result.State = "failed"
				t.Result.StopReason = "backend_error"
			}
			if t.Result.State == "failed" && t.Result.NewVideos > 0 {
				t.Result.State = "partial"
			}
			t.Result.Message = applog.Redact(runErr.Error())
			if failureStage == "" {
				failureStage = t.Result.Stage
			}
			t.Result.AddIssue(crawljob.Issue{Stage: failureStage, Code: t.Result.StopReason, Message: t.Result.Message})
		} else if t.Result.Failed > 0 {
			t.Result.State = "failed"
			if t.Result.NewVideos > 0 {
				t.Result.State = "partial"
			}
			runErr = fmt.Errorf("%d crawler items failed", t.Result.Failed)
		} else {
			t.Result.State = "completed"
			if !t.Result.TargetReached && t.Result.NewVideos > 0 && t.Result.StopReason != "source_exhausted" {
				t.Result.State = "partial"
			}
		}
		t.saveTerminal(ctx)
		_ = os.RemoveAll(t.dir)
		applog.Info(ctx, fmt.Sprintf("Crawler finished state=%s reason=%s target=%d pages=%d checked=%d known=%d resolved=%d new=%d duplicates=%d failed=%d limit=%s crawl_elapsed=%s total_elapsed=%s", t.Result.State, t.Result.StopReason, t.Result.TargetNew, t.Result.Pages, t.Result.Checked, t.Result.Known, t.Result.Resolved, t.Result.NewVideos, t.Result.Duplicates, t.Result.Failed, c.cfg.RunTimeout, crawlElapsed, time.Since(t.Result.StartedAt)), applog.Fields{Stage: "complete"})
	}()
	crawlErr := func() error {
		if err := runCtx.Err(); err != nil {
			return err
		}
		if err := t.stage(runCtx, "discover"); err != nil {
			return err
		}
		if err := c.cfg.Catalog.BackfillCrawlerSources(runCtx, t.Result.DriveID); err != nil {
			return err
		}
		return c.crawl(runCtx, ctx, t)
	}()
	crawlElapsed = time.Since(t.Result.StartedAt)
	switch {
	case ctx.Err() != nil:
		crawlErr = errors.Join(crawlErr, ctx.Err())
	case t.context.Err() != nil:
		crawlErr = errors.Join(crawlErr, t.context.Err())
	case errors.Is(context.Cause(runCtx), errCrawlTimeLimit):
		// Only the acquisition deadline is an expected stop. An earlier operation
		// timeout or a real processing failure must retain its own outcome.
		if crawlErr == nil || ((errors.Is(crawlErr, context.DeadlineExceeded) || errors.Is(crawlErr, errCrawlTimeLimit)) && !errors.Is(crawlErr, errOperationTimeout)) {
			t.Result.StopReason = "time_limit"
			crawlErr = nil
			applog.Info(ctx, fmt.Sprintf("Crawler time limit reached limit=%s target=%d new=%d checked=%d known=%d resolved=%d", c.cfg.RunTimeout, t.Result.TargetNew, t.Result.NewVideos, t.Result.Checked, t.Result.Known, t.Result.Resolved), applog.Fields{Stage: t.Result.Stage})
		}
	}
	// End the acquisition budget before generation/upload. A long upload must
	// not retroactively turn a successful crawl into an acquisition timeout.
	cancel()
	if crawlErr != nil {
		failureStage = t.Result.Stage
	}
	// Acquisition failures do not prevent processing the crawler's existing
	// backlog. A user stop with no completed imports still skips follow-up work;
	// hard cancellation always stops the whole pipeline.
	if ctx.Err() == nil && (t.context.Err() == nil || t.Result.NewVideos > 0) && followUp != nil {
		if err := followUp(ctx, t); err != nil {
			if crawlErr != nil || t.context.Err() != nil {
				t.Result.AddIssue(crawljob.Issue{Stage: t.Result.Stage, Code: "backend_error", Message: applog.Redact(err.Error())})
			}
			return &t.Result, errors.Join(crawlErr, err)
		}
	}
	if crawlErr != nil {
		return &t.Result, crawlErr
	}
	return &t.Result, nil
}
func (c *Crawler) crawl(ctx, processingCtx context.Context, t *Task) error {
	s, err := startSession(ctx, sessionConfig{PythonPath: c.cfg.PythonPath, ScriptPath: t.scriptPath, WorkDir: c.cfg.WorkDir, JobPath: t.jobPath, ProxyURL: c.cfg.ProxyURL, OperationTimeout: c.cfg.OperationTimeout, StopGrace: c.cfg.StopGrace, MaxStdoutBytes: c.cfg.MaxStdoutBytes, MaxStderrBytes: c.cfg.MaxStderrBytes, Diagnostic: func(line string) {
		var diagnostic struct {
			Level   string `json:"level"`
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(line), &diagnostic) != nil || diagnostic.Message == "" {
			diagnostic.Message = line
		}
		fields := applog.Fields{Component: "scriptcrawler:script", Stage: "script"}
		switch diagnostic.Level {
		case "error":
			applog.Error(ctx, diagnostic.Message, nil, fields)
		case "warning":
			applog.Warn(ctx, diagnostic.Message, nil, fields)
		default:
			applog.Info(ctx, diagnostic.Message, fields)
		}
	}})
	if err != nil {
		return err
	}
	defer s.Close()

	r := &t.Result
	var cursor *string
	cursors := map[string]bool{}
	pages := map[string]bool{}
	candidates := map[string]bool{}
	for r.NewVideos < r.TargetNew {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := t.stage(ctx, "discover"); err != nil {
			return err
		}
		var page *pageResponse
		err = t.operation(ctx, func() error {
			r.DiscoverCalls++
			var err error
			page, err = s.Discover(ctx, cursor, c.cfg.PageSize)
			return err
		})
		if err != nil {
			return err
		}
		r.Pages++
		r.Checked += len(page.Items)
		// Page identity ignores volatile locator tokens and ordering.
		keys := make([]string, 0, len(page.Items))
		for _, candidate := range page.Items {
			keys = append(keys, candidate.DiscoveryKey)
		}
		sort.Strings(keys)
		signature, _ := json.Marshal(keys)
		if len(keys) > 0 && pages[string(signature)] {
			return protocolError("repeated discovery page")
		}
		pages[string(signature)] = true
		if page.NextCursor != nil {
			if cursors[*page.NextCursor] {
				return protocolError("repeated next_cursor")
			}
			cursors[*page.NextCursor] = true
		}
		identities := make([]catalog.CrawlerIdentity, len(page.Items))
		for i, candidate := range page.Items {
			identities[i] = catalog.CrawlerIdentity{DiscoveryKey: candidate.DiscoveryKey, SourceID: candidate.SourceID}
		}
		known, err := c.cfg.Catalog.KnownCrawlerCandidates(ctx, r.DriveID, identities)
		if err != nil {
			return err
		}
		pending := make([]Candidate, 0, len(page.Items))
		for _, candidate := range page.Items {
			if candidates[candidate.DiscoveryKey] {
				r.Repeated++
				continue
			}
			candidates[candidate.DiscoveryKey] = true
			if known[candidate.DiscoveryKey] {
				r.Known++
				if candidate.SourceID != "" {
					if err := c.cfg.Catalog.BindCrawlerDiscovery(ctx, r.DriveID, candidate.DiscoveryKey, candidate.SourceID); err != nil {
						return err
					}
				}
				continue
			}
			pending = append(pending, candidate)
		}
		for _, candidate := range pending {
			if r.NewVideos >= r.TargetNew {
				break
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			// Source IDs encountered earlier on this page may just have committed.
			current, err := c.cfg.Catalog.KnownCrawlerCandidates(ctx, r.DriveID, []catalog.CrawlerIdentity{{DiscoveryKey: candidate.DiscoveryKey, SourceID: candidate.SourceID}})
			if err != nil {
				return err
			}
			if current[candidate.DiscoveryKey] {
				r.Known++
				if err := c.cfg.Catalog.BindCrawlerDiscovery(ctx, r.DriveID, candidate.DiscoveryKey, candidate.SourceID); err != nil {
					return err
				}
				continue
			}
			if err := t.stage(ctx, "resolve"); err != nil {
				return err
			}
			var item Item
			err = t.operation(ctx, func() error {
				r.ResolveCalls++
				var err error
				item, err = s.Resolve(ctx, candidate)
				return err
			})
			if err != nil {
				var failure *ScriptError
				if errors.As(err, &failure) && failure.Scope == "item" && failure.Code != "auth_required" {
					r.AddIssue(crawljob.Issue{Stage: "resolve", DiscoveryKey: candidate.DiscoveryKey, Code: failure.Code, Message: applog.Redact(failure.Message)})
					continue
				}
				return err
			}
			r.Resolved++
			// Persist the handoff even if stop raced a successful resolve. URL
			// acquisition still uses the original, cancelable context below.
			stageCtx, stageCancel := context.WithTimeout(processingCtx, 5*time.Second)
			err = t.stage(stageCtx, "import")
			stageCancel()
			if err != nil {
				return err
			}
			outcome, err := c.Importer.Import(ctx, processingCtx, item)
			if err != nil {
				if ctx.Err() != nil && (errors.Is(err, ctx.Err()) || errors.Is(err, context.Cause(ctx))) && !errors.Is(err, errOperationTimeout) {
					return ctx.Err()
				}
				code := "import_failed"
				if errors.Is(err, errOperationTimeout) {
					code = "operation_timeout"
				}
				r.AddIssue(crawljob.Issue{Stage: "import", DiscoveryKey: candidate.DiscoveryKey, Code: code, Message: applog.Redact(err.Error())})
				if ctx.Err() != nil {
					return ctx.Err()
				}
				continue
			}
			switch outcome {
			case ImportAdded:
				r.NewVideos++
				r.ImportedVideoIDs = append(r.ImportedVideoIDs, importVideoID(r.DriveID, item.SourceID))
			case ImportKnown:
				r.Known++
			case ImportDuplicate:
				r.Duplicates++
			}
		}
		if err := t.save(ctx); err != nil {
			return err
		}
		applog.Info(ctx, fmt.Sprintf("Discovery page=%d checked=%d known=%d new=%d duplicates=%d", r.Pages, r.Checked, r.Known, r.NewVideos, r.Duplicates), applog.Fields{Stage: "discover"})
		if r.NewVideos >= r.TargetNew {
			r.StopReason = "target_reached"
			break
		}
		if r.StopReason != "" {
			break
		}
		if page.NextCursor == nil {
			r.StopReason = "source_exhausted"
			break
		}
		cursor = page.NextCursor
	}
	return s.Stop(ctx, r.StopReason)
}

func (t *Task) operation(ctx context.Context, run func() error) error {
	for {
		err := run()
		var failure *ScriptError
		if !errors.As(err, &failure) || !failure.Retryable || failure.Code == "auth_required" || failure.Code == "not_found" || t.Result.Retries >= t.Result.Budget.Retries {
			return err
		}
		delay := time.Duration(failure.RetryAfterSeconds) * time.Second
		if delay <= 0 {
			delay = time.Second
		}
		t.Result.Retries++
		if err := t.save(ctx); err != nil {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
