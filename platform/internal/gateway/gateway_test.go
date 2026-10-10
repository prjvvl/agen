package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/gen/agen/v1/agenv1connect"
	"github.com/prjvvl/agen/platform/internal/hub"
	"github.com/prjvvl/agen/platform/internal/manager"
	"github.com/prjvvl/agen/platform/internal/store"
	"github.com/prjvvl/agen/platform/internal/testutil"
)

const admin = "admin-secret-for-tests"

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stack is a Hub + scheduler + one Nest (Manager and Gateway) with real
// agen-host instances.
type stack struct {
	st     *store.Store
	c      agenv1connect.HubServiceClient
	hubURL string
	hubSrv *httptest.Server
	gwURL  string
	logs   *syncBuf
	mgr    *manager.Manager
	gw     *Gateway
}

func (k *stack) client(token string) agenv1connect.HubServiceClient {
	return agenv1connect.NewHubServiceClient(http.DefaultClient, k.hubURL, connect.WithProtoJSON(), connect.WithInterceptors(connect.UnaryInterceptorFunc(
		func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				req.Header().Set("Authorization", "Bearer "+token)
				return next(ctx, req)
			}
		})))
}

func startStack(t *testing.T) *stack {
	t.Helper()
	bin := testutil.HostBin(t)
	dir := t.TempDir()
	storeURL := "sqlite:" + filepath.ToSlash(filepath.Join(dir, "agen.db"))
	st, err := store.Open(context.Background(), storeURL)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := hub.New(st, admin)
	h.Poll = 20 * time.Millisecond
	mux := http.NewServeMux()
	p, hh := h.Handler()
	mux.Handle(p, hh)
	nestAPI := h.Nest()
	nestAPI.WatchPoll = 50 * time.Millisecond
	p, nh := nestAPI.Handler()
	mux.Handle(p, nh)
	hubSrv := httptest.NewServer(mux)

	ctx, cancel := context.WithCancel(context.Background())
	sched := h.NewScheduler("test-hub")
	sched.Tick, sched.Log, sched.Autoscaling = 100*time.Millisecond, log, false
	schedDone := make(chan struct{})
	go func() { sched.Run(ctx); close(schedDone) }()
	k := &stack{st: st, hubURL: hubSrv.URL, hubSrv: hubSrv, logs: logs}
	k.c = k.client(admin)
	jt, err := k.c.CreateJoinToken(ctx, connect.NewRequest(&agenv1.CreateJoinTokenRequest{TtlSeconds: 60}))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	k.gwURL = "http://" + ln.Addr().String()
	m, err := manager.New(manager.Config{HubURL: hubSrv.URL, JoinToken: jt.Msg.Token, Name: "local", Capacity: 8, HostBin: bin,
		StoreURL: storeURL, DataDir: filepath.Join(dir, "nest"), GatewayURL: k.gwURL, Heartbeat: 200 * time.Millisecond,
		DispatchPoll: 50 * time.Millisecond, LeaseSeconds: 5, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	k.mgr = m
	gw := New(m, k.gwURL, log)
	k.gw = gw
	gwSrv := &http.Server{Handler: gw.Handler()}
	go gwSrv.Serve(ln)
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- m.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-mgrDone
		<-schedDone
		gwSrv.Close()
		hubSrv.Close()
		st.Close()
		if t.Failed() {
			t.Logf("logs:\n%s", logs.String())
		}
	})
	return k
}

func (k *stack) deploy(t *testing.T, name string, files map[string][]byte, desired int32) {
	t.Helper()
	ctx := context.Background()
	if _, err := k.c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: name, BundleFiles: files})); err != nil {
		t.Fatal(err)
	}
	if _, err := k.c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: &agenv1.DeploymentRef{Name: name}, Desired: desired})); err != nil {
		t.Fatal(err)
	}
}

func (k *stack) waitReady(t *testing.T, n int) {
	t.Helper()
	waitFor(t, "ready instances", 60*time.Second, func() bool {
		r, _ := k.c.ListInstances(context.Background(), connect.NewRequest(&agenv1.ListInstancesRequest{}))
		ready := 0
		for _, in := range r.Msg.Instances {
			if in.State == agenv1.InstanceState_INSTANCE_STATE_READY {
				ready++
			}
		}
		return ready == n
	})
}

