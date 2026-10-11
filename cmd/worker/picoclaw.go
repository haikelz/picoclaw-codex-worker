package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"picoclaw-codex-worker/internal/worker"
)

const (
	cronJobName   = "coding-worker-poll"
	cronSchedule  = "0 8-17 * * 1-5"
	launchdLabel  = "com.haikel.picoclaw-codex-worker"
	picoclawModel = "gpt-6.1-sol"
)

// object returns parent[key] as a JSON object, creating it when absent.
func object(parent map[string]any, key string) map[string]any {
	if child, ok := parent[key].(map[string]any); ok {
		return child
	}

	child := map[string]any{}
	parent[key] = child
	return child
}

// writeJSON replaces path atomically with a private, indented JSON file.
func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}

	file, err := os.CreateTemp(filepath.Dir(path), ".worker-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	defer func() {
		// After a successful rename the temporary name no longer exists; removal only cleans up failures.
		_ = os.Remove(file.Name())
	}()
	if err := file.Chmod(0600); err != nil {
		return errors.Join(fmt.Errorf("restrict temporary file: %w", err), file.Close())
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return errors.Join(fmt.Errorf("write temporary file: %w", err), file.Close())
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}

	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}

	return nil
}

func readObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	return value, nil
}

func xmlText(value string) string {
	var buf bytes.Buffer
	// Writes to a bytes.Buffer cannot fail.
	_ = xml.EscapeText(&buf, []byte(value))

	return buf.String()
}

// setupPicoclaw installs the worker into an isolated PicoClaw home, schedules polling, and writes
// the launchd service. It starts the service only with --start.
func setupPicoclaw(
	ctx context.Context,
	cfg config,
	args []string,
) error {
	flags := flag.NewFlagSet("setup picoclaw", flag.ContinueOnError)
	start := flags.Bool("start", false, "start local gateway with launchd")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: worker setup picoclaw [--start]")
	}

	domain := "gui/" + strconv.Itoa(os.Getuid())
	if exec.CommandContext(ctx, "launchctl", "print", domain+"/"+launchdLabel).Run() == nil {
		return errors.New("stop the launchd gateway before setup; editing a running cron store is unsafe")
	}

	workerBinary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find worker binary: %w", err)
	}
	workerBinary, err = filepath.EvalSymlinks(workerBinary)
	if err != nil {
		return fmt.Errorf("resolve worker binary: %w", err)
	}
	picoclawBinary := filepath.Join(filepath.Dir(workerBinary), "picoclaw")
	if _, err := os.Stat(picoclawBinary); err != nil {
		return fmt.Errorf("install PicoClaw beside the worker binary: %w", err)
	}
	picoHome := filepath.Join(cfg.dataDir, "picoclaw")

	if err := installWorkspaceBinary(workerBinary, picoHome); err != nil {
		return err
	}
	if err := configurePicoclaw(ctx, picoclawBinary, picoHome); err != nil {
		return err
	}
	jobsPath, err := schedulePoll(ctx, picoclawBinary, picoHome)
	if err != nil {
		return err
	}
	plist, err := writeLaunchAgent(cfg, picoclawBinary, picoHome)
	if err != nil {
		return err
	}
	fmt.Println("PicoClaw command job configured:", jobsPath)
	fmt.Println("launchd service prepared:", plist)

	if !*start {
		return nil
	}

	if _, err := starcoClient(ctx, cfg, ""); err != nil {
		return err
	}
	auth, err := readObject(filepath.Join(picoHome, "auth.json"))
	credentials, ok := auth["credentials"].(map[string]any)
	if err != nil || !ok || credentials["openai"] == nil {
		return fmt.Errorf("authorize ChatGPT first: PICOCLAW_HOME=%q %s auth login --provider openai", picoHome, picoclawBinary)
	}
	output, err := exec.CommandContext(ctx, "launchctl", "bootstrap", domain, plist).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(output)))
	}
	fmt.Println("PicoClaw gateway started through launchd")

	return nil
}

// installWorkspaceBinary copies the worker into the PicoClaw workspace, where the cron command runs it.
func installWorkspaceBinary(workerBinary, picoHome string) error {
	workspaceBinary := filepath.Join(picoHome, "workspace", "bin", "worker")
	if err := os.MkdirAll(filepath.Dir(workspaceBinary), 0700); err != nil {
		return fmt.Errorf("create workspace bin directory: %w", err)
	}

	data, err := os.ReadFile(workerBinary)
	if err != nil {
		return fmt.Errorf("read worker binary: %w", err)
	}
	if err := os.WriteFile(workspaceBinary, data, 0700); err != nil {
		return fmt.Errorf("install workspace worker: %w", err)
	}

	return nil
}

