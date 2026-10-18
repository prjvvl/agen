package hub

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// GetActivity says what each unfinished run is doing (waiting for an agent,
// an approval, the model or a tool), counts queues, and raises warnings;
// GetMetrics groups model calls by the model that served them.
func TestActivityAndModelMetrics(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	for _, name := range []string{"lead", "writer", "gate"} {
		f := helloFiles(t)
		f["x-agen/agent.md"] = []byte("---\nname: hello\ndescription: x\nskills: [greeting]\nmaxTurns: 10\n---\nHi.\n")
		if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: name, BundleFiles: f})); err != nil {
			t.Fatal(err)
		}
	}
	now := store.NowMs()
	old := now - 40*60*1000
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.store.DB().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ('s1','x','default','lead','{}',$1,$1)", now)
	exec("INSERT INTO conversations (id, session_id, created_ms) VALUES ('c1','s1',$1)", now)
	run := func(id, dep, parent string, step int, started int64) {
		exec("INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ($1,'x','default',$2,'{}',$3,$3)", "s-"+id, dep, started)
		exec("INSERT INTO conversations (id, session_id, created_ms) VALUES ($1,$2,$3)", "c-"+id, "s-"+id, started)
		exec("INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, trace_id, started_ms, parent_run_id, step) "+
			"VALUES ($1,$6,$7,'default',$2,'running','do the thing','t1',$3,$4,$5)", id, dep, started, parent, step, "s-"+id, "c-"+id)
		d, _ := e.store.GetDeployment(ctx, "default", dep)
		exec("UPDATE runs SET definition_digest = $1 WHERE id = $2", d.DefinitionDigest, id)
	}
	span := func(id, runID, name, status string, start, end int64, attrs string) {
		exec("INSERT INTO spans (span_id, trace_id, run_id, name, start_ms, end_ms, status, attributes, seq) VALUES ($1,'t1',$2,$3,$4,$5,$6,$7,0)",
			id, runID, name, start, end, status, attrs)
	}
	run("r-lead", "lead", "", 9, now-60_000)           // turn 9 of 10, delegating
	run("r-writer", "writer", "r-lead", 1, now-30_000) // the delegated run, in a model call
	run("r-gate", "gate", "", 2, old)                  // waiting for an approval for 40 min
	run("r-stuck", "writer", "", 3, old)               // a tool call open for 40 min
	span("sp1", "r-lead", "agen.tool", "unfinished", now-30_000, now-30_000, `{"gen_ai.tool.name":"call_agent","agen.delegate.target":"writer"}`)
	span("sp2", "r-writer", "gen_ai.chat", "unfinished", now-1_000, now-1_000, `{}`)
	span("sp3", "r-stuck", "agen.tool", "unfinished", old, old, `{"gen_ai.tool.name":"web.fetch"}`)
	exec("INSERT INTO approvals (id, namespace, deployment, run_id, tool, arguments, state, requested_by, created_ms, expires_ms) "+
		"VALUES ('ap1','default','gate','r-gate','deploy','{}','pending','admin',$1,$2)", old, now+3_600_000)
	// Finished model calls: deepseek served lead (one failed), a fallback.
	for i, a := range []struct{ status, attrs string }{
		{"ok", `{"gen_ai.provider.name":"openrouter","gen_ai.request.model":"typesafe/jev-router","gen_ai.response.model":"deepseek/v4","gen_ai.usage.input_tokens":100,"gen_ai.usage.output_tokens":10,"agen.usage.cost_usd":0.001}`},
		{"error", `{"gen_ai.provider.name":"openrouter","gen_ai.request.model":"deepseek/v4"}`},
		{"ok", `{"gen_ai.provider.name":"openrouter","gen_ai.request.model":"backup/m","gen_ai.response.model":"backup/m","agen.fallback":true}`},
	} {
		span(fmt.Sprintf("m%d", i), "r-lead", "gen_ai.chat", a.status, now-50_000, now-49_000, a.attrs)
	}
	// A burst: twelve tasks in one conversation; one due task waiting long.
	for i := 0; i < 12; i++ {
		if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "gate"), Input: "again", ConversationKey: "loop"})); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE tasks SET created_ms = $1 WHERE deployment = 'gate'", old)
	exec("UPDATE tasks SET created_ms = $1 WHERE id IN (SELECT id FROM tasks WHERE deployment = 'gate' LIMIT 11)", now)

	res, err := e.client(e.token(t, ScopeViewer)).GetActivity(ctx, connect.NewRequest(&agenv1.GetActivityRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]*agenv1.ActiveRun{}
	for _, r := range res.Msg.Runs {
		by[r.RunId] = r
	}
	for id, want := range map[string][2]string{
		"r-lead":   {"agent", "writer"},
		"r-writer": {"model", ""},
		"r-gate":   {"approval", ""},
		"r-stuck":  {"tool", "web.fetch"},
	} {
		r := by[id]
		if r == nil || r.Activity != want[0] || r.Detail != want[1] {
			t.Fatalf("%s: %v, want %v", id, r, want)
		}
	}
	if by["r-lead"].MaxTurns != 10 || by["r-gate"].ApprovalId != "ap1" || by["r-writer"].ParentRunId != "r-lead" {
		t.Fatalf("runs: %v", res.Msg.Runs)
	}
	kinds := map[string]string{}
	for _, w := range res.Msg.Warnings {
		kinds[w.Kind+"/"+w.Ref.Name] = w.Message
	}
	for _, want := range []string{"near_turn_limit/lead", "waiting_approval/gate", "quiet_run/writer", "burst/gate", "queue_waiting/gate", "blocked_by_approval/gate"} {
		if _, ok := kinds[want]; !ok {
			t.Errorf("missing warning %s in %v", want, kinds)
		}
	}
	if len(res.Msg.Queues) != 1 || res.Msg.Queues[0].Queued != 12 {
		t.Fatalf("queues: %v", res.Msg.Queues)
	}

	m, err := c.GetMetrics(ctx, connect.NewRequest(&agenv1.GetMetricsRequest{WindowSeconds: 3600}))
	if err != nil {
		t.Fatal(err)
	}
	models := map[string]*agenv1.ModelMetrics{}
	for _, x := range m.Msg.Models {
		models[x.Model] = x
	}
	if d := models["deepseek/v4"]; d == nil || d.Calls != 2 || d.Failed != 1 || d.Usage.InputTokens != 100 {
		t.Fatalf("served model stats: %v", m.Msg.Models)
	}
	if b := models["backup/m"]; b == nil || b.Fallbacks != 1 {
		t.Fatalf("fallback stats: %v", m.Msg.Models)
	}
	var edge *agenv1.CallEdge
	for _, x := range m.Msg.Edges {
		if x.From.Name == "lead" && x.To.Name == "writer" {
			edge = x
		}
	}
	if edge == nil || edge.Active != 1 {
		t.Fatalf("edges: %v", m.Msg.Edges)
	}
	if c := by["r-writer"].Caller; c.GetName() != "lead" {
		t.Fatalf("caller of the delegated run: %v", c)
	}
	entries := map[string]int32{}
	for _, x := range m.Msg.Entries {
		entries[x.Source+">"+x.To.Name] = x.Runs
	}
	// Runs with no task and no parent came from A2A callers outside the fleet;
	// the delegated writer run is an edge, not an entry.
	if entries["a2a>lead"] != 1 || entries["a2a>gate"] != 1 || entries["a2a>writer"] != 1 {
		t.Fatalf("entries: %v", m.Msg.Entries)
	}
	if !strings.Contains(kinds["burst/gate"], "loop") {
		t.Fatalf("burst message: %q", kinds["burst/gate"])
	}
}
