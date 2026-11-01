package hub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// insertRun records one run with the given usage for taskID (3.5, part:
// tree budgets need real usage rows to narrow against), with its own
// fresh session and conversation.
func insertRun(t *testing.T, e env, taskID string, inputTokens, outputTokens int64, costUSD float64) {
	t.Helper()
	ctx := context.Background()
	db := e.store.DB()
	sid, cid, rid, now := store.NewID(), store.NewID(), store.NewID(), store.NowMs()
	if _, err := db.ExecContext(ctx, "INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ($1,'a','default','hello','{}',$2,$2)", sid, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO conversations (id, session_id, created_ms) VALUES ($1,$2,$3)", cid, sid, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, trace_id, started_ms, ended_ms, task_id, input_tokens, output_tokens, cost_usd) "+
			"VALUES ($1,$2,$3,'default','hello','succeeded','hi','t',$4,$4,$5,$6,$7,$8)",
		rid, sid, cid, now, taskID, inputTokens, outputTokens, costUSD); err != nil {
		t.Fatal(err)
	}
}

// A child task's effective budget narrows to the tree's remaining budget
// (3.5, part): the root's own overrides.budget is the tree budget; a
// child's effective budget is the smaller of its own request and that
// remainder (the root's limit minus the tree's usage so far). With no
// root budget, nothing narrows.
// features: tasks.run
func TestSubmitChildTaskNarrowsToTheTreesRemainingBudget(t *testing.T) {
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

	root, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "root",
		Overrides: &agenv1.TaskOverrides{Budget: &agenv1.RunBudget{MaxTokens: 1000}}}))
	if err != nil {
		t.Fatal(err)
	}
	rl, err := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "hello"), Max: 1, LeaseSeconds: 30}))
	if err != nil || len(rl.Msg.Tasks) != 1 {
		t.Fatalf("lease root: %v %v", rl, err)
	}

	// No usage yet: the first child gets the whole tree budget.
	ch1, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "worker"), Input: "c1", ParentTaskId: root.Msg.Task.Id}))
	if err != nil {
		t.Fatal(err)
	}
	if ch1.Msg.Task.Overrides.GetBudget().GetMaxTokens() != 1000 {
		t.Fatalf("first child budget=%+v, want maxTokens=1000", ch1.Msg.Task.Overrides)
	}

	// The tree has now spent 400: the next child's remainder is 600.
	insertRun(t, e, root.Msg.Task.Id, 300, 100, 0)
	ch2, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "worker"), Input: "c2", ParentTaskId: root.Msg.Task.Id}))
	if err != nil {
		t.Fatal(err)
	}
	if ch2.Msg.Task.Overrides.GetBudget().GetMaxTokens() != 600 {
		t.Fatalf("second child budget=%+v, want maxTokens=600", ch2.Msg.Task.Overrides)
	}

	// A child's own smaller request still wins over a larger remainder.
	ch3, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "worker"), Input: "c3", ParentTaskId: root.Msg.Task.Id,
		Overrides: &agenv1.TaskOverrides{Budget: &agenv1.RunBudget{MaxTokens: 50}}}))
	if err != nil {
		t.Fatal(err)
	}
	if ch3.Msg.Task.Overrides.GetBudget().GetMaxTokens() != 50 {
		t.Fatalf("third child budget=%+v, want its own smaller maxTokens=50", ch3.Msg.Task.Overrides)
	}

	// The tree has now spent the rest (600 more, 1000 total): the
	// remainder floors at 1, it does not read as "unlimited".
	insertRun(t, e, ch1.Msg.Task.Id, 600, 0, 0)
	ch4, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "worker"), Input: "c4", ParentTaskId: root.Msg.Task.Id}))
	if err != nil {
		t.Fatal(err)
	}
	if ch4.Msg.Task.Overrides.GetBudget().GetMaxTokens() != 1 {
		t.Fatalf("fourth child budget=%+v, want an exhausted remainder floored at 1", ch4.Msg.Task.Overrides)
	}
}

// treeBudgetThresholds fires budget.threshold events (scope "tree") at
// 50/80/100% of the root's own overrides.budget, once each for the
// tree's whole lifetime (not re-fired by a later call at the same spend).
// features: budgets
func TestTreeBudgetThresholdsFireOncePerCrossing(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	root, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "root",
		Overrides: &agenv1.TaskOverrides{Budget: &agenv1.RunBudget{MaxUsd: 0.10}}}))
	if err != nil {
		t.Fatal(err)
	}
	insertRun(t, e, root.Msg.Task.Id, 0, 0, 0.06) // 60%: crosses 50.
	task, err := e.store.GetTask(ctx, root.Msg.Task.Id)
	if err != nil {
		t.Fatal(err)
	}
	e.hub.treeBudgetThresholds(ctx, task)
	e.hub.treeBudgetThresholds(ctx, task) // same spend again: no duplicate.

	evs, _, err := e.store.ListEvents(ctx, store.EventFilter{Namespaces: []string{"default"}, Types: []string{"budget.threshold"}})
	if err != nil {
		t.Fatal(err)
	}
	var tree []store.Event
	for _, ev := range evs {
		if ev.Data["scope"] == "tree" {
			tree = append(tree, ev)
		}
	}
	if len(tree) != 1 || tree[0].Data["percent"].(float64) != 50 {
		t.Fatalf("tree threshold events: %+v, want exactly one at 50%%", tree)
	}

	child, err := e.store.SubmitTask(ctx, store.Task{Namespace: "default", Deployment: "hello", Input: "child", ParentTaskID: root.Msg.Task.Id, RootTaskID: root.Msg.Task.Id})
	if err != nil {
		t.Fatal(err)
	}
	insertRun(t, e, child.ID, 0, 0, 0.02) // 80% of the tree total: crosses 80 too.
	e.hub.treeBudgetThresholds(ctx, task)
	evs, _, err = e.store.ListEvents(ctx, store.EventFilter{Namespaces: []string{"default"}, Types: []string{"budget.threshold"}})
	if err != nil {
		t.Fatal(err)
	}
	tree = nil
	for _, ev := range evs {
		if ev.Data["scope"] == "tree" {
			tree = append(tree, ev)
		}
	}
	if len(tree) != 2 {
		t.Fatalf("after crossing 80%%: %+v, want 50 and 80, not a re-fired 50", tree)
	}
}

// With no budget on the root at all, a child's overrides are unaffected:
// nothing narrows.
// features: tasks.run
func TestSubmitChildTaskWithNoTreeBudgetNarrowsNothing(t *testing.T) {
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

	root, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "root"}))
	if err != nil {
		t.Fatal(err)
	}
	rl, err := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "hello"), Max: 1, LeaseSeconds: 30}))
	if err != nil || len(rl.Msg.Tasks) != 1 {
		t.Fatalf("lease root: %v %v", rl, err)
	}
	ch, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "worker"), Input: "c1", ParentTaskId: root.Msg.Task.Id}))
	if err != nil {
		t.Fatal(err)
	}
	if ch.Msg.Task.Overrides != nil {
		t.Fatalf("child overrides=%+v, want none (no root budget to narrow by)", ch.Msg.Task.Overrides)
	}
}
