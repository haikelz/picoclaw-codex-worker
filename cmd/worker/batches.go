package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"picoclaw-codex-worker/internal/store"
)

// batchWindow returns the hourly collection window ending at now's hour, Monday–Friday 08:00–17:00
// Asia/Jakarta. The 08:00 window starts at 17:00 on the previous working day.
func batchWindow(now time.Time) (time.Time, time.Time, bool) {
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	now = now.In(loc)
	if now.Weekday() == time.Saturday || now.Weekday() == time.Sunday || now.Hour() < 8 || now.Hour() > 17 {
		return time.Time{}, time.Time{}, false
	}

	end := now.Truncate(time.Hour)
	start := end.Add(-time.Hour)
	if end.Hour() == 8 {
		previous := end.AddDate(0, 0, -1)
		for previous.Weekday() == time.Saturday || previous.Weekday() == time.Sunday {
			previous = previous.AddDate(0, 0, -1)
		}
		start = time.Date(previous.Year(), previous.Month(), previous.Day(), 17, 0, 0, 0, loc)
	}

	return start, end, true
}

// collectBatch records the current window, catching up from the last checkpoint after missed polls.
func collectBatch(
	ctx context.Context,
	s *store.Store,
	now time.Time,
) error {
	start, end, allowed := batchWindow(now)
	if !allowed {
		return errors.New("outside batch polling hours")
	}

	checkpoint, err := s.State(ctx, "starco:batch-checkpoint")
	if err != nil {
		return err
	}
	if checkpoint != "" {
		previous, err := time.Parse(time.RFC3339, checkpoint)
		if err != nil {
			return fmt.Errorf("parse batch checkpoint: %w", err)
		}
		if !previous.Before(end) {
			return nil
		}
		start = previous
	}

	return s.CollectBatch(ctx, start, end)
}

// nextBatch selects queue members of the first task's batch and repository, preserving queue order.
func nextBatch(
	ctx context.Context,
	s *store.Store,
	queue []store.Task,
) ([]store.Task, string, error) {
	var selected []store.Task
	var batch string
	var repo string

	for _, task := range queue {
		member, err := s.TaskBatch(ctx, task.ID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, "", err
		}

		if batch == "" {
			batch = member
			repo = task.RepositoryID
		}
		if member == batch && task.RepositoryID == repo {
			selected = append(selected, task)
		}
	}

	return selected, batch, nil
}
