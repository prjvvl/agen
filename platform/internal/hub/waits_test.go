package hub

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// A queued task says why it has not started.
// features: tasks.run
func TestQueuedTasksSayWhyTheyWait(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	submit := func(req *agenv1.SubmitTaskRequest) string {
		t.Helper()
		req.Ref = ref("", "hello")
		r, err := c.SubmitTask(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg.Task.Id
	}
	reason := func(id string) string {
		t.Helper()
		r, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: id}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg.Task.WaitReason
	}
	first := submit(&agenv1.SubmitTaskRequest{Input: "a", ConversationKey: "c1"})
	if got := reason(first); got != "asleep" {
		t.Fatalf("asleep deployment: %q", got)
	}
	later := submit(&agenv1.SubmitTaskRequest{Input: "b", NotBefore: timestamppb.New(time.Now().Add(time.Hour))})
	if got := reason(later); got != "not_before" {
		t.Fatalf("scheduled: %q", got)
	}
	second := submit(&agenv1.SubmitTaskRequest{Input: "c", ConversationKey: "c1"})
	if got := reason(second); got != "conversation_busy" {
		t.Fatalf("behind its conversation: %q", got)
	}
	// Wanted but nowhere to run (no Nest).
	if _, err := e.store.UpdateDeployment(ctx, "default", "hello", func(d *store.Deployment) error { d.Desired = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := reason(first); got != "capacity" {
		t.Fatalf("no nest: %q", got)
	}
	// ListTasks says the same.
	l, err := c.ListTasks(ctx, connect.NewRequest(&agenv1.ListTasksRequest{Deployment: "hello"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range l.Msg.Tasks {
		want := map[string]string{first: "capacity", later: "not_before", second: "conversation_busy"}[tk.Id]
		if tk.WaitReason != want {
			t.Fatalf("list: %s says %q, want %q", tk.Id, tk.WaitReason, want)
		}
	}
	// The deployment as a whole comes first.
	if _, err := c.PauseDeployment(ctx, connect.NewRequest(&agenv1.PauseDeploymentRequest{Ref: ref("", "hello"), Paused: true})); err != nil {
		t.Fatal(err)
	}
	if got := reason(second); got != "paused" {
		t.Fatalf("paused: %q", got)
	}
	if _, err := c.PauseDeployment(ctx, connect.NewRequest(&agenv1.PauseDeploymentRequest{Ref: ref("", "hello"), Paused: false})); err != nil {
		t.Fatal(err)
	}
	now := store.NowMs()
	for _, q := range []string{
		"INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ('s1','x','default','hello','{}',$1,$1)",
		"INSERT INTO conversations (id, session_id, created_ms) VALUES ('c1','s1',$1)",
		"INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, trace_id, started_ms, ended_ms, cost_usd) VALUES ('r1','s1','c1','default','hello','succeeded','x','t1',$1,$1,1.0)",
	} {
		if _, err := e.store.DB().ExecContext(ctx, q, now); err != nil {
			t.Fatal(err)
		}
	}
	if got := reason(first); got != "budget" {
		t.Fatalf("daily budget used up: %q", got)
	}
	// Terminal tasks have no reason.
	if _, err := c.CancelTask(ctx, connect.NewRequest(&agenv1.CancelTaskRequest{Id: first})); err != nil {
		t.Fatal(err)
	}
	if got := reason(first); got != "" {
		t.Fatalf("cancelled: %q", got)
	}
}
