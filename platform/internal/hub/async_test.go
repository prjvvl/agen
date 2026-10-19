package hub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// A run started over A2A (no task) may start child tasks when it is an
// unfinished run of the asking instance on the asking Nest; the child's
// result goes back to the run's conversation once.
// features: delegation.async
func TestChildTaskFromARunRepliesOnce(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	for _, name := range []string{"lead", "writer"} {
		if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: name, BundleFiles: helloFiles(t)})); err != nil {
			t.Fatal(err)
		}
	}
	id, n := e.enroll(t, "n", 4, nil)
	c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "lead"), Desired: 1}))
	e.scheduler().Step(ctx)
	if _, err := n.ReportStatus(ctx, connect.NewRequest(&agenv1.ReportStatusRequest{NestId: id, Capacity: 4, Instances: []*agenv1.Instance{
		{Id: "i1", Deployment: "lead", State: agenv1.InstanceState_INSTANCE_STATE_BUSY}}})); err != nil {
		t.Fatal(err)
	}
	now := store.NowMs()
	for _, q := range []string{
		"INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ('s1','lead','default','lead','{}',$1,$1)",
		"INSERT INTO conversations (id, session_id, created_ms) VALUES ('c1','s1',$1)",
		"INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, started_ms, owner) VALUES ('live','s1','c1','default','lead','running','x',$1,'i1')",
		"INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, started_ms, ended_ms, owner) VALUES ('done','s1','c1','default','lead','succeeded','x',$1,$1,'i1')",
	} {
		if _, err := e.store.DB().ExecContext(ctx, q, now); err != nil {
			t.Fatal(err)
		}
	}
	start := func(run, instance string) (*agenv1.Task, error) {
		r, err := n.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: id, InstanceId: instance, Ref: ref("", "writer"),
			Input: "write", ParentRunId: run, ReplyConversationKey: "async:s1", IdempotencyKey: "k-" + run + instance}))
		if err != nil {
			return nil, err
		}
		return r.Msg.Task, nil
	}
	if _, err := start("live", "i2"); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("another instance's run: %v", err)
	}
	if _, err := start("done", "i1"); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("finished run: %v", err)
	}
	child, err := start("live", "i1")
	if err != nil {
		t.Fatal(err)
	}
	if child.ReplyDeployment != "lead" || child.ReplyConversationKey != "async:s1" || child.Depth != 1 || child.ParentRunId != "live" {
		t.Fatalf("child: %v", child)
	}
	if _, err := e.store.DB().ExecContext(ctx, "UPDATE tasks SET state = 'succeeded', output = 'Written.' WHERE id = $1", child.Id); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		e.hub.afterTaskEnded(ctx, child.Id)
	}
	tasks, _ := e.store.ListTasks(ctx, "default", "lead", "", 0)
	if len(tasks) != 1 || tasks[0].ConversationKey != "async:s1" || tasks[0].Source != "reply:writer" ||
		tasks[0].Input != "[Result of the task you started on writer (task "+child.Id+", succeeded)]\n\nWritten." {
		t.Fatalf("reply tasks: %+v", tasks)
	}

	// A task's run can start children before the Nest's next status report
	// lists its instance: the task's lease names the Nest and instance. Only
	// that Nest may ask for it.
	sub, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "lead"), Input: "go"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: id, Ref: ref("", "lead"), Max: 5})); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"UPDATE tasks SET instance_id = 'i9', run_id = 'fresh' WHERE id = '" + sub.Msg.Task.Id + "'",
		"INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ('s2','lead','default','lead','{}',$1,$1)",
		"INSERT INTO conversations (id, session_id, created_ms) VALUES ('c2','s2',$1)",
		"INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, started_ms, owner, task_id) VALUES ('fresh','s2','c2','default','lead','running','go',$1,'i9','" + sub.Msg.Task.Id + "')",
	} {
		if _, err := e.store.DB().ExecContext(ctx, q, now); err != nil {
			t.Fatal(err)
		}
	}
	other, n2 := e.enroll(t, "n2", 4, nil)
	if _, err := n2.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: other, InstanceId: "i9", Ref: ref("", "writer"),
		Input: "write", ParentRunId: "fresh", IdempotencyKey: "k-other"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("another nest: %v", err)
	}
	if _, err := start("fresh", "i9"); err != nil {
		t.Fatalf("leased task's run before its instance is reported: %v", err)
	}
}
