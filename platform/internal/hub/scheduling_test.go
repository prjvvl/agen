package hub

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// SubmitTask validates not_after (bounded ahead, after not_before, not in
// the past) and resubmitting a schedule_key cancels the queued task that
// already has it.
// features: tasks.schedule
func TestSubmitTaskNotAfterAndScheduleKey(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x",
		NotAfter: timestamppb.New(time.Now().Add(-time.Minute))})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("not_after in the past accepted: %v", err)
	}
	if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x",
		NotBefore: timestamppb.New(time.Now().Add(time.Hour)), NotAfter: timestamppb.New(time.Now().Add(time.Minute))})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("not_after before not_before accepted: %v", err)
	}
	sub, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x",
		NotAfter: timestamppb.New(time.Now().Add(time.Hour))}))
	if err != nil || sub.Msg.Task.NotAfter == nil {
		t.Fatalf("not_after on the created task: %v %v", sub, err)
	}

	first, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "a", ScheduleKey: "nightly"}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "b", ScheduleKey: "nightly"}))
	if err != nil || second.Msg.Task.ScheduleKey != "nightly" {
		t.Fatalf("second: %v %v", second, err)
	}
	got, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: first.Msg.Task.Id}))
	if err != nil || got.Msg.Task.State != agenv1.TaskState_TASK_STATE_CANCELLED {
		t.Fatalf("the earlier task with the same key must be cancelled: %v %v", got, err)
	}
}

// A queued task past not_after is expired by the scheduler, with no task
// left running or failing from a budget/capacity issue.
// features: tasks.schedule
func TestSchedulerExpiresOverdueTasks(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	// There is no Nest, so the task would otherwise wait for capacity
	// forever; not_after must still end it.
	sub, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.DB().ExecContext(ctx, "UPDATE tasks SET not_after_ms = $1 WHERE id = $2", store.NowMs()-1000, sub.Msg.Task.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.scheduler().Step(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: sub.Msg.Task.Id}))
	if err != nil || got.Msg.Task.State != agenv1.TaskState_TASK_STATE_FAILED || got.Msg.Task.Outcome.GetReason() != "error" {
		t.Fatalf("expired task: %v %v", got, err)
	}
}

// A task waiting specifically on a hold (wait_for "resume") past its
// held_until deadline is woken by the scheduler, the same way an overdue
// task is expired: nothing else would notice a waiting task's deadline.
// features: tasks.hold
func TestSchedulerWakesExpiredHolds(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	idA, a := e.enroll(t, "nest-a", 4, nil)
	c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "hello"), Desired: 1}))
	e.scheduler().Step(ctx)
	sub, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	leased, err := a.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idA, Ref: ref("", "hello"), Max: 1, LeaseSeconds: 30}))
	if err != nil || len(leased.Msg.Tasks) != 1 {
		t.Fatalf("lease: %v %v", leased, err)
	}
	if _, err := c.PauseTask(ctx, connect.NewRequest(&agenv1.PauseTaskRequest{Id: sub.Msg.Task.Id})); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.DB().ExecContext(ctx,
		"UPDATE tasks SET state = 'waiting', lease_id = '', lease_nest_id = '', lease_expires_ms = 0, wait_for = 'resume', held_until_ms = $1 WHERE id = $2",
		store.NowMs()-1000, sub.Msg.Task.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.scheduler().Step(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: sub.Msg.Task.Id}))
	if err != nil || got.Msg.Task.State != agenv1.TaskState_TASK_STATE_QUEUED || got.Msg.Task.Held || got.Msg.Task.WaitFor != "" {
		t.Fatalf("woken task: %v %v, want queued, not held, no waitFor", got, err)
	}
}
