# PicoClaw Codex worker

Local macOS worker for Starco tasks. PicoClaw triggers hourly polling; a separate worker process executes one batch per repository through one coding-agent invocation in a shared isolated dev worktree. `WORKER_AGENT` selects the agent: `codex` (default) uses **`gpt-6.1-sol` with reasoning effort `low`**; `claude` uses Claude Code headless with **`claude-opus-5-5`**.

## Execution rules

- Monday–Friday, Asia/Jakarta: hourly collection at 08:00–17:00 inclusive. At 08:00 collect Created On since 17:00 on the previous working day; at 09:00–17:00 collect the previous hour. Starts are inclusive, ends exclusive. The 17:00 batch executes immediately when free and may finish after hours. Drain may execute already collected batches after hours, but never admits new tasks without poll collection.
- Batch membership and collection checkpoint persist atomically. Failed/missed polling catches up from the last checkpoint into the next collection. No automatic pre-window historical backlog on first start. Blocked/unmapped tasks within collected windows retain membership. One prompt/session lists every eligible task in a batch/repository; already assigned sessions are skipped. Each task stores the same batch session and worktree, with separate results/review states.
- Only `Ready` tasks enter the new-work queue. `High` is urgent and goes first. Within urgent tasks, and within all other tasks, use creation time ascending, then numeric task ID. Medium does not outrank an older Low task.
- A declared dependency must be `Completed` in Starco. A missing/invisible dependency blocks execution.
- Polling can continue while Codex works. Before each next batch, synchronize again so status and priority changes are respected. Urgent work never cancels an active batch. A dependency-blocked task becoming eligible later may require a follow-up session rather than joining an already running batch.
- Before selection, the worker skips tasks explicitly assigned in Codex or Claude Code session user messages (`~/.codex/sessions`, `~/.claude/projects`; subagent sessions are ignored), preserving urgent/FIFO order for the remaining tasks. New prompts carry `Starco task ID: starco-N`. Legacy messages match explicit lines such as `implement BOOK-14`, `work on BOOK-14`, or `kerjakan BOOK-14`; title-only matches, incidental references, and assistant/tool output do not count. Assigned sessions are conservatively skipped whether active, completed, or interrupted (inspect interrupted sessions manually). Approved local retries may resume their own linked session when it belongs to the configured agent; after switching agents the retry starts a fresh session. Session read/parse failures block execution. The worker no longer resumes an unrelated latest repository session.
- Project IDs require explicit mappings to enabled, registered repositories. Missing mappings become `UNMAPPED`, not guessed repositories.
- The user chooses the repository mapping. Work always runs on `dev`, never on task-specific branches. An existing local `dev` is preserved as-is; otherwise remote `dev` is fetched, or a local `dev` is created from the registered checkout's committed HEAD when neither exists. Remote lookup failures stop execution; there is no automatic push. A `dev` already checked out elsewhere blocks execution for human review. Finish/review and release the previous task worktree before starting another task in the same repository. Existing non-dev task worktrees require manual migration.
- SQLite snapshots are atomic. Local execution/review state survives repeated polling. Tasks removed from the remote queue become ineligible, not deleted.
- A machine-local execution lock prevents duplicate workers on this laptop. Starco's status update is not an atomic distributed claim: do not run another automation worker for this account on another machine.
- Before execution, mark the task `Doing`. Uncertain status updates and interrupted executions enter `HUMAN_REVIEW`; they are not automatically retried.
- Successful checks produce local `READY_FOR_REVIEW`. The worker does **not** auto-commit, push, merge, deploy, send approval, or mark remote tasks Completed. Remote approval is a manual step after delivery.
- Execution is disabled by default. Without `WORKER_EXECUTE=true`, polling only synchronizes local state.

Batch prompts require the software-engineer role, strict `.agents` guidance, harness repository protocol, a step-by-step plan before coding, testing and documentation. Operational `PROMPT.md`, `PLAN.md`, `REPORT.md` and `results.json` live under the private data directory's `artifacts/REPO/batch-*`, not in company Git. Missing plan/report, invalid per-task results or failed checks prevent successful review status. The plan-before-code requirement is a prompt instruction; the worker checks final artifacts, not a temporal proof of agent behavior. Usage/provider failures stop the batch without automatic retry and leave tasks for human review; the detailed error is in the Codex log.

## Build

Requires Go, Git, the selected agent CLI (Codex logged in with ChatGPT, or Claude Code logged in with `claude`), and a local PicoClaw binary. Place PicoClaw beside the worker binary.

```sh
mkdir -p .bin
go build -o .bin/worker ./cmd/worker
# Place the picoclaw executable at .bin/picoclaw.
./.bin/worker setup picoclaw
```

`setup picoclaw` configures PicoClaw's OpenAI OAuth provider with `gpt-6.1-sol`, a command cron job, and a launchd service. Coding tasks run through Codex CLI with `gpt-6.1-sol` and reasoning effort `low`. It does not start the service until `--start` is supplied.

## Accounts

PicoClaw and Codex keep separate ChatGPT sign-ins. Complete the PicoClaw sign-in in the isolated home:

```sh
PICOCLAW_HOME="$HOME/Library/Application Support/picoclaw-codex-worker/picoclaw" .bin/picoclaw auth login --provider openai
PICOCLAW_HOME="$HOME/Library/Application Support/picoclaw-codex-worker/picoclaw" .bin/picoclaw auth status
./.bin/worker setup picoclaw
```

The final setup command reapplies the explicit Codex model after PicoClaw's login changes its default model. The coding worker uses the existing Codex CLI login (`codex login status`).

