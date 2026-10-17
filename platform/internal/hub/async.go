package hub

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// Async delegation (docs/architecture.md §6): a run queues a child task with
// a reply conversation key; when the child ends, the Hub submits its result
// as a task to the parent's deployment in that conversation, so the parent
// continues where it left off instead of waiting.

// maxLineage bounds the parent-run walk that derives an A2A run's depth.
const maxLineage = 64

// childParent is the parent of a child task: the task this Nest leases, or,
// for a run started over A2A (no task), that run, when it is an unfinished
// run of the calling instance on this Nest.
func (n *NestAPI) childParent(ctx context.Context, m *agenv1.SubmitChildTaskRequest) (store.Task, error) {
	if m.ParentTaskId != "" {
		parent, err := n.requireNestTask(ctx, m.NestId, m.ParentTaskId)
		if err != nil {
			return parent, err
		}
		if parent.Terminal() {
			return parent, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("parent task %s already finished", parent.ID))
		}
		return parent, nil
	}
	if err := requireNest(ctx, m.NestId); err != nil {
		return store.Task{}, err
	}
	if m.ParentRunId == "" || m.InstanceId == "" {
		return store.Task{}, invalid("parent_task_id, or parent_run_id and instance_id, are required")
	}
	run, err := n.hub.Store.GetRun(ctx, m.ParentRunId)
	if err != nil {
		return store.Task{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("run %q not found", m.ParentRunId))
	}
	owner, err := n.hub.Store.RunOwner(ctx, run.ID)
	if err != nil || owner != m.InstanceId || run.EndedMs != 0 {
		return store.Task{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("run %q is not an unfinished run of instance %s", run.ID, m.InstanceId))
	}
	insts, err := n.hub.Store.ListInstances(ctx, run.Namespace, run.Deployment, m.NestId)
	if err != nil {
		return store.Task{}, connectErr(err)
	}
	onNest := false
	for _, in := range insts {
		onNest = onNest || in.ID == m.InstanceId
	}
	if !onNest {
		return store.Task{}, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("instance %s is not on nest %s", m.InstanceId, m.NestId))
	}
	if run.TaskID != "" {
		if t, err := n.hub.Store.GetTask(ctx, run.TaskID); err == nil {
			return t, nil
		}
	}
	// An A2A run: depth is the number of runs above it.
	depth, cur := 0, run
	for cur.ParentRunID != "" && depth < maxLineage {
		up, err := n.hub.Store.GetRun(ctx, cur.ParentRunID)
		if err != nil {
			break
		}
		depth, cur = depth+1, up
	}
	return store.Task{Namespace: run.Namespace, Deployment: run.Deployment, Depth: depth, RootRunID: run.RootRunID,
		SubmittedBy: run.RequestedBy, Labels: run.Labels}, nil
}

// replyResult submits a finished child task's result to the conversation
// that started it (once: the idempotency key is the child's id).
func (h *Hub) replyResult(ctx context.Context, t store.Task) {
	if t.ReplyDeployment == "" || !t.Terminal() {
		return
	}
	var body string
	switch t.State {
	case "succeeded":
		body = t.Output
	case "failed":
		body = "It failed: " + t.Error
	default:
		body = "It was " + t.State + "."
	}
	input := fmt.Sprintf("[Result of the task you started on %s (task %s, %s)]\n\n%s", t.Deployment, t.ID, t.State, body)
	_, err := h.Store.SubmitTask(ctx, store.Task{Namespace: t.Namespace, Deployment: t.ReplyDeployment, Input: input,
		Source: "reply:" + t.Deployment, IdempotencyKey: "reply:" + t.ID, ConversationKey: t.ReplyConversationKey,
		ParentTaskID: t.ID, ParentRunID: t.RunID, RootRunID: t.RootRunID, Depth: max(t.Depth-1, 0), Traceparent: t.Traceparent,
		SubmittedBy: t.SubmittedBy, Labels: t.Labels})
	if err != nil {
		h.logger().Warn("could not deliver an async result", "task", t.ID, "to", t.Namespace+"/"+t.ReplyDeployment, "err", err)
		return
	}
	_ = h.Store.TouchActivity(ctx, t.Namespace, t.ReplyDeployment)
}
