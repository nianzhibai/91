package crawlerupload

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/drives/pikpak"
	"github.com/nianzhibai/91/backend/internal/drives/scriptcrawler"
	"github.com/nianzhibai/91/backend/internal/persistence"
	"github.com/nianzhibai/91/backend/internal/readretry"
	"github.com/nianzhibai/91/backend/internal/scopedproxy"
	"github.com/nianzhibai/91/backend/internal/uploadjob"
)

func (m *Migrator) run(ctx context.Context, driveIDs []string) error {
	if m.cfg.Catalog == nil || m.cfg.Registry == nil {
		return errors.New("上传服务未配置")
	}
	if driveIDs == nil {
		for _, source := range m.cfg.Registry.All() {
			if source.Kind() != scriptcrawler.Kind {
				continue
			}
			row, err := m.getDrive(ctx, source.ID())
			if err != nil {
				return err
			}
			if row != nil && scriptcrawler.IsConfigured(row.Credentials) && strings.TrimSpace(row.Credentials["upload_drive_id"]) != "" {
				driveIDs = append(driveIDs, source.ID())
			}
		}
	}
	var failures []error
	stoppedTargets := make(map[string]error)
	for _, id := range driveIDs {
		result, err := m.acceptDrive(ctx, id)
		if err == nil {
			err = m.runDrive(ctx, id, stoppedTargets, result)
		}
		if err != nil {
			failures = append(failures, err)
		}
		if ctx.Err() != nil {
			break
		}
	}
	runErr := errors.Join(failures...)
	if err := ctx.Err(); err != nil && !errors.Is(runErr, err) {
		runErr = errors.Join(runErr, err)
	}
	return runErr
}

// runDrive owns the outcome of a sweep, including failures before the first
// upload. Persist it before publishing idle and releasing task admission,
// even after cancellation.
func (m *Migrator) runDrive(ctx context.Context, driveID string, stoppedTargets map[string]error, result uploadjob.Result) (runErr error) {
	ctx = applog.WithFields(ctx, applog.Fields{TaskID: result.TaskID, DriveID: driveID, Component: "crawlerupload"})
	result.State = "running"
	result.StartedAt = time.Now()
	applog.Info(ctx, "爬虫上传开始", applog.Fields{})
	defer func() {
		if result.TargetDriveID != "" && targetUploadRejected(runErr) {
			stoppedTargets[result.TargetDriveID] = runErr
		}
		if delay, limited := drives.RateLimitRetryAfter(runErr); limited && result.TargetDriveID != "" {
			if delay <= 0 {
				delay = time.Minute
			}
			m.setCooldown(result.TargetDriveID, delay)
		}
		result.FinishedAt = time.Now()
		result.RemainingCount = max(0, result.CandidateCount-result.ProcessedCount())
		switch {
		case ctx.Err() != nil:
			result.State = "canceled"
			runErr = ctx.Err()
		case runErr != nil:
			result.State = "failed"
			if result.UploadedCount+result.ReusedCount > 0 {
				result.State = "partial"
			}
		case result.FailedCount > 0:
			result.State = "failed"
			if result.UploadedCount+result.ReusedCount > 0 {
				result.State = "partial"
			}
			runErr = fmt.Errorf("%d 个视频上传失败", result.FailedCount)
		case result.BlockedCount > 0:
			result.State = "blocked"
			if result.UploadedCount+result.ReusedCount > 0 {
				result.State = "partial"
			}
		default:
			result.State = "succeeded"
		}
		if runErr != nil {
			result.Message = applog.Redact(runErr.Error())
		}
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		saveErr := persistence.RLockContext(saveCtx)
		if saveErr == nil {
			saveErr = m.cfg.Catalog.SaveCrawlerUploadResult(saveCtx, result)
			persistence.RUnlock()
		}
		if saveErr != nil {
			applog.Error(ctx, "保存上传结果失败", saveErr, applog.Fields{Stage: "save_result"})
			runErr = errors.Join(runErr, saveErr)
		}
		message := fmt.Sprintf("爬虫上传结束 state=%s target=%s candidates=%d uploaded=%d reused=%d blocked=%d failed=%d remaining=%d",
			result.State, result.TargetDriveID, result.CandidateCount, result.UploadedCount, result.ReusedCount, result.BlockedCount, result.FailedCount, result.RemainingCount)
		if runErr != nil {
			applog.Error(ctx, message, runErr, applog.Fields{})
		} else if result.BlockedCount > 0 {
			applog.Warn(ctx, message, nil, applog.Fields{})
		} else {
			applog.Info(ctx, message, applog.Fields{})
		}
		// Polling clients may stop refreshing as soon as they observe idle.
		m.reportUploadProgress(UploadProgress{DriveID: driveID, State: "idle"})
	}()
	if err := m.cfg.Catalog.SaveCrawlerUploadResult(ctx, result); err != nil {
		return err
	}
	// An admitted task must reach outcome persistence even if it was canceled
	// before the worker started. Stop before any catalog reads or remote work.
	if err := ctx.Err(); err != nil {
		return err
	}
	before, count, err := m.cfg.Catalog.CrawlerUploadScope(ctx, driveID)
	if err != nil {
		return err
	}
	result.CandidateCount = count
	plan, err := m.migrationPlan(ctx, driveID)
	if err != nil {
		return err
	}
	result.TargetDriveID = plan.targetDriveID
	if err := stoppedTargets[plan.targetDriveID]; err != nil {
		return fmt.Errorf("本轮已停止向该网盘上传: %w", err)
	}
	if active, until := m.inCooldown(plan.targetDriveID); active {
		return fmt.Errorf("上传目标处于限流或验证码冷却期，请在 %s 后重试", until.Format(time.RFC3339))
	}
	err = m.migrateCandidates(ctx, plan, before, &result)
	if err != nil {
		return err
	}
	// Orphan cleanup remains separate from candidate selection. Unknown local
	// files and pending restores are retained for the existing restore workflow.
	deleted, err := m.cleanupOldLocalVideos(ctx, plan)
	if err != nil {
		return fmt.Errorf("清理已迁移的本地文件: %w", err)
	}
	if deleted > 0 {
		applog.Info(ctx, fmt.Sprintf("已清理 %d 个已迁移文件的本地副本", deleted), applog.Fields{Stage: "cleanup"})
	}
	return nil
}

