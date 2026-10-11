package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestStarcoQueueOrderingSyncAndReviewProtection(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.Background()
	if err := s.AddProject(ctx, Project{ID: "project", Name: "Project"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRepository(ctx, Repository{ID: "repo", ProjectID: "project", Name: "Repository", Path: "/synthetic/repo", Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC)
	snapshot := []StarcoTask{
		{RemoteID: 1, Priority: "Low", Eligible: true, Task: Task{ID: "starco-1", EmailID: "starco:1", RepositoryID: "repo", CreatedAt: base}},
		{RemoteID: 2, Priority: "High", Eligible: true, Task: Task{ID: "starco-2", EmailID: "starco:2", RepositoryID: "repo", CreatedAt: base.Add(time.Hour)}},
		{RemoteID: 3, Priority: "Medium", Eligible: true, Task: Task{ID: "starco-3", EmailID: "starco:3", RepositoryID: "repo", CreatedAt: base.Add(2 * time.Hour)}},
		{RemoteID: 4, Priority: "High", Eligible: false, Task: Task{ID: "starco-4", EmailID: "starco:4", RepositoryID: "repo", CreatedAt: base}},
		{RemoteID: 5, Priority: "High", Eligible: true, Task: Task{ID: "starco-5", EmailID: "starco:5", CreatedAt: base}},
	}
	for range 2 {
		if err := s.SyncStarco(ctx, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	queue, err := s.StarcoQueue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 3 || queue[0].ID != "starco-2" || queue[1].ID != "starco-1" || queue[2].ID != "starco-3" {
		t.Fatalf("wrong priority/FIFO: %+v", queue)
	}
	all, err := s.Tasks(ctx)
	if err != nil || len(all) != 5 {
		t.Fatalf("dedup: %d %v", len(all), err)
	}

	snapshot[2].Priority = "High"
	if err := s.SyncStarco(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	queue, err = s.StarcoQueue(ctx)
	if err != nil || queue[1].ID != "starco-3" {
		t.Fatalf("priority update: %+v %v", queue, err)
	}

	task := queue[0]
	task.Status = "READY_FOR_REVIEW"
	task.Worktree = "/synthetic/worktree"
	if err := s.UpdateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncStarco(ctx, snapshot[1:]); err != nil {
		t.Fatal(err)
	}
	queue, err = s.StarcoQueue(ctx)
	if err != nil || len(queue) != 1 || queue[0].ID != "starco-3" {
		t.Fatalf("removed/review tasks requeued: %+v %v", queue, err)
	}
	saved, err := s.Task(ctx, task.ID)
	if err != nil || saved.Status != "READY_FOR_REVIEW" || saved.Worktree != task.Worktree {
		t.Fatalf("review state lost: %+v %v", saved, err)
	}

	if err := s.SetEnabled(ctx, "repositories", "repo", false); err != nil {
		t.Fatal(err)
	}
	queue, err = s.StarcoQueue(ctx)
	if err != nil || len(queue) != 0 {
		t.Fatalf("disabled repository executable: %+v %v", queue, err)
	}
}

func TestStarcoSyncRollsBackInvalidSnapshot(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.Background()
	snapshot := []StarcoTask{{RemoteID: 1, Eligible: true, Task: Task{ID: "starco-1", EmailID: "starco:1", CreatedAt: time.Now()}}}
	if err := s.SyncStarco(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncStarco(ctx, []StarcoTask{{RemoteID: 0}}); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
	var eligible bool
	if err := s.DB.QueryRowContext(ctx, `SELECT eligible FROM starco_tasks WHERE remote_id=1`).Scan(&eligible); err != nil || !eligible {
		t.Fatalf("failed sync changed eligibility: %t %v", eligible, err)
	}
}