// configurePicoclaw onboards the isolated PicoClaw home once and pins its workspace and model.
func configurePicoclaw(
	ctx context.Context,
	picoclawBinary string,
	picoHome string,
) error {
	picoConfig := filepath.Join(picoHome, "config.json")
	if _, err := os.Stat(picoConfig); errors.Is(err, os.ErrNotExist) {
		command := exec.CommandContext(ctx, picoclawBinary, "onboard")
		command.Env = append(os.Environ(), "PICOCLAW_HOME="+picoHome)
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("picoclaw onboard: %w: %s", err, output)
		}
	} else if err != nil {
		return fmt.Errorf("inspect PicoClaw config: %w", err)
	}

	settings, err := readObject(picoConfig)
	if err != nil {
		return err
	}
	defaults := object(object(settings, "agents"), "defaults")
	defaults["workspace"] = filepath.Join(picoHome, "workspace")
	defaults["restrict_to_workspace"] = true
	defaults["model_name"] = picoclawModel
	defaults["provider"] = "openai"
	settings["model_list"] = []any{
		map[string]any{
			"model_name":  picoclawModel,
			"model":       picoclawModel,
			"provider":    "openai",
			"auth_method": "oauth",
		},
	}
	tools := object(settings, "tools")
	object(tools, "cron")["exec_timeout_minutes"] = 60
	object(tools, "exec")["timeout_seconds"] = 3600

	return writeJSON(picoConfig, settings)
}

// schedulePoll creates the cron job once, then rewrites it to run the worker poll hourly on weekdays.
func schedulePoll(
	ctx context.Context,
	picoclawBinary string,
	picoHome string,
) (string, error) {
	jobsPath := filepath.Join(picoHome, "workspace", "cron", "jobs.json")
	if _, err := os.Stat(jobsPath); errors.Is(err, os.ErrNotExist) {
		command := exec.CommandContext(
			ctx,
			picoclawBinary,
			"cron", "add",
			"--name", cronJobName,
			"--cron", cronSchedule,
			"--message", "Run the local coding worker poll.",
		)
		command.Env = append(os.Environ(), "PICOCLAW_HOME="+picoHome, "TZ=Asia/Jakarta")
		if output, err := command.CombinedOutput(); err != nil {
			return "", fmt.Errorf("picoclaw cron add: %w: %s", err, output)
		}
	} else if err != nil {
		return "", fmt.Errorf("inspect PicoClaw cron store: %w", err)
	}

	jobs, err := readObject(jobsPath)
	if err != nil {
		return "", err
	}
	if jobs["version"] != float64(1) {
		return "", errors.New("unsupported PicoClaw cron store version; inspect before setup")
	}
	items, ok := jobs["jobs"].([]any)
	if !ok {
		return "", errors.New("invalid PicoClaw cron jobs")
	}

	found := false
	for _, item := range items {
		job, ok := item.(map[string]any)
		if !ok || job["name"] != cronJobName {
			continue
		}

		payload := object(job, "payload")
		payload["command"] = "./bin/worker poll"
		payload["kind"] = "agent_turn"
		job["schedule"] = map[string]any{
			"kind": "cron",
			"expr": cronSchedule,
		}
		object(job, "state")["nextRunAtMs"] = nil
		job["enabled"] = true
		job["updatedAtMs"] = time.Now().UnixMilli()
		found = true
	}
	if !found {
		return "", errors.New("PicoClaw cron job not found")
	}

	return jobsPath, writeJSON(jobsPath, jobs)
}

// writeLaunchAgent writes the gateway plist with the selected agent CLI on PATH and Jakarta time.
func writeLaunchAgent(
	cfg config,
	picoclawBinary string,
	picoHome string,
) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	agentBinary := cfg.codex
	if cfg.agent == worker.AgentClaude {
		agentBinary = cfg.claude
	}
	agentBinary, err = exec.LookPath(agentBinary)
	if err != nil {
		return "", fmt.Errorf("find %s CLI: %w", cfg.agent, err)
	}

	launchAgents := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(launchAgents, 0700); err != nil {
		return "", fmt.Errorf("create LaunchAgents directory: %w", err)
	}
	plist := filepath.Join(launchAgents, launchdLabel+".plist")
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>gateway</string></array>
<key>EnvironmentVariables</key><dict><key>PICOCLAW_HOME</key><string>%s</string><key>PATH</key><string>%s</string><key>TZ</key><string>Asia/Jakarta</string><key>WORKER_DATA_DIR</key><string>%s</string></dict>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
<key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`,
		xmlText(launchdLabel),
		xmlText(picoclawBinary),
		xmlText(picoHome),
		xmlText(filepath.Dir(agentBinary)+":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"),
		xmlText(cfg.dataDir),
		xmlText(filepath.Join(cfg.dataDir, "picoclaw.stdout.log")),
		xmlText(filepath.Join(cfg.dataDir, "picoclaw.stderr.log")),
	)
	if err := os.WriteFile(plist, []byte(content), 0600); err != nil {
		return "", fmt.Errorf("write launchd plist: %w", err)
	}

	return plist, nil
}
