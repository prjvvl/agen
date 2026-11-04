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

// AcquireLock grants a fencing token for a lock named mid-run (1.5, part),
// refuses a second task while it's held, and ReleaseLock frees it for
// another task to take at a fence that keeps advancing rather than
// resetting. Neither needs a lease on the task it names (low-stakes, like
// ReportProgress/IsHeld): the engine has no lease id to send.
// features: tasks.run
func TestNestAcquireLockAndReleaseLock(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	idB, b := e.enroll(t, "nest-b", 4, nil)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	a, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "a"}))
	if err != nil {
		t.Fatal(err)
	}
	other, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "other"}))
	if err != nil {
		t.Fatal(err)
	}

	r1, err := b.AcquireLock(ctx, connect.NewRequest(&agenv1.NestAcquireLockRequest{NestId: idB, TaskId: a.Msg.Task.Id, Name: "x", TtlSeconds: 60}))
	if err != nil || r1.Msg.Fence != 1 {
		t.Fatalf("first grant: %v %v", r1, err)
	}
	if _, err := b.AcquireLock(ctx, connect.NewRequest(&agenv1.NestAcquireLockRequest{NestId: idB, TaskId: other.Msg.Task.Id, Name: "x", TtlSeconds: 60})); code(err) != connect.CodeAlreadyExists {
		t.Fatalf("a second holder should be refused: %v", err)
	}
	if _, err := b.AcquireLock(ctx, connect.NewRequest(&agenv1.NestAcquireLockRequest{NestId: idB, TaskId: a.Msg.Task.Id, Name: "x", TtlSeconds: 0})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("ttl_seconds=0 should be rejected: %v", err)
	}
	if _, err := b.ReleaseLock(ctx, connect.NewRequest(&agenv1.NestReleaseLockRequest{NestId: idB, TaskId: a.Msg.Task.Id, Name: "x"})); err != nil {
		t.Fatal(err)
	}
	r2, err := b.AcquireLock(ctx, connect.NewRequest(&agenv1.NestAcquireLockRequest{NestId: idB, TaskId: other.Msg.Task.Id, Name: "x", TtlSeconds: 60}))
	if err != nil || r2.Msg.Fence != 2 {
		t.Fatalf("fence after a release should advance to 2, not reset: %v %v", r2, err)
	}
	// Releasing a lock nobody holds (or not this task's) is a no-op.
	if _, err := b.ReleaseLock(ctx, connect.NewRequest(&agenv1.NestReleaseLockRequest{NestId: idB, TaskId: a.Msg.Task.Id, Name: "no-such-lock"})); err != nil {
		t.Fatal(err)
	}
}
