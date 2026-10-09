package agen_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prjvvl/agen/sdks/go/agen"
)

func call(name string, args map[string]any) map[string]any {
	return map[string]any{"toolCalls": []any{map[string]any{"name": name, "arguments": args}}}
}

func say(text, expect string) map[string]any {
	m := map[string]any{"text": text}
	if expect != "" {
		m["expect"] = expect
	}
	return m
}

func addTool() agen.Tool {
	return agen.Tool{
		Name:        "add",
		Description: "Add a and b",
		ReadOnly:    true,
		Func: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var a struct{ A, B int }
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", err
			}
			return strings.TrimSpace(jsonNum(a.A + a.B)), nil
		},
	}
}

func jsonNum(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestGoToolRoundTripAndStreaming(t *testing.T) {
	a, err := agen.New(agen.Spec{
		Name:     "calc",
		Provider: agen.Fake(call("add", map[string]any{"a": 2, "b": 3}), say("The answer is 5", "5")),
	}, agen.WithTool(addTool()))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var mu sync.Mutex
	var streamed strings.Builder
	res, err := a.Run(context.Background(), "2+3?", agen.OnDelta(func(s string) {
		mu.Lock()
		streamed.WriteString(s)
		mu.Unlock()
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "succeeded" || res.Output != "The answer is 5" {
		t.Fatalf("%+v", res)
	}
	if streamed.String() != "The answer is 5" {
		t.Fatalf("streamed %q", streamed.String())
	}
	if res.RunID == "" || res.TraceID == "" || res.Usage.InputTokens == 0 {
		t.Fatalf("missing fields: %+v", res)
	}
}

func TestToolErrorsAndPanicsReachTheModel(t *testing.T) {
	failing := agen.Tool{Name: "fail", Func: func(context.Context, json.RawMessage) (string, error) {
		return "", errors.New("disk full")
	}}
	panicky := agen.Tool{Name: "boom", Func: func(context.Context, json.RawMessage) (string, error) {
		panic("kaboom")
	}}
	a, err := agen.New(agen.Spec{
		Name: "t",
		Provider: agen.Fake(
			call("fail", nil), call("boom", map[string]any{}), say("recovered", "tool panicked: kaboom"),
		),
	}, agen.WithTool(failing), agen.WithTool(panicky))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	res, err := a.Run(context.Background(), "go")
	if err != nil || res.Output != "recovered" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestContextCancellation(t *testing.T) {
	a, err := agen.New(agen.Spec{
		Name:     "slow",
		Provider: agen.Fake(map[string]any{"text": "late", "delayMs": 20000}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := a.Run(ctx, "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "cancelled" || time.Since(start) > 5*time.Second {
		t.Fatalf("status %s after %v", res.Status, time.Since(start))
	}
}

func TestBundleAndApprovals(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	hello := filepath.Join(filepath.Dir(file), "..", "..", "..", "examples", "bundles", "hello")
	a, err := agen.New(agen.Spec{Bundle: hello})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(context.Background(), "hi")
	a.Close()
	if err != nil || res.Output != "Hello! Nice to meet you." {
		t.Fatalf("%+v %v", res, err)
	}

	var asked []string
	pay := agen.Tool{Name: "pay", Func: func(context.Context, json.RawMessage) (string, error) { return "paid", nil }}
	b, err := agen.New(agen.Spec{
		Name:        "payer",
		Permissions: map[string]any{"default": "ask"},
		Provider:    agen.Fake(call("pay", map[string]any{"to": "bob"}), say("done", "paid")),
	}, agen.WithTool(pay), agen.WithApprovals(func(_ context.Context, tool string, args json.RawMessage) bool {
		asked = append(asked, tool+string(args))
		return true
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	res, err = b.Run(context.Background(), "pay bob")
	if err != nil || res.Output != "done" {
		t.Fatalf("%+v %v", res, err)
	}
	if len(asked) != 1 || !strings.Contains(asked[0], "bob") {
		t.Fatalf("approvals: %v", asked)
	}
}

func TestInvalidSpecIsAnError(t *testing.T) {
	_, err := agen.New(agen.Spec{Name: "x"}) // no provider
	var e *agen.Error
	if !errors.As(err, &e) || e.Code != "invalid_spec" {
		t.Fatalf("got %v", err)
	}
	if agen.Version() == "" {
		t.Fatal("empty version")
	}
}

func TestRunCancelReachesRunningTool(t *testing.T) {
	sawCancel := make(chan struct{})
	block := agen.Tool{Name: "block", Func: func(ctx context.Context, _ json.RawMessage) (string, error) {
		<-ctx.Done()
		close(sawCancel)
		return "", ctx.Err()
	}}
	a, err := agen.New(agen.Spec{Name: "c", Provider: agen.Fake(call("block", nil))}, agen.WithTool(block))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res, err := a.Run(ctx, "go")
	if err != nil || res.Status != "cancelled" {
		t.Fatalf("%+v %v", res, err)
	}
	select {
	case <-sawCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("tool context was never cancelled")
	}
}

func TestCloseDuringRunIsSafe(t *testing.T) {
	started := make(chan struct{})
	block := agen.Tool{Name: "block", Func: func(ctx context.Context, _ json.RawMessage) (string, error) {
		close(started)
		<-ctx.Done() // Close cancels in-flight tool calls
		return "", ctx.Err()
	}}
	a, err := agen.New(agen.Spec{Name: "c", Provider: agen.Fake(call("block", nil), say("after", ""))}, agen.WithTool(block))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *agen.Result, 1)
	go func() {
		r, _ := a.Run(context.Background(), "go")
		done <- r
	}()
	<-started
	closed := make(chan struct{})
	go func() { a.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung")
	}
	if r := <-done; r == nil {
		t.Fatal("run returned no result")
	}
	if _, err := a.Run(context.Background(), "x"); err == nil {
		t.Fatal("Run after Close must fail")
	}
}

func TestConcurrentRuns(t *testing.T) {
	a, err := agen.New(agen.Spec{
		Name: "par",
		// One shared script serves all runs, so use a turn-order-independent reply.
		Provider: map[string]any{"type": "fake", "cycle": true, "responses": []any{map[string]any{"text": "two", "delayMs": 50}}},
	}, agen.WithTool(addTool()))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := a.Run(context.Background(), "1+1")
			if err != nil {
				errs <- err
			} else if r.Status != "succeeded" {
				errs <- errors.New(r.Status + ": " + r.Error)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
