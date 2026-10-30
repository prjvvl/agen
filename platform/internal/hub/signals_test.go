package hub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// QueryChildTask (query_agent's RPC) and SignalChildTask (signal_agent's)
// both work only on the caller's own descendants, walking the parent_task_id
// chain; a task outside that chain is refused.
func TestQueryAndSignalChildTaskOnlyWorkOnDescendants(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	idB, b := e.enroll(t, "nest-b", 4, nil)
	for _, name := range []string{"hello", "worker"} {
		args := &agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)}
		if name != "hello" {
			args.Name = name
		}
		if _, err := c.CreateDeployment(ctx, connect.NewRequest(args)); err != nil {
			t.Fatal(err)
		}
		c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", name), Desired: 1}))
	}
	e.scheduler().Step(ctx)
	if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "root"})); err != nil {
		t.Fatal(err)
	}
	// A second, unrelated root task.
	if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "unrelated"})); err != nil {
		t.Fatal(err)
	}
	leased, err := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "hello"), Max: 2, LeaseSeconds: 30}))
	if err != nil || len(leased.Msg.Tasks) != 2 {
		t.Fatalf("lease: %v %v", leased, err)
	}
	var root, unrelated *agenv1.Task
	for _, tk := range leased.Msg.Tasks {
		if tk.Input == "root" {
			root = tk
		} else {
			unrelated = tk
		}
	}
	if root == nil || unrelated == nil {
		t.Fatalf("missing a task: %v", leased.Msg.Tasks)
	}
	child, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "worker"), Input: "child",
		ParentTaskId: root.Id}))
	if err != nil {
		t.Fatal(err)
	}

	// query_agent's RPC: allowed on a descendant, denied on the unrelated task.
	q, err := b.QueryChildTask(ctx, connect.NewRequest(&agenv1.NestQueryChildTaskRequest{NestId: idB, CallerTaskId: root.Id, TaskId: child.Msg.Task.Id}))
	if err != nil || q.Msg.Id != child.Msg.Task.Id || q.Msg.State != agenv1.TaskState_TASK_STATE_QUEUED {
		t.Fatalf("query descendant: %v %v", q, err)
	}
	if _, err := b.QueryChildTask(ctx, connect.NewRequest(&agenv1.NestQueryChildTaskRequest{NestId: idB, CallerTaskId: root.Id, TaskId: unrelated.Id})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("query non-descendant should be denied: %v", err)
	}

	// signal_agent's RPC: allowed on a descendant, denied on the unrelated task.
	if _, err := b.SignalChildTask(ctx, connect.NewRequest(&agenv1.NestSignalChildTaskRequest{NestId: idB, CallerTaskId: root.Id, TaskId: child.Msg.Task.Id, Message: "hurry up"})); err != nil {
		t.Fatalf("signal descendant: %v", err)
	}
	if _, err := b.SignalChildTask(ctx, connect.NewRequest(&agenv1.NestSignalChildTaskRequest{NestId: idB, CallerTaskId: root.Id, TaskId: unrelated.Id, Message: "x"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("signal non-descendant should be denied: %v", err)
	}

	events, err := c.ListEvents(ctx, connect.NewRequest(&agenv1.ListEventsRequest{Namespace: "default", Types: []string{"task.signalled"}}))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events.Msg.Events {
		if ev.TaskId == child.Msg.Task.Id {
			found = true
		}
	}
	if !found {
		t.Fatalf("no task.signalled event for the signalled child: %v", events.Msg.Events)
	}
}

// A grandchild (two SubmitChildTask hops) is still a descendant of the root.
func TestQueryChildTaskWorksTwoHopsDown(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	idB, b := e.enroll(t, "nest-b", 4, nil)
	for _, name := range []string{"hello", "worker", "helper"} {
		args := &agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)}
		if name != "hello" {
			args.Name = name
		}
		if _, err := c.CreateDeployment(ctx, connect.NewRequest(args)); err != nil {
			t.Fatal(err)
		}
		c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", name), Desired: 1}))
	}
	e.scheduler().Step(ctx)
	if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "root"})); err != nil {
		t.Fatal(err)
	}
	rl, err := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "hello"), Max: 1, LeaseSeconds: 30}))
	if err != nil || len(rl.Msg.Tasks) != 1 {
		t.Fatalf("lease root: %v %v", rl, err)
	}
	root := rl.Msg.Tasks[0]
	child, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "worker"), Input: "child", ParentTaskId: root.Id}))
	if err != nil {
		t.Fatal(err)
	}
	cl, err := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "worker"), Max: 1, LeaseSeconds: 30}))
	if err != nil || len(cl.Msg.Tasks) != 1 {
		t.Fatalf("lease child: %v %v", cl, err)
	}
	gc, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "helper"), Input: "grandchild", ParentTaskId: cl.Msg.Tasks[0].Id}))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := b.QueryChildTask(ctx, connect.NewRequest(&agenv1.NestQueryChildTaskRequest{NestId: idB, CallerTaskId: root.Id, TaskId: gc.Msg.Task.Id})); err != nil {
		t.Fatalf("query a grandchild from the root: %v", err)
	}
	if _, err := b.QueryChildTask(ctx, connect.NewRequest(&agenv1.NestQueryChildTaskRequest{NestId: idB, CallerTaskId: child.Msg.Task.Id, TaskId: gc.Msg.Task.Id})); err != nil {
		t.Fatalf("query a child from its own parent: %v", err)
	}
}
