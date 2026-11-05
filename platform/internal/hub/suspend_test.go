package hub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// GetTask exposes a suspended task's TASK_STATE_WAITING and wait_for
// (3.3, part: the Go-side durable-suspension infrastructure, not yet
// driven by the engine).
func TestGetTaskExposesWaitingState(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)

	task, err := e.store.SubmitTask(ctx, store.Task{Namespace: "default", Deployment: "hello", Input: "x"})
	if err != nil {
		t.Fatal(err)
	}
	leased, err := e.store.LeaseTasks(ctx, "n1", "default", "hello", 1, 60_000)
	if err != nil || len(leased) != 1 {
		t.Fatalf("lease: %v %v", leased, err)
	}
	if err := e.store.SuspendTask(ctx, task.ID, leased[0].LeaseID, "child:c1"); err != nil {
		t.Fatal(err)
	}

	got, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: task.ID}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg.Task.State != agenv1.TaskState_TASK_STATE_WAITING || got.Msg.Task.WaitFor != "child:c1" {
		t.Fatalf("got state=%v waitFor=%q", got.Msg.Task.State, got.Msg.Task.WaitFor)
	}

	woken, err := e.store.WriteInbox(ctx, store.InboxRow{Namespace: "default", TaskID: task.ID, RunID: "r1", Kind: "child", Ref: "c1", Payload: `{"status":"done"}`})
	if err != nil || !woken {
		t.Fatalf("write inbox: woken=%v err=%v", woken, err)
	}
	got, err = c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: task.ID}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg.Task.State != agenv1.TaskState_TASK_STATE_QUEUED || got.Msg.Task.WaitFor != "" {
		t.Fatalf("got state=%v waitFor=%q", got.Msg.Task.State, got.Msg.Task.WaitFor)
	}
}
