package agen_test

// Shared SDK conformance suite (spec/conformance/cases) for the Go SDK.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prjvvl/agen/sdks/go/agen"
)

type confCase struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Agent       json.RawMessage `json:"agent"`
	Tools       []confTool      `json:"tools"`
	Approvals   string          `json:"approvals"`
	Runs        []confRun       `json:"runs"`
	Expect      []confExpect    `json:"expect"`
	CreateError string          `json:"createError"`
}

type confTool struct {
	Name           string                     `json:"name"`
	ReadOnly       bool                       `json:"readOnly"`
	TimeoutSeconds int                        `json:"timeoutSeconds"`
	Behavior       map[string]json.RawMessage `json:"behavior"`
}

type confRun struct {
	Input             string      `json:"input"`
	Options           confOptions `json:"options"`
	CloseAfterMs      *int        `json:"closeAfterMs"`
	SameSession       bool        `json:"sameSession"`
	CancelAfterMs     *int        `json:"cancelAfterMs"`
	CancelBeforeStart bool        `json:"cancelBeforeStart"`
	Concurrent        int         `json:"concurrent"`
}

type confOptions struct {
	TaskID string `json:"taskId"`
}

type confExpect struct {
	MaxWallMs                  int       `json:"maxWallMs"`
	TraceIncludes              []string  `json:"traceIncludes"`
	Status                     string    `json:"status"`
	Output                     *string   `json:"output"`
	MinDeltas                  int       `json:"minDeltas"`
	DeltasEqualOutput          bool      `json:"deltasEqualOutput"`
	ToolCalls                  *[]string `json:"toolCalls"`
	ApprovalsAsked             *[]string `json:"approvalsAsked"`
	ToolCancelled              bool      `json:"toolCancelled"`
	SameConversationAsPrevious bool      `json:"sameConversationAsPrevious"`
	SameRunAsPrevious          bool      `json:"sameRunAsPrevious"`
}

type confState struct {
	mu            sync.Mutex
	calls         []string
	approvals     []string
	toolCancelled atomic.Bool
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func confToolFor(t confTool, st *confState) agen.Tool {
	b := t.Behavior
	return agen.Tool{
		Name:           t.Name,
		ReadOnly:       t.ReadOnly,
		TimeoutSeconds: t.TimeoutSeconds,
		Func: func(ctx context.Context, args json.RawMessage) (string, error) {
			st.mu.Lock()
			st.calls = append(st.calls, t.Name)
			st.mu.Unlock()
			switch {
			case b["return"] != nil:
				var s string
				if json.Unmarshal(b["return"], &s) == nil {
					return s, nil
				}
				var v any
				_ = json.Unmarshal(b["return"], &v)
				out, err := json.Marshal(v)
				return string(out), err
			case b["echoArgs"] != nil:
				var v any
				_ = json.Unmarshal(args, &v)
				out, err := json.Marshal(v)
				return string(out), err
			case b["error"] != nil:
				var msg string
				_ = json.Unmarshal(b["error"], &msg)
				return "", errors.New(msg)
			case b["sleepMs"] != nil:
				var ms int
				_ = json.Unmarshal(b["sleepMs"], &ms)
				select {
				case <-time.After(time.Duration(ms) * time.Millisecond):
				case <-ctx.Done():
					st.toolCancelled.Store(true)
				}
				return "slept", nil
			case b["nonJson"] != nil:
				// Go tools return strings; an unencodable value surfaces as an error.
				return "", errors.New("json: unsupported value (not JSON)")
			}
			return "", errors.New("unknown behavior")
		},
	}
}

func TestConformance(t *testing.T) {
	root := repoRoot()
	files, err := filepath.Glob(filepath.Join(root, "spec", "conformance", "cases", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no conformance cases: %v", err)
	}
	sort.Strings(files)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var c confCase
		// Unknown keys anywhere fail the test: a fixture is never half-checked.
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			t.Fatalf("%s: %v (update the runner)", f, err)
		}
		t.Run(c.Name, func(t *testing.T) { runConfCase(t, root, c) })
	}
}