func writerFiles(t *testing.T) map[string][]byte {
	f := testutil.BundleFiles(t, "hello")
	f["x-agen/fake-script.json"] = []byte(`{"cycle":true,"responses":[{"text":"Draft ready."}]}`)
	return f
}

// P6 + E6: an agent delegates to another over A2A through the Nest Gateway
// (resolved via the Manager, not the Hub); the callee's run joins the
// caller's trace; with the Hub gone, delegation still works.
func TestDirectA2ADelegationSurvivesHubOutage(t *testing.T) {
	k := startStack(t)
	// As on a distributed Nest: callers must present a Hub-signed call token
	// (boss's Manager gets one for writer when it resolves it).
	k.gw.RequireAuth = true
	ctx := context.Background()
	c, st, logs, hubSrv, gwURL := k.c, k.st, k.logs, k.hubSrv, k.gwURL
	boss := testutil.BundleFiles(t, "hello")
	boss["x-agen/config.json"] = []byte(`{"kind":"pool","scale":{"min":0,"max":2},
		"delegates":[{"name":"writer","description":"Writes drafts"}],
		"permissions":{"default":"deny","rules":[{"tool":"call_agent","action":"allow"}]}}`)
	boss["x-agen/fake-script.json"] = []byte(`{"cycle":true,"responses":[
		{"toolCalls":[{"name":"call_agent","arguments":{"agent":"writer","message":"draft it"}}]},
		{"text":"Boss: done","expect":"Draft ready."}]}`)
	k.deploy(t, "writer", writerFiles(t), 1)
	k.deploy(t, "boss", boss, 1)
	k.waitReady(t, 2)

	// A task for boss: boss calls writer over A2A.
	sub, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: &agenv1.DeploymentRef{Name: "boss"}, Input: "get me a draft"}))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: sub.Msg.Task.Id, WaitSeconds: 30}))
	if err != nil || got.Msg.Task.State != agenv1.TaskState_TASK_STATE_SUCCEEDED || got.Msg.Task.Output != "Boss: done" {
		t.Fatalf("boss task: %v %v", got, err)
	}
	bossRunID := got.Msg.Task.RunId
	wr, _ := c.ListRuns(ctx, connect.NewRequest(&agenv1.ListRunsRequest{Namespace: "default", Deployment: "writer"}))
	if len(wr.Msg.Runs) != 1 {
		t.Fatalf("writer runs: %v", wr.Msg.Runs)
	}
	wrun := wr.Msg.Runs[0]
	if wrun.ParentRunId != bossRunID || wrun.RootRunId != bossRunID || wrun.Status != "succeeded" {
		t.Fatalf("writer lineage: %v (boss run %s)", wrun, bossRunID)
	}

	// One linked trace across both agents.
	br, _ := c.ListRuns(ctx, connect.NewRequest(&agenv1.ListRunsRequest{Namespace: "default", Deployment: "boss"}))
	traceID := br.Msg.Runs[0].TraceId
	if wrun.TraceId != traceID {
		t.Fatalf("writer trace %s != boss trace %s", wrun.TraceId, traceID)
	}
	tr, err := c.GetTrace(ctx, connect.NewRequest(&agenv1.GetTraceRequest{TraceId: traceID}))
	if err != nil || len(tr.Msg.Runs) != 2 {
		t.Fatalf("trace runs: %v %v", tr, err)
	}
	var bossTool, writerRoot *agenv1.Span
	for _, s := range tr.Msg.Spans {
		if s.RunId == bossRunID && s.Name == "agen.tool" {
			bossTool = s
		}
		if s.RunId == wrun.Id && s.Name == "agen.run" {
			writerRoot = s
		}
	}
	if bossTool == nil || writerRoot == nil || writerRoot.ParentSpanId != bossTool.SpanId {
		t.Fatalf("writer's run span must be a child of boss's tool span: %v / %v", bossTool, writerRoot)
	}

	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "message/send", "params": map[string]any{
		"message": map[string]any{"kind": "message", "role": "user", "messageId": "outage-1", "parts": []map[string]string{{"kind": "text", "text": "again"}}}}})
	// Without a call token the Gateway refuses the call.
	anon, err := http.Post(gwURL+"/a2a/default/boss", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	anonRaw, _ := io.ReadAll(anon.Body)
	anon.Body.Close()
	if anon.StatusCode != http.StatusUnauthorized || !strings.Contains(string(anonRaw), "-32052") {
		t.Fatalf("anonymous call: %d %s", anon.StatusCode, anonRaw)
	}
	// An operator gets a call token for boss when resolving it.
	res, err := c.Resolve(ctx, connect.NewRequest(&agenv1.ResolveRequest{Ref: &agenv1.DeploymentRef{Name: "boss"}}))
	if err != nil || res.Msg.Token == "" {
		t.Fatalf("resolve: %v %v", res, err)
	}

	// The Hub goes away. A direct A2A call to boss still delegates to writer:
	// Gateway -> boss -> Manager (cached resolve and call token) -> Gateway ->
	// writer; Gateways verify tokens with their cached Hub key.
	// Listener first: a connection accepted in between would still reach the Hub.
	hubSrv.Listener.Close()
	hubSrv.CloseClientConnections()
	req, _ := http.NewRequest(http.MethodPost, gwURL+"/a2a/default/boss", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+res.Msg.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"state":"completed"`) || !strings.Contains(string(raw), "Boss: done") {
		t.Fatalf("A2A with the Hub down: %s", raw)
	}
	if !strings.Contains(logs.String(), "resolving from cache") {
		t.Fatal("expected the Manager to resolve writer from its cache while the Hub is down")
	}
	var writerRuns int
	if err := st.DB().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM runs WHERE deployment = 'writer' AND status = 'succeeded'").Scan(&writerRuns); err != nil {
		t.Fatal(err)
	}
	if writerRuns != 2 {
		t.Fatalf("writer runs after outage call: %d", writerRuns)
	}
}

// An "ask" tool call in a managed agent becomes a durable approval at the
// Hub; the person who submitted the task cannot approve it; approve lets
// the call run, deny stops it, and an unanswered ask expires.
func TestDurableApprovalsForManagedAgents(t *testing.T) {
	k := startStack(t)
	ctx := context.Background()
	tok, err := k.c.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "alice", Scopes: []string{"operator", "approver"}}))
	if err != nil {
		t.Fatal(err)
	}
	alice := k.client(tok.Msg.Secret)
	gated := testutil.BundleFiles(t, "hello")
	gated["x-agen/config.json"] = []byte(`{"kind":"pool","scale":{"min":0,"max":2},"delegates":[{"name":"writer"}],
		"permissions":{"default":"deny","rules":[{"tool":"call_agent","action":"ask"}],"approvalTimeout":"3s"}}`)
	gated["x-agen/fake-script.json"] = []byte(`{"cycle":true,"responses":[
		{"toolCalls":[{"name":"call_agent","arguments":{"agent":"writer","message":"publish"}}]},
		{"text":"gated done"}]}`)
	k.deploy(t, "writer", writerFiles(t), 1)
	k.deploy(t, "gated", gated, 1)
	k.waitReady(t, 2)
	writerRuns := func() int {
		var n int
		k.st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE deployment = 'writer'").Scan(&n)
		return n
	}
	pending := func() *agenv1.Approval {
		var a *agenv1.Approval
		waitFor(t, "pending approval", 20*time.Second, func() bool {
			r, err := k.c.ListApprovals(ctx, connect.NewRequest(&agenv1.ListApprovalsRequest{State: agenv1.ApprovalState_APPROVAL_STATE_PENDING}))
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Msg.Approvals) == 1 {
				a = r.Msg.Approvals[0]
			}
			return a != nil
		})
		return a
	}
	finish := func(id string) *agenv1.Task {
		r, err := k.c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: id, WaitSeconds: 30}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg.Task
	}

	// 1. Alice asks; she cannot approve her own request; admin can.
	s1, err := alice.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: &agenv1.DeploymentRef{Name: "gated"}, Input: "go"}))
	if err != nil {
		t.Fatal(err)
	}
	a := pending()
	if a.Deployment != "gated" || a.Tool != "call_agent" || a.RequestedBy != "token:"+tok.Msg.Token.Id || a.Arguments.AsMap()["agent"] != "writer" {
		t.Fatalf("approval: %v", a)
	}
	if st, _ := k.c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: s1.Msg.Task.Id})); st.Msg.Task.State != agenv1.TaskState_TASK_STATE_RUNNING {
		t.Fatalf("task while waiting: %v", st.Msg.Task.State)
	}
	if _, err := alice.DecideApproval(ctx, connect.NewRequest(&agenv1.DecideApprovalRequest{Id: a.Id, Approve: true})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("self-approval: %v", err)
	}
	before := writerRuns()
	if _, err := k.c.DecideApproval(ctx, connect.NewRequest(&agenv1.DecideApprovalRequest{Id: a.Id, Approve: true})); err != nil {
		t.Fatal(err)
	}
	if tk := finish(s1.Msg.Task.Id); tk.State != agenv1.TaskState_TASK_STATE_SUCCEEDED || tk.Output != "gated done" {
		t.Fatalf("approved task: %v", tk)
	}
	if writerRuns() != before+1 {
		t.Fatal("approved call did not reach writer")
	}

	// 2. Admin asks, Alice (approver) denies: the call never happens.
	s2, _ := k.c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: &agenv1.DeploymentRef{Name: "gated"}, Input: "go"}))
	a = pending()
	if a.RequestedBy != "admin" {
		t.Fatalf("requested_by: %q", a.RequestedBy)
	}
	if _, err := alice.DecideApproval(ctx, connect.NewRequest(&agenv1.DecideApprovalRequest{Id: a.Id, Approve: false})); err != nil {
		t.Fatal(err)
	}
	if tk := finish(s2.Msg.Task.Id); tk.State != agenv1.TaskState_TASK_STATE_SUCCEEDED {
		t.Fatalf("denied task: %v", tk)
	}
	if writerRuns() != before+1 {
		t.Fatal("denied call reached writer")
	}

	// 3. Nobody answers: the ask expires after approvalTimeout (3s).
	s3, _ := k.c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: &agenv1.DeploymentRef{Name: "gated"}, Input: "go"}))
	a = pending()
	start := time.Now()
	if tk := finish(s3.Msg.Task.Id); tk.State != agenv1.TaskState_TASK_STATE_SUCCEEDED {
		t.Fatalf("expired task: %v", tk)
	}
	if el := time.Since(start); el > 15*time.Second {
		t.Fatalf("ask took %s to expire", el)
	}
	if writerRuns() != before+1 {
		t.Fatal("expired call reached writer")
	}
	waitFor(t, "approval expired", 10*time.Second, func() bool {
		got, _ := k.c.ListApprovals(ctx, connect.NewRequest(&agenv1.ListApprovalsRequest{}))
		for _, ap := range got.Msg.Approvals {
			if ap.Id == a.Id {
				return ap.State == agenv1.ApprovalState_APPROVAL_STATE_EXPIRED
			}
		}
		return false
	})
	// The tool results the model saw name the decision.
	var msgs string
	rows, err := k.st.DB().QueryContext(ctx, "SELECT body FROM messages")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		rows.Scan(&c)
		msgs += c + "\n"
	}
	rows.Close()
	if !strings.Contains(msgs, "not approved (denied)") || !strings.Contains(msgs, "not approved (expired)") {
		t.Fatalf("tool results:\n%s", msgs)
	}

	// 4. Alice calls gated directly over A2A with her call token: the run is
	// hers, so the approval names her and she cannot approve it either.
	k.gw.RequireAuth = true
	res, err := alice.Resolve(ctx, connect.NewRequest(&agenv1.ResolveRequest{Ref: &agenv1.DeploymentRef{Name: "gated"}}))
	if err != nil || res.Msg.Token == "" {
		t.Fatalf("resolve as alice: %v %v", res, err)
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "message/send", "params": map[string]any{
		"message": map[string]any{"kind": "message", "role": "user", "messageId": "alice-a2a", "parts": []map[string]string{{"kind": "text", "text": "go"}}}}})
	answered := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, k.gwURL+"/a2a/default/gated", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+res.Msg.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			answered <- err.Error()
			return
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		answered <- string(raw)
	}()
	a = pending()
	if a.RequestedBy != "token:"+tok.Msg.Token.Id {
		t.Fatalf("A2A approval requested_by: %q", a.RequestedBy)
	}
	if _, err := alice.DecideApproval(ctx, connect.NewRequest(&agenv1.DecideApprovalRequest{Id: a.Id, Approve: true})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("self-approval over A2A: %v", err)
	}
	if _, err := k.c.DecideApproval(ctx, connect.NewRequest(&agenv1.DecideApprovalRequest{Id: a.Id, Approve: true})); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-answered:
		if !strings.Contains(raw, "gated done") || !strings.Contains(raw, `"agen.caller":"user:token:`+tok.Msg.Token.Id+`"`) {
			t.Fatalf("A2A answer: %s", raw)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("A2A call not answered after approval")
	}
}

// An agent that keeps delegating to itself is stopped by
// max_delegation_depth (the chain unwinds cleanly), and a run that keeps
// calling is stopped by max_fan_out.
func TestDelegationLimitsStopRunawayLoops(t *testing.T) {
	k := startStack(t)
	ctx := context.Background()
	loop := testutil.BundleFiles(t, "hello")
	loop["x-agen/config.json"] = []byte(`{"kind":"pool","scale":{"min":0,"max":1,"maxConcurrency":8},
		"delegates":[{"name":"loop"}],"limits":{"maxDelegationDepth":3},
		"permissions":{"default":"deny","rules":[{"tool":"call_agent","action":"allow"}]}}`)
	loop["x-agen/fake-script.json"] = []byte(`{"perRun":true,"responses":[
		{"toolCalls":[{"name":"call_agent","arguments":{"agent":"loop","message":"again"}}]},
		{"text":"stopped"}]}`)
	fan := testutil.BundleFiles(t, "hello")
	fan["x-agen/config.json"] = []byte(`{"kind":"pool","scale":{"min":0,"max":1},
		"delegates":[{"name":"writer"}],"limits":{"maxFanOut":2},
		"permissions":{"default":"deny","rules":[{"tool":"call_agent","action":"allow"}]}}`)
	calls := `{"toolCalls":[{"name":"call_agent","arguments":{"agent":"writer","message":"more"}}]}`
	fan["x-agen/fake-script.json"] = []byte(`{"perRun":true,"responses":[` + calls + `,` + calls + `,` + calls + `,{"text":"fanned"}]}`)
	k.deploy(t, "loop", loop, 1)
	k.deploy(t, "fan", fan, 1)
	k.deploy(t, "writer", writerFiles(t), 1)
	k.waitReady(t, 3)
	count := func(dep string) (n int) {
		k.st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE deployment = $1", dep).Scan(&n)
		return
	}

	s, _ := k.c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: &agenv1.DeploymentRef{Name: "loop"}, Input: "go"}))
	r, err := k.c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: s.Msg.Task.Id, WaitSeconds: 60}))
	if err != nil || r.Msg.Task.State != agenv1.TaskState_TASK_STATE_SUCCEEDED || r.Msg.Task.Output != "stopped" {
		t.Fatalf("runaway loop task: %v %v", r, err)
	}
	// Depth 0 (the task) plus depths 1..3; the call from depth 3 is refused.
	if n := count("loop"); n != 4 {
		t.Fatalf("loop runs: %d, want 4", n)
	}
	var refused int
	k.st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM messages WHERE body LIKE '%exceeds max_delegation_depth 3%'").Scan(&refused)
	if refused == 0 {
		t.Fatal("the deepest call must be refused by max_delegation_depth")
	}

	s, _ = k.c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: &agenv1.DeploymentRef{Name: "fan"}, Input: "go"}))
	r, err = k.c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: s.Msg.Task.Id, WaitSeconds: 60}))
	if err != nil || r.Msg.Task.State != agenv1.TaskState_TASK_STATE_SUCCEEDED || r.Msg.Task.Output != "fanned" {
		t.Fatalf("fan-out task: %v %v", r, err)
	}
	if n := count("writer"); n != 2 {
		t.Fatalf("writer runs: %d, want 2 (max_fan_out)", n)
	}
}

// A2A load scales a pool up: calls waiting on busy instances make the
// Gateway ask the Hub for more instances (up to max).
func TestA2ALoadScalesPoolUp(t *testing.T) {
	k := startStack(t)
	ctx := context.Background()
	slow := testutil.BundleFiles(t, "hello")
	slow["x-agen/config.json"] = []byte(`{"kind":"pool","scale":{"min":0,"max":3,"maxConcurrency":1}}`)
	slow["x-agen/fake-script.json"] = []byte(`{"perRun":true,"responses":[{"text":"slow answer","delayMs":2000}]}`)
	k.deploy(t, "slow", slow, 1)
	k.waitReady(t, 1)
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan string, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": i, "method": "message/send", "params": map[string]any{
				"message": map[string]any{"kind": "message", "role": "user", "messageId": "load-" + string(rune('a'+i)), "parts": []map[string]string{{"kind": "text", "text": "hi"}}}}})
			resp, err := http.Post(k.gwURL+"/a2a/default/slow", "application/json", bytes.NewReader(body))
			if err != nil {
				errs <- err.Error()
				return
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if !strings.Contains(string(raw), "slow answer") {
				errs <- string(raw)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("call failed: %s", e)
	}
	var owners int
	k.st.DB().QueryRowContext(ctx, "SELECT COUNT(DISTINCT owner) FROM runs WHERE deployment = 'slow'").Scan(&owners)
	d, _ := k.c.GetDeployment(ctx, connect.NewRequest(&agenv1.GetDeploymentRequest{Ref: &agenv1.DeploymentRef{Name: "slow"}}))
	t.Logf("3 calls answered in %s on %d instances; desired now %d", time.Since(start).Round(time.Millisecond), owners, d.Msg.Deployment.Desired)
	if owners < 2 || d.Msg.Deployment.Desired < 2 || d.Msg.Deployment.Desired > 3 {
		t.Fatalf("A2A load did not scale the pool: %d instances served, desired %d", owners, d.Msg.Deployment.Desired)
	}
}

func sendA2A(t *testing.T, url, messageID string) (string, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "message/send", "params": map[string]any{
		"message": map[string]any{"kind": "message", "role": "user", "messageId": messageID, "parts": []map[string]string{{"kind": "text", "text": "hi"}}}}})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Error(err)
		return "", nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	res, _ := out["result"].(map[string]any)
	return string(raw), res
}

// With RequireAuth, only authenticated callers wake a sleeping
// deployment through its card, and an A2A task belongs to the caller that
// created it (message ids can be guessed).
func TestGatewayBindsTasksToCallers(t *testing.T) {
	k := startStack(t)
	k.gw.RequireAuth = true
	ctx := context.Background()
	sleepy := testutil.BundleFiles(t, "hello")
	sleepy["x-agen/config.json"] = []byte(`{"kind":"pool","scale":{"min":0,"max":1}}`)
	if _, err := k.c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "sleepy", BundleFiles: sleepy})); err != nil {
		t.Fatal(err)
	}
	card, err := http.Get(k.gwURL + "/a2a/default/sleepy/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	card.Body.Close()
	time.Sleep(500 * time.Millisecond)
	if d, _ := k.c.GetDeployment(ctx, connect.NewRequest(&agenv1.GetDeploymentRequest{Ref: &agenv1.DeploymentRef{Name: "sleepy"}})); card.StatusCode != http.StatusUnauthorized ||
		d.Msg.Deployment.GetDesired() != 0 {
		t.Fatalf("anonymous card: %d, desired %d", card.StatusCode, d.Msg.Deployment.GetDesired())
	}

	k.deploy(t, "writer", writerFiles(t), 1)
	k.waitReady(t, 1)
	bobTok, _ := k.c.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "bob", Scopes: []string{"operator"}}))
	token := func(c agenv1connect.HubServiceClient) string {
		r, err := c.Resolve(ctx, connect.NewRequest(&agenv1.ResolveRequest{Ref: &agenv1.DeploymentRef{Name: "writer"}}))
		if err != nil || r.Msg.Token == "" {
			t.Fatalf("resolve: %v %v", r, err)
		}
		return r.Msg.Token
	}
	adminTok, bob := token(k.c), token(k.client(bobTok.Msg.Secret))
	call := func(tok, method string, params map[string]any) (int, string) {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		req, _ := http.NewRequest(http.MethodPost, k.gwURL+"/a2a/default/writer", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	msg := map[string]any{"message": map[string]any{"kind": "message", "role": "user", "messageId": "secret-msg", "parts": []map[string]string{{"kind": "text", "text": "hi"}}}}
	if _, raw := call(adminTok, "message/send", msg); !strings.Contains(raw, "Draft ready.") {
		t.Fatalf("admin send: %s", raw)
	}
	byMsg := map[string]any{"metadata": map[string]any{"messageId": "secret-msg"}}
	adminID := a2aTaskID("default", "writer", "user:admin", "secret-msg")
	if _, raw := call(bob, "tasks/get", map[string]any{"id": adminID}); strings.Contains(raw, "Draft ready.") || !strings.Contains(raw, "-32001") {
		t.Fatalf("bob read admin's task: %s", raw)
	}
	if _, raw := call(bob, "tasks/cancel", byMsg); !strings.Contains(raw, "-32002") {
		t.Fatalf("bob cancel: %s", raw)
	}
	// The same message id from bob is bob's own task (a new run), not a
	// replay of admin's.
	if _, raw := call(bob, "message/send", msg); !strings.Contains(raw, "Draft ready.") || strings.Contains(raw, adminID) {
		t.Fatalf("bob's send with admin's message id: %s", raw)
	}
	if _, raw := call(adminTok, "tasks/get", map[string]any{"id": adminID}); !strings.Contains(raw, "Draft ready.") {
		t.Fatalf("admin reads own task: %s", raw)
	}
}

// An agent's "platform" secrets come from the Hub through its Manager: it
// cannot start until the secret is set, then runs, and the value never
// reaches the Store.
func TestPlatformSecretsReachInstances(t *testing.T) {
	k := startStack(t)
	ctx := context.Background()
	const value = "sk-platform-secret-value-42"
	vault := testutil.BundleFiles(t, "hello")
	vault["x-agen/secrets.json"] = []byte(`{"API_KEY":{"source":"platform"}}`)
	k.deploy(t, "vault", vault, 1)
	time.Sleep(3 * time.Second)
	if r, _ := k.c.ListInstances(ctx, connect.NewRequest(&agenv1.ListInstancesRequest{Deployment: "vault"})); len(r.Msg.Instances) > 0 {
		for _, in := range r.Msg.Instances {
			if in.State == agenv1.InstanceState_INSTANCE_STATE_READY {
				t.Fatal("instance ready without its platform secret")
			}
		}
	}
	if _, err := k.c.SetSecret(ctx, connect.NewRequest(&agenv1.SetSecretRequest{Name: "API_KEY", Value: value, Deployments: []string{"vault"}})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "vault ready once the secret is set", 60*time.Second, func() bool {
		r, _ := k.c.ListInstances(ctx, connect.NewRequest(&agenv1.ListInstancesRequest{Deployment: "vault"}))
		for _, in := range r.Msg.Instances {
			if in.State == agenv1.InstanceState_INSTANCE_STATE_READY {
				return true
			}
		}
		return false
	})
	sub, err := k.c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: &agenv1.DeploymentRef{Name: "vault"}, Input: "hi"}))
	if err != nil {
		t.Fatal(err)
	}
	got, err := k.c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: sub.Msg.Task.Id, WaitSeconds: 30}))
	if err != nil || got.Msg.Task.State != agenv1.TaskState_TASK_STATE_SUCCEEDED {
		t.Fatalf("vault task: %v %v", got, err)
	}
	for _, q := range []string{"SELECT body FROM messages", "SELECT attributes FROM spans", "SELECT message FROM logs", "SELECT input || output FROM runs"} {
		rows, err := k.st.DB().QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		for rows.Next() {
			var s string
			rows.Scan(&s)
			if strings.Contains(s, value) {
				t.Fatalf("secret value in the Store (%s)", q)
			}
		}
		rows.Close()
	}
	if strings.Contains(k.logs.String(), value) {
		t.Fatal("secret value in the Nest logs")
	}
}

// An instance that does not answer within RunTimeout ends the call as
// failed (and is told to cancel) instead of holding it forever.
func TestA2ACallDeadline(t *testing.T) {
	k := startStack(t)
	k.gw.RunTimeout = time.Second
	f := testutil.BundleFiles(t, "hello")
	f["x-agen/fake-script.json"] = []byte(`{"cycle":true,"responses":[{"text":"too late","delayMs":8000}]}`)
	k.deploy(t, "stuck", f, 1)
	k.waitReady(t, 1)
	start := time.Now()
	raw, res := sendA2A(t, k.gwURL+"/a2a/default/stuck", "deadline-1")
	st, _ := res["status"].(map[string]any)
	if st["state"] != "failed" || !strings.Contains(raw, "no answer within 1s") {
		t.Fatalf("stuck call: %s", raw)
	}
	if el := time.Since(start); el > 6*time.Second {
		t.Fatalf("call held for %s", el)
	}
}

// An instance dying mid-call: the Gateway retries on another instance with
// the same task id, which resumes the same run; the failure is never cached.
// Concurrent sends of one message id produce one run.
func TestGatewayRetriesAndDeduplicatesA2ACalls(t *testing.T) {
	k := startStack(t)
	ctx := context.Background()
	slow := testutil.BundleFiles(t, "hello")
	slow["x-agen/config.json"] = []byte(`{"kind":"pool","scale":{"min":0,"max":2,"maxConcurrency":1}}`)
	slow["x-agen/fake-script.json"] = []byte(`{"perRun":true,"responses":[{"text":"survived","delayMs":2500}]}`)
	k.deploy(t, "slow", slow, 2)
	k.waitReady(t, 2)
	url := k.gwURL + "/a2a/default/slow"
	taskID := a2aTaskID("default", "slow", "", "crash-1")

	done := make(chan map[string]any, 1)
	go func() { _, res := sendA2A(t, url, "crash-1"); done <- res }()
	// Kill the instance running the call.
	var owner string
	waitFor(t, "call running", 20*time.Second, func() bool {
		k.st.DB().QueryRowContext(ctx, "SELECT owner FROM runs WHERE task_id = $1", taskID).Scan(&owner)
		return owner != ""
	})
	time.Sleep(300 * time.Millisecond)
	if !k.mgr.KillInstance(owner) {
		t.Fatal("kill")
	}
	var res map[string]any
	select {
	case res = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("call not answered after the instance died")
	}
	status, _ := res["status"].(map[string]any)
	if status["state"] != "completed" {
		t.Fatalf("after crash: %v", res)
	}
	var runs int
	var runOwner string
	k.st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE task_id = $1", taskID).Scan(&runs)
	k.st.DB().QueryRowContext(ctx, "SELECT owner FROM runs WHERE task_id = $1", taskID).Scan(&runOwner)
	if runs != 1 || runOwner == owner {
		t.Fatalf("the same run must resume on another instance: %d runs, owner %s (killed %s)", runs, runOwner, owner)
	}
	// Retrying the message returns the finished task (no second run).
	_, again := sendA2A(t, url, "crash-1")
	if again["id"] != res["id"] {
		t.Fatalf("retry: %v", again)
	}

	// Two concurrent sends of one new message: one run, same answer.
	k.waitReady(t, 2)
	var wg sync.WaitGroup
	results := make([]map[string]any, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, results[i] = sendA2A(t, url, "twice-1") }(i)
	}
	wg.Wait()
	k.st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE task_id = $1", a2aTaskID("default", "slow", "", "twice-1")).Scan(&runs)
	m0, _ := results[0]["metadata"].(map[string]any)
	m1, _ := results[1]["metadata"].(map[string]any)
	if runs != 1 || m0["agen.run_id"] == nil || m0["agen.run_id"] != m1["agen.run_id"] {
		t.Fatalf("concurrent duplicate sends: %d runs, %v / %v", runs, results[0], results[1])
	}
}

// A cold A2A wake starts one instance, not two: an instance that is still
// starting is not "saturated".
func TestColdWakeDoesNotOverScale(t *testing.T) {
	k := startStack(t)
	ctx := context.Background()
	cold := testutil.BundleFiles(t, "hello")
	cold["x-agen/config.json"] = []byte(`{"kind":"pool","scale":{"min":0,"max":3,"maxConcurrency":1}}`)
	k.deploy(t, "cold", cold, 0)
	raw, res := sendA2A(t, k.gwURL+"/a2a/default/cold", "cold-1")
	if st, _ := res["status"].(map[string]any); st["state"] != "completed" {
		t.Fatalf("cold call: %s", raw)
	}
	time.Sleep(2500 * time.Millisecond) // past a few saturation throttle windows
	d, _ := k.c.GetDeployment(ctx, connect.NewRequest(&agenv1.GetDeploymentRequest{Ref: &agenv1.DeploymentRef{Name: "cold"}}))
	if d.Msg.Deployment.Desired != 1 {
		t.Fatalf("cold wake scaled to %d, want 1", d.Msg.Deployment.Desired)
	}
}
