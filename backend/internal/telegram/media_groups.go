package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
)

// Telegram sends album members as separate updates without an end marker.
// Wait for a quiet interval so captions on photos and subsequent updates can be
// included before any video is queued. The inbox survives receiver restarts.
const mediaGroupQuietPeriod = 2 * time.Second

func (s *Service) stageMediaGroup(ctx context.Context, u update) error {
	payload, err := json.Marshal(u)
	if err != nil {
		return err
	}
	m := u.Message
	return s.cat.StageTelegramMediaGroupUpdate(ctx, catalog.TelegramMediaGroupUpdate{
		BotID: s.BotID(), ChatID: m.Chat.ID, MessageID: m.ID, UpdateID: u.ID,
		MediaGroupID: m.MediaGroupID, Payload: string(payload),
	})
}

func (s *Service) processMediaGroups(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mediaGroupMu.Lock()
			s.mu.RLock()
			ready := s.status.State == "connected" && s.client != nil && s.client.Synced()
			s.mu.RUnlock()
			if !ready {
				s.mediaGroupMu.Unlock()
				continue
			}
			err := s.flushMediaGroups(ctx, time.Now().Add(-mediaGroupQuietPeriod))
			s.mediaGroupMu.Unlock()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				applog.Error(ctx, "保存 TG 媒体组失败，稍后重试", err, applog.Fields{Component: "telegram", Stage: "media_group"})
				if !wait(ctx, 5*time.Second) {
					return
				}
			}
		}
	}
}

func (s *Service) flushMediaGroups(ctx context.Context, before time.Time) error {
	groups, err := s.cat.PendingTelegramMediaGroups(ctx, s.BotID(), before)
	if err != nil {
		return err
	}
	for _, group := range groups {
		imports, err := s.prepareMediaGroup(ctx, group)
		if err != nil {
			return err
		}
		accepted, err := s.cat.AcceptTelegramMediaGroup(ctx, group, imports, s.settings().MaxPendingJobs)
		if err != nil {
			return err
		}
		if accepted {
			s.wakeImportWorker()
		}
	}
	return nil
}

func (s *Service) prepareMediaGroup(ctx context.Context, group catalog.TelegramMediaGroup) ([]catalog.TelegramImport, error) {
	updates := make([]update, len(group.Updates))
	for i, entry := range group.Updates {
		u := &updates[i]
		if err := json.Unmarshal([]byte(entry.Payload), u); err != nil {
			return nil, err
		}
		m := u.Message
		if m == nil || u.ID != entry.UpdateID || m.ID != entry.MessageID || m.Chat.ID != group.ChatID || m.MediaGroupID != group.ID {
			return nil, errors.New("TG 媒体组消息信息不一致")
		}
		if m.Date <= 0 {
			m.Date = entry.ReceivedAt / 1000
		}
	}
	return s.prepareMediaGroupImports(ctx, updates, group.ID)
}

func (s *Service) prepareMediaGroupImports(ctx context.Context, updates []update, groupID string) ([]catalog.TelegramImport, error) {
	caption := ""
	var groupDate int64
	videoCount := 0
	for _, u := range updates {
		m := u.Message
		// Recheck authorization after a configuration change or restart, including
		// messages used only as a caption source.
		if m.Chat.Type != "private" || !s.Allowed(m.From.ID) {
			continue
		}
		if groupDate == 0 {
			groupDate = m.Date
		}
		if caption == "" {
			caption = strings.TrimSpace(m.Caption)
		}
		if videoMedia(m) != nil {
			videoCount++
		}
	}
	imports := make([]catalog.TelegramImport, 0, len(updates))
	videoIndex := 0
	photoReply := false
	for _, u := range updates {
		input, err := s.prepareImport(ctx, u)
		if err != nil {
			return nil, err
		}
		m := u.Message
		if m.Chat.Type == "private" && s.Allowed(m.From.ID) {
			if videoMedia(m) != nil {
				videoIndex++
			}
			if input.Source != nil {
				input.Source.MediaGroupID = groupID
				input.Source.MediaGroupTitle = caption
				input.Source.MediaGroupDate = groupDate
				ownCaption := strings.TrimSpace(m.Caption)
				if ownCaption == "" {
					ownCaption = caption
				}
				suffix := ""
				sharedCaption := caption != "" && ownCaption == caption
				untitled := ownCaption == "" && strings.TrimSpace(input.Source.FileName) == ""
				if videoCount > 1 && (sharedCaption || untitled) {
					suffix = fmt.Sprintf(" - %d", videoIndex)
				}
				input.Title = videoTitleWithSuffix(ownCaption, input.Source.FileName, m.Date, suffix)
			} else if len(m.Photo) > 0 && videoMedia(m) == nil {
				// Photos contribute captions without producing files or rejection
				// replies in a video album. A photo-only album gets one explanation.
				if videoCount > 0 || photoReply {
					input.Receipt.Response = "__ignore__"
				}
				photoReply = true
			}
		}
		imports = append(imports, input)
	}
	return imports, nil
}
