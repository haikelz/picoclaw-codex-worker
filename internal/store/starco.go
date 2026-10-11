package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// StarcoTask is a complete remote snapshot, not a request to restart local work.
type StarcoTask struct {
	Task              Task
	RemoteID          int
	RemoteProjectID   int
	RemoteProjectName string
	Priority          string
	Eligible          bool
}

// The upsert refreshes task text and routing only while local work has not started.
const upsertStarcoTask = `INSERT INTO tasks(id,email_id,project_id,repository_id,title,description,status,created_at)
VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET
title=CASE WHEN tasks.status IN ('QUEUED','UNMAPPED') THEN excluded.title ELSE tasks.title END,
description=CASE WHEN tasks.status IN ('QUEUED','UNMAPPED') THEN excluded.description ELSE tasks.description END,
project_id=CASE WHEN tasks.status IN ('QUEUED','UNMAPPED') AND tasks.worktree='' THEN excluded.project_id ELSE tasks.project_id END,
repository_id=CASE WHEN tasks.status IN ('QUEUED','UNMAPPED') AND tasks.worktree='' THEN excluded.repository_id ELSE tasks.repository_id END,
status=CASE WHEN tasks.status IN ('QUEUED','UNMAPPED') AND tasks.worktree='' THEN excluded.status ELSE tasks.status END`

const upsertStarcoRemote = `INSERT INTO starco_tasks(task_id,remote_id,priority,eligible) VALUES(?,?,?,?)
ON CONFLICT(task_id) DO UPDATE SET priority=excluded.priority,eligible=excluded.eligible`

func (s *Store) initializeStarco(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS starco_tasks (
	task_id TEXT PRIMARY KEY REFERENCES tasks(id),
	remote_id INTEGER NOT NULL UNIQUE,
	priority TEXT NOT NULL,
	eligible INTEGER NOT NULL
)`)
	if err != nil {
		return fmt.Errorf("create starco_tasks: %w", err)
	}

	return nil
}

// SyncStarco applies the full snapshot atomically and preserves execution/review state.
func (s *Store) SyncStarco(ctx context.Context, snapshot []StarcoTask) error {
	if err := s.initializeStarco(ctx); err != nil {
		return err
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Starco sync: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `UPDATE starco_tasks SET eligible=0`); err != nil {
		return fmt.Errorf("reset Starco eligibility: %w", err)
	}

	for _, remote := range snapshot {
		t := remote.Task
		if t.ID == "" || remote.RemoteID <= 0 || (remote.Eligible && t.CreatedAt.IsZero()) {
			return errors.New("invalid Starco snapshot")
		}
		if t.CreatedAt.IsZero() {
			// Ineligible historical tasks need no execution record.
			continue
		}

		status := StatusQueued
		if t.RepositoryID == "" {
			status = StatusUnmapped
		}

		if _, err := tx.ExecContext(
			ctx,
			upsertStarcoTask,
			t.ID,
			t.EmailID,
			t.ProjectID,
			t.RepositoryID,
			t.Title,
			t.Description,
			status,
			t.CreatedAt.UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("upsert task %s: %w", t.ID, err)
		}
		if _, err := tx.ExecContext(
			ctx,
			upsertStarcoRemote,
			t.ID,
			remote.RemoteID,
			remote.Priority,
			remote.Eligible,
		); err != nil {
			return fmt.Errorf("upsert Starco task %d: %w", remote.RemoteID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Starco sync: %w", err)
	}

	return nil
}

// StarcoQueue returns only remotely eligible tasks, High first then FIFO.
func (s *Store) StarcoQueue(ctx context.Context) ([]Task, error) {
	if err := s.initializeStarco(ctx); err != nil {
		return nil, err
	}

	rows, err := s.DB.QueryContext(ctx, `SELECT
	t.id,t.email_id,t.project_id,t.repository_id,t.title,t.description,t.status,t.attempt,
	t.branch,t.worktree,t.session_id,t.source_session_id,t.feedback,t.created_at
FROM tasks t
JOIN starco_tasks r ON r.task_id=t.id
JOIN repositories repo ON repo.id=t.repository_id AND repo.enabled=1
JOIN projects p ON p.id=repo.project_id AND p.enabled=1
WHERE t.status='QUEUED' AND r.eligible=1
ORDER BY CASE WHEN lower(r.priority)='high' THEN 0 ELSE 1 END,julianday(t.created_at),r.remote_id`)
	if err != nil {
		return nil, fmt.Errorf("query Starco queue: %w", err)
	}
	defer rows.Close()

	var result []Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}

		result = append(result, task)
	}

	return result, rows.Err()
}
