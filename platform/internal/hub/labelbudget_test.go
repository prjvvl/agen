package hub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// CreateDeployment validates budget.labelUsdPerDay: each key must be
// "key=value" with a valid label key, same as a real task label.
// features: tasks.run
func TestCreateDeploymentValidatesLabelUsdPerDay(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t),
		Budget: &agenv1.Budget{LabelUsdPerDay: map[string]float64{"no-equals-sign": 5}}})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("a label rule with no '=' was accepted: %v", err)
	}
	dep, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t),
		Budget: &agenv1.Budget{LabelUsdPerDay: map[string]float64{"team=apollo": 5}}}))
	if err != nil || dep.Msg.Deployment.Budget.LabelUsdPerDay["team=apollo"] != 5 {
		t.Fatalf("create with a valid labelUsdPerDay rule: %v %v", dep, err)
	}
}

// A task carrying an exhausted label_usd_per_day label is excluded from
// LeaseTasks (released right back, not hard-skipped up front), while a
// sibling task of the SAME deployment without that label leases
// normally — unlike max_usd_per_day, an exhausted label does not hold
// the rest of the deployment's queue back (3.5, part: label budgets).
// features: tasks.run
func TestLeaseTasksExcludesACandidateWithAnExhaustedLabelBudget(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	idA, a := e.enroll(t, "nest-a", 4, nil)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t),
		Budget: &agenv1.Budget{LabelUsdPerDay: map[string]float64{"team=apollo": 1}}})); err != nil {
		t.Fatal(err)
	}
	c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "hello"), Desired: 1}))
	e.scheduler().Step(ctx)

	over, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "over-budget",
		Labels: map[string]string{"team": "apollo"}}))
	if err != nil {
		t.Fatal(err)
	}
	under, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "under-budget",
		Labels: map[string]string{"team": "other"}}))
	if err != nil {
		t.Fatal(err)
	}
	// team=apollo has already spent its $1/day limit.
	insertRun(t, e, over.Msg.Task.Id, 100, 0, 1.5)

	leased, err := a.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idA, Ref: ref("", "hello"), Max: 10, LeaseSeconds: 30}))
	if err != nil {
		t.Fatal(err)
	}
	if len(leased.Msg.Tasks) != 1 || leased.Msg.Tasks[0].Id != under.Msg.Task.Id {
		t.Fatalf("leased %v, want exactly the under-budget task %s", leased.Msg.Tasks, under.Msg.Task.Id)
	}

	gotOver, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: over.Msg.Task.Id}))
	if err != nil || gotOver.Msg.Task.State != agenv1.TaskState_TASK_STATE_QUEUED || gotOver.Msg.Task.WaitReason != "label_budget" {
		t.Fatalf("over-budget task: %v %v, want still queued with wait_reason=label_budget", gotOver, err)
	}
}

// labelBudgetThresholds fires budget.threshold events (scope "label") at
// 50/80/100% of a configured label_usd_per_day rule, only for a task that
// actually carries the matching label.
// features: budgets
func TestLabelBudgetThresholdsFireForAMatchingLabelOnly(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t),
		Budget: &agenv1.Budget{LabelUsdPerDay: map[string]float64{"team=apollo": 1}}})); err != nil {
		t.Fatal(err)
	}
	apollo, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x", Labels: map[string]string{"team": "apollo"}}))
	if err != nil {
		t.Fatal(err)
	}
	other, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x", Labels: map[string]string{"team": "other"}}))
	if err != nil {
		t.Fatal(err)
	}
	insertRun(t, e, apollo.Msg.Task.Id, 0, 0, 0.6) // 60% of team=apollo's $1 limit.

	d, err := e.store.GetDeployment(ctx, "default", "hello")
	if err != nil {
		t.Fatal(err)
	}
	apolloTask, _ := e.store.GetTask(ctx, apollo.Msg.Task.Id)
	otherTask, _ := e.store.GetTask(ctx, other.Msg.Task.Id)
	e.hub.labelBudgetThresholds(ctx, d, apolloTask)
	e.hub.labelBudgetThresholds(ctx, d, otherTask) // doesn't carry the label: no event.

	evs, _, err := e.store.ListEvents(ctx, store.EventFilter{Namespaces: []string{"default"}, Types: []string{"budget.threshold"}})
	if err != nil {
		t.Fatal(err)
	}
	var labelEvs []store.Event
	for _, ev := range evs {
		if ev.Data["scope"] == "label" {
			labelEvs = append(labelEvs, ev)
		}
	}
	if len(labelEvs) != 1 || labelEvs[0].Data["label"] != "team=apollo" || labelEvs[0].Data["percent"].(float64) != 50 {
		t.Fatalf("label threshold events: %+v, want exactly one, team=apollo at 50%%", labelEvs)
	}
}

// GetMetrics rolls up today's spend against every configured
// label_usd_per_day rule (3.5, part), regardless of the call's own
// window — a label budget resets daily, not by buckets.
// features: observe.metrics
func TestGetMetricsRollsUpLabelBudgets(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t),
		Budget: &agenv1.Budget{LabelUsdPerDay: map[string]float64{"team=apollo": 1}}})); err != nil {
		t.Fatal(err)
	}
	apollo, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x", Labels: map[string]string{"team": "apollo"}}))
	if err != nil {
		t.Fatal(err)
	}
	insertRun(t, e, apollo.Msg.Task.Id, 0, 0, 0.4)

	m, err := c.GetMetrics(ctx, connect.NewRequest(&agenv1.GetMetricsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Msg.Deployments) != 1 {
		t.Fatalf("deployments: %v", m.Msg.Deployments)
	}
	lb := m.Msg.Deployments[0].LabelBudgets
	if len(lb) != 1 || lb[0].Label != "team=apollo" || lb[0].LimitUsd != 1 || lb[0].SpentUsd < 0.399 || lb[0].SpentUsd > 0.401 {
		t.Fatalf("label budgets: %+v", lb)
	}
}
