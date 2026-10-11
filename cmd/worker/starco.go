package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"picoclaw-codex-worker/internal/starco"
	"picoclaw-codex-worker/internal/store"
	"picoclaw-codex-worker/internal/worker"
)

const (
	starcoKeychainService = "picoclaw-codex-worker.starco"
	starcoTaskPrefix      = "starco-"
	starcoProjectKey      = "starco:project:"
)

type starcoSource interface {
	Tasks(context.Context) ([]starco.Task, error)
	Detail(context.Context, starco.Task) (starco.Task, error)
}

// starcoClient logs in with the given password, else WORKER_STARCO_PASSWORD, else macOS Keychain.
func starcoClient(
	ctx context.Context,
	cfg config,
	password string,
) (*starco.Client, error) {
	if cfg.starcoUsername == "" {
		return nil, errors.New("set WORKER_STARCO_USERNAME")
	}

	if password == "" {
		password = cfg.starcoPassword
	}
	if password == "" {
		output, err := exec.CommandContext(
			ctx,
			"security",
			"find-generic-password",
			"-a", cfg.starcoUsername,
			"-s", starcoKeychainService,
			"-w",
		).Output()
		if err != nil {
			return nil, errors.New("no Starco password: set WORKER_STARCO_PASSWORD in the env file or store it in macOS Keychain")
		}
		password = strings.TrimSuffix(string(output), "\n")
	}

	client, err := starco.New(cfg.starcoURL)
	if err != nil {
		return nil, fmt.Errorf("create Starco client: %w", err)
	}
	if err := client.Login(ctx, cfg.starcoUsername, password); err != nil {
		return nil, fmt.Errorf("log in to Starco: %w", err)
	}

	return client, nil
}

func starcoCommand(
	ctx context.Context,
	s *store.Store,
	cfg config,
	args []string,
) error {
	usage := errors.New("usage: worker starco inspect|sync|queue|map")
	if len(args) == 0 {
		return usage
	}

	switch args[0] {
	case "map":
		flags := flag.NewFlagSet("starco map", flag.ContinueOnError)
		projectID := flags.Int("project", 0, "numeric Starco project ID")
		repositoryID := flags.String("repository", "", "registered local repository ID")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *projectID <= 0 || *repositoryID == "" || flags.NArg() != 0 {
			return errors.New("usage: worker starco map --project NUMBER --repository ID")
		}

		repositories, err := s.Repositories(ctx)
		if err != nil {
			return err
		}
		for _, repository := range repositories {
			if repository.ID == *repositoryID && repository.Enabled {
				return s.SetState(ctx, starcoProjectKey+strconv.Itoa(*projectID), repository.ID)
			}
		}

		return errors.New("repository must be registered and enabled")
	case "queue":
		if len(args) != 1 {
			return errors.New("usage: worker starco queue")
		}

		queue, err := eligibleQueue(ctx, s, cfg)
		if err != nil {
			return err
		}
		for _, task := range queue {
			fmt.Printf("%s\t%s\t%s\t%s\n", task.ID, task.CreatedAt.Format(time.RFC3339), task.RepositoryID, task.Title)
		}

		return nil
	case "inspect", "sync":
		flags := flag.NewFlagSet("starco "+args[0], flag.ContinueOnError)
		stdin := flags.Bool("password-stdin", false, "read transient password from stdin instead of Keychain")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		return inspectStarco(ctx, s, cfg, args[0] == "sync", *stdin)
	default:
		return usage
	}
}

// inspectStarco prints the remote candidate snapshot and, for sync, persists it locally.
func inspectStarco(
	ctx context.Context,
	s *store.Store,
	cfg config,
	persist bool,
	passwordFromStdin bool,
) error {
	if persist {
		lock, err := acquireLock(cfg.dataDir)
		if err != nil {
			return err
		}
		defer releaseLock(lock)
	}

	var password string
	if passwordFromStdin {
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			return errors.New("password stdin is empty")
		}
		password = scanner.Text()
	}

	client, err := starcoClient(ctx, cfg, password)
	if err != nil {
		return err
	}
	snapshot, err := starcoSnapshot(ctx, s, client)
	if err != nil {
		return err
	}
	if persist {
		if err := s.SyncStarco(ctx, snapshot); err != nil {
			return err
		}
	}

	for _, remote := range snapshot {
		fmt.Printf(
			"%s\t%s\tdependency-ready=%t\tstarco-project=%d (%s)\trepository=%s\tcreated=%s\t%s\n",
			remote.Task.ID,
			remote.Priority,
			remote.Eligible,
			remote.RemoteProjectID,
			remote.RemoteProjectName,
			remote.Task.RepositoryID,
			remote.Task.CreatedAt.Format(time.RFC3339),
			remote.Task.Title,
		)
	}
	fmt.Printf("%d candidate tasks inspected; no remote task was changed\n", len(snapshot))

	return nil
}

