package starco

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestClientSessionPaginationDetailAndStart(t *testing.T) {
	pages := 0
	mutations := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			if r.Method == http.MethodGet {
				respond(t, w, `<input name="_token" value="login-token">`)
				return
			}
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("email") != "dev@example.test" || r.Form.Get("password") != "synthetic-password" || r.Form.Get("_token") != "login-token" {
				t.Error("unexpected login form")
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "authenticated", Path: "/"})
			respond(t, w, "ok")
			return
		}
		if _, err := r.Cookie("session"); err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if r.URL.Path == "/account/tasks" {
			if r.Header.Get("X-Requested-With") == "" {
				respond(t, w, `<meta name="csrf-token" content="session-token">`)
				return
			}
			pages++
			start, _ := strconv.Atoi(r.URL.Query().Get("start"))
			var rows []Task
			for i := start; i < min(start+100, 105); i++ {
				rows = append(rows, Task{ID: i + 1, Status: "Ready", Priority: "Medium"})
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"recordsFiltered": 105, "data": rows}); err != nil {
				t.Error(err)
			}
			return
		}
		if r.URL.Path == "/account/tasks/change-status" {
			mutations++
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Method != http.MethodPost || r.Form.Get("_token") != "session-token" || r.Form.Get("status") != "doing" || r.Form.Get("taskId") != "1" {
				t.Error("unexpected status mutation")
			}
			respond(t, w, `{"status":"success"}`)
			return
		}
		respond(t, w, `<section id="task-detail-section"><h3>Build widgets</h3><div><p>Description</p><div><p>First step.</p><ul><li>Second step.</li></ul></div></div><div><p>Created On</p><p>07-10-2026 10:17 am</p></div></section>`)
	}))
	defer server.Close()

	client, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = server.Client().Transport
	ctx := context.Background()
	if err := client.Login(ctx, "dev@example.test", "synthetic-password"); err != nil {
		t.Fatal(err)
	}
	rows, err := client.Tasks(ctx)
	if err != nil || len(rows) != 105 || pages != 2 {
		t.Fatalf("pagination: rows=%d pages=%d err=%v", len(rows), pages, err)
	}
	task, err := client.Detail(ctx, rows[0])
	if err != nil || task.Title != "Build widgets" || task.Description != "First step.\n- Second step." || task.CreatedAt.Format("2006-01-02T15:04:05Z07:00") != "2026-10-07T10:17:00+07:00" {
		t.Fatalf("detail: %+v err=%v", task, err)
	}
	if mutations != 0 {
		t.Fatal("read-only operations mutated task")
	}
	if err := client.Start(ctx, 999, "Ready"); err == nil || mutations != 0 {
		t.Fatalf("missing task mutated: mutations=%d err=%v", mutations, err)
	}
	if err := client.Start(ctx, 1, "Doing"); err == nil || mutations != 0 {
		t.Fatalf("unexpected status mutated: mutations=%d err=%v", mutations, err)
	}
	if err := client.Start(ctx, 1, "Ready"); err != nil || mutations != 1 {
		t.Fatalf("start: mutations=%d err=%v", mutations, err)
	}
}

func TestClientRejectsInvalidResponses(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "login HTML instead of JSON", body: `<html>Log In</html>`},
		{name: "missing count", body: `{"data":[]}`},
		{name: "truncated pagination", body: `{"recordsFiltered":2,"data":[{"id":1,"status":"Ready"}]}`},
		{name: "duplicate task", body: `{"recordsFiltered":2,"data":[{"id":1,"status":"Ready"},{"id":1,"status":"Ready"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				respond(t, w, test.body)
			}))
			defer server.Close()
			client, err := New(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			client.http.Transport = server.Client().Transport

			if _, err := client.Tasks(context.Background()); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
}

func TestLoginFailureAndRedirectProtection(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/account/tasks" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		respond(t, w, `<input name="_token" value="token">`)
	}))
	defer server.Close()
	client, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = server.Client().Transport

	if err := client.Login(context.Background(), "dev@example.test", "wrong"); err == nil || !strings.Contains(err.Error(), "session expired") {
		t.Fatalf("invalid login should fail: %v", err)
	}
	request, err := http.NewRequest(http.MethodGet, "https://other.example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.http.CheckRedirect(request, nil); err == nil {
		t.Fatal("cross-origin redirect allowed")
	}
	for _, origin := range []string{"http://example.test", "https://user:pass@example.test", "https://example.test/path"} {
		if _, err := New(origin); err == nil {
			t.Fatalf("unsafe origin accepted: %s", origin)
		}
	}
}

// respond writes a canned response body; a failed write fails the test.
func respond(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := io.WriteString(w, body); err != nil {
		t.Error(err)
	}
}
