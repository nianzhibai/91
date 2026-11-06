package catalog

import (
	"context"
	"database/sql"
)

// A savepoint lets callers persist a single queue rejection while rolling back
// every file claim and job in the batch, within their receipt transaction.
func admitTelegramBatch(ctx context.Context, tx *sql.Tx, imports []TelegramImport, limit int) ([]string, []string, string, error) {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT telegram_batch`); err != nil {
		return nil, nil, "", err
	}
	jobIDs := make([]string, 0, len(imports))
	var duplicateJobIDs []string
	seen := make(map[string]bool)
	for _, input := range imports {
		jobID, duplicate, response, err := admitTelegramSource(ctx, tx, input.Source, input.ID, input.Title, limit)
		if err != nil {
			return nil, nil, "", err
		}
		if response != "" {
			if _, err := tx.ExecContext(ctx, `ROLLBACK TO telegram_batch`); err != nil {
				return nil, nil, "", err
			}
			if _, err := tx.ExecContext(ctx, `RELEASE telegram_batch`); err != nil {
				return nil, nil, "", err
			}
			return nil, nil, response, nil
		}
		if !seen[jobID] {
			jobIDs = append(jobIDs, jobID)
			if duplicate {
				duplicateJobIDs = append(duplicateJobIDs, jobID)
			}
			seen[jobID] = true
		}
	}
	_, err := tx.ExecContext(ctx, `RELEASE telegram_batch`)
	return jobIDs, duplicateJobIDs, "", err
}
