package hub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
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
