package hub

import (
	"context"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// CreateDeployment validates conversations.queueMode and debounce, and a
// deployment in "collect" mode merges a second quick SubmitTask of the same
// conversation into the first's still-queued task.
// features: tasks.queue-modes
func TestSubmitTaskCollectsIntoTheStillQueuedTaskOfItsConversation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t),
		Conversations: &agenv1.ConversationPolicy{QueueMode: "whenever"}})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid queue_mode accepted: %v", err)
	}
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t),
		Conversations: &agenv1.ConversationPolicy{QueueMode: "collect", Debounce: "not-a-duration"}})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid debounce accepted: %v", err)
	}
	dep, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t),
		Conversations: &agenv1.ConversationPolicy{QueueMode: "collect", Debounce: "1m"}}))
	if err != nil || dep.Msg.Deployment.Conversations.QueueMode != "collect" {
		t.Fatalf("create with collect mode: %v %v", dep, err)
	}

	first, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "first", ConversationKey: "c1"}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "second", ConversationKey: "c1"}))
	if err != nil {
		t.Fatal(err)
	}
	if second.Msg.Task.Id != first.Msg.Task.Id {
		t.Fatalf("second submission got its own task %s, want merged into %s", second.Msg.Task.Id, first.Msg.Task.Id)
	}
	got, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: first.Msg.Task.Id}))
	if err != nil || got.Msg.Task.Input != "first\n\nsecond" {
		t.Fatalf("merged task: %v %v", got, err)
	}

	// A separate conversation is never merged with c1's.
	third, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "third", ConversationKey: "c2"}))
	if err != nil || third.Msg.Task.Id == first.Msg.Task.Id {
		t.Fatalf("a different conversation merged into c1's task: %v %v", third, err)
	}
}

// The default queue mode (unset, "followup") queues a conversation's
// second submission as its own task, same as before this feature existed.
// features: tasks.queue-modes
func TestSubmitTaskFollowupModeQueuesSeparately(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	first, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "first", ConversationKey: "c1"}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "second", ConversationKey: "c1"}))
	if err != nil {
		t.Fatal(err)
	}
	if second.Msg.Task.Id == first.Msg.Task.Id {
		t.Fatal("followup mode (the default) merged two submissions into one task")
	}
}

// "steer" queue mode delivers a new submission into the conversation's
// currently running task as a "steer" run_inbox entry, fire-and-forget
// like a signal: no separate task, the submission's response is the
// running task itself. With nothing running, it falls through and queues
// normally, same as followup.
// features: tasks.queue-modes
func TestSubmitTaskSteerDeliversIntoTheRunningTask(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t),
		Conversations: &agenv1.ConversationPolicy{QueueMode: "steer"}})); err != nil {
		t.Fatal(err)
	}
	idA, a := e.enroll(t, "nest-a", 4, nil)
	c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "hello"), Desired: 1}))
	e.scheduler().Step(ctx)

	first, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "first", ConversationKey: "c1"}))
	if err != nil {
		t.Fatal(err)
	}
	// Nothing running yet (still queued): steer falls through and queues
	// separately, same as followup.
	second, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "second", ConversationKey: "c1"}))
	if err != nil {
		t.Fatal(err)
	}
	if second.Msg.Task.Id == first.Msg.Task.Id {
		t.Fatal("steer merged into a queued (not running) task")
	}

	leased, err := a.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idA, Ref: ref("", "hello"), Max: 1, LeaseSeconds: 30}))
	if err != nil || len(leased.Msg.Tasks) != 1 {
		t.Fatalf("lease: %v %v", leased, err)
	}
	if _, err := a.ExtendLease(ctx, connect.NewRequest(&agenv1.ExtendLeaseRequest{NestId: idA, TaskId: leased.Msg.Tasks[0].Id, LeaseId: leased.Msg.Tasks[0].LeaseId, LeaseSeconds: 30, InstanceId: "i1"})); err != nil {
		t.Fatal(err)
	}

	third, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "third", ConversationKey: "c1"}))
	if err != nil {
		t.Fatal(err)
	}
	if third.Msg.Task.Id != leased.Msg.Tasks[0].Id {
		t.Fatalf("steer into the running task returned %s, want the running task %s", third.Msg.Task.Id, leased.Msg.Tasks[0].Id)
	}
	var kind, payload string
	if err := e.store.DB().QueryRowContext(ctx, "SELECT kind, payload FROM run_inbox WHERE task_id = $1", leased.Msg.Tasks[0].Id).Scan(&kind, &payload); err != nil {
		t.Fatalf("no run_inbox row written for the steer: %v", err)
	}
	if kind != "steer" || payload != "third" {
		t.Fatalf("got kind=%q payload=%q, want steer/third", kind, payload)
	}
}

