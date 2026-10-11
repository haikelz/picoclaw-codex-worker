package worker

import (
	"os"
	"path/filepath"
	"testing"

	"picoclaw-codex-worker/internal/store"
)

func TestFilterSessionTasks(t *testing.T) {
	root := t.TempDir()
	log := `{"type":"session_meta","payload":{"id":"session-1","source":"cli"}}
{"type":"event_msg","payload":{"type":"user_message","message":"kerjakan BOOK-14"}}
{"type":"event_msg","payload":{"type":"task_complete"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Starco task ID: starco-2\nAssignment"}]}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"input_text","text":"implement BOOK-16"}]}}
{"type":"event_msg","payload":{"type":"user_message","message":"Reference BOOK-17 only"}}
{"type":"event_msg","payload":{"type":"user_message","message":"implement BOOK-180"}}
`
	if err := os.WriteFile(filepath.Join(root, "session.jsonl"), []byte(log), 0600); err != nil {
		t.Fatal(err)
	}
	queue := []store.Task{
		{ID: "starco-1", Title: "BOOK-14: oldest"},
		{ID: "starco-2", Title: "BOOK-15: active"},
		{ID: "starco-3", Title: "BOOK-16: next"},
		{ID: "starco-4", Title: "BOOK-17: reference"},
		{ID: "starco-5", Title: "BOOK-18: boundary"},
	}
	eligible, skipped, err := FilterSessionTasks(root, "", queue)
	if err != nil || len(skipped) != 2 || len(eligible) != 3 || eligible[0].ID != "starco-3" || eligible[2].ID != "starco-5" {
		t.Fatalf("queue=%+v skipped=%+v err=%v", eligible, skipped, err)
	}
	queue[1].SessionID = "session-1"
	eligible, skipped, err = FilterSessionTasks(root, "", queue)
	if err != nil || len(skipped) != 1 || eligible[0].ID != "starco-2" {
		t.Fatalf("approved retry lost: %+v %v", eligible, err)
	}
}

func TestFilterMissingAndMalformedSessions(t *testing.T) {
	root := t.TempDir()
	queue := []store.Task{{ID: "starco-1", Title: "BOOK-14: task"}}
	eligible, _, err := FilterSessionTasks(filepath.Join(root, "missing"), filepath.Join(root, "missing-claude"), queue)
	if err != nil || len(eligible) != 1 {
		t.Fatalf("missing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "bad.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"s\"}}\nnot-json\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := FilterSessionTasks(root, "", queue); err == nil {
		t.Fatal("corrupt history should block execution")
	}
}

func TestActualSessionHistory(t *testing.T) {
	root := os.Getenv("WORKER_TEST_SESSIONS")
	if root == "" {
		t.Skip("optional read-only local session smoke")
	}
	queue := []store.Task{{ID: "starco-synthetic-connectivity", Title: "SYNTHETIC-ONLY: session parser check"}}
	if _, _, err := FilterSessionTasks(root, "", queue); err != nil {
		t.Fatal(err)
	}
}
