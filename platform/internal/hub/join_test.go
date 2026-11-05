package hub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// UnfinishedChildren counts a task's own unfinished joined (non-detached)
// children; completing one brings it back down.
// features: tasks.progress
func TestUnfinishedChildrenCountsDownAsChildrenFinish(t *testing.T) {
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
	rl, err := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "hello"), Max: 1, LeaseSeconds: 30}))
	if err != nil || len(rl.Msg.Tasks) != 1 {
		t.Fatalf("lease root: %v %v", rl, err)
	}
	root := rl.Msg.Tasks[0]

	zero, err := b.UnfinishedChildren(ctx, connect.NewRequest(&agenv1.NestUnfinishedChildrenRequest{NestId: idB, TaskId: root.Id}))
	if err != nil || zero.Msg.Count != 0 {
		t.Fatalf("before starting any child: %v %v", zero, err)
	}

	// A joined child (no replyConversationKey) counts.
	child, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "worker"), Input: "child",
		ParentTaskId: root.Id}))
	if err != nil {
		t.Fatal(err)
	}
	one, err := b.UnfinishedChildren(ctx, connect.NewRequest(&agenv1.NestUnfinishedChildrenRequest{NestId: idB, TaskId: root.Id}))
	if err != nil || one.Msg.Count != 1 {
		t.Fatalf("with one joined child running: %v %v", one, err)
	}

	// An async (detach: true) child never counts.
	if _, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "worker"), Input: "detached",
		ParentTaskId: root.Id, ReplyConversationKey: "async:s1"})); err != nil {
		t.Fatal(err)
	}
	stillOne, err := b.UnfinishedChildren(ctx, connect.NewRequest(&agenv1.NestUnfinishedChildrenRequest{NestId: idB, TaskId: root.Id}))
	if err != nil || stillOne.Msg.Count != 1 {
		t.Fatalf("an async child must not count: %v %v", stillOne, err)
	}

	cl, err := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "worker"), Max: 2, LeaseSeconds: 30}))
	if err != nil || len(cl.Msg.Tasks) != 2 {
		t.Fatalf("lease children: %v %v", cl, err)
	}
	for _, tk := range cl.Msg.Tasks {
		if tk.Id == child.Msg.Task.Id {
			if _, err := b.CompleteTask(ctx, connect.NewRequest(&agenv1.CompleteTaskRequest{NestId: idB, TaskId: tk.Id, LeaseId: tk.LeaseId, Success: true, Output: "done"})); err != nil {
				t.Fatal(err)
			}
		}
	}
	backToZero, err := b.UnfinishedChildren(ctx, connect.NewRequest(&agenv1.NestUnfinishedChildrenRequest{NestId: idB, TaskId: root.Id}))
	if err != nil || backToZero.Msg.Count != 0 {
		t.Fatalf("after the joined child finished: %v %v", backToZero, err)
	}
}
