package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// client calls the LoopSandbox API with one API key.
type client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// statusError is an answer with a status the caller did not expect.
type statusError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%s %s: status %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// do sends the request, decodes a JSON answer into out when out is not nil, and
// returns how long the request took. Any status other than want is an error.
func (c *client) do(ctx context.Context, method, path string, body, out any, want int) (time.Duration, error) {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode the body of %s %s: %w", method, path, err)
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, payload)
	if err != nil {
		return 0, fmt.Errorf("build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	elapsed := time.Since(start)
	if err != nil {
		return 0, fmt.Errorf("read the answer of %s %s: %w", method, path, err)
	}
	if resp.StatusCode != want {
		return 0, &statusError{Method: method, Path: path, Status: resp.StatusCode, Body: string(raw)}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return 0, fmt.Errorf("decode the answer of %s %s: %w", method, path, err)
		}
	}
	return elapsed, nil
}

type sandbox struct {
	ID string `json:"id"`
}

// create creates a sandbox with the given extra fields and returns it with the
// time the request took.
func (c *client) create(ctx context.Context, image string, extra map[string]any) (sandbox, time.Duration, error) {
	body := map[string]any{"image": image, "ttl_seconds": 1800}
	for k, v := range extra {
		body[k] = v
	}
	var sb sandbox
	elapsed, err := c.do(ctx, http.MethodPost, "/v1/sandboxes", body, &sb, http.StatusCreated)
	return sb, elapsed, err
}

// execTrue runs the command "true" and returns how long the request took. A
// non-zero exit code is an error: the sandbox did not run the command.
func (c *client) execTrue(ctx context.Context, id string) (time.Duration, error) {
	var out struct {
		ExitCode int `json:"exit_code"`
	}
	elapsed, err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+id+"/executions", map[string]any{"cmd": []string{"true"}}, &out, http.StatusOK)
	if err != nil {
		return 0, err
	}
	if out.ExitCode != 0 {
		return 0, fmt.Errorf("sandbox %s: the command true exited with %d", id, out.ExitCode)
	}
	return elapsed, nil
}

func (c *client) destroy(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/v1/sandboxes/"+id, nil, nil, http.StatusNoContent)
	return err
}

// capabilities returns the driver name and the set of capabilities it supports.
func (c *client) capabilities(ctx context.Context) (string, map[string]bool, error) {
	var out struct {
		Driver       string `json:"driver"`
		Capabilities []struct {
			Name      string `json:"name"`
			Supported bool   `json:"supported"`
		} `json:"capabilities"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/v1/capabilities", nil, &out, http.StatusOK); err != nil {
		return "", nil, err
	}
	supported := make(map[string]bool, len(out.Capabilities))
	for _, cp := range out.Capabilities {
		supported[cp.Name] = cp.Supported
	}
	return out.Driver, supported, nil
}
