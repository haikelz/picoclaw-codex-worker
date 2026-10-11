package worker

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"picoclaw-codex-worker/internal/store"
)

// ClaudeSessionPrefix marks stored session IDs owned by Claude Code; unprefixed IDs belong to Codex.
const ClaudeSessionPrefix = "claude:"

// sessionMeta is the first line of a Codex session file.
type sessionMeta struct {
	Type    string `json:"type"`
	Payload struct {
		ID     string          `json:"id"`
		Source json.RawMessage `json:"source"`
	} `json:"payload"`
}

// FilterSessionTasks preserves queue order and conservatively skips tasks assigned in Codex or Claude sessions.
// Only explicit assignments in user messages count; references and tool output do not.
func FilterSessionTasks(
	codexRoot string,
	claudeRoot string,
	queue []store.Task,
) ([]store.Task, map[string]string, error) {
	if len(queue) == 0 {
		return queue, map[string]string{}, nil
	}

	patterns := make(map[string]*regexp.Regexp, len(queue))
	for _, task := range queue {
		code, _, _ := strings.Cut(task.Title, ":")
		keys := regexp.QuoteMeta(task.ID)
		if code != "" {
			keys += "|" + regexp.QuoteMeta(code)
		}
		patterns[task.ID] = regexp.MustCompile(`(?im)^(?:Starco task ID:\s*|(?:implement|work on|kerjakan|task)\s+(?:task\s+)?)(?:` + keys + `)(?:\b|$)`)
	}

	skipped := make(map[string]string)
	match := func(session, message string) {
		for _, task := range queue {
			if task.SessionID == session || task.SourceSessionID == session {
				// Explicitly approved retries resume their own session.
				continue
			}
			if patterns[task.ID].MatchString(message) {
				skipped[task.ID] = session
			}
		}
	}
	if err := walkSessions(codexRoot, func(path string, scanner *bufio.Scanner) error {
		if !scanner.Scan() {
			return scanner.Err()
		}
		var meta sessionMeta
		if json.Unmarshal(scanner.Bytes(), &meta) != nil ||
			meta.Type != "session_meta" ||
			meta.Payload.ID == "" ||
			strings.Contains(string(meta.Payload.Source), `"subagent"`) {
			return nil
		}

		for scanner.Scan() {
			var event struct {
				Type    string `json:"type"`
				Payload struct {
					Type    string `json:"type"`
					Role    string `json:"role"`
					Message string `json:"message"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"payload"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				return fmt.Errorf("parse Codex session %s: %w", path, err)
			}

			var message string
			if event.Type == "event_msg" && event.Payload.Type == "user_message" {
				message = event.Payload.Message
			} else if event.Type == "response_item" && event.Payload.Type == "message" && event.Payload.Role == "user" {
				for _, part := range event.Payload.Content {
					if part.Type == "input_text" {
						message += part.Text + "\n"
					}
				}
			}
			match(meta.Payload.ID, message)
		}

		return scanner.Err()
	}); err != nil {
		return nil, nil, err
	}
	if err := walkSessions(claudeRoot, func(path string, scanner *bufio.Scanner) error {
		for scanner.Scan() {
			var event struct {
				Type        string `json:"type"`
				IsSidechain bool   `json:"isSidechain"`
				SessionID   string `json:"sessionId"`
				Message     struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				return fmt.Errorf("parse Claude session %s: %w", path, err)
			}
			if event.Type != "user" || event.IsSidechain || event.SessionID == "" {
				continue
			}
			// Content is a plain string for typed prompts, or parts where tool results are not user text.
			var message string
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(event.Message.Content, &message) != nil && json.Unmarshal(event.Message.Content, &parts) == nil {
				for _, part := range parts {
					if part.Type == "text" {
						message += part.Text + "\n"
					}
				}
			}
			match(ClaudeSessionPrefix+event.SessionID, message)
		}

		return scanner.Err()
	}); err != nil {
		return nil, nil, err
	}
	var eligible []store.Task
	for _, task := range queue {
		if skipped[task.ID] == "" {
			eligible = append(eligible, task)
		}
	}

	return eligible, skipped, nil
}

// walkSessions scans every JSONL file under root; a missing or unset root has no sessions.
func walkSessions(root string, scan func(string, *bufio.Scanner) error) error {
	if root == "" {
		return nil
	}
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect session root: %w", err)
	}

	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open session %s: %w", path, err)
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), maxAgentLineBytes)
		scanErr := scan(path, scanner)

		return errors.Join(scanErr, file.Close())
	})
}
