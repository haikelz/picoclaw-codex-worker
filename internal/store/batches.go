package store

import (
	"context"
	"fmt"
	"time"
)

const createBatchTasks = `CREATE TABLE IF NOT EXISTS batch_tasks(
	task_id TEXT PRIMARY KEY REFERENCES tasks(id),
	batch_end TEXT NOT NULL
)`

// CollectBatch records membership and checkpoint together, including blocked/unmapped tasks.
func (s *Store) CollectBatch(
	ctx context.Context,
	start time.Time,
	end time.Time,
) error {
	if _, err := s.DB.ExecContext(ctx, createBatchTasks); err != nil {
		return fmt.Errorf("create batch_tasks: %w", err)
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin batch collection: %w", err)
	}
	defer tx.Rollback()

	batchEnd := end.UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(
		ctx,
		`INSERT OR IGNORE INTO batch_tasks(task_id,batch_end)
SELECT t.id,? FROM tasks t JOIN starco_tasks r ON r.task_id=t.id
WHERE julianday(t.created_at)>=julianday(?) AND julianday(t.created_at)<julianday(?)`,
		batchEnd,
		start.UTC().Format(time.RFC3339),
		batchEnd,
	); err != nil {
		return fmt.Errorf("collect batch members: %w", err)
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO state(key,value) VALUES('starco:batch-checkpoint',?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		batchEnd,
	); err != nil {
		return fmt.Errorf("save batch checkpoint: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit batch collection: %w", err)
	}

	return nil
}

// TaskBatch returns the batch a task belongs to. A non-member returns sql.ErrNoRows.
func (s *Store) TaskBatch(ctx context.Context, id string) (string, error) {
	if _, err := s.DB.ExecContext(ctx, createBatchTasks); err != nil {
		return "", fmt.Errorf("create batch_tasks: %w", err)
	}

	var batch string
	err := s.DB.QueryRowContext(ctx, `SELECT batch_end FROM batch_tasks WHERE task_id=?`, id).Scan(&batch)
	if err != nil {
		return "", fmt.Errorf("read task batch: %w", err)
	}

	return batch, nil
}
