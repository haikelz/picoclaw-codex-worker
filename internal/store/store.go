// Package store persists projects, repositories, tasks, executions, and reviews in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

// Local task statuses. Remote Starco statuses live in package starco.
const (
	StatusUnmapped       = "UNMAPPED"
	StatusQueued         = "QUEUED"
	StatusClaiming       = "CLAIMING"
	StatusClaimed        = "CLAIMED"
	StatusRunning        = "RUNNING"
	StatusValidating     = "VALIDATING"
	StatusReadyForReview = "READY_FOR_REVIEW"
	StatusHumanReview    = "HUMAN_REVIEW"
	StatusApproved       = "APPROVED"
	StatusDone           = "DONE"
)

// Review decisions recorded in task_reviews.
const (
	DecisionApproved         = "APPROVED"
	DecisionChangesRequested = "CHANGES_REQUESTED"
)

const schema = `PRAGMA foreign_keys=ON;
PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS projects (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	aliases TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS repositories (
	id TEXT PRIMARY KEY,
	project_id TEXT NOT NULL REFERENCES projects(id),
	name TEXT NOT NULL,
	url TEXT NOT NULL DEFAULT '',
	local_path TEXT NOT NULL UNIQUE,
	default_branch TEXT NOT NULL,
	aliases TEXT NOT NULL DEFAULT '',
	test_command TEXT NOT NULL DEFAULT '',
	lint_command TEXT NOT NULL DEFAULT '',
	build_command TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS tasks (
	id TEXT PRIMARY KEY,
	email_id TEXT NOT NULL UNIQUE,
	project_id TEXT NOT NULL DEFAULT '',
	repository_id TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL,
	description TEXT NOT NULL,
	status TEXT NOT NULL,
	attempt INTEGER NOT NULL DEFAULT 0,
	branch TEXT NOT NULL DEFAULT '',
	worktree TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	source_session_id TEXT NOT NULL DEFAULT '',
	feedback TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS task_executions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id TEXT NOT NULL REFERENCES tasks(id),
	attempt INTEGER NOT NULL,
	started_at TEXT NOT NULL,
	finished_at TEXT NOT NULL,
	exit_code INTEGER NOT NULL,
	log_path TEXT NOT NULL,
	validation TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS task_reviews (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id TEXT NOT NULL REFERENCES tasks(id),
	decision TEXT NOT NULL,
	feedback TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS state (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);`

const taskColumns = `id,email_id,project_id,repository_id,title,description,status,attempt,
branch,worktree,session_id,source_session_id,feedback,created_at`

// Store is the worker's SQLite database. It allows one open connection.
type Store struct {
	DB *sql.DB
}

// Project groups repositories under a stable slug.
type Project struct {
	ID      string
	Name    string
	Aliases string
	Enabled bool
}

// Repository is a registered local checkout with its validation commands.
type Repository struct {
	ID           string
	ProjectID    string
	Name         string
	URL          string
	Path         string
	Branch       string
	Aliases      string
	TestCommand  string
	LintCommand  string
	BuildCommand string
	Enabled      bool
}

// Task is local execution state. EmailID is a legacy unique source key.
type Task struct {
	ID              string
	EmailID         string
	ProjectID       string
	RepositoryID    string
	Title           string
	Description     string
	Status          string
	Branch          string
	Worktree        string
	SessionID       string
	SourceSessionID string
	Feedback        string
	Attempt         int
	CreatedAt       time.Time
}

// Open creates the database file with mode 0600 and applies the schema.
func Open(path string) (*Store, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("create database: %w", err)
	}
	if err := file.Chmod(0600); err != nil {
		return nil, errors.Join(fmt.Errorf("restrict database mode: %w", err), file.Close())
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close database file: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		return nil, errors.Join(fmt.Errorf("initialize database: %w", err), db.Close())
	}

	return &Store{
		DB: db,
	}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.DB.Close()
}

// AddProject inserts a project.
func (s *Store) AddProject(ctx context.Context, p Project) error {
	_, err := s.DB.ExecContext(
		ctx,
		`INSERT INTO projects(id,name,aliases) VALUES(?,?,?)`,
		p.ID,
		p.Name,
		p.Aliases,
	)
	if err != nil {
		return fmt.Errorf("insert project: %w", err)
	}

	return nil
}

// Projects lists projects by ID.
func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,name,aliases,enabled FROM projects ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query projects: %w", err)
	}
	defer rows.Close()

	var result []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Aliases, &p.Enabled); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}

		result = append(result, p)
	}

	return result, rows.Err()
}

// AddRepository inserts a repository.
func (s *Store) AddRepository(ctx context.Context, r Repository) error {
	_, err := s.DB.ExecContext(
		ctx,
		`INSERT INTO repositories(
	id,project_id,name,url,local_path,default_branch,aliases,test_command,lint_command,build_command
) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		r.ID,
		r.ProjectID,
		r.Name,
		r.URL,
		r.Path,
		r.Branch,
		r.Aliases,
		r.TestCommand,
		r.LintCommand,
		r.BuildCommand,
	)
	if err != nil {
		return fmt.Errorf("insert repository: %w", err)
	}

	return nil
}

// Repositories lists repositories by project, then ID.
func (s *Store) Repositories(ctx context.Context) ([]Repository, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT
	id,project_id,name,url,local_path,default_branch,aliases,test_command,lint_command,build_command,enabled
FROM repositories
ORDER BY project_id,id`)
	if err != nil {
		return nil, fmt.Errorf("query repositories: %w", err)
	}
	defer rows.Close()

	var result []Repository
	for rows.Next() {
		var r Repository
		if err := rows.Scan(
			&r.ID,
			&r.ProjectID,
			&r.Name,
			&r.URL,
			&r.Path,
			&r.Branch,
			&r.Aliases,
			&r.TestCommand,
			&r.LintCommand,
			&r.BuildCommand,
			&r.Enabled,
		); err != nil {
			return nil, fmt.Errorf("scan repository: %w", err)
		}

		result = append(result, r)
	}

	return result, rows.Err()
}