Store the Starco password in the login Keychain. Run this in your terminal, replacing the account placeholder; **leave `-w` last** so macOS prompts instead of exposing the password in command arguments:

```sh
security add-generic-password -U -a '<STARCO_ACCOUNT_EMAIL>' -s picoclaw-codex-worker.starco -w
```

The worker reads it through `security find-generic-password`.

Alternatively, set `WORKER_STARCO_PASSWORD` in the private env file (mode 0600); it takes precedence over Keychain. Quote values with leading or trailing spaces. The worker removes it from its environment after loading, so agent and validation subprocesses do not inherit it, and Claude's Read tool is denied the default env file. Keychain remains the safer option: the env file is still plain text readable by your user, and a coding agent's shell commands could read it. Confirm access on first use if macOS prompts. Never place the password in the repository or agent prompt. Cookies remain in memory; each synchronization logs in again, so expired sessions fail closed and the next poll can authenticate afresh.

## Configure

Create `$HOME/Library/Application Support/picoclaw-codex-worker/worker.env` with mode `0600`:

```dotenv
WORKER_STARCO_URL=https://app.starco.id
WORKER_STARCO_USERNAME=<STARCO_ACCOUNT_EMAIL>
WORKER_EXECUTE=false
# codex (default) or claude
WORKER_AGENT=codex
```

`WORKER_DATA_DIR` overrides private storage. `WORKER_ENV_FILE` overrides the env-file location. `WORKER_CODEX_BINARY`, `WORKER_CODEX_SESSIONS`, `WORKER_CLAUDE_BINARY` and `WORKER_CLAUDE_SESSIONS` override the CLI and session-history paths. Rerun `setup picoclaw` after changing `WORKER_AGENT` so the launchd `PATH` includes the selected CLI.

Claude runs with `--permission-mode auto` and denies `git commit/push/merge/rebase` and `security`. Unlike the Codex `workspace-write` sandbox, Claude has no OS-level write sandbox: its permission classifier gates actions. Claude result errors (for example usage limits) fail the run even when the CLI exits 0. The repo-local `worker.env` is not loaded by default; use `WORKER_ENV_FILE="$PWD/worker.env"` explicitly when testing locally. Env files must have mode 0600.

Register projects/repositories and explicitly map each Starco project ID. Names below are examples; choose the real checkout, branch, and validation commands:

```sh
./.bin/worker project add --id demo --name Demo
./.bin/worker repository add --id demo-api --project demo --name API --path /absolute/path/to/repo --url git@github.com:org/repo.git --branch dev --test 'go test ./...' --lint 'go vet ./...' --build 'go build ./...'
./.bin/worker project list
./.bin/worker repository list
./.bin/worker starco inspect
./.bin/worker starco map --project 11 --repository demo-api
./.bin/worker starco sync
./.bin/worker starco queue
```

`inspect` reads remote task details without changing remote tasks or the local queue (opening the command still initializes local storage). `sync` persists the snapshot but never starts Codex. `queue` shows mapped, eligible local tasks in execution order. An unmapped queue stays empty; `task list` shows `UNMAPPED` records. `inspect` and `sync` optionally accept `--password-stdin` for transient checks without saving a password.

Mappings are one Starco project to one repository. If a project spans several repositories, do not guess: keep it disabled until routing is specified. Attachments/comments are not ingested automatically; only the task description is used.

## Operate

```sh
./.bin/worker poll
./.bin/worker task list
./.bin/worker task show TASK_ID
./.bin/worker task approve TASK_ID
./.bin/worker task reject --reason 'Use the service layer' TASK_ID
./.bin/worker setup picoclaw --start
```

After verifying mappings and the queue, set `WORKER_EXECUTE=true` in the private env file. `poll` synchronizes and starts an independent drain process; `drain` can also be run in the foreground. The drain log is `drain.log` in the data directory. Codex logs and validation results are available with `task show`.

After approval, review and commit manually in the task worktree, deliver through your normal process, then mark the matching branch HEAD commit with `worker task complete --commit SHA TASK_ID`. Update Starco approval manually after delivery. Rejection requeues the existing worktree; only locally owned retries can resume a remote Doing task.

For an interrupted/uncertain claim, inspect the remote status and local worktree first. Use `task assign --repository REPO_ID TASK_ID` only after confirming it is safe to retry; return the task to Ready in Starco if no worktree exists.

The cron job is stored in the isolated PicoClaw workspace. The launchd service runs on the Mac, with `TZ=Asia/Jakarta` (PicoClaw 0.3.1 does not apply the schedule's `tz` field). Stop it before running setup again:

```sh
launchctl bootout "gui/$(id -u)/com.haikel.picoclaw-codex-worker"
```

Stopping the gateway prevents future polling but does not stop an already detached drain. Set `WORKER_EXECUTE=false` to disable future drain launches and stop the gateway. An active drain caches its configuration, so changing the env file will not stop it from taking more tasks; stop its confirmed process deliberately if necessary, then inspect interrupted tasks before restarting. A sleeping/offline laptop misses polling; the next successful poll reads the full task list, not just newly created tasks. Missed hourly ticks are not replayed.

## Validation

```sh
golangci-lint fmt --diff ./...
golangci-lint run ./...
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go build -o .bin/worker ./cmd/worker
```

Tests use synthetic TLS servers, temporary SQLite databases, local Git repositories, and a fake Codex executable; no live task is mutated. Live smoke verification should use `starco inspect` or `starco sync` with execution disabled.
