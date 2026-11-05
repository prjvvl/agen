package hub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// CancelTask refuses a mode other than "", "hard" or "graceful".
// features: tasks.cancel-graceful
func TestCancelTaskRejectsAnUnknownMode(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	s, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CancelTask(ctx, connect.NewRequest(&agenv1.CancelTaskRequest{Id: s.Msg.Task.Id, Mode: "whenever"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("unknown mode accepted: %v", err)
	}
}

// CancelTask(tree=true, mode="graceful") writes a "cancel" run_inbox entry
// to every leased/running task of the tree, and (with no live engine to
// drain it in this test) hard-stops all of them once the short grace
// period passes, same as CancelTask(tree=true) without a mode.
// features: tasks.cancel-graceful
func TestGracefulCancelTreeStopsSurvivorsAfterItsGracePeriod(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	idB, b := e.enroll(t, "nest-b", 4, nil)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "worker", BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "hello"), Desired: 1}))
	c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "worker"), Desired: 1}))
	e.scheduler().Step(ctx)
	if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "root"})); err != nil {
		t.Fatal(err)
	}
	rl, err := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "hello"), Max: 1, LeaseSeconds: 30}))
	if err != nil || len(rl.Msg.Tasks) != 1 {
		t.Fatalf("lease root: %v %v", rl, err)
	}
	root := rl.Msg.Tasks[0]
	if _, err := b.ExtendLease(ctx, connect.NewRequest(&agenv1.ExtendLeaseRequest{NestId: idB, TaskId: root.Id, LeaseId: root.LeaseId, LeaseSeconds: 30, InstanceId: "i1"})); err != nil {
		t.Fatal(err)
	}
	ch, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "worker"), Input: "child",
		ParentTaskId: root.Id}))
	if err != nil {
		t.Fatal(err)
	}
	cl, err := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "worker"), Max: 1, LeaseSeconds: 30}))
	if err != nil || len(cl.Msg.Tasks) != 1 {
		t.Fatalf("lease child: %v %v", cl, err)
	}
	if _, err := b.ExtendLease(ctx, connect.NewRequest(&agenv1.ExtendLeaseRequest{NestId: idB, TaskId: cl.Msg.Tasks[0].Id, LeaseId: cl.Msg.Tasks[0].LeaseId, LeaseSeconds: 30, InstanceId: "i2"})); err != nil {
		t.Fatal(err)
	}

	r, err := c.CancelTask(ctx, connect.NewRequest(&agenv1.CancelTaskRequest{Id: ch.Msg.Task.Id, Tree: true, Mode: "graceful", GracePeriodSeconds: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Msg.Cancelled) != 2 {
		t.Fatalf("cancelled %d tasks, want 2 (root+child): %v", len(r.Msg.Cancelled), r.Msg.Cancelled)
	}
	if r.Msg.Task.State != agenv1.TaskState_TASK_STATE_CANCELLED {
		t.Fatalf("named task (the child) state=%v, want cancelled once its grace period passed", r.Msg.Task.State)
	}
	gotRoot, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: root.Id}))
	if err != nil || gotRoot.Msg.Task.State != agenv1.TaskState_TASK_STATE_CANCELLED {
		t.Fatalf("root state: %v %v, want cancelled too", gotRoot, err)
	}
}
