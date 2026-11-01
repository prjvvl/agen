package hub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// PauseTask holds a task; ResumeTask ends the hold (3.4, part: hold).
// features: tasks.hold
func TestPauseTaskAndResumeTask(t *testing.T) {
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

	p, err := c.PauseTask(ctx, connect.NewRequest(&agenv1.PauseTaskRequest{Id: s.Msg.Task.Id, Reason: "testing"}))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Msg.Task.Held || p.Msg.Task.HoldReason != "testing" {
		t.Fatalf("got held=%v reason=%q", p.Msg.Task.Held, p.Msg.Task.HoldReason)
	}
	got, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: s.Msg.Task.Id}))
	if err != nil || !got.Msg.Task.Held {
		t.Fatalf("GetTask does not reflect the hold: %v %v", got, err)
	}

	r, err := c.ResumeTask(ctx, connect.NewRequest(&agenv1.ResumeTaskRequest{Id: s.Msg.Task.Id}))
	if err != nil {
		t.Fatal(err)
	}
	if r.Msg.Task.Held {
		t.Fatalf("still held after ResumeTask: %v", r.Msg.Task)
	}
}

// PauseDeployment(holdRuns=true) holds every unfinished task of the
// deployment, not just its own future leasing (2.8); resuming clears the
// hold and the tasks keep working normally.
// features: deploy.lifecycle
func TestPauseDeploymentWithHoldRunsHoldsEveryUnfinishedTaskThenResumeClearsIt(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	t1, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	t2, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "y"}))
	if err != nil {
		t.Fatal(err)
	}

	p, err := c.PauseDeployment(ctx, connect.NewRequest(&agenv1.PauseDeploymentRequest{Ref: ref("", "hello"), Paused: true, HoldRuns: true, Reason: "maintenance"}))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Msg.Deployment.Paused || len(p.Msg.Held) != 2 {
		t.Fatalf("got paused=%v held=%d, want paused and both tasks held", p.Msg.Deployment.Paused, len(p.Msg.Held))
	}
	for _, id := range []string{t1.Msg.Task.Id, t2.Msg.Task.Id} {
		got, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: id}))
		if err != nil || !got.Msg.Task.Held || got.Msg.Task.HoldReason != "maintenance" {
			t.Fatalf("task %s: held=%v reason=%q, want held with reason", id, got.Msg.Task.Held, got.Msg.Task.HoldReason)
		}
	}

	r, err := c.PauseDeployment(ctx, connect.NewRequest(&agenv1.PauseDeploymentRequest{Ref: ref("", "hello"), Paused: false}))
	if err != nil {
		t.Fatal(err)
	}
	if r.Msg.Deployment.Paused {
		t.Fatal("still paused after resume")
	}
	for _, id := range []string{t1.Msg.Task.Id, t2.Msg.Task.Id} {
		got, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: id}))
		if err != nil || got.Msg.Task.Held {
			t.Fatalf("task %s still held after resume: %v %v", id, got, err)
		}
	}
}

// Pausing without hold_runs only stops leasing: already-unfinished tasks
// are never touched.
// features: deploy.lifecycle
func TestPauseDeploymentWithoutHoldRunsLeavesTasksUnheld(t *testing.T) {
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
	if _, err := c.PauseDeployment(ctx, connect.NewRequest(&agenv1.PauseDeploymentRequest{Ref: ref("", "hello"), Paused: true})); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: s.Msg.Task.Id}))
	if err != nil || got.Msg.Task.Held {
		t.Fatalf("task held without hold_runs: %v %v", got, err)
	}
}

// PauseTask and ResumeTask with tree=true cover the whole tree, mirroring
// CancelTask's own tree support.
// features: tasks.hold
func TestPauseTaskWithTreeHoldsTheWholeTree(t *testing.T) {
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
	ch, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "worker"), Input: "child",
		ParentTaskId: root.Id}))
	if err != nil {
		t.Fatal(err)
	}

	p, err := c.PauseTask(ctx, connect.NewRequest(&agenv1.PauseTaskRequest{Id: ch.Msg.Task.Id, Tree: true, Reason: "tree hold"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Msg.Held) != 2 {
		t.Fatalf("held %d tasks, want 2 (root+child): %v", len(p.Msg.Held), p.Msg.Held)
	}
	if !p.Msg.Task.Held {
		t.Fatalf("named task (the child) not held: %v", p.Msg.Task)
	}
	gotRoot, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: root.Id}))
	if err != nil || !gotRoot.Msg.Task.Held {
		t.Fatalf("root held: %v %v, want true too", gotRoot, err)
	}

	r, err := c.ResumeTask(ctx, connect.NewRequest(&agenv1.ResumeTaskRequest{Id: ch.Msg.Task.Id, Tree: true}))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Msg.Resumed) != 2 {
		t.Fatalf("resumed %d tasks, want 2: %v", len(r.Msg.Resumed), r.Msg.Resumed)
	}
	gotRoot, err = c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: root.Id}))
	if err != nil || gotRoot.Msg.Task.Held {
		t.Fatalf("root still held after ResumeTree: %v %v", gotRoot, err)
	}
}
