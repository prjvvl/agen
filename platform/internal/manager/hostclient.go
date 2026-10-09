package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// HostClient calls an agen-host instance's HostService (Connect unary JSON:
// POST /agen.v1.HostService/<Method>). The host's responses carry fields
// beyond the proto (e.g. run usage), so this client decodes plain JSON
// instead of strict protojson.
type HostClient struct {
	BaseURL string
	// Token is the instance's host token (AGEN_HOST_TOKEN), sent as a
	// bearer token; the host refuses calls without it.
	Token string
	HTTP  *http.Client
}

// HostError is a Connect error returned by a host.
type HostError struct {
	Status  int
	Code    string
	Message string
}

func (e *HostError) Error() string { return fmt.Sprintf("host %s: %s", e.Code, e.Message) }

// HostHealth is HostService.Health's response.
type HostHealth struct {
	InstanceID   string   `json:"instanceId"`
	Ready        bool     `json:"ready"`
	RunningTasks int      `json:"runningTasks"`
	Draining     bool     `json:"draining"`
	Problems     []string `json:"problems"`
}

// HostTask is the task part of a RunTask request.
type HostTask struct {
	ID          string `json:"id"`
	Input       string `json:"input"`
	ParentRunID string `json:"parentRunId,omitempty"`
	RootRunID   string `json:"rootRunId,omitempty"`
	Traceparent string `json:"traceparent,omitempty"`
	// Delegation depth of the run (0 = not delegated).
	Depth int `json:"depth,omitempty"`
	// RequestedBy is the verified user call token of an A2A call not made
	// by an agent; the Hub checks its signature and names its user in the
	// run's approvals.
	RequestedBy string `json:"requestedBy,omitempty"`
	// Caller is the verified agent caller ("agent:<ns>/<dep>"); the host
	// checks the lineage claims against the Store.
	Caller string `json:"caller,omitempty"`
}

// HostRunResult is RunTask's response.
type HostRunResult struct {
	Success bool   `json:"success"`
	Output  string `json:"output"`
	Error   string `json:"error"`
	Run     struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		TraceID string `json:"traceId"`
	} `json:"run"`
}

func (c *HostClient) call(ctx context.Context, method string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/agen.v1.HostService/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		he := &HostError{Status: resp.StatusCode}
		_ = json.Unmarshal(b, he)
		if he.Code == "" {
			he.Code, he.Message = "unknown", string(b)
		}
		return he
	}
	return json.Unmarshal(b, out)
}

// Health reports whether the instance can take tasks.
func (c *HostClient) Health(ctx context.Context) (HostHealth, error) {
	var h HostHealth
	return h, c.call(ctx, "Health", struct{}{}, &h)
}

// RunTask runs (or resumes, or returns the result of) a task. It blocks until
// the run ends.
func (c *HostClient) RunTask(ctx context.Context, t HostTask) (HostRunResult, error) {
	var r HostRunResult
	return r, c.call(ctx, "RunTask", map[string]any{"task": t}, &r)
}

// CancelTask cancels a running task; false if the host was not running it.
func (c *HostClient) CancelTask(ctx context.Context, taskID string) (bool, error) {
	var r struct {
		Cancelled bool `json:"cancelled"`
	}
	err := c.call(ctx, "CancelTask", map[string]string{"taskId": taskID}, &r)
	return r.Cancelled, err
}

// Drain stops the instance from taking new tasks.
func (c *HostClient) Drain(ctx context.Context) error {
	var r struct{}
	return c.call(ctx, "Drain", struct{}{}, &r)
}
