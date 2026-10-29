package hub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// SubmitTask validates overrides (deny/ask only, a parseable duration) and
// stores them; GetTask/ListTasks return them.
// features: tasks.run
func TestSubmitTaskValidatesAndStoresOverrides(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	// An "allow" rule would widen, not narrow: refused.
	if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x",
		Overrides: &agenv1.TaskOverrides{Permissions: []*agenv1.OverrideRule{{Tool: "x", Action: "allow"}}}})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("allow rule accepted: %v", err)
	}
	if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x",
		Overrides: &agenv1.TaskOverrides{Budget: &agenv1.RunBudget{MaxDuration: "nope"}}})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad duration accepted: %v", err)
	}
	sub, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x",
		Overrides: &agenv1.TaskOverrides{Tools: []string{"read.*"}, Budget: &agenv1.RunBudget{MaxTokens: 5000, MaxDuration: "10m"}}}))
	if err != nil {
		t.Fatal(err)
	}
	if sub.Msg.Task.Overrides == nil || sub.Msg.Task.Overrides.Tools[0] != "read.*" || sub.Msg.Task.Overrides.Budget.MaxTokens != 5000 {
		t.Fatalf("overrides on the created task: %v", sub.Msg.Task.Overrides)
	}
	got, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: sub.Msg.Task.Id}))
	if err != nil || got.Msg.Task.Overrides.GetBudget().GetMaxDuration() != "10m" {
		t.Fatalf("GetTask overrides: %v %v", got, err)
	}
	// A plain task has none.
	plain, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "y"}))
	if err != nil || plain.Msg.Task.Overrides != nil {
		t.Fatalf("plain task: %v %v", plain, err)
	}
}

// A child task created by SubmitChildTask (start_agent's path) combines its
// own overrides with its parent's: tool and skill allowlists intersect,
// and budgets take the smaller of each field. A delegate without
// "overrides: true" set is enforced by the calling engine, not here, so
// the Hub itself just combines whatever it is given.
// features: delegation.async
func TestSubmitChildTaskCombinesOverridesWithTheParent(t *testing.T) {
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
	if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "parent",
		Overrides: &agenv1.TaskOverrides{Tools: []string{"read.*", "write.*"}, Budget: &agenv1.RunBudget{MaxTokens: 10_000}}})); err != nil {
		t.Fatal(err)
	}
	pl, err := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "hello"), Max: 1, LeaseSeconds: 30}))
	if err != nil || len(pl.Msg.Tasks) != 1 {
		t.Fatalf("lease parent: %v %v", pl, err)
	}
	parent := pl.Msg.Tasks[0]
	ch, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "worker"), Input: "child",
		ParentTaskId: parent.Id,
		Overrides:    &agenv1.TaskOverrides{Tools: []string{"write.*", "exec.*"}, Budget: &agenv1.RunBudget{MaxTokens: 2_000}}}))
	if err != nil {
		t.Fatal(err)
	}
	ov := ch.Msg.Task.Overrides
	if ov == nil || len(ov.Tools) != 1 || ov.Tools[0] != "write.*" {
		t.Fatalf("tools must be the intersection: %v", ov)
	}
	if ov.Budget.MaxTokens != 2_000 {
		t.Fatalf("budget must take the smaller value: %v", ov.Budget)
	}
	// A malformed override on SubmitChildTask is refused too.
	if _, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "worker"), Input: "x",
		ParentTaskId: parent.Id, Overrides: &agenv1.TaskOverrides{Permissions: []*agenv1.OverrideRule{{Tool: "x", Action: "allow"}}}})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("allow rule accepted: %v", err)
	}
}
