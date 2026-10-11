package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"picoclaw-codex-worker/internal/store"
)

func TestBatchWindowsAndCheckpoint(t *testing.T) {
	for _, test := range []struct{ now, start, end string }{
		{"2026-10-07T08:00:00+07:00", "2026-10-06T17:00:00+07:00", "2026-10-07T08:00:00+07:00"},
		{"2026-10-12T08:00:00+07:00", "2026-10-09T17:00:00+07:00", "2026-10-12T08:00:00+07:00"},
		{"2026-10-07T17:00:10+07:00", "2026-10-07T16:00:00+07:00", "2026-10-07T17:00:00+07:00"},
	} {
		now, _ := time.Parse(time.RFC3339, test.now)
		start, end, ok := batchWindow(now)
		if !ok || start.Format(time.RFC3339) != test.start || end.Format(time.RFC3339) != test.end {
			t.Fatalf("window: %s %s %t", start, end, ok)
		}
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.Background()
	start, _ := time.Parse(time.RFC3339, "2026-10-07T09:00:00+07:00")
	var snapshot []store.StarcoTask
	for i, instant := range []time.Time{start.Add(-time.Second), start, start.Add(30 * time.Minute), start.Add(time.Hour)} {
		id := []string{"old", "start", "middle", "end"}[i]
		snapshot = append(snapshot, store.StarcoTask{RemoteID: i + 1, Eligible: true, Task: store.Task{ID: id, EmailID: id, CreatedAt: instant}})
	}
	if err := s.SyncStarco(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := collectBatch(ctx, s, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"start", "middle"} {
		if _, err := s.TaskBatch(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"old", "end"} {
		if _, err := s.TaskBatch(ctx, id); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("unexpected membership %s: %v", id, err)
		}
	}
	if err := collectBatch(ctx, s, start.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TaskBatch(ctx, "end"); err != nil {
		t.Fatal("missed polling lost task", err)
	}
	if err := collectBatch(ctx, s, start.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	queue := []store.Task{
		{ID: "old", RepositoryID: "repo-a"},
		{ID: "start", RepositoryID: "repo-a"},
		{ID: "middle", RepositoryID: "repo-b"},
		{ID: "end", RepositoryID: "repo-a"},
	}
	selected, _, err := nextBatch(ctx, s, queue)
	if err != nil || len(selected) != 1 || selected[0].ID != "start" {
		t.Fatalf("batch must isolate interval/repo and exclude historical: %+v %v", selected, err)
	}
}
