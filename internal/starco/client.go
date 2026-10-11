// Package starco owns the session-authenticated Starco web contract.
package starco

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Remote task statuses. Compare with strings.EqualFold; Starco's casing varies by endpoint.
const (
	StatusReady     = "Ready"
	StatusDoing     = "Doing"
	StatusCompleted = "Completed"
)

const (
	pageSize     = 100
	maxTasks     = 100000
	maxBodyBytes = 16 * 1024 * 1024
)

// Task is a Starco task. List rows fill the JSON fields; Detail fills the rest.
type Task struct {
	ID          int    `json:"id"`
	Code        string `json:"task_short_code"`
	Priority    string `json:"priority"`
	Status      string `json:"status"`
	ProjectName string `json:"project_name"`
	Project     struct {
		ID int `json:"id"`
	} `json:"project"`
	DependencyID *int `json:"dependent_task_id"`
	Title        string
	Description  string
	CreatedAt    time.Time
}

// Client is a session-authenticated Starco web client. It is not safe for concurrent use.
type Client struct {
	base  *url.URL
	http  *http.Client
	token string
}

// New keeps cookies in memory and refuses cross-origin redirects.
func New(baseURL string) (*Client, error) {
	base, err := url.Parse(baseURL)
	if err != nil ||
		base.Host == "" ||
		base.Scheme != "https" ||
		base.User != nil ||
		base.RawQuery != "" ||
		base.Fragment != "" ||
		(base.Path != "" && base.Path != "/") {
		return nil, errors.New("base URL must be an HTTPS origin")
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create cookie jar: %w", err)
	}

	return &Client{
		base: base,
		http: &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if req.URL.Host != base.Host || req.URL.Scheme != base.Scheme || len(via) >= 10 {
					return errors.New("refuse cross-origin or excessive redirect")
				}

				return nil
			},
		},
	}, nil
}

func (c *Client) request(
	ctx context.Context,
	method string,
	path string,
	form url.Values,
	ajax bool,
) ([]byte, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}

	route, query, _ := strings.Cut(path, "?")
	target := c.base.ResolveReference(&url.URL{Path: route})
	target.RawQuery = query
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if ajax {
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Accept", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}
	if strings.HasPrefix(path, "/account/") && resp.Request.URL.Path == "/login" {
		return nil, errors.New("session expired; authenticate again")
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(data) > maxBodyBytes {
		return nil, errors.New("response exceeds size limit")
	}

	return data, nil
}

// Login signs in and keeps the authenticated CSRF token for later mutations.
func (c *Client) Login(
	ctx context.Context,
	username string,
	password string,
) error {
	if username == "" || password == "" {
		return errors.New("username and password are required")
	}

	data, err := c.request(ctx, http.MethodGet, "/login", nil, false)
	if err != nil {
		return fmt.Errorf("open login page: %w", err)
	}
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("parse login page: %w", err)
	}
	walk(doc, func(n *html.Node) {
		if n.Data == "input" && attr(n, "name") == "_token" {
			c.token = attr(n, "value")
		}
	})
	if c.token == "" {
		return errors.New("login CSRF token missing")
	}

	if _, err := c.request(ctx, http.MethodPost, "/login", url.Values{
		"_token":   {c.token},
		"email":    {username},
		"password": {password},
	}, false); err != nil {
		return fmt.Errorf("submit login: %w", err)
	}

	// A successful POST alone is insufficient: invalid credentials return HTML too.
	data, err = c.request(ctx, http.MethodGet, "/account/tasks", nil, false)
	if err != nil {
		return fmt.Errorf("verify login: %w", err)
	}
	doc, err = html.Parse(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("parse task page: %w", err)
	}
	var token string
	walk(doc, func(n *html.Node) {
		if n.Data == "meta" && attr(n, "name") == "csrf-token" {
			token = attr(n, "content")
		}
	})
	if token == "" {
		return errors.New("authenticated CSRF token missing")
	}

	c.token = token
	return nil
}