func (m *Migrator) migrateCandidates(ctx context.Context, plan migrationPlan, before int64, result *uploadjob.Result) error {
	uploadCtx, err := scopedproxy.WithURL(ctx, plan.uploadProxyURL)
	if err != nil {
		return fmt.Errorf("上传代理无效: %w", err)
	}
	progress := func(title string) {
		m.reportUploadProgress(UploadProgress{DriveID: plan.source.ID(), State: "uploading", CurrentTitle: title,
			QueueLength: max(0, result.CandidateCount-result.ProcessedCount()), DoneCount: result.ProcessedCount(), TotalCount: result.CandidateCount})
	}
	progress("")
	after, parent := "", ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		videos, err := m.cfg.Catalog.ListCrawlerUploadCandidates(ctx, plan.source.ID(), after, before, m.cfg.PageSize)
		if err != nil {
			return err
		}
		if len(videos) == 0 {
			return nil
		}
		for _, v := range videos {
			if err := ctx.Err(); err != nil {
				return err
			}
			after = v.ID
			progress(v.Title)
			itemCtx := applog.WithFields(uploadCtx, applog.Fields{VideoID: v.ID, FileID: v.FileID})
			if issue := inspectLocalFile(plan.source, v); issue != nil {
				recordUploadIssue(itemCtx, result, v, *issue, false)
				continue
			}
			// Already catalogued remote content can be reused without moving
			// bytes or requiring local derived assets to be regenerated.
			duplicate, err := m.cfg.Catalog.FindEquivalentVideoOnDrive(ctx, v, plan.targetDriveID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if duplicate != nil {
				_, err = m.bindToExistingTarget(itemCtx, v, duplicate, plan)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					recordUploadIssue(itemCtx, result, v, uploadjob.Issue{Stage: "catalog", Reason: "migration_failed", Message: err.Error()}, false)
				} else {
					result.ReusedCount++
					if m.cfg.OnMigrated != nil {
						m.cfg.OnMigrated(v.ID)
					}
					applog.Info(itemCtx, "已复用目标网盘中的视频", applog.Fields{Stage: "complete"})
				}
				continue
			}
			issue, err := m.assetBlockReason(ctx, v)
			if err != nil {
				return err
			}
			if issue != nil {
				recordUploadIssue(itemCtx, result, v, *issue, true)
				continue
			}
			if parent == "" {
				parent, err = plan.target.EnsureDir(itemCtx, plan.uploadDir)
				if err != nil {
					return fmt.Errorf("创建上传目录: %w", err)
				}
				if strings.TrimSpace(parent) == "" {
					return errors.New("上传网盘返回了空目录标识")
				}
			}
			applog.Info(itemCtx, "开始上传视频到 "+plan.targetDriveID, applog.Fields{Stage: "upload"})
			reused, err := m.uploadWithReconciliation(itemCtx, v, plan, parent)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				recordUploadIssue(itemCtx, result, v, uploadjob.Issue{Stage: "upload", Reason: "upload_failed", Message: err.Error()}, false)
				if _, limited := drives.RateLimitRetryAfter(err); limited {
					return err
				}
				if pikpak.IsCaptchaError(err) {
					m.setCooldown(plan.targetDriveID, m.cfg.CaptchaCooldown)
					return err
				}
				if targetUploadRejected(err) {
					return err
				}
				continue
			}
			if reused {
				result.ReusedCount++
			} else {
				result.UploadedCount++
			}
			if m.cfg.OnMigrated != nil {
				m.cfg.OnMigrated(v.ID)
			}
			applog.Info(itemCtx, "视频已迁移到 "+plan.targetDriveID, applog.Fields{Stage: "complete"})
		}
		progress("")
	}
}

