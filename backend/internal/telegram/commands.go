package telegram

import (
	"context"
	"errors"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/schedule"
)

const (
	commandStart  = "start"
	commandHelp   = "help"
	commandID     = "id"
	commandStatus = "status"
)

// Menu configuration belongs to the receiver session, not the connection probe.
// Run it independently so a slow or failed registration cannot delay imports.
func (s *Service) syncCommandMenu(ctx context.Context) {
	backoff := 10 * time.Second
	for ctx.Err() == nil {
		s.mu.RLock()
		c, connected := s.client, s.status.State == "connected"
		s.mu.RUnlock()
		if c == nil || !connected {
			if !wait(ctx, time.Second) {
				return
			}
			continue
		}

		requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := c.SetCommands(requestCtx)
		cancel()
		if err == nil || ctx.Err() != nil {
			return
		}
		applog.Warn(ctx, "同步 TG 命令菜单失败，稍后重试", err, applog.Fields{Component: "telegram", Stage: "command_menu"})
		delay := backoff
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.RetryAfter > delay {
			delay = apiErr.RetryAfter
		}
		if !wait(ctx, delay) {
			return
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

func (s *Service) statusMessage(ctx context.Context, now time.Time) (botMessage, error) {
	timezone := schedule.DefaultTimezone
	if s.statusTimezone != nil {
		timezone = s.statusTimezone()
	}
	_, location, err := schedule.LoadTimezone(timezone)
	if err != nil {
		return botMessage{}, err
	}
	stats, err := s.cat.CountTelegramImports(ctx, now.In(location))
	if err != nil {
		return botMessage{}, err
	}
	return renderStatusMessage(stats), nil
}