func runConfCase(t *testing.T, root string, c confCase) {
	var agentKeys map[string]json.RawMessage
	if err := json.Unmarshal(c.Agent, &agentKeys); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"name": true, "bundle": true, "instructions": true, "model": true, "provider": true,
		"permissions": true, "maxTurns": true, "approvalTimeoutSeconds": true}
	for k := range agentKeys {
		if !allowed[k] {
			t.Fatalf("agent: unknown key %q (update the runner)", k)
		}
	}
	var spec agen.Spec
	if err := json.Unmarshal(c.Agent, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Bundle != "" {
		spec.Bundle = filepath.Join(root, filepath.FromSlash(spec.Bundle))
	}
	st := &confState{}
	var opts []agen.Option
	for _, tl := range c.Tools {
		opts = append(opts, agen.WithTool(confToolFor(tl, st)))
	}
	switch c.Approvals {
	case "":
	case "approve", "deny", "hang":
		mode := c.Approvals
		opts = append(opts, agen.WithApprovals(func(ctx context.Context, tool string, _ json.RawMessage) bool {
			st.mu.Lock()
			st.approvals = append(st.approvals, tool)
			st.mu.Unlock()
			if mode == "hang" {
				select {
				case <-ctx.Done():
				case <-time.After(5 * time.Second):
				}
			}
			return mode == "approve"
		}))
	default:
		t.Fatalf("unknown approvals mode %q", c.Approvals)
	}
	a, err := agen.New(spec, opts...)
	if c.CreateError != "" {
		var e *agen.Error
		if !errors.As(err, &e) || e.Code != c.CreateError {
			t.Fatalf("want error code %s, got %v", c.CreateError, err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if len(c.Runs) != len(c.Expect) {
		t.Fatal("runs/expect length mismatch")
	}
	type got struct {
		res    *agen.Result
		deltas []string
	}
	var prev *agen.Result
	for i, run := range c.Runs {
		exp := c.Expect[i]
		n := run.Concurrent
		if n == 0 {
			n = 1
		}
		st.mu.Lock()
		callsBefore, approvalsBefore := len(st.calls), len(st.approvals)
		st.mu.Unlock()
		results := make([]got, n)
		started := time.Now()
		var wg sync.WaitGroup
		for j := 0; j < n; j++ {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				var mu sync.Mutex
				var deltas []string
				ropts := []agen.RunOption{
					agen.OnDelta(func(s string) { mu.Lock(); deltas = append(deltas, s); mu.Unlock() }),
					agen.OnReset(func() { mu.Lock(); deltas = nil; mu.Unlock() }),
				}
				if run.CloseAfterMs != nil {
					time.AfterFunc(time.Duration(*run.CloseAfterMs)*time.Millisecond, a.Close)
				}
				if id := run.Options.TaskID; id != "" {
					ropts = append(ropts, agen.TaskID(id))
				}
				if run.SameSession {
					ropts = append(ropts, agen.Session(prev.SessionID))
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if run.CancelBeforeStart {
					cancel()
				}
				if run.CancelAfterMs != nil {
					time.AfterFunc(time.Duration(*run.CancelAfterMs)*time.Millisecond, cancel)
				}
				r, err := a.Run(ctx, run.Input, ropts...)
				if err != nil {
					t.Errorf("run: %v", err)
					return
				}
				mu.Lock()
				results[j] = got{r, append([]string(nil), deltas...)}
				mu.Unlock()
			}(j)
		}
		wg.Wait()
		if wall := time.Since(started); exp.MaxWallMs > 0 && wall > time.Duration(exp.MaxWallMs)*time.Millisecond {
			t.Fatalf("took %v > %d ms", wall, exp.MaxWallMs)
		}
		for _, g := range results {
			if g.res == nil {
				t.Fatal("missing result")
			}
			r := g.res
			if r.Status != exp.Status {
				t.Fatalf("status %s, want %s (%+v)", r.Status, exp.Status, r)
			}
			if exp.Output != nil && r.Output != *exp.Output {
				t.Fatalf("output %q, want %q", r.Output, *exp.Output)
			}
			if len(g.deltas) < exp.MinDeltas {
				t.Fatalf("deltas %d < %d", len(g.deltas), exp.MinDeltas)
			}
			if exp.DeltasEqualOutput && strings.Join(g.deltas, "") != r.Output {
				t.Fatalf("deltas %q != output %q", strings.Join(g.deltas, ""), r.Output)
			}
			st.mu.Lock()
			calls, approvals := append([]string{}, st.calls[callsBefore:]...), append([]string{}, st.approvals[approvalsBefore:]...)
			st.mu.Unlock()
			if exp.ToolCalls != nil && n == 1 && !reflect.DeepEqual(calls, *exp.ToolCalls) {
				t.Fatalf("tool calls %v, want %v", calls, *exp.ToolCalls)
			}
			if exp.ApprovalsAsked != nil && !reflect.DeepEqual(approvals, *exp.ApprovalsAsked) {
				t.Fatalf("approvals %v, want %v", approvals, *exp.ApprovalsAsked)
			}
			if exp.ToolCancelled {
				deadline := time.Now().Add(3 * time.Second)
				for !st.toolCancelled.Load() && time.Now().Before(deadline) {
					time.Sleep(20 * time.Millisecond)
				}
				if !st.toolCancelled.Load() {
					t.Fatal("tool never saw the cancel")
				}
			}
			if exp.SameConversationAsPrevious && r.ConversationID != prev.ConversationID {
				t.Fatal("conversation changed")
			}
			if exp.SameRunAsPrevious && r.RunID != prev.RunID {
				t.Fatal("task ran again")
			}
			if len(exp.TraceIncludes) > 0 {
				spans, err := a.Trace(r.TraceID)
				if err != nil {
					t.Fatal(err)
				}
				names := map[string]bool{}
				for _, s := range spans {
					names[s.Name] = true
				}
				for _, want := range exp.TraceIncludes {
					if !names[want] {
						t.Fatalf("trace lacks %s: %v", want, names)
					}
				}
			}
		}
		prev = results[len(results)-1].res
	}
}
