package hub

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// Daily budget: once today's spend reaches max_usd_per_day, nothing new
// starts — the autoscaler holds 0 despite queued work, no leases are handed
// out, wake-ups are refused — and the next UTC day resumes.
func TestDailyBudgetStopsNewWorkUntilTomorrow(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	defer func(old func() int64) { store.NowMs = old }(store.NowMs)
	f := helloFiles(t)
	f["x-agen/config.json"] = []byte(`{"kind":"pool","scale":{"min":0,"max":3},"budget":{"maxUsdPerDay":1.0}}`)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: f})); err != nil {
		t.Fatal(err)
	}
	idA, a := e.enroll(t, "a", 4, nil)
	sched := e.scheduler()
	now := time.Now()
	for i, q := range []string{
		"INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ('s1','hello','default','hello','{}',$1,$1)",
		"INSERT INTO conversations (id, session_id, created_ms, closed_ms) VALUES ('c1','s1',$1,$1)",
		"INSERT INTO conversations (id, session_id, created_ms, closed_ms) VALUES ('c2','s1',$1,$1)",
		"INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, started_ms, ended_ms, cost_usd) VALUES ('r1','s1','c1','default','hello','succeeded','x',$1,$1,0.6)",
	} {
		_ = i
		if _, err := e.store.DB().ExecContext(ctx, q, now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "work"}))
	sched.Step(ctx)
	d, _ := c.GetDeployment(ctx, connect.NewRequest(&agenv1.GetDeploymentRequest{Ref: ref("", "hello")}))
	if d.Msg.Deployment.Desired != 1 || d.Msg.Deployment.BudgetExhausted || d.Msg.Deployment.SpentUsdToday != 0.6 {
		t.Fatalf("under budget: %v", d.Msg.Deployment)
	}
	// A second run takes today's spend to $1.20 (over $1.00).
	if _, err := e.store.DB().ExecContext(ctx, "INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, started_ms, ended_ms, cost_usd) VALUES ('r2','s1','c2','default','hello','succeeded','x',$1,$1,0.6)", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	sched.Step(ctx)
	d, _ = c.GetDeployment(ctx, connect.NewRequest(&agenv1.GetDeploymentRequest{Ref: ref("", "hello")}))
	if d.Msg.Deployment.Desired != 0 || !d.Msg.Deployment.BudgetExhausted {
		t.Fatalf("over budget: %v", d.Msg.Deployment)
	}
	l, err := a.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idA, Ref: ref("", "hello"), Max: 5}))
	if err == nil && len(l.Msg.Tasks) != 0 {
		t.Fatalf("leased work over budget: %v", l.Msg.Tasks)
	}
	if _, err := c.RequestWake(ctx, connect.NewRequest(&agenv1.RequestWakeRequest{Ref: ref("", "hello")})); code(err) != connect.CodeResourceExhausted {
		t.Fatalf("wake over budget: %v", err)
	}
	if _, err := c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "hello"), Desired: 2})); code(err) != connect.CodeResourceExhausted {
		t.Fatalf("manual scale-up over budget: %v", err)
	}
	q, _ := c.ListTasks(ctx, connect.NewRequest(&agenv1.ListTasksRequest{Deployment: "hello"}))
	if q.Msg.Tasks[0].State != agenv1.TaskState_TASK_STATE_QUEUED {
		t.Fatalf("queued work must wait: %v", q.Msg.Tasks[0])
	}
	// Next UTC day: yesterday's spend no longer counts; the queued task
	// brings the deployment back.
	tomorrow := now.UTC().Truncate(24 * time.Hour).Add(24*time.Hour + time.Minute)
	store.NowMs = func() int64 { return tomorrow.UnixMilli() }
	sched.Step(ctx)
	d, _ = c.GetDeployment(ctx, connect.NewRequest(&agenv1.GetDeploymentRequest{Ref: ref("", "hello")}))
	if d.Msg.Deployment.Desired != 1 || d.Msg.Deployment.BudgetExhausted || d.Msg.Deployment.SpentUsdToday != 0 {
		t.Fatalf("next day: %v", d.Msg.Deployment)
	}
}
