package hub

import (
	"context"
	"encoding/json"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// RecordFeedback validates its score and labels, and is scoped to the
// task's namespace; ListFeedback filters by deployment, labels and time.
// features: feedback
func TestRecordAndListFeedback(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	sub, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RecordFeedback(ctx, connect.NewRequest(&agenv1.RecordFeedbackRequest{TaskId: sub.Msg.Task.Id, Score: 1.5})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("out-of-range score accepted: %v", err)
	}
	if _, err := c.RecordFeedback(ctx, connect.NewRequest(&agenv1.RecordFeedbackRequest{Score: 1})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("no task accepted: %v", err)
	}
	if _, err := c.RecordFeedback(ctx, connect.NewRequest(&agenv1.RecordFeedbackRequest{TaskId: "no-such-task", Score: 1})); code(err) != connect.CodeNotFound {
		t.Fatalf("unknown task: %v", err)
	}
	rec, err := c.RecordFeedback(ctx, connect.NewRequest(&agenv1.RecordFeedbackRequest{TaskId: sub.Msg.Task.Id, Score: 0.8, Note: "mostly right",
		Labels: map[string]string{"judge": "rubric-v1"}}))
	if err != nil || rec.Msg.Feedback.Deployment != "hello" || rec.Msg.Feedback.By != "admin" {
		t.Fatalf("record: %v %v", rec, err)
	}
	list, err := c.ListFeedback(ctx, connect.NewRequest(&agenv1.ListFeedbackRequest{Deployment: "hello"}))
	if err != nil || len(list.Msg.Feedback) != 1 || list.Msg.Feedback[0].Id != rec.Msg.Feedback.Id {
		t.Fatalf("list: %v %v", list, err)
	}
	byLabel, err := c.ListFeedback(ctx, connect.NewRequest(&agenv1.ListFeedbackRequest{Labels: map[string]string{"judge": "rubric-v2"}}))
	if err != nil || len(byLabel.Msg.Feedback) != 0 {
		t.Fatalf("a different label value matches nothing: %v %v", byLabel, err)
	}
}

// An approval decision is recorded as feedback too: approved -> 1.0, denied
// -> 0.0, by "approval".
// features: approvals, feedback
func TestApprovalDecisionsAreRecordedAsFeedback(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	sub, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	now := store.NowMs()
	for _, q := range []string{
		"INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ('s1','hello','default','hello','{}',$1,$1)",
		"INSERT INTO conversations (id, session_id, created_ms) VALUES ('c1','s1',$1)",
		"INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, trace_id, started_ms, task_id) VALUES ('r1','s1','c1','default','hello','waiting_approval','x','t1',$1,'" + sub.Msg.Task.Id + "')",
	} {
		if _, err := e.store.DB().ExecContext(ctx, q, now); err != nil {
			t.Fatal(err)
		}
	}
	a, err := e.store.CreateApproval(ctx, store.Approval{Namespace: "default", Deployment: "hello", RunID: "r1", Tool: "pay",
		RequestedBy: "trigger:cron:x", Arguments: json.RawMessage(`{}`)}, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.DecideApproval(ctx, connect.NewRequest(&agenv1.DecideApprovalRequest{Id: a.ID, Approve: true})); err != nil {
		t.Fatal(err)
	}
	list, err := c.ListFeedback(ctx, connect.NewRequest(&agenv1.ListFeedbackRequest{}))
	if err != nil || len(list.Msg.Feedback) != 1 || list.Msg.Feedback[0].Score != 1 || list.Msg.Feedback[0].By != "approval" ||
		list.Msg.Feedback[0].TaskId != sub.Msg.Task.Id {
		t.Fatalf("approved feedback: %v %v", list, err)
	}

	// A second approval, denied: score 0.
	b, err := e.store.CreateApproval(ctx, store.Approval{Namespace: "default", Deployment: "hello", RunID: "r1", Tool: "pay2",
		RequestedBy: "trigger:cron:x", Arguments: json.RawMessage(`{}`)}, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.DecideApproval(ctx, connect.NewRequest(&agenv1.DecideApprovalRequest{Id: b.ID, Approve: false})); err != nil {
		t.Fatal(err)
	}
	list2, err := c.ListFeedback(ctx, connect.NewRequest(&agenv1.ListFeedbackRequest{}))
	if err != nil || len(list2.Msg.Feedback) != 2 {
		t.Fatalf("after deny: %v %v", list2, err)
	}
	denied := list2.Msg.Feedback[0]
	if denied.Score != 0 || denied.By != "approval" {
		t.Fatalf("denied feedback: %+v", denied)
	}
}
