package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"picoclaw-codex-worker/internal/store"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestRunnerCreatesIsolatedReviewWorktree(t *testing.T) {
	t.Run("new session", func(t *testing.T) {
		testReviewWorktree(t, "QUEUED", "")
	})
	t.Run("claimed resumed session", func(t *testing.T) {
		testReviewWorktree(t, "CLAIMED", "prior-session")
	})
	t.Run("existing local dev", func(t *testing.T) {
		testReviewWorktree(t, "QUEUED", "", "local")
	})
	t.Run("existing remote dev", func(t *testing.T) {
		testReviewWorktree(t, "QUEUED", "", "remote")
	})
	t.Run("dev checked out elsewhere", func(t *testing.T) {
		testReviewWorktree(t, "QUEUED", "", "busy")
	})
	t.Run("two tasks one batch session", func(t *testing.T) {
		testReviewWorktree(t, "CLAIMED", "", "batch")
	})
	t.Run("batch usage limit no retry", func(t *testing.T) {
		testReviewWorktree(t, "CLAIMED", "", "batch-limit")
	})
}

func testReviewWorktree(t *testing.T, status, sourceSession string, devMode ...string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	repo := filepath.Join(root, "repo")
	git(t, root, "init", "--bare", origin)
	git(t, root, "clone", origin, repo)
	git(t, repo, "config", "user.name", "Test")
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "README.md")
	git(t, repo, "commit", "-m", "base")
	git(t, repo, "push", "-u", "origin", "main")
	if len(devMode) != 0 {
		git(t, repo, "branch", "dev")
		if devMode[0] == "remote" {
			git(t, repo, "push", "origin", "dev")
			git(t, repo, "branch", "-d", "dev")
		}
		if devMode[0] == "busy" {
			git(t, repo, "worktree", "add", filepath.Join(root, "busy-dev"), "dev")
		}
	}
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(data, "worker.db"))
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
	if err := s.AddRepository(ctx, store.Repository{
		ID:          "demo-api",
		ProjectID:   "demo",
		Name:        "API",
		Path:        repo,
		URL:         origin,
		Branch:      "main",
		TestCommand: "test -f done.txt",
	}); err != nil {
		t.Fatal(err)
	}
	task := store.Task{
		ID:              "task1",
		EmailID:         "synthetic:1",
		ProjectID:       "demo",
		RepositoryID:    "demo-api",
		Title:           "Add done file",
		Description:     "Assignment",
		Status:          status,
		SourceSessionID: sourceSession,
		CreatedAt:       time.Now(),
	}
	if _, err := s.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(root, "fake-codex")
	fakeCodex := `#!/bin/sh
printf '%s\n' "$@" > codex.args
printf done > done.txt
printf '{"type":"thread.started","thread_id":"session-one"}\n'
`
	if err := os.WriteFile(fake, []byte(fakeCodex), 0700); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Store: s, DataDir: data, CodexBinary: fake, Timeout: time.Minute}
	if len(devMode) != 0 && strings.HasPrefix(devMode[0], "batch") {
		second := task
		second.ID, second.EmailID = "task2", "synthetic:2"
		if _, err := s.InsertTask(ctx, second); err != nil {
			t.Fatal(err)
		}
		script := `#!/bin/sh
cat > batch.prompt
artifacts=$(sed -n 's/^Artifacts: //p' batch.prompt)
echo call >> calls
echo done > done.txt
echo plan > "$artifacts/PLAN.md"
echo report > "$artifacts/REPORT.md"
echo '{"task1":{"verified":true,"evidence":"test -f done.txt passed"},"task2":{"verified":false,"evidence":"blocked"}}' > "$artifacts/results.json"
echo '{"type":"thread.started","thread_id":"batch-session"}'
`
		if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		limited := devMode[0] == "batch-limit"
		if limited {
			if err := os.WriteFile(fake, []byte(script+"echo 'usage limit reached'\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
		}
		runErr := runner.RunBatch(ctx, "test", []store.Task{task, second})
		if (runErr != nil) != limited {
			t.Fatalf("Codex outcome: %v", runErr)
		}
		first, err := s.Task(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		other, err := s.Task(ctx, second.ID)
		if err != nil {
			t.Fatal(err)
		}
		expected := "READY_FOR_REVIEW"
		if limited {
			expected = "HUMAN_REVIEW"
		}
		if first.Status != expected || other.Status != "HUMAN_REVIEW" || first.SessionID != other.SessionID || first.Worktree != other.Worktree {
			t.Fatalf("batch result: %+v %+v", first, other)
		}
		calls, err := os.ReadFile(filepath.Join(first.Worktree, "calls"))
		if err != nil || string(calls) != "call\n" {
			t.Fatal("expected one Codex call", err)
		}
		prompt, err := os.ReadFile(filepath.Join(first.Worktree, "batch.prompt"))
		if err != nil ||
			!strings.Contains(string(prompt), "Starco task ID: task1") ||
			!strings.Contains(string(prompt), "Starco task ID: task2") ||
			!strings.Contains(string(prompt), "PLAN.md") {
			t.Fatal("missing batch instructions", err)
		}
		return
	}
	result, err := runner.Run(ctx, task.ID)
	if len(devMode) != 0 && devMode[0] == "busy" {
		if err == nil || result.Status != "HUMAN_REVIEW" {
			t.Fatalf("busy dev must block: result=%+v err=%v", result, err)
		}
		return
	}
	if err != nil || result.Status != "READY_FOR_REVIEW" || result.SessionID != "session-one" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "done.txt")); !os.IsNotExist(err) {
		t.Fatalf("source checkout changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(result.Worktree, "done.txt")); err != nil {
		t.Fatal(err)
	}
	if result.Branch != "dev" || git(t, result.Worktree, "symbolic-ref", "--short", "HEAD") != "dev" {
		t.Fatal("worker must execute on dev")
	}
	if git(t, repo, "symbolic-ref", "--short", "HEAD") != "main" {
		t.Fatal("source checkout branch changed")
	}
	args, err := os.ReadFile(filepath.Join(result.Worktree, "codex.args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "-m\ngpt-6.1-sol\n-c\nmodel_reasoning_effort=low\n--json\n") {
		t.Fatalf("Codex model/effort incorrect: %s", args)
	}
	if sourceSession != "" && !strings.Contains(string(args), "resume\n") {
		t.Fatalf("expected resumed session: %s", args)
	}
}

func TestClaudeAgentCommandAndSession(t *testing.T) {
	runner := &Runner{Agent: "claude", ArtifactsDir: "/artifacts"}
	name, args, err := runner.agentCommand(&store.Task{SessionID: "claude:c1"})
	joined := strings.Join(args, " ")
	if err != nil ||
		name != "claude" ||
		!strings.Contains(joined, "--model claude-opus-5-5") ||
		!strings.Contains(joined, "--add-dir /artifacts") ||
		!strings.HasSuffix(joined, "--resume c1") {
		t.Fatalf("claude command: %s %s %v", name, joined, err)
	}
	// Sessions from the other agent start fresh instead of failing to resume.
	if _, args, _ := runner.agentCommand(&store.Task{SessionID: "codex-thread"}); strings.Contains(strings.Join(args, " "), "--resume") {
		t.Fatalf("resumed Codex session with Claude: %v", args)
	}
	if _, args, _ := (&Runner{}).agentCommand(&store.Task{SessionID: "claude:c1"}); strings.Contains(strings.Join(args, " "), "resume") {
		t.Fatalf("resumed Claude session with Codex: %v", args)
	}
	if _, _, err := (&Runner{Agent: "gemini"}).agentCommand(&store.Task{}); err == nil {
		t.Fatal("unknown agent accepted")
	}
	session, err := agentSession([]byte(`{"type":"system","subtype":"hook_started","session_id":"c1"}
{"type":"system","subtype":"init","session_id":"c1"}
{"type":"result","subtype":"success","is_error":true,"result":"usage limit reached"}
`))
	if session != "claude:c1" || err == nil || !strings.Contains(err.Error(), "usage limit") {
		t.Fatalf("session=%q err=%v", session, err)
	}
}