// eligibleQueue returns the local queue minus tasks already assigned in agent sessions.
func eligibleQueue(
	ctx context.Context,
	s *store.Store,
	cfg config,
) ([]store.Task, error) {
	queue, err := s.StarcoQueue(ctx)
	if err != nil {
		return nil, err
	}
	queue, skipped, err := worker.FilterSessionTasks(cfg.sessions, cfg.claudeSessions, queue)
	if err != nil {
		return nil, err
	}

	for id, session := range skipped {
		fmt.Printf("skip %s: already assigned in agent session %s (active, completed, or needs review)\n", id, session)
	}

	return queue, nil
}

// starcoSnapshot reads Ready tasks, plus Doing tasks this worker owns as approved retries.
func starcoSnapshot(
	ctx context.Context,
	s *store.Store,
	client starcoSource,
) ([]store.StarcoTask, error) {
	remote, err := client.Tasks(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Starco tasks: %w", err)
	}
	repositories, err := s.Repositories(ctx)
	if err != nil {
		return nil, err
	}
	local, err := s.Tasks(ctx)
	if err != nil {
		return nil, err
	}

	statuses := make(map[int]string, len(remote))
	for _, task := range remote {
		statuses[task.ID] = task.Status
	}
	ownedRetries := make(map[string]bool)
	for _, task := range local {
		ownedRetries[task.ID] = task.Status == store.StatusQueued && task.Worktree != "" && task.Feedback != ""
	}

	var snapshot []store.StarcoTask
	for _, task := range remote {
		id := starcoTaskPrefix + strconv.Itoa(task.ID)
		ready := strings.EqualFold(task.Status, starco.StatusReady)
		ownedRetry := strings.EqualFold(task.Status, starco.StatusDoing) && ownedRetries[id]
		if !ready && !ownedRetry {
			continue
		}

		task, err = client.Detail(ctx, task)
		if err != nil {
			return nil, fmt.Errorf("read Starco task %d: %w", task.ID, err)
		}
		if task.Project.ID <= 0 {
			return nil, fmt.Errorf("task %d has no project ID", task.ID)
		}

		mapping, err := s.State(ctx, starcoProjectKey+strconv.Itoa(task.Project.ID))
		if err != nil {
			return nil, err
		}
		var projectID string
		var repositoryID string
		for _, repository := range repositories {
			if repository.ID == mapping && repository.Enabled {
				projectID = repository.ProjectID
				repositoryID = repository.ID
			}
		}

		eligible := task.DependencyID == nil || strings.EqualFold(statuses[*task.DependencyID], starco.StatusCompleted)
		snapshot = append(snapshot, store.StarcoTask{
			RemoteID:          task.ID,
			RemoteProjectID:   task.Project.ID,
			RemoteProjectName: task.ProjectName,
			Priority:          task.Priority,
			Eligible:          eligible,
			Task: store.Task{
				ID:           id,
				EmailID:      "starco:" + strconv.Itoa(task.ID),
				ProjectID:    projectID,
				RepositoryID: repositoryID,
				Title:        task.Code + ": " + task.Title,
				Description: fmt.Sprintf(
					"Starco project %d (%s), task %d.\n%s",
					task.Project.ID,
					task.ProjectName,
					task.ID,
					task.Description,
				),
				CreatedAt: task.CreatedAt,
			},
		})
	}

	return snapshot, nil
}

func syncStarco(
	ctx context.Context,
	s *store.Store,
	cfg config,
) (*starco.Client, error) {
	lock, err := acquireLock(cfg.dataDir)
	if err != nil {
		return nil, err
	}
	defer releaseLock(lock)

	client, err := starcoClient(ctx, cfg, "")
	if err != nil {
		return nil, err
	}
	snapshot, err := starcoSnapshot(ctx, s, client)
	if err != nil {
		return nil, err
	}
	if err := s.SyncStarco(ctx, snapshot); err != nil {
		return nil, err
	}

	return client, nil
}

