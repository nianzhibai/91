package telegram

import (
	"context"
	"errors"
	"time"

	"github.com/video-site/backend/internal/catalog"
)

func (s *Service) acceptMessageLink(ctx context.Context, u update, link messageLink) error {
	m := u.Message
	receipt := catalog.TelegramReceipt{BotID: s.BotID(), UpdateID: u.ID, ChatID: m.Chat.ID, MessageID: m.ID, SenderID: m.From.ID}
	s.mu.RLock()
	c := s.client
	s.mu.RUnlock()
	var members []*message
	var err error
	if c == nil {
		err = errors.New("Telegram 尚未连接，请稍后重新发送链接")
	} else {
		requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		members, err = c.ResolveMediaLink(requestCtx, link)
		cancel()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = errors.New("Telegram 链接解析超时，请稍后重新发送链接")
		}
		receipt.Response = err.Error()
		return s.cat.AcceptTelegramUpdate(ctx, receipt, nil, "", "", s.settings().MaxPendingJobs)
	}
	updates := make([]update, 0, len(members))
	groupID := ""
	for _, member := range members {
		resolved := *member
		// Keep replies and authorization attached to the sender's private
		// message. The encoded file reference retains the remote message ID.
		resolved.ID, resolved.From, resolved.Chat = m.ID, m.From, m.Chat
		if resolved.Date <= 0 {
			resolved.Date = m.Date
		}
		groupID = resolved.MediaGroupID
		updates = append(updates, update{ID: u.ID, Message: &resolved})
	}
	var inputs []catalog.TelegramImport
	if groupID != "" {
		inputs, err = s.prepareMediaGroupImports(ctx, updates, groupID)
	} else if len(updates) == 1 {
		var input catalog.TelegramImport
		input, err = s.prepareImport(ctx, updates[0])
		inputs = []catalog.TelegramImport{input}
	}
	if err != nil {
		return err
	}
	videos := make([]catalog.TelegramImport, 0, len(inputs))
	for _, input := range inputs {
		if input.Source != nil {
			videos = append(videos, input)
		} else if input.Receipt.Response != "__ignore__" {
			receipt.Response = input.Receipt.Response
		}
	}
	if receipt.Response != "" || len(videos) == 0 {
		if receipt.Response == "" {
			receipt.Response = responseUnsupported
		}
		return s.cat.AcceptTelegramUpdate(ctx, receipt, nil, "", "", s.settings().MaxPendingJobs)
	}
	if err := s.cat.AcceptTelegramMediaLink(ctx, receipt, videos, s.settings().MaxPendingJobs); err != nil {
		return err
	}
	s.wakeImportWorker()
	return nil
}
