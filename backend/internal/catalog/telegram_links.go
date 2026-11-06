package catalog

import (
	"context"
	"errors"
)

// AcceptTelegramMediaLink admits the whole group with one durable receipt.
// Queue exhaustion rolls back all new file claims and jobs before rejection.
func (c *Catalog) AcceptTelegramMediaLink(ctx context.Context, receipt TelegramReceipt, imports []TelegramImport, limit int) error {
	if len(imports) == 0 {
		return errors.New("empty Telegram media link")
	}
	for _, input := range imports {
		if input.Source == nil || input.Source.BotID != receipt.BotID || input.Source.SenderID != receipt.SenderID || input.Receipt.Response != "" {
			return errors.New("invalid Telegram media link import")
		}
	}
	if len(imports) == 1 {
		input := imports[0]
		return c.AcceptTelegramUpdate(ctx, receipt, input.Source, input.ID, input.Title, limit)
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM telegram_receipts WHERE bot_id=? AND (update_id=? OR (chat_id=? AND message_id=?))`, receipt.BotID, receipt.UpdateID, receipt.ChatID, receipt.MessageID).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	receipt.JobIDs, receipt.DuplicateJobIDs, receipt.Response, err = admitTelegramBatch(ctx, tx, imports, limit)
	if err != nil {
		return err
	}
	if err := acceptTelegramUpdate(ctx, tx, receipt, nil, "", "", limit); err != nil {
		return err
	}
	return tx.Commit()
}