func pollStarco(
	ctx context.Context,
	s *store.Store,
	cfg config,
	args []string,
) error {
	if len(args) != 0 {
		return errors.New("usage: worker poll")
	}
	if _, _, allowed := batchWindow(time.Now()); !allowed {
		fmt.Println("outside working hours; no polling or new execution")
		return nil
	}

	if _, err := syncStarco(ctx, s, cfg); err != nil {
		return err
	}
	if err := collectBatch(ctx, s, time.Now()); err != nil {
		return err
	}
	if !cfg.execute {
		fmt.Println("Starco synchronized; execution disabled (WORKER_EXECUTE=false)")
		return nil
	}

	// Transfer execution to an independent process so hourly polling can continue.
	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find worker binary: %w", err)
	}
	log, err := os.OpenFile(filepath.Join(cfg.dataDir, "drain.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open drain log: %w", err)
	}
	cmd := exec.Command(binary, "drain")
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
	// The child holds its own descriptor, so closing ours only affects this process.
	if err := errors.Join(cmd.Start(), log.Close()); err != nil {
		return fmt.Errorf("start drain: %w", err)
	}

	return cmd.Process.Release()
}

// drainStarco claims and runs batches until the queue is empty. Only one drain runs at a time.
func drainStarco(
	ctx context.Context,
	s *store.Store,
	cfg config,
) error {
	if !cfg.execute {
		return errors.New("execution disabled; configure mapping and WORKER_EXECUTE=true first")
	}

	// Only the execution-lock owner may recover interrupted task state.
	lock, err := acquireFileLock(filepath.Join(cfg.dataDir, "execution.lock"))
	if errors.Is(err, errWorkerBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer releaseLock(lock)

	if err := recoverStarco(ctx, s); err != nil {
		return err
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		client, err := syncStarco(ctx, s, cfg)
		if err != nil {
			return err
		}
		queue, err := eligibleQueue(ctx, s, cfg)
		if err != nil {
			return err
		}
		queue, batchID, err := nextBatch(ctx, s, queue)
		if err != nil {
			return err
		}
		if len(queue) == 0 {
			return nil
		}

		for i := range queue {
			claimed, err := claimStarcoTask(ctx, s, client, queue[i])
			if err != nil {
				return err
			}
			queue[i] = claimed
		}

		if err := taskRunner(s, cfg).RunBatch(ctx, batchID, queue); err != nil {
			return err
		}
	}
}

// claimStarcoTask marks a task Doing in Starco. A failed claim leaves the task for human review.
func claimStarcoTask(
	ctx context.Context,
	s *store.Store,
	client *starco.Client,
	task store.Task,
) (store.Task, error) {
	remoteID, err := strconv.Atoi(strings.TrimPrefix(task.ID, starcoTaskPrefix))
	if err != nil {
		return task, fmt.Errorf("parse Starco task ID %s: %w", task.ID, err)
	}

	task.Status = store.StatusClaiming
	if err := s.UpdateTask(ctx, task); err != nil {
		return task, err
	}

	expectedStatus := starco.StatusReady
	if task.Worktree != "" && task.Feedback != "" {
		expectedStatus = starco.StatusDoing
	}
	if err := client.Start(ctx, remoteID, expectedStatus); err != nil {
		task.Status = store.StatusHumanReview
		task.Feedback = err.Error()
		return task, errors.Join(fmt.Errorf("start Starco task %d: %w", remoteID, err), s.UpdateTask(ctx, task))
	}

	task.Status = store.StatusClaimed
	return task, s.UpdateTask(ctx, task)
}

// recoverStarco moves interrupted or uncertain claims to human review; they are never retried automatically.
func recoverStarco(ctx context.Context, s *store.Store) error {
	tasks, err := s.Tasks(ctx)
	if err != nil {
		return err
	}

	for _, task := range tasks {
		interrupted := task.Status == store.StatusRunning ||
			task.Status == store.StatusValidating ||
			task.Status == store.StatusClaiming ||
			task.Status == store.StatusClaimed
		if !strings.HasPrefix(task.ID, starcoTaskPrefix) || !interrupted {
			continue
		}

		task.Status = store.StatusHumanReview
		task.Feedback = "interrupted execution or uncertain remote claim; inspect Starco and worktree before retrying"
		if err := s.UpdateTask(ctx, task); err != nil {
			return err
		}
	}

	return nil
}