// A bundle's own x-agen/config.json may set conversations.queueMode to any
// of the four modes the Hub handles (the schema previously only allowed
// followup and collect, even though steer and interrupt already worked).
// features: tasks.queue-modes
func TestBundleConversationsQueueModeSchemaAllowsAllFourModes(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	for i, mode := range []string{"followup", "collect", "steer", "interrupt"} {
		f := helloFiles(t)
		f["x-agen/config.json"] = []byte(fmt.Sprintf(`{"conversations":{"queueMode":%q}}`, mode))
		if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: fmt.Sprintf("hello-%d", i), BundleFiles: f})); err != nil {
			t.Fatalf("queueMode %q rejected: %v", mode, err)
		}
	}
}

// SubmitTask.delivery overrides the deployment's own queueMode for one
// submission: a "followup" deployment still collects when a submission
// asks for it, and an invalid value is rejected the same as an invalid
// deployment-level queueMode.
// features: tasks.queue-modes
func TestSubmitTaskDeliveryOverridesTheDeploymentsQueueMode(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x", Delivery: "whenever"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid delivery accepted: %v", err)
	}
	first, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "first", ConversationKey: "c1"}))
	if err != nil {
		t.Fatal(err)
	}
	// The deployment's own queueMode is unset ("followup"); this one
	// submission still collects because it says so itself.
	second, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "second", ConversationKey: "c1", Delivery: "collect"}))
	if err != nil {
		t.Fatal(err)
	}
	if second.Msg.Task.Id != first.Msg.Task.Id {
		t.Fatalf("delivery=collect did not merge: got task %s, want merged into %s", second.Msg.Task.Id, first.Msg.Task.Id)
	}
	got, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: first.Msg.Task.Id}))
	if err != nil || got.Msg.Task.Input != "first\n\nsecond" {
		t.Fatalf("merged task: %v %v", got, err)
	}
}

// A merged ("collect") submission's own idempotency key is recorded, so
// retrying it returns the task it already merged into instead of merging a
// second time.
// features: tasks.queue-modes
func TestCollectMergeIsIdempotencyKeyAware(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t),
		Conversations: &agenv1.ConversationPolicy{QueueMode: "collect"}})); err != nil {
		t.Fatal(err)
	}
	first, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "first", ConversationKey: "c1"}))
	if err != nil {
		t.Fatal(err)
	}
	merged, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "second", ConversationKey: "c1", IdempotencyKey: "k1"}))
	if err != nil || merged.Msg.Task.Id != first.Msg.Task.Id {
		t.Fatalf("first merge: %v %v", merged, err)
	}
	// Retrying the same key must not merge "second" into the task again.
	retried, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "second", ConversationKey: "c1", IdempotencyKey: "k1"}))
	if err != nil || retried.Msg.Task.Id != first.Msg.Task.Id {
		t.Fatalf("retried merge: %v %v", retried, err)
	}
	got, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: first.Msg.Task.Id}))
	if err != nil || got.Msg.Task.Input != "first\n\nsecond" {
		t.Fatalf("retrying the same idempotency key merged again: %v %v", got, err)
	}
}

// "interrupt" queue mode gracefully cancels the conversation's currently
// running task (same mechanism as CancelTask(mode="graceful")), then
// queues the new submission as its own, independent task.
// features: tasks.queue-modes
func TestSubmitTaskInterruptGracefullyCancelsTheRunningTaskThenQueuesItsOwn(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t),
		Conversations: &agenv1.ConversationPolicy{QueueMode: "interrupt"}})); err != nil {
		t.Fatal(err)
	}
	idA, a := e.enroll(t, "nest-a", 4, nil)
	c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "hello"), Desired: 1}))
	e.scheduler().Step(ctx)

	first, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "first", ConversationKey: "c1"}))
	if err != nil {
		t.Fatal(err)
	}
	leased, err := a.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idA, Ref: ref("", "hello"), Max: 1, LeaseSeconds: 30}))
	if err != nil || len(leased.Msg.Tasks) != 1 || leased.Msg.Tasks[0].Id != first.Msg.Task.Id {
		t.Fatalf("lease: %v %v", leased, err)
	}
	if _, err := a.ExtendLease(ctx, connect.NewRequest(&agenv1.ExtendLeaseRequest{NestId: idA, TaskId: first.Msg.Task.Id, LeaseId: leased.Msg.Tasks[0].LeaseId, LeaseSeconds: 30, InstanceId: "i1"})); err != nil {
		t.Fatal(err)
	}

	// No live engine attached to drain the "cancel" entry and finish on
	// its own, so once InterruptGrace (10ms in tests) passes, the first
	// task is hard-stopped.
	second, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "second", ConversationKey: "c1"}))
	if err != nil {
		t.Fatal(err)
	}
	if second.Msg.Task.Id == first.Msg.Task.Id {
		t.Fatal("interrupt should queue its own task, not merge into or return the interrupted one")
	}
	gotFirst, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: first.Msg.Task.Id}))
	if err != nil || gotFirst.Msg.Task.State != agenv1.TaskState_TASK_STATE_CANCELLED {
		t.Fatalf("interrupted task: %v %v, want cancelled once its grace period passed", gotFirst, err)
	}
}
