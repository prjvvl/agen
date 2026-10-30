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

// root_task_id (3.4, part) is always set (unlike root_run_id, which stays
// empty until a run starts): a root task is its own root, and it is
// inherited all the way down a delegation chain, not just one level, so
// GetTaskTree can find the whole tree from any task in it.
// features: tasks.tree
func TestSubmitChildTaskInheritsRootTaskIDDownTheWholeChain(t *testing.T) {
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
	if root.RootTaskId != root.Id {
		t.Fatalf("root's own root_task_id=%q, want self (%q)", root.RootTaskId, root.Id)
	}

	ch, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "worker"), Input: "child",
		ParentTaskId: root.Id}))
	if err != nil || ch.Msg.Task.RootTaskId != root.Id {
		t.Fatalf("child root_task_id: %v %v, want %q", ch, err, root.Id)
	}
	cl, err := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "worker"), Max: 1, LeaseSeconds: 30}))
	if err != nil || len(cl.Msg.Tasks) != 1 {
		t.Fatalf("lease child: %v %v", cl, err)
	}
	child := cl.Msg.Tasks[0]

	gc, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "helper"), Input: "grandchild",
		ParentTaskId: child.Id}))
	if err != nil || gc.Msg.Task.RootTaskId != root.Id {
		t.Fatalf("grandchild root_task_id: %v %v, want %q (inherited past one level)", gc, err, root.Id)
	}

	tree, err := c.GetTaskTree(ctx, connect.NewRequest(&agenv1.GetTaskTreeRequest{TaskId: gc.Msg.Task.Id}))
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Msg.Tasks) != 3 {
		t.Fatalf("tree from the grandchild has %d tasks, want 3 (root+child+grandchild): %v", len(tree.Msg.Tasks), tree.Msg.Tasks)
	}
	ids := map[string]bool{}
	for _, tk := range tree.Msg.Tasks {
		ids[tk.Id] = true
	}
	if !ids[root.Id] || !ids[child.Id] || !ids[gc.Msg.Task.Id] {
		t.Fatalf("tree missing a member: %v", tree.Msg.Tasks)
	}
}
