package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// defaultURL is where the API listens when started with its defaults.
const defaultURL = "http://localhost:8080"

// apiError is an error answer of the API.
type apiError struct {
	Status  int
	Code    string
	Message string
	// RetryAfter is the Retry-After header, set on a 429.
	RetryAfter string
}

func (e *apiError) Error() string {
	msg := fmt.Sprintf("%s (HTTP %d %s)", e.Message, e.Status, e.Code)
	if e.RetryAfter != "" {
		msg += "; retry after " + e.RetryAfter + "s"
	}
	return msg
}

// isNotFound reports whether err is the API saying the thing does not exist.
func isNotFound(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// client calls the HTTP API with one API key.
type client struct {
	base string
	key  string
	http *http.Client
}

// newClient reads the configuration from the environment.
func newClient(getenv func(string) string) (*client, error) {
	key := getenv("LOOPSANDBOX_API_KEY")
	if key == "" {
		return nil, errors.New("LOOPSANDBOX_API_KEY is not set")
	}
	base := getenv("LOOPSANDBOX_URL")
	if base == "" {
		base = defaultURL
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("LOOPSANDBOX_URL=%q is not an http or https URL", base)
	}
	// No client-wide timeout: an exec may run for as long as the caller allows,
	// and every call carries the context of the command.
	return &client{base: strings.TrimRight(base, "/"), key: key, http: &http.Client{}}, nil
}

// do sends a JSON request and decodes a JSON answer into out, when out is not nil.
func (c *client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("%s %s: read the answer: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		ae := &apiError{Status: resp.StatusCode, Code: "unknown", Message: strings.TrimSpace(string(raw)),
			RetryAfter: resp.Header.Get("Retry-After")}
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &envelope) == nil && envelope.Error.Code != "" {
			ae.Code, ae.Message = envelope.Error.Code, envelope.Error.Message
		}
		return ae
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: decode the answer: %w", method, path, err)
	}
	return nil
}

// sandbox is a sandbox as the API returns it.
type sandbox struct {
	ID        string            `json:"id"`
	Image     string            `json:"image"`
	State     string            `json:"state"`
	CreatedAt string            `json:"created_at"`
	ExpiresAt string            `json:"expires_at"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type createRequest struct {
	Image      string            `json:"image,omitempty"`
	Template   string            `json:"template,omitempty"`
	TTLSeconds int64             `json:"ttl_seconds,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

func (c *client) createSandbox(ctx context.Context, req createRequest) (*sandbox, error) {
	var sb sandbox
	return &sb, c.do(ctx, http.MethodPost, "/v1/sandboxes", req, &sb)
}

// listSandboxes returns every sandbox, following the cursor to the last page.
func (c *client) listSandboxes(ctx context.Context, state string) ([]sandbox, error) {
	var all []sandbox
	cursor := ""
	for {
		q := url.Values{"limit": {"100"}}
		if state != "" {
			q.Set("state", state)
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Sandboxes  []sandbox `json:"sandboxes"`
			NextCursor string    `json:"next_cursor"`
		}
		if err := c.do(ctx, http.MethodGet, "/v1/sandboxes?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Sandboxes...)
		if page.NextCursor == "" {
			return all, nil
		}
		cursor = page.NextCursor
	}
}

func (c *client) destroySandbox(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(id), nil, nil)
}

type execResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

func (c *client) exec(ctx context.Context, id string, cmd []string, timeout time.Duration) (*execResult, error) {
	body := map[string]any{"cmd": cmd}
	if timeout > 0 {
		body["timeout_ms"] = timeout.Milliseconds()
	}
	var res execResult
	return &res, c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/executions", body, &res)
}

type logPage struct {
	Entries []struct {
		Stream string `json:"stream"`
		Data   string `json:"data"`
	} `json:"entries"`
	Next      int64 `json:"next"`
	Truncated bool  `json:"truncated"`
}

func (c *client) logs(ctx context.Context, id string, after int64, wait time.Duration) (*logPage, error) {
	path := fmt.Sprintf("/v1/sandboxes/%s/logs?after=%d&wait_ms=%d", url.PathEscape(id), after, wait.Milliseconds())
	var page logPage
	return &page, c.do(ctx, http.MethodGet, path, nil, &page)
}

// ptyURL is the WebSocket address of the terminal of a sandbox.
func (c *client) ptyURL(id string) string {
	return "ws" + strings.TrimPrefix(c.base, "http") + "/v1/sandboxes/" + url.PathEscape(id) + "/pty"
}