// Tasks retrieves every page; pagination inconsistencies fail closed.
func (c *Client) Tasks(ctx context.Context) ([]Task, error) {
	var result []Task
	seen := make(map[int]bool)
	total := -1

	for start := 0; start < maxTasks; start += pageSize {
		q := url.Values{
			"draw":             {"1"},
			"start":            {strconv.Itoa(start)},
			"length":           {strconv.Itoa(pageSize)},
			"status":           {"all"},
			"assignedTo":       {"all"},
			"assignedBY":       {"all"},
			"projectId":        {"all"},
			"priority":         {"all"},
			"clientID":         {"all"},
			"label":            {"all"},
			"category_id":      {"all"},
			"billable":         {"all"},
			"pinned":           {"all"},
			"milestone_id":     {"all"},
			"date_filter_on":   {"start_date"},
			"columns[0][data]": {"id"},
			"columns[0][name]": {"id"},
			"order[0][column]": {"0"},
			"order[0][dir]":    {"asc"},
		}
		data, err := c.request(ctx, http.MethodGet, "/account/tasks?"+q.Encode(), nil, true)
		if err != nil {
			return nil, fmt.Errorf("list tasks: %w", err)
		}

		var page struct {
			Total *int   `json:"recordsFiltered"`
			Data  []Task `json:"data"`
		}
		if err := json.Unmarshal(data, &page); err != nil || page.Total == nil {
			return nil, errors.New("invalid task list response")
		}
		if total >= 0 && total != *page.Total {
			return nil, errors.New("task list changed during pagination; retry next poll")
		}
		total = *page.Total

		for _, task := range page.Data {
			if task.ID <= 0 || task.Status == "" || seen[task.ID] {
				return nil, errors.New("invalid or duplicate task")
			}

			seen[task.ID] = true
			result = append(result, task)
		}

		if len(result) == total {
			return result, nil
		}
		if len(page.Data) != pageSize || len(result) > total {
			return nil, errors.New("incomplete task pagination")
		}
	}

	return nil, errors.New("task count exceeds limit")
}

// Detail fills a task's title, description, and creation time from its detail page.
func (c *Client) Detail(ctx context.Context, task Task) (Task, error) {
	data, err := c.request(ctx, http.MethodGet, "/account/tasks/"+strconv.Itoa(task.ID), nil, false)
	if err != nil {
		return task, fmt.Errorf("open task detail: %w", err)
	}
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return task, fmt.Errorf("parse task detail: %w", err)
	}

	var created string
	walk(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}

		if attr(n, "id") == "task-detail-section" {
			walk(n, func(child *html.Node) {
				if task.Title == "" && child.Data == "h3" {
					task.Title = nodeText(child)
				}
			})
		}

		if n.Data != "p" {
			return
		}
		label := nodeText(n)
		if label != "Description" && label != "Created On" {
			return
		}
		for sibling := n.NextSibling; sibling != nil; sibling = sibling.NextSibling {
			if sibling.Type != html.ElementNode {
				continue
			}

			if label == "Description" {
				task.Description = descriptionText(sibling)
			} else {
				created = nodeText(sibling)
			}
			break
		}
	})

	location, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		return task, fmt.Errorf("load Asia/Jakarta: %w", err)
	}
	task.CreatedAt, err = time.ParseInLocation("02-01-2006 03:04 pm", strings.ToLower(created), location)
	if err != nil || task.Title == "" || task.Description == "" {
		return task, errors.New("task detail missing title, description, or valid creation time")
	}

	return task, nil
}

// Start rechecks remote state and marks a task Doing. Mutations are never retried.
func (c *Client) Start(
	ctx context.Context,
	id int,
	expectedStatus string,
) error {
	if expectedStatus != StatusReady && expectedStatus != StatusDoing {
		return errors.New("invalid expected task status")
	}

	tasks, err := c.Tasks(ctx)
	if err != nil {
		return err
	}
	matched := false
	for _, task := range tasks {
		if task.ID == id && strings.EqualFold(task.Status, expectedStatus) {
			matched = true
		}
	}
	if !matched {
		return errors.New("task status changed or task disappeared before execution")
	}

	data, err := c.request(ctx, http.MethodPost, "/account/tasks/change-status", url.Values{
		"_token": {c.token},
		"taskId": {strconv.Itoa(id)},
		"status": {"doing"},
		"sortBy": {"id"},
	}, true)
	if err != nil {
		return fmt.Errorf("change task status: %w", err)
	}
	var result struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &result); err != nil || result.Status != "success" {
		return errors.New("status update not confirmed; inspect task before retrying")
	}

	return nil
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}

	return ""
}

func walk(n *html.Node, visit func(*html.Node)) {
	visit(n)
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		walk(child, visit)
	}
}

func nodeText(n *html.Node) string {
	var text strings.Builder
	walk(n, func(child *html.Node) {
		if child.Type == html.TextNode {
			text.WriteString(child.Data)
			text.WriteByte(' ')
		}
	})

	return strings.Join(strings.Fields(text.String()), " ")
}

func descriptionText(n *html.Node) string {
	var text strings.Builder
	endLine := func() {
		if text.Len() > 0 && !strings.HasSuffix(text.String(), "\n") {
			text.WriteByte('\n')
		}
	}

	var visit func(*html.Node)
	visit = func(child *html.Node) {
		if child.Type == html.TextNode {
			text.WriteString(child.Data)
			return
		}
		if child.Data == "script" || child.Data == "style" {
			return
		}

		block := false
		switch child.Data {
		case "p", "div", "li", "pre", "h1", "h2", "h3", "h4", "h5", "h6", "br":
			block = true
			endLine()
		}
		if child.Data == "li" {
			text.WriteString("- ")
		}

		for nested := child.FirstChild; nested != nil; nested = nested.NextSibling {
			visit(nested)
		}

		if block {
			endLine()
		}
	}
	visit(n)

	return strings.TrimSpace(text.String())
}
