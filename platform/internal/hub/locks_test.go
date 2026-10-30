package hub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// SubmitTask validates and stores declared locks; LeaseTasks only leases a
// task once every one of its locks is free, and GetTask reports
// wait_reason "locks" for one still waiting.
// features: tasks.run
func TestSubmitTaskLocks(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	idB, b := e.enroll(t, "nest-b", 4, nil)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "hello"), Desired: 1}))
	e.scheduler().Step(ctx)
	if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x",
		Locks: make([]string, 17)})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("too many locks accepted: %v", err)
	}
	a, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "a", Locks: []string{"repo:x"}}))
	if err != nil || len(a.Msg.Task.Locks) != 1 || a.Msg.Task.Locks[0] != "repo:x" {
		t.Fatalf("submit a: %v %v", a, err)
	}
	bTask, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "b", Locks: []string{"repo:x"}}))
	if err != nil {
		t.Fatal(err)
	}

	pl, err := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "hello"), Max: 10, LeaseSeconds: 30}))
	if err != nil || len(pl.Msg.Tasks) != 1 || pl.Msg.Tasks[0].Id != a.Msg.Task.Id {
		t.Fatalf("only a should lease: %v %v", pl, err)
	}
	got, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: bTask.Msg.Task.Id}))
	if err != nil || got.Msg.Task.WaitReason != "locks" {
		t.Fatalf("b's wait reason: %v %v", got, err)
	}

	// a ends: its lock is released, so b can now be leased.
	if _, err := b.CompleteTask(ctx, connect.NewRequest(&agenv1.CompleteTaskRequest{NestId: idB, TaskId: a.Msg.Task.Id, LeaseId: pl.Msg.Tasks[0].LeaseId,
		Success: true, Output: "ok", RunId: "r1", InstanceId: "i1"})); err != nil {
		t.Fatal(err)
	}
	pl2, err := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "hello"), Max: 10, LeaseSeconds: 30}))
	if err != nil || len(pl2.Msg.Tasks) != 1 || pl2.Msg.Tasks[0].Id != bTask.Msg.Task.Id {
		t.Fatalf("b should now lease: %v %v", pl2, err)
	}
}
