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
}
