package hub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// SubmitTask and SubmitChildTask validate on_parent_end (3.4, part): the
// parent-close policy for a task's own children.
// features: tasks.run
func TestSubmitTaskValidatesOnParentEnd(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x", OnParentEnd: "whenever"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid on_parent_end accepted: %v", err)
	}
	got, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x", OnParentEnd: "abandon"}))
	if err != nil || got.Msg.Task.OnParentEnd != "abandon" {
		t.Fatalf("submit with on_parent_end=abandon: %v %v", got, err)
	}
	// Unset keeps the cancel default (stored and reported as "").
	plain, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "y"}))
	if err != nil || plain.Msg.Task.OnParentEnd != "" {
		t.Fatalf("submit with unset on_parent_end: %v %v", plain, err)
	}
}

// SubmitChildTask carries on_parent_end through to the child it creates and
// rejects a bad value, same as SubmitTask.
// features: tasks.run
func TestSubmitChildTaskCarriesOnParentEnd(t *testing.T) {
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
	if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "parent"})); err != nil {
		t.Fatal(err)
	}
	pl, err := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "hello"), Max: 1, LeaseSeconds: 30}))
	if err != nil || len(pl.Msg.Tasks) != 1 {
		t.Fatalf("lease parent: %v %v", pl, err)
	}
	parent := pl.Msg.Tasks[0]
	if _, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "worker"), Input: "child",
		ParentTaskId: parent.Id, OnParentEnd: "whenever"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid on_parent_end accepted: %v", err)
	}
	ch, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "worker"), Input: "child",
		ParentTaskId: parent.Id, OnParentEnd: "abandon"}))
	if err != nil || ch.Msg.Task.OnParentEnd != "abandon" {
		t.Fatalf("child on_parent_end: %v %v", ch, err)
	}
}
