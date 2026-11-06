package catalog

import (
	"context"
	"errors"
	"time"
)

// TelegramMediaGroupUpdate is a durable inbox entry. The Telegram receiver owns
// the payload format and title policy; the catalog owns delivery and admission.
type TelegramMediaGroupUpdate struct {
	BotID, ChatID, MessageID, UpdateID int64
	MediaGroupID, Payload              string
	ReceivedAt                         int64
}

type TelegramMediaGroup struct {
	BotID, ChatID int64
	ID            string
	Updates       []TelegramMediaGroupUpdate
}

type TelegramImport struct {
	Receipt   TelegramReceipt
	Source    *TelegramSource
	ID, Title string
}

// StageTelegramMediaGroupUpdate persists the message before acknowledging it.
// Redelivery must not extend the group's collection window.
func (c *Catalog) StageTelegramMediaGroupUpdate(ctx context.Context, u TelegramMediaGroupUpdate) error {
	if u.MediaGroupID == "" || u.Payload == "" {
		return errors.New("missing Telegram media group or payload")
	}
	_, err := c.db.ExecContext(ctx, `INSERT INTO telegram_media_group_updates
 (bot_id,chat_id,media_group_id,message_id,update_id,payload,received_at)
 SELECT ?,?,?,?,?,?,? WHERE NOT EXISTS (
  SELECT 1 FROM telegram_receipts WHERE bot_id=? AND (update_id=? OR (chat_id=? AND message_id=?))
 ) ON CONFLICT DO NOTHING`, u.BotID, u.ChatID, u.MediaGroupID, u.MessageID, u.UpdateID, u.Payload, time.Now().UnixMilli(), u.BotID, u.UpdateID, u.ChatID, u.MessageID)
	return err
}

// PendingTelegramMediaGroups returns groups with no new members since before.
// Order by receipt time, using message IDs to break ties within a millisecond.
// Update IDs are opaque identities and do not encode arrival order.
func (c *Catalog) PendingTelegramMediaGroups(ctx context.Context, botID int64, before time.Time) ([]TelegramMediaGroup, error) {
	rows, err := c.db.QueryContext(ctx, `WITH ready AS (
 SELECT bot_id,chat_id,media_group_id,MIN(received_at) AS first_received, MIN(message_id) AS first_message
 FROM telegram_media_group_updates WHERE bot_id=?
 GROUP BY bot_id,chat_id,media_group_id HAVING MAX(received_at)<=?
 ORDER BY first_received,first_message,chat_id,media_group_id LIMIT 30
 ) SELECT u.bot_id,u.chat_id,u.media_group_id,u.message_id,u.update_id,u.payload,u.received_at
 FROM telegram_media_group_updates u JOIN ready r
 ON u.bot_id=r.bot_id AND u.chat_id=r.chat_id AND u.media_group_id=r.media_group_id
 ORDER BY r.first_received,r.first_message,r.chat_id,r.media_group_id,u.message_id`, botID, before.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []TelegramMediaGroup
	for rows.Next() {
		var u TelegramMediaGroupUpdate
		if err := rows.Scan(&u.BotID, &u.ChatID, &u.MediaGroupID, &u.MessageID, &u.UpdateID, &u.Payload, &u.ReceivedAt); err != nil {
			return nil, err
		}
		if len(groups) == 0 || groups[len(groups)-1].ChatID != u.ChatID || groups[len(groups)-1].ID != u.MediaGroupID {
			groups = append(groups, TelegramMediaGroup{BotID: u.BotID, ChatID: u.ChatID, ID: u.MediaGroupID})
		}
		group := &groups[len(groups)-1]
		group.Updates = append(group.Updates, u)
	}
	return groups, rows.Err()
}

// AcceptTelegramMediaGroup atomically replaces buffered updates with import
// jobs, one group receipt, and silent deduplication receipts for other members.
// If a new member arrived after the snapshot, collect again before admission.
func (c *Catalog) AcceptTelegramMediaGroup(ctx context.Context, group TelegramMediaGroup, imports []TelegramImport, limit int) (bool, error) {
	if len(imports) == 0 || len(imports) != len(group.Updates) {
		return false, errors.New("incomplete Telegram media group admission")
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT update_id FROM telegram_media_group_updates
 WHERE bot_id=? AND chat_id=? AND media_group_id=? ORDER BY message_id`, group.BotID, group.ChatID, group.ID)
	if err != nil {
		return false, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if len(ids) != len(group.Updates) {
		return false, nil
	}
	for i, id := range ids {
		if id != group.Updates[i].UpdateID {
			return false, nil
		}
		r := imports[i].Receipt
		if r.BotID != group.BotID || r.ChatID != group.ChatID || r.UpdateID != id || r.MessageID != group.Updates[i].MessageID {
			return false, errors.New("Telegram media group receipt does not match buffered update")
		}
	}
	anchor := 0
	for i, input := range imports {
		if input.Source != nil || input.Receipt.Response != "__ignore__" {
			anchor = i
			break
		}
	}
	response := ""
	var videos []TelegramImport
	for _, input := range imports {
		if input.Source != nil {
			videos = append(videos, input)
		} else if input.Receipt.Response != "__ignore__" && response == "" {
			response = input.Receipt.Response
		}
	}
	var jobIDs, duplicateJobIDs []string
	if len(videos) == 0 && response == "" {
		response = "__ignore__"
	}
	if len(videos) > 0 && response == "" {
		jobIDs, duplicateJobIDs, response, err = admitTelegramBatch(ctx, tx, videos, limit)
		if err != nil {
			return false, err
		}
	}
	for i, input := range imports {
		receipt := input.Receipt
		receipt.JobID, receipt.JobIDs, receipt.Response = "", nil, "__ignore__"
		receipt.DuplicateJobIDs = nil
		if i == anchor {
			receipt.JobIDs, receipt.Response = jobIDs, response
			receipt.DuplicateJobIDs = duplicateJobIDs
		}
		if err := acceptTelegramUpdate(ctx, tx, receipt, nil, "", "", limit); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM telegram_media_group_updates WHERE bot_id=? AND chat_id=? AND media_group_id=?`, group.BotID, group.ChatID, group.ID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