// SetEnabled toggles a project or repository. A missing ID returns sql.ErrNoRows.
func (s *Store) SetEnabled(
	ctx context.Context,
	table string,
	id string,
	enabled bool,
) error {
	if table != "projects" && table != "repositories" {
		return errors.New("invalid table")
	}

	result, err := s.DB.ExecContext(ctx, `UPDATE `+table+` SET enabled=? WHERE id=?`, enabled, id)
	if err != nil {
		return fmt.Errorf("update %s: %w", table, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count updated %s: %w", table, err)
	}

	if n == 0 {
		return sql.ErrNoRows
	}

	return nil
}

// InsertTask inserts a task unless its ID or source key exists, and reports whether it inserted.
func (s *Store) InsertTask(ctx context.Context, t Task) (bool, error) {
	result, err := s.DB.ExecContext(
		ctx,
		`INSERT OR IGNORE INTO tasks(
	id,email_id,project_id,repository_id,title,description,status,created_at
) VALUES(?,?,?,?,?,?,?,?)`,
		t.ID,
		t.EmailID,
		t.ProjectID,
		t.RepositoryID,
		t.Title,
		t.Description,
		t.Status,
		t.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return false, fmt.Errorf("insert task: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count inserted tasks: %w", err)
	}

	return n == 1, nil
}

// Tasks lists tasks, newest first.
func (s *Store) Tasks(ctx context.Context) ([]Task, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("query tasks: %w", err)
	}
	defer rows.Close()

	var result []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}

		result = append(result, t)
	}

	return result, rows.Err()
}

// Task returns one task. A missing ID returns sql.ErrNoRows.
func (s *Store) Task(ctx context.Context, id string) (Task, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=?`, id)
	return scanTask(row)
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTask(row scanner) (Task, error) {
	var t Task
	var created string
	if err := row.Scan(
		&t.ID,
		&t.EmailID,
		&t.ProjectID,
		&t.RepositoryID,
		&t.Title,
		&t.Description,
		&t.Status,
		&t.Attempt,
		&t.Branch,
		&t.Worktree,
		&t.SessionID,
		&t.SourceSessionID,
		&t.Feedback,
		&created,
	); err != nil {
		return t, fmt.Errorf("scan task: %w", err)
	}

	createdAt, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return t, fmt.Errorf("parse task creation time: %w", err)
	}
	t.CreatedAt = createdAt

	return t, nil
}

// UpdateTask saves a task's mutable fields.
func (s *Store) UpdateTask(ctx context.Context, t Task) error {
	_, err := s.DB.ExecContext(
		ctx,
		`UPDATE tasks SET
	project_id=?,repository_id=?,status=?,attempt=?,branch=?,worktree=?,session_id=?,source_session_id=?,feedback=?
WHERE id=?`,
		t.ProjectID,
		t.RepositoryID,
		t.Status,
		t.Attempt,
		t.Branch,
		t.Worktree,
		t.SessionID,
		t.SourceSessionID,
		t.Feedback,
		t.ID,
	)
	if err != nil {
		return fmt.Errorf("update task: %w", err)
	}

	return nil
}

// State returns a stored value, or "" when the key is absent.
func (s *Store) State(ctx context.Context, key string) (string, error) {
	var value string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM state WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read state %s: %w", key, err)
	}

	return value, nil
}

// SetState stores a value.
func (s *Store) SetState(
	ctx context.Context,
	key string,
	value string,
) error {
	_, err := s.DB.ExecContext(
		ctx,
		`INSERT INTO state(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key,
		value,
	)
	if err != nil {
		return fmt.Errorf("write state %s: %w", key, err)
	}

	return nil
}

// AddExecution records one agent attempt and its validation result.
func (s *Store) AddExecution(
	ctx context.Context,
	taskID string,
	attempt int,
	started time.Time,
	exitCode int,
	logPath string,
	validation string,
) error {
	_, err := s.DB.ExecContext(
		ctx,
		`INSERT INTO task_executions(
	task_id,attempt,started_at,finished_at,exit_code,log_path,validation
) VALUES(?,?,?,?,?,?,?)`,
		taskID,
		attempt,
		started.UTC().Format(time.RFC3339Nano),
		time.Now().UTC().Format(time.RFC3339Nano),
		exitCode,
		logPath,
		validation,
	)
	if err != nil {
		return fmt.Errorf("insert execution: %w", err)
	}

	return nil
}

// LastExecution returns the newest execution's log path and validation, or empty values.
func (s *Store) LastExecution(ctx context.Context, taskID string) (string, string, error) {
	var logPath string
	var validation string
	err := s.DB.QueryRowContext(
		ctx,
		`SELECT log_path,validation FROM task_executions WHERE task_id=? ORDER BY id DESC LIMIT 1`,
		taskID,
	).Scan(&logPath, &validation)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read last execution: %w", err)
	}

	return logPath, validation, nil
}

// AddReview records a review decision.
func (s *Store) AddReview(
	ctx context.Context,
	taskID string,
	decision string,
	feedback string,
) error {
	_, err := s.DB.ExecContext(
		ctx,
		`INSERT INTO task_reviews(task_id,decision,feedback,created_at) VALUES(?,?,?,?)`,
		taskID,
		decision,
		feedback,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert review: %w", err)
	}

	return nil
}