func targetUploadRejected(err error) bool {
	return drives.ErrorMentionsHTTPStatus(err, http.StatusUnauthorized, http.StatusForbidden, http.StatusInsufficientStorage)
}

func recordUploadIssue(ctx context.Context, result *uploadjob.Result, v *catalog.Video, issue uploadjob.Issue, blocked bool) {
	issue.VideoID, issue.Title = v.ID, v.Title
	issue.Message = applog.Redact(issue.Message)
	result.AddIssue(issue)
	fields := applog.Fields{Stage: issue.Stage}
	message := issue.Reason + ": " + issue.Message
	if blocked {
		result.BlockedCount++
		applog.Warn(ctx, message, nil, fields)
	} else {
		result.FailedCount++
		applog.Error(ctx, message, nil, fields)
	}
}

func inspectLocalFile(source LocalSource, v *catalog.Video) *uploadjob.Issue {
	issue := func(reason, message string) *uploadjob.Issue {
		return &uploadjob.Issue{Stage: "source", Reason: reason, Message: message}
	}
	path, err := source.VideoPath(v.FileID)
	if err != nil {
		return issue("invalid_path", "本地视频路径无效: "+err.Error())
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return issue("missing_file", "本地视频文件不存在")
		}
		return issue("unreadable_file", "无法检查本地视频: "+err.Error())
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return issue("invalid_file", "本地视频不是有效的非空文件")
	}
	f, err := os.Open(path)
	if err != nil {
		return issue("unreadable_file", "无法读取本地视频: "+err.Error())
	}
	defer f.Close()
	if info.Size() != v.Size {
		return issue("size_mismatch", "本地文件大小与视频记录不一致")
	}
	return nil
}

// assetBlockReason is the only upload eligibility policy. Both manual and
// scheduled runs apply it to each remaining local video, not crawler history.
func (m *Migrator) assetBlockReason(ctx context.Context, v *catalog.Video) (*uploadjob.Issue, error) {
	issue := func(reason, message string) *uploadjob.Issue {
		return &uploadjob.Issue{Stage: "assets", Reason: reason, Message: message}
	}
	if !strings.EqualFold(strings.TrimSpace(v.FingerprintStatus), "ready") && strings.TrimSpace(v.SampledSHA256) == "" {
		if v.FingerprintStatus == "failed" {
			return issue("fingerprint_failed", "指纹生成失败，请重试指纹生成"), nil
		}
		return issue("fingerprint_pending", "等待视频指纹生成"), nil
	}
	if m.cfg.PreviewEnabled != nil && !m.cfg.PreviewEnabled() {
		return nil, nil
	}
	if strings.EqualFold(strings.TrimSpace(v.PreviewStatus), "ready") {
		return nil, nil
	}
	ready, err := m.cfg.Catalog.HasReadyEquivalentPreview(ctx, v)
	if err != nil || ready {
		return nil, err
	}
	if v.PreviewStatus == "failed" {
		return issue("preview_failed", "预览生成失败，请重试预览生成"), nil
	}
	return issue("preview_pending", "等待预览视频生成"), nil
}

func (m *Migrator) uploadWithReconciliation(ctx context.Context, v *catalog.Video, plan migrationPlan, parent string) (bool, error) {
	reused, err := m.migrateOne(ctx, v, plan, parent)
	if _, limited := drives.RateLimitRetryAfter(err); limited {
		return reused, err
	}
	if ctx.Err() != nil || !readretry.Transient(err) {
		return reused, err
	}
	// Only adapters that can refresh the remote directory may retry a write
	// with an unknown outcome. migrateOne checks the deterministic destination
	// before sending any bytes again. The source is reopened for the new attempt.
	refresher, ok := plan.target.(interface{ invalidateExisting(string) })
	if !ok {
		return reused, err
	}
	refresher.invalidateExisting(parent)
	applog.Warn(ctx, "上传连接中断，核对目标文件后重试一次", err, applog.Fields{Stage: "retry"})
	if err := readretry.Wait(ctx, time.Second); err != nil {
		return false, err
	}
	return m.migrateOne(ctx, v, plan, parent)
}

// acceptDrive persists queued before an asynchronous caller reports acceptance.
func (m *Migrator) acceptDrive(ctx context.Context, driveID string) (uploadjob.Result, error) {
	result := uploadjob.Result{TaskID: uuid.NewString(), ParentTaskID: applog.ContextFields(ctx).TaskID, DriveID: driveID, State: "queued", AcceptedAt: time.Now()}
	if m.cfg.Catalog == nil || m.cfg.Registry == nil {
		return result, errors.New("上传服务未配置")
	}
	if err := persistence.RLockContext(ctx); err != nil {
		return result, err
	}
	defer persistence.RUnlock()
	return result, m.cfg.Catalog.SaveCrawlerUploadResult(ctx, result)
}
