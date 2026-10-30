package hub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// ReportProgress records a task's latest progress and emits a task.progress
// event, rate-limited to one per 10s bucket (repeated calls in the same
// bucket only append one event); it no-ops on an already-terminal task.
// features: tasks.progress
func TestReportProgressRecordsAndRateLimitsTheEvent(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	idB, b := e.enroll(t, "nest-b", 4, nil)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "hello"), Desired: 1}))
	e.scheduler().Step(ctx)
	if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x"})); err != nil {
		t.Fatal(err)
	}
	lease, err := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "hello"), Max: 1, LeaseSeconds: 30}))
	if err != nil || len(lease.Msg.Tasks) != 1 {
		t.Fatalf("lease: %v %v", lease, err)
	}
	task := lease.Msg.Tasks[0]

	for i := 0; i < 3; i++ {
		if _, err := b.ReportProgress(ctx, connect.NewRequest(&agenv1.NestReportProgressRequest{NestId: idB, TaskId: task.Id,
			Progress: &agenv1.TaskProgress{Step: "reading", Done: false}})); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: task.Id}))
	if err != nil || got.Msg.Task.Progress == nil || got.Msg.Task.Progress.Step != "reading" {
		t.Fatalf("task progress: %v %v", got, err)
	}
	events, err := c.ListEvents(ctx, connect.NewRequest(&agenv1.ListEventsRequest{Namespace: "default", Types: []string{"task.progress"}}))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range events.Msg.Events {
		if ev.TaskId == task.Id {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("got %d task.progress events for 3 calls in the same 10s bucket, want 1", n)
	}

	// Cancel the task, then report again: ignored, no error, no change.
	if _, err := c.CancelTask(ctx, connect.NewRequest(&agenv1.CancelTaskRequest{Id: task.Id})); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReportProgress(ctx, connect.NewRequest(&agenv1.NestReportProgressRequest{NestId: idB, TaskId: task.Id,
		Progress: &agenv1.TaskProgress{Step: "too late", Done: true}})); err != nil {
		t.Fatal(err)
	}
	got, err = c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: task.Id}))
	if err != nil || got.Msg.Task.Progress.Step != "reading" {
		t.Fatalf("progress after task ended: %v %v, want unchanged", got, err)
	}
}

// QueryTask's activity, derived from the task's own open span, covers the
// "model" and "tool" cases GetActivity does (a simplified cut: no
// fleet-wide warnings, no "agent" case for a task waiting on a delegate).
// features: tasks.progress
func TestQueryTaskReportsActivityFromTheOpenSpan(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	task, err := e.store.SubmitTask(ctx, store.Task{Namespace: "default", Deployment: "hello", Input: "x"})
	if err != nil {
		t.Fatal(err)
	}
	now := store.NowMs()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.store.DB().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ('s1','x','default','hello','{}',$1,$1)", now)
	exec("INSERT INTO conversations (id, session_id, created_ms) VALUES ('c1','s1',$1)", now)
	exec("INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, trace_id, started_ms, task_id, step) "+
		"VALUES ('r1','s1','c1','default','hello','running','x','t1',$1,$2,1)", now, task.ID)
	exec("UPDATE tasks SET run_id = 'r1', state = 'running' WHERE id = $1", task.ID)
	exec("INSERT INTO spans (span_id, trace_id, run_id, name, start_ms, end_ms, status, attributes, seq) "+
		"VALUES ('sp1', 't1', 'r1', 'agen.tool', $1, $1, 'unfinished', $2, 0)", now-5_000, `{"gen_ai.tool.name":"web.fetch"}`)

	got, err := c.QueryTask(ctx, connect.NewRequest(&agenv1.QueryTaskRequest{Id: task.ID}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg.Activity != "tool" || got.Msg.Detail != "web.fetch" {
		t.Fatalf("activity=%q detail=%q, want tool/web.fetch: %v", got.Msg.Activity, got.Msg.Detail, got)
	}
}
