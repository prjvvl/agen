// Package agen embeds the Agen agent engine in Go programs.
//
//	agent, err := agen.New(agen.Spec{
//		Name:         "helper",
//		Instructions: "You are terse.",
//		Model:        "deepseek/deepseek-v4-flash",
//		Provider:     agen.OpenRouter(""), // key from $OPENROUTER_API_KEY
//	}, agen.WithTool(agen.Tool{
//		Name:        "add",
//		Description: "Add two integers",
//		Parameters:  json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}}}`),
//		Func: func(ctx context.Context, args json.RawMessage) (string, error) { ... },
//	}))
//	defer agent.Close()
//	res, err := agent.Run(ctx, "What is 2+3?", agen.OnDelta(func(s string) { fmt.Print(s) }))
//
// The engine is the same Rust engine used by the Agen platform; this package
// is a thin cgo wrapper over its C ABI.
package agen

/*
#include "agen.h"
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/cgo"
	"sync"
	"sync/atomic"
)

// Tool is a tool implemented in Go.
type Tool struct {
	Name        string
	Description string
	// JSON Schema for the arguments object (default: any object).
	Parameters json.RawMessage
	// ReadOnly tools are not recorded in the effect ledger. Tools are
	// side-effecting by default.
	ReadOnly bool
	// TimeoutSeconds after which the call is abandoned (outcome unknown).
	TimeoutSeconds int
	Func           func(ctx context.Context, args json.RawMessage) (string, error)
}

// Provider selects the model provider for inline agents.
type Provider map[string]any

// Fake returns a scripted provider (tests and examples). Each response is
// e.g. {"text": "hi"} or {"toolCalls": [{"name": "add", "arguments": {...}}]}.
func Fake(responses ...map[string]any) Provider {
	return Provider{"type": "fake", "responses": responses}
}

// OpenRouter uses OpenRouter; an empty key reads $OPENROUTER_API_KEY.
func OpenRouter(apiKey string) Provider {
	p := Provider{"type": "openrouter"}
	if apiKey != "" {
		p["apiKey"] = apiKey
	}
	return p
}

// OpenAI uses the OpenAI API directly; an empty key reads $OPENAI_API_KEY.
func OpenAI(apiKey string) Provider {
	p := Provider{"type": "openai"}
	if apiKey != "" {
		p["apiKey"] = apiKey
	}
	return p
}

// Spec describes an agent. Either Bundle, or Name + Provider (+ Instructions,
// Model) must be set.
type Spec struct {
	Bundle       string            `json:"bundle,omitempty"`
	Name         string            `json:"name,omitempty"`
	Instructions string            `json:"instructions,omitempty"`
	Model        string            `json:"model,omitempty"`
	Provider     Provider          `json:"provider,omitempty"`
	MaxTurns     int               `json:"maxTurns,omitempty"`
	MaxOutput    int               `json:"maxOutputTokens,omitempty"`
	Store        string            `json:"store,omitempty"` // default: in-memory
	Namespace    string            `json:"namespace,omitempty"`
	Deployment   string            `json:"deployment,omitempty"`
	Secrets      map[string]string `json:"secrets,omitempty"`
	Permissions  any               `json:"permissions,omitempty"`
	Temperature  *float64          `json:"temperature,omitempty"`
	// ApprovalTimeoutSeconds after which a pending approval counts as expired.
	ApprovalTimeoutSeconds int `json:"approvalTimeoutSeconds,omitempty"`
}

// ApprovalFunc decides "ask" permissions; return true to approve.
type ApprovalFunc func(ctx context.Context, tool string, args json.RawMessage) bool

// Option configures an Agent.
type Option func(*Agent)

// WithTool adds a Go tool.
func WithTool(t Tool) Option { return func(a *Agent) { a.tools[t.Name] = t } }

// WithApprovals routes "ask" permissions to fn (otherwise they are denied).
func WithApprovals(fn ApprovalFunc) Option { return func(a *Agent) { a.approve = fn } }

// Error is an engine error with a stable code.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func parseError(s string) error {
	var e Error
	if json.Unmarshal([]byte(s), &e) != nil || e.Code == "" {
		return &Error{Code: "internal", Message: s}
	}
	return &e
}

// Usage of one run.
type Usage struct {
	InputTokens  uint64  `json:"inputTokens"`
	OutputTokens uint64  `json:"outputTokens"`
	CostUSD      float64 `json:"costUsd"`
}

// Result of a run.
type Result struct {
	RunID          string `json:"runId"`
	SessionID      string `json:"sessionId"`
	ConversationID string `json:"conversationId"`
	Status         string `json:"status"` // succeeded | failed | cancelled
	Output         string `json:"output"`
	Error          string `json:"error"`
	Usage          Usage  `json:"usage"`
	TraceID        string `json:"traceId"`
}

// Agent is an embedded agent. Run may be called concurrently; Close waits
// for in-flight runs and host calls to finish.
type Agent struct {
	ptr     *C.AgenAgent
	handle  cgo.Handle
	tools   map[string]Tool
	approve ApprovalFunc

	mu      sync.RWMutex // guards closed; held (read) while using ptr
	closed  bool
	wg      sync.WaitGroup
	reqMu   sync.Mutex
	reqs    map[string]context.CancelFunc // in-flight host requests
	baseCtx context.Context
	stopAll context.CancelFunc
}

// New creates an agent.
func New(spec Spec, opts ...Option) (*Agent, error) {
	a := &Agent{tools: map[string]Tool{}, reqs: map[string]context.CancelFunc{}}
	a.baseCtx, a.stopAll = context.WithCancel(context.Background())
	for _, o := range opts {
		o(a)
	}
	body, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	tools := []map[string]any{}
	for _, t := range a.tools {
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object"}`)
		}
		tm := map[string]any{"name": t.Name, "description": t.Description, "parameters": params, "sideEffect": !t.ReadOnly}
		if t.TimeoutSeconds > 0 {
			tm["timeoutSeconds"] = t.TimeoutSeconds
		}
		tools = append(tools, tm)
	}
	m["tools"] = tools
	m["hostApprovals"] = a.approve != nil
	full, _ := json.Marshal(m)

	a.handle = cgo.NewHandle(a)
	ptr, errJSON := ffiNew(string(full), a.handle)
	if ptr == nil {
		a.handle.Delete()
		return nil, parseError(errJSON)
	}
	a.ptr = ptr
	return a, nil
}

// Close cancels running tool calls, waits for runs and host calls to
// finish, and frees the agent. Further calls return an error.
func (a *Agent) Close() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	a.mu.Unlock()
	a.stopAll()
	// Make in-flight runs return: cancels them and fails pending host requests.
	ffiShutdown(a.ptr)
	a.wg.Wait()
	ffiFree(a.ptr)
	a.handle.Delete()
}

// enter registers in-flight work; false if the agent is closed.
func (a *Agent) enter() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.closed {
		return false
	}
	a.wg.Add(1)
	return true
}

type hostRequest struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	Name      string          `json:"name"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

// onHostRequest runs on the engine's dispatcher thread: never block it.
func (a *Agent) onHostRequest(raw string) {
	var r hostRequest
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return
	}
	if r.Kind == "cancel" {
		a.reqMu.Lock()
		if cancel, ok := a.reqs[r.ID]; ok {
			cancel()
		}
		a.reqMu.Unlock()
		return
	}
	if !a.enter() {
		// Closing: Close calls the engine's shutdown, which fails every
		// pending request, so dropping this one cannot hang a run.
		return
	}
	ctx, cancel := context.WithCancel(a.baseCtx)
	a.reqMu.Lock()
	a.reqs[r.ID] = cancel
	a.reqMu.Unlock()
	go func() {
		defer a.wg.Done()
		defer func() {
			a.reqMu.Lock()
			delete(a.reqs, r.ID)
			a.reqMu.Unlock()
			cancel()
		}()
		switch r.Kind {
		case "tool":
			t, ok := a.tools[r.Name]
			if !ok || t.Func == nil {
				ffiComplete(a.ptr, r.ID, "no Go implementation for tool "+r.Name, true)
				return
			}
			out, err := safeCall(func() (string, error) { return t.Func(ctx, r.Arguments) })
			if err != nil {
				ffiComplete(a.ptr, r.ID, err.Error(), true)
				return
			}
			ffiComplete(a.ptr, r.ID, out, false)
		case "approval":
			decision := "denied"
			if a.approve != nil && a.approve(ctx, r.Tool, r.Arguments) {
				decision = "approved"
			}
			ffiComplete(a.ptr, r.ID, decision, false)
		}
	}()
}

func safeCall(f func() (string, error)) (out string, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("tool panicked: %v", p)
		}
	}()
	return f()
}

// RunOption configures one run.
type RunOption func(*runConfig)

type runConfig struct {
	opts    map[string]any
	onDelta func(string)
	onReset func()
}

// OnDelta streams text as it is generated.
func OnDelta(fn func(string)) RunOption { return func(c *runConfig) { c.onDelta = fn } }

// OnReset is called when a model request is retried: discard streamed text.
func OnReset(fn func()) RunOption { return func(c *runConfig) { c.onReset = fn } }

// Session continues an existing session (conversation history).
func Session(id string) RunOption { return func(c *runConfig) { c.opts["sessionId"] = id } }

// TaskID makes the run idempotent: running the same task again returns its result.
func TaskID(id string) RunOption { return func(c *runConfig) { c.opts["taskId"] = id } }

// NewConversation starts a fresh conversation in the session.
func NewConversation() RunOption { return func(c *runConfig) { c.opts["newConversation"] = true } }

// Traceparent continues a W3C trace.
func Traceparent(tp string) RunOption { return func(c *runConfig) { c.opts["traceparent"] = tp } }

type runState struct {
	cfg *runConfig
}

func (r *runState) onEvent(raw string) {
	var ev struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(raw), &ev) != nil {
		return
	}
	switch ev.Type {
	case "delta":
		if r.cfg.onDelta != nil {
			r.cfg.onDelta(ev.Text)
		}
	case "reset":
		if r.cfg.onReset != nil {
			r.cfg.onReset()
		}
	}
}

var cancelSeq atomic.Uint64

// Run runs the agent to completion. Cancelling ctx cancels the run (the
// result then has Status "cancelled").
func (a *Agent) Run(ctx context.Context, input string, opts ...RunOption) (*Result, error) {
	if !a.enter() {
		return nil, errors.New("agen: agent is closed")
	}
	defer a.wg.Done()
	cfg := &runConfig{opts: map[string]any{}}
	for _, o := range opts {
		o(cfg)
	}
	key := fmt.Sprintf("go-%d", cancelSeq.Add(1))
	cfg.opts["cancelKey"] = key
	optJSON, _ := json.Marshal(cfg.opts)

	st := &runState{cfg: cfg}
	h := cgo.NewHandle(st)
	defer h.Delete()

	stop := make(chan struct{})
	watcherDone := make(chan struct{})
	// The watcher must exit before Run returns: after that, Close may free ptr.
	defer func() { close(stop); <-watcherDone }()
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			// The engine remembers a cancel that arrives before the run starts.
			ffiCancel(a.ptr, key)
		case <-stop:
		}
	}()

	res, errJSON := ffiRun(a.ptr, input, string(optJSON), h)
	if res == "" {
		return nil, parseError(errJSON)
	}
	var out Result
	if err := json.Unmarshal([]byte(res), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Span is one operation in a run's trace.
type Span struct {
	SpanID       string         `json:"spanId"`
	ParentSpanID string         `json:"parentSpanId"`
	Name         string         `json:"name"`
	RunID        string         `json:"runId"`
	StartMs      int64          `json:"startMs"`
	EndMs        int64          `json:"endMs"`
	Status       string         `json:"status"`
	Attributes   map[string]any `json:"attributes"`
}

// Trace returns the spans of a run's trace (Result.TraceID).
func (a *Agent) Trace(traceID string) ([]Span, error) {
	if !a.enter() {
		return nil, errors.New("agen: agent is closed")
	}
	defer a.wg.Done()
	res, errJSON := ffiTrace(a.ptr, traceID)
	if res == "" {
		return nil, parseError(errJSON)
	}
	var spans []Span
	if err := json.Unmarshal([]byte(res), &spans); err != nil {
		return nil, err
	}
	return spans, nil
}
