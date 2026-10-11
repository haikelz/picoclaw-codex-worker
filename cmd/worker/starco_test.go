package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"picoclaw-codex-worker/internal/starco"
	"picoclaw-codex-worker/internal/store"
)

func TestWorkingHoursJakarta(t *testing.T) {
	for _, test := range []struct {
		instant string
		allowed bool
	}{
		{instant: "2026-10-07T07:59:59+07:00", allowed: false},
		{instant: "2026-10-07T08:00:00+07:00", allowed: true},
		{instant: "2026-10-07T16:59:59+07:00", allowed: true},
		{instant: "2026-10-07T17:00:00+07:00", allowed: false},
		{instant: "2026-10-10T09:00:00+07:00", allowed: false},
		{instant: "2026-10-07T01:00:00Z", allowed: true},
	} {
		now, err := time.Parse(time.RFC3339, test.instant)
		if err != nil {
			t.Fatal(err)
		}
		if workingHours(now) != test.allowed {
			t.Errorf("%s: expected %t", test.instant, test.allowed)
		}
	}
}

func TestExclusiveExecutionLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "execution.lock")
	first, err := acquireFileLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseLock(first)
	if second, err := acquireFileLock(path); !errors.Is(err, errWorkerBusy) {
		if second != nil {
			releaseLock(second)
		}
		t.Fatalf("duplicate execution not blocked: %v", err)
	}
}

func TestDrainDisabledDoesNotAccessCredentials(t *testing.T) {
	err := drainStarco(context.Background(), nil, config{execute: false})
	if err == nil {
		t.Fatal("disabled drain accepted")
	}
}

type fixtureSource struct {
	tasks []starco.Task
}

func (f fixtureSource) Tasks(context.Context) ([]starco.Task, error) {
	return f.tasks, nil
}

func (f fixtureSource) Detail(_ context.Context, task starco.Task) (starco.Task, error) {
	task.Title = "Synthetic task"
	task.Description = "Implement a synthetic change"
	task.CreatedAt = time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
	return task, nil
}

func TestSnapshotFiltersDependenciesAndMapsProjects(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := s.AddProject(ctx, store.Project{ID: "demo", Name: "Demo"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRepository(ctx, store.Repository{ID: "repo", ProjectID: "demo", Path: "/synthetic/repo", Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetState(ctx, "starco:project:11", "repo"); err != nil {
		t.Fatal(err)
	}

	parent := 1
	missing := 999
	remote := []starco.Task{
		{ID: 1, Status: "Completed"},
		{ID: 2, Status: "Ready", DependencyID: &parent},
		{ID: 3, Status: "Ready", DependencyID: &missing},
		{ID: 4, Status: "Waiting Approval"},
		{ID: 5, Status: "Cancel"},
		{ID: 6, Status: "Doing"},
	}
	for i := range remote {
		remote[i].Project.ID = 11
	}
	rows, err := starcoSnapshot(ctx, s, fixtureSource{tasks: remote})
	if err != nil || len(rows) != 2 || !rows[0].Eligible || rows[1].Eligible || rows[0].Task.RepositoryID != "repo" {
		t.Fatalf("snapshot: %+v err=%v", rows, err)
	}
	if err := s.SyncStarco(ctx, rows); err != nil {
		t.Fatal(err)
	}
	queue, err := s.StarcoQueue(ctx)
	if err != nil || len(queue) != 1 || queue[0].ID != "starco-2" {
		t.Fatalf("dependency queue: %+v err=%v", queue, err)
	}
}

func TestRecoverInterruptedTasksNeverRequeues(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, status := range []string{"CLAIMING", "CLAIMED", "RUNNING", "VALIDATING", "READY_FOR_REVIEW"} {
		if _, err := s.InsertTask(ctx, store.Task{
			ID:        "starco-" + status,
			EmailID:   "synthetic:" + status,
			Status:    status,
			CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := recoverStarco(ctx, s); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.Tasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.ID == "starco-READY_FOR_REVIEW" {
			if task.Status != "READY_FOR_REVIEW" {
				t.Fatal("review state lost")
			}
			continue
		}
		if task.Status != "HUMAN_REVIEW" || task.Feedback == "" {
			t.Fatalf("unsafe interrupted task recovery: %+v", task)
		}
	}
}

func TestEnvPasswordIsNotInherited(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "worker.env")
	if err := os.WriteFile(envFile, []byte("WORKER_STARCO_PASSWORD=\"p@ss=word\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKER_DATA_DIR", dir)
	t.Setenv("WORKER_ENV_FILE", envFile)
	// t.Setenv restores the original value; unset it so the env file is the only source.
	t.Setenv(passwordEnv, "")
	if err := os.Unsetenv(passwordEnv); err != nil {
		t.Fatal(err)
	}

	cfg, err := configuration()

	if err != nil || cfg.starcoPassword != "p@ss=word" {
		t.Fatalf("password=%q err=%v", cfg.starcoPassword, err)
	}
	if _, found := os.LookupEnv(passwordEnv); found {
		t.Fatal("password leaked into the environment inherited by agent subprocesses")
	}
}
