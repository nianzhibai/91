package telegram

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/video-site/backend/internal/applog"
	"github.com/video-site/backend/internal/catalog"
)

func (s *Service) notify(ctx context.Context) {
	for ctx.Err() == nil {
		s.mu.RLock()
		c, botID := s.client, s.botID
		s.mu.RUnlock()
		// Restore and setup errors must not trigger notifications to old chats.
		if c != nil && s.Available() {
			receipts, err := s.cat.PendingTelegramReceipts(ctx, botID)
			if err == nil {
				for _, r := range receipts {
					if ctx.Err() != nil {
						return
					}
					s.notifyOne(ctx, c, r)
					if !wait(ctx, time.Second) {
						return
					}
				}
			}
		}
		if !wait(ctx, 2*time.Second) {
			return
		}
	}
}
func (s *Service) notifyOne(ctx context.Context, c transport, r catalog.TelegramReceipt) {
	message, state := renderReceiptMessage(r), "replied"
	if len(r.JobIDs) > 0 {
		jobs := make([]*catalog.RemoteUploadJob, 0, len(r.JobIDs))
		terminal := true
		for _, id := range r.JobIDs {
			job, err := s.cat.GetRemoteUploadJob(ctx, id)
			if err != nil {
				return
			}
			jobs = append(jobs, job)
			terminal = terminal && job.Terminal()
		}
		message = renderMediaGroupMessage(jobs, r.DuplicateJobIDs, s.settings().SiteBaseURL, func(id string) int64 {
			return s.downloadProgress.speed(id, time.Now())
		})
		state = "group:terminal"
		if !terminal {
			state = fmt.Sprintf("progress:%x", sha256.Sum256([]byte(message.text)))
		}
	} else if r.JobID != "" {
		j, err := s.cat.GetRemoteUploadJob(ctx, r.JobID)
		if err != nil {
			return
		}
		message = renderImportMessage(j, s.settings().SiteBaseURL, s.downloadProgress.speed(j.ID, time.Now()))
		if slices.Contains(r.DuplicateJobIDs, j.ID) {
			message = renderDuplicateImportMessage(j, s.settings().SiteBaseURL)
		}
		state = j.State
		if !j.Terminal() {
			// Persist the rendered content identity so restarts and unchanged
			// progress do not cause duplicate edits.
			state = fmt.Sprintf("progress:%x", sha256.Sum256([]byte(message.text)))
		}
	} else if r.Response == responseStatus {
		message = renderReceiptMessage(catalog.TelegramReceipt{Response: responseNeedsAccess, SenderID: r.SenderID})
		if s.Allowed(r.SenderID) {
			var err error
			message, err = s.statusMessage(ctx, time.Now())
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				applog.Warn(ctx, "读取 TG 转存统计失败", err, applog.Fields{Component: "telegram", Stage: "status"})
				message = botMessage{text: "⚠️ 暂时无法读取转存统计，请稍后重试 /status。"}
			}
		}
	}
	if r.Delivered == state {
		// Move unchanged tasks behind other eligible receipts as well.
		_ = s.cat.SaveTelegramNotification(ctx, r, r.ReplyID, state, 0)
		return
	}
	sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	replyID, err := c.Send(sendCtx, r, message)
	var apiErr *APIError
	if err != nil && r.ReplyID != 0 && errors.As(err, &apiErr) && apiErr.Code == 400 {
		r.ReplyID = 0
		replyID, err = c.Send(sendCtx, r, message)
	}
	if err != nil {
		delay := time.Duration(1<<min(r.Attempts, 8)) * 10 * time.Second
		if errors.As(err, &apiErr) && apiErr.RetryAfter > delay {
			delay = apiErr.RetryAfter
		}
		_ = s.cat.SaveTelegramNotification(ctx, r, 0, "", delay)
		return
	}
	if replyID == 0 {
		replyID = r.ReplyID
	}
	_ = s.cat.SaveTelegramNotification(ctx, r, replyID, state, 0)
}
