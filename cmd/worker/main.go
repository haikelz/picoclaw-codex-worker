// Command worker polls Starco and runs a coding agent on mapped tasks in local dev worktrees.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"picoclaw-codex-worker/internal/store"
	"picoclaw-codex-worker/internal/worker"
)

const passwordEnv = "WORKER_STARCO_PASSWORD"

var (
	slug          = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	errWorkerBusy = errors.New("worker is already polling or running")
)

type config struct {
	dataDir        string
	sessions       string
	codex          string
	agent          string
	claude         string
	claudeSessions string
	starcoURL      string
	starcoUsername string
	starcoPassword string
	execute        bool
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return fallback
}

func configuration() (config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return config{}, fmt.Errorf("find home directory: %w", err)
	}
	data := env("WORKER_DATA_DIR", filepath.Join(home, "Library", "Application Support", "picoclaw-codex-worker"))
	if err := loadEnv(env("WORKER_ENV_FILE", filepath.Join(data, "worker.env"))); err != nil {
		return config{}, err
	}

	execute := env("WORKER_EXECUTE", "false")
	if execute != "true" && execute != "false" {
		return config{}, errors.New("WORKER_EXECUTE must be true or false")
	}
	agent := env("WORKER_AGENT", worker.AgentCodex)
	if agent != worker.AgentCodex && agent != worker.AgentClaude {
		return config{}, errors.New("WORKER_AGENT must be codex or claude")
	}

	// Unset after reading so agent and validation subprocesses never inherit the password;
	// a detached drain reloads it from the env file.
	password := os.Getenv(passwordEnv)
	if err := os.Unsetenv(passwordEnv); err != nil {
		return config{}, fmt.Errorf("unset %s: %w", passwordEnv, err)
	}

	return config{
		dataDir:        data,
		sessions:       env("WORKER_CODEX_SESSIONS", filepath.Join(home, ".codex", "sessions")),
		codex:          env("WORKER_CODEX_BINARY", worker.AgentCodex),
		agent:          agent,
		claude:         env("WORKER_CLAUDE_BINARY", worker.AgentClaude),
		claudeSessions: env("WORKER_CLAUDE_SESSIONS", filepath.Join(home, ".claude", "projects")),
		starcoURL:      env("WORKER_STARCO_URL", "https://app.starco.id"),
		starcoUsername: env("WORKER_STARCO_USERNAME", ""),
		starcoPassword: password,
		execute:        execute == "true",
	}, nil
}

