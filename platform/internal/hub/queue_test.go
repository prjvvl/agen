package hub

import (
	"context"
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