// loadEnv sets variables from a private KEY=value file without overriding the process environment.
func loadEnv(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect env file: %w", err)
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("env file %s must be private (chmod 600)", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read env file: %w", err)
	}

	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := strings.TrimSpace(string(raw))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("invalid env line %q", line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
			value = value[1 : len(value)-1]
		}

		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("set %s: %w", key, err)
		}
	}

	return nil
}

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) (err error) {
	if len(args) == 0 {
		return errors.New("usage: worker starco ... | poll | drain | project ... | repository ... | task ... | setup picoclaw")
	}

	cfg, err := configuration()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.dataDir, 0700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	s, err := store.Open(filepath.Join(cfg.dataDir, "worker.db"))
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, s.Close())
	}()

	switch args[0] {
	case "project":
		return projectCommand(ctx, s, args[1:])
	case "repository":
		return repositoryCommand(ctx, s, args[1:])
	case "task":
		return taskCommand(ctx, s, cfg, args[1:])
	case "starco":
		return starcoCommand(ctx, s, cfg, args[1:])
	case "drain":
		if len(args) != 1 {
			return errors.New("usage: worker drain")
		}
		return drainStarco(ctx, s, cfg)
	case "poll":
		return pollStarco(ctx, s, cfg, args[1:])
	case "setup":
		if len(args) < 2 || args[1] != "picoclaw" {
			return errors.New("usage: worker setup picoclaw [--start]")
		}
		return setupPicoclaw(ctx, cfg, args[2:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func projectCommand(
	ctx context.Context,
	s *store.Store,
	args []string,
) error {
	usage := errors.New("usage: worker project add|list|enable|disable")
	if len(args) == 0 {
		return usage
	}

	switch args[0] {
	case "add":
		flags := flag.NewFlagSet("project add", flag.ContinueOnError)
		id := flags.String("id", "", "project ID")
		name := flags.String("name", "", "display name")
		aliases := flags.String("aliases", "", "comma-separated aliases")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if !slug.MatchString(*id) || *name == "" {
			return errors.New("project requires --id slug and --name")
		}

		return s.AddProject(ctx, store.Project{
			ID:      *id,
			Name:    *name,
			Aliases: *aliases,
		})
	case "list":
		projects, err := s.Projects(ctx)
		if err != nil {
			return err
		}

		for _, p := range projects {
			fmt.Printf("%s\t%s\t%t\t%s\n", p.ID, p.Name, p.Enabled, p.Aliases)
		}
		return nil
	case "enable", "disable":
		if len(args) != 2 {
			return errors.New("provide a project ID")
		}
		return s.SetEnabled(ctx, "projects", args[1], args[0] == "enable")
	default:
		return usage
	}
}

func repositoryCommand(
	ctx context.Context,
	s *store.Store,
	args []string,
) error {
	usage := errors.New("usage: worker repository add|list|enable|disable")
	if len(args) == 0 {
		return usage
	}

	switch args[0] {
	case "add":
		flags := flag.NewFlagSet("repository add", flag.ContinueOnError)
		id := flags.String("id", "", "repository ID")
		project := flags.String("project", "", "project ID")
		name := flags.String("name", "", "display name")
		path := flags.String("path", "", "absolute checkout path")
		url := flags.String("url", "", "origin URL")
		branch := flags.String("branch", "dev", "required working branch (dev)")
		aliases := flags.String("aliases", "", "comma-separated aliases")
		test := flags.String("test", "", "test command")
		lint := flags.String("lint", "", "lint command")
		build := flags.String("build", "", "build command")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *branch != "dev" {
			return errors.New("worker always uses branch dev")
		}
		if !slug.MatchString(*id) || !slug.MatchString(*project) || *name == "" || !filepath.IsAbs(*path) {
			return errors.New("repository requires --id, --project, --name, absolute --path, and --branch")
		}

		canonical, err := filepath.EvalSymlinks(*path)
		if err != nil {
			return fmt.Errorf("resolve repository path: %w", err)
		}
		output, err := exec.CommandContext(ctx, "git", "-C", canonical, "rev-parse", "--show-toplevel").Output()
		if err != nil || strings.TrimSpace(string(output)) != canonical {
			return errors.New("repository path must be an existing Git root")
		}

		return s.AddRepository(ctx, store.Repository{
			ID:           *id,
			ProjectID:    *project,
			Name:         *name,
			URL:          *url,
			Path:         canonical,
			Branch:       *branch,
			Aliases:      *aliases,
			TestCommand:  *test,
			LintCommand:  *lint,
			BuildCommand: *build,
		})
	case "list":
		repositories, err := s.Repositories(ctx)
		if err != nil {
			return err
		}

		for _, r := range repositories {
			fmt.Printf("%s\t%s\t%s\t%t\t%s\n", r.ProjectID, r.ID, r.Path, r.Enabled, r.Branch)
		}
		return nil
	case "enable", "disable":
		if len(args) != 2 {
			return errors.New("provide a repository ID")
		}
		return s.SetEnabled(ctx, "repositories", args[1], args[0] == "enable")
	default:
		return usage
	}
}

func taskRunner(s *store.Store, cfg config) *worker.Runner {
	return &worker.Runner{
		Store:        s,
		DataDir:      cfg.dataDir,
		Timeout:      45 * time.Minute,
		Agent:        cfg.agent,
		CodexBinary:  cfg.codex,
		ClaudeBinary: cfg.claude,
	}
}

func taskCommand(
	ctx context.Context,
	s *store.Store,
	cfg config,
	args []string,
) error {
	usage := errors.New("usage: worker task list|show|assign|run|approve|reject|complete")
	if len(args) == 0 {
		return usage
	}

	switch args[0] {
	case "list":
		tasks, err := s.Tasks(ctx)
		if err != nil {
			return err
		}

		for _, t := range tasks {
			fmt.Printf("%s\t%s\t%s\t%s\n", t.ID, t.ProjectID, t.Status, t.Title)
		}
		return nil
	case "show":
		if len(args) != 2 {
			return errors.New("usage: worker task show ID")
		}
		return showTask(ctx, s, args[1])
	case "assign":
		flags := flag.NewFlagSet("task assign", flag.ContinueOnError)
		repositoryID := flags.String("repository", "", "registered repository ID")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 || *repositoryID == "" {
			return errors.New("usage: worker task assign --repository ID TASK_ID")
		}
		return assignTask(ctx, s, flags.Arg(0), *repositoryID)
	case "run":
		if len(args) != 2 {
			return errors.New("usage: worker task run ID")
		}
		if strings.HasPrefix(args[1], "starco-") {
			return errors.New("use worker drain for Starco tasks to revalidate remote status and priority")
		}
		if !workingHours(time.Now()) {
			return errors.New("outside 08:00-17:00 Asia/Jakarta on weekdays")
		}

		lock, err := acquireLock(cfg.dataDir)
		if err != nil {
			return err
		}
		defer releaseLock(lock)

		t, err := taskRunner(s, cfg).Run(ctx, args[1])
		fmt.Printf("%s\t%s\n", t.ID, t.Status)
		return err
	case "approve":
		if len(args) != 2 {
			return errors.New("usage: worker task approve ID")
		}
		t, err := s.Task(ctx, args[1])
		if err != nil {
			return err
		}

		if t.Status != store.StatusReadyForReview {
			return errors.New("task is not ready for review")
		}
		if err := s.AddReview(ctx, t.ID, store.DecisionApproved, ""); err != nil {
			return err
		}

		t.Status = store.StatusApproved
		return s.UpdateTask(ctx, t)
	case "reject":
		flags := flag.NewFlagSet("task reject", flag.ContinueOnError)
		reason := flags.String("reason", "", "review feedback")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 || strings.TrimSpace(*reason) == "" {
			return errors.New("usage: worker task reject --reason TEXT ID")
		}
		t, err := s.Task(ctx, flags.Arg(0))
		if err != nil {
			return err
		}

		if t.Status != store.StatusReadyForReview {
			return errors.New("task is not ready for review")
		}
		if err := s.AddReview(ctx, t.ID, store.DecisionChangesRequested, *reason); err != nil {
			return err
		}

		t.Status = store.StatusQueued
		t.Feedback = *reason
		t.Attempt = 0
		return s.UpdateTask(ctx, t)
	case "complete":
		flags := flag.NewFlagSet("task complete", flag.ContinueOnError)
		sha := flags.String("commit", "", "commit SHA")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 || *sha == "" {
			return errors.New("usage: worker task complete --commit SHA ID")
		}
		return completeTask(ctx, s, flags.Arg(0), *sha)
	default:
		return usage
	}
}

func showTask(
	ctx context.Context,
	s *store.Store,
	id string,
) error {
	t, err := s.Task(ctx, id)
	if err != nil {
		return err
	}
	logPath, validation, err := s.LastExecution(ctx, t.ID)
	if err != nil {
		return err
	}

	fmt.Printf(
		"ID: %s\nEmail ID: %s\nProject: %s\nRepository: %s\nTitle: %s\nDescription: %s\nStatus: %s\n"+
			"Branch: %s\nWorktree: %s\nSource agent session: %s\nTask agent session: %s\nAttempt: %d\nFeedback: %s\n",
		t.ID,
		t.EmailID,
		t.ProjectID,
		t.RepositoryID,
		t.Title,
		t.Description,
		t.Status,
		t.Branch,
		t.Worktree,
		t.SourceSessionID,
		t.SessionID,
		t.Attempt,
		t.Feedback,
	)
	if validation != "" {
		fmt.Printf("Validation:\n%s\nLog: %s\n", validation, logPath)
	}

	if t.Worktree == "" {
		return nil
	}
	// Git output is informational here: a missing worktree prints its error instead of failing the command.
	status, _ := exec.CommandContext(ctx, "git", "-C", t.Worktree, "status", "--short").CombinedOutput()
	fmt.Printf("Files changed:\n%s", status)
	diff, _ := exec.CommandContext(ctx, "git", "-C", t.Worktree, "diff", "HEAD").CombinedOutput()
	fmt.Printf("Diff:\n%s", diff)

	return nil
}

func assignTask(
	ctx context.Context,
	s *store.Store,
	taskID string,
	repositoryID string,
) error {
	t, err := s.Task(ctx, taskID)
	if err != nil {
		return err
	}
	if t.Status != store.StatusHumanReview {
		return errors.New("only HUMAN_REVIEW tasks can be assigned")
	}

	repositories, err := s.Repositories(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, r := range repositories {
		if r.ID == repositoryID && r.Enabled {
			t.ProjectID = r.ProjectID
			t.RepositoryID = r.ID
			found = true
		}
	}
	if !found {
		return errors.New("repository is not registered or enabled")
	}

	t.Status = store.StatusQueued
	t.Feedback = ""
	return s.UpdateTask(ctx, t)
}

func completeTask(
	ctx context.Context,
	s *store.Store,
	taskID string,
	sha string,
) error {
	t, err := s.Task(ctx, taskID)
	if err != nil {
		return err
	}
	if t.Status != store.StatusApproved {
		return errors.New("task is not approved")
	}

	head, err := exec.CommandContext(ctx, "git", "-C", t.Worktree, "rev-parse", t.Branch).Output()
	if err != nil {
		return fmt.Errorf("resolve task branch: %w", err)
	}
	commit, err := exec.CommandContext(ctx, "git", "-C", t.Worktree, "rev-parse", sha).Output()
	if err != nil {
		return fmt.Errorf("resolve commit: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(head)), strings.TrimSpace(string(commit))) {
		return errors.New("commit is not the task branch HEAD")
	}
	dirty, err := exec.CommandContext(ctx, "git", "-C", t.Worktree, "status", "--porcelain").Output()
	if err != nil {
		return fmt.Errorf("check worktree status: %w", err)
	}
	if len(strings.TrimSpace(string(dirty))) > 0 {
		return errors.New("task worktree still has uncommitted changes")
	}

	t.Status = store.StatusDone
	return s.UpdateTask(ctx, t)
}

func workingHours(now time.Time) bool {
	location, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		return false
	}
	local := now.In(location)

	return local.Weekday() != time.Saturday &&
		local.Weekday() != time.Sunday &&
		local.Hour() >= 8 &&
		local.Hour() < 17
}

func acquireLock(dataDir string) (*os.File, error) {
	return acquireFileLock(filepath.Join(dataDir, "worker.lock"))
}

func acquireFileLock(path string) (*os.File, error) {
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(errWorkerBusy, closeErr)
		}

		return nil, errors.Join(fmt.Errorf("lock %s: %w", path, err), closeErr)
	}

	return lock, nil
}

// releaseLock is best-effort: closing the descriptor, or process exit, releases the flock regardless.
func releaseLock(lock *os.File) {
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
}
