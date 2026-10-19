package hub

import (
	"context"
	"fmt"
	"strings"
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
	// A run whose recorded call token does not verify starts nothing, and
	// a token is never stored as a submitter.
	if _, err := e.store.DB().ExecContext(ctx, "UPDATE runs SET requested_by = 'not-a-signed-token' WHERE id = 'live'"); err != nil {
		t.Fatal(err)
	}
	if _, err := start("live", "i1"); code(err) != connect.CodePermissionDenied {
		t.Fatalf("unverifiable requester: %v", err)
	}
	if _, err := e.store.DB().ExecContext(ctx, "UPDATE runs SET requested_by = '' WHERE id = 'live'"); err != nil {
		t.Fatal(err)
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
	// The result waits until the run that started the child has ended.
	leaseLead := func() int {
		l, err := n.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: id, Ref: ref("", "lead"), Max: 5}))
		if err != nil {
			t.Fatal(err)
		}
		return len(l.Msg.Tasks)
	}
	if got := leaseLead(); got != 0 {
		t.Fatalf("result leased while the starting run is still running: %d", got)
	}
	if _, err := e.store.DB().ExecContext(ctx, "UPDATE runs SET status = 'succeeded', ended_ms = $1 WHERE id = 'live'", now+1); err != nil {
		t.Fatal(err)
	}
	if got := leaseLead(); got != 1 {
		t.Fatalf("result not leased after the run ended: %d", got)
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
	fresh, err := start("fresh", "i9")
	if err != nil {
		t.Fatalf("leased task's run before its instance is reported: %v", err)
	}
	// Deleting the deployment a child runs on tells its caller.
	if _, err := c.DeleteDeployment(ctx, connect.NewRequest(&agenv1.DeleteDeploymentRequest{Ref: ref("", "writer")})); err != nil {
		t.Fatal(err)
	}
	tasks, _ = e.store.ListTasks(ctx, "default", "lead", "", 0)
	told := false
	for _, x := range tasks {
		told = told || (x.Source == "reply:writer" && strings.Contains(x.Input, fresh.Id+", cancelled"))
	}
	if !told {
		t.Fatalf("no cancelled result for %s after the delete: %+v", fresh.Id, tasks)
	}
}

// A run whose instance is gone (a Nest lost, a crash before the run ended)
// is ended by the leader with its open spans and pending approvals, unless
// it is young (its instance may not have reported yet) or its task is back
// in the queue to be resumed.
// features: tasks.run
func TestOrphanedRunsAreEnded(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "lead", BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	id, n := e.enroll(t, "n", 4, nil)
	if _, err := n.ReportStatus(ctx, connect.NewRequest(&agenv1.ReportStatusRequest{NestId: id, Capacity: 4, Instances: []*agenv1.Instance{
		{Id: "alive", Deployment: "lead", State: agenv1.InstanceState_INSTANCE_STATE_BUSY}}})); err != nil {
		t.Fatal(err)
	}
	queued, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "lead"), Input: "again"}))
	if err != nil {
		t.Fatal(err)
	}
	now := store.NowMs()
	old := now - 10*60_000
	for i, r := range []struct {
		id, owner, task string
		started         int64
	}{
		{"orphan", "gone", "", old},
		{"young", "gone", "", now},
		{"alive-run", "alive", "", old},
		{"resumable", "gone", queued.Msg.Task.Id, old},
	} {
		sid, cid := fmt.Sprintf("s%d", i), fmt.Sprintf("c%d", i)
		for _, q := range []string{
			"INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ('" + sid + "','lead','default','lead','{}',$1,$1)",
			"INSERT INTO conversations (id, session_id, created_ms) VALUES ('" + cid + "','" + sid + "',$1)",
			"INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, started_ms, owner, task_id) VALUES ('" + r.id + "','" + sid + "','" + cid + "','default','lead','running','x',$1,'" + r.owner + "','" + r.task + "')",
			"INSERT INTO spans (span_id, trace_id, run_id, name, start_ms, end_ms, status) VALUES ('sp-" + r.id + "','tr','" + r.id + "','gen_ai.chat',$1,$1,'unfinished')",
			"INSERT INTO approvals (id, namespace, deployment, run_id, tool, state, created_ms, expires_ms) VALUES ('ap-" + r.id + "','default','lead','" + r.id + "','t','pending',$1,$1+3600000)",
		} {
			if _, err := e.store.DB().ExecContext(ctx, q, r.started); err != nil {
				t.Fatal(err)
			}
		}
	}
	e.scheduler().Step(ctx)
	state := func(q, id string) string {
		var s string
		if err := e.store.DB().QueryRowContext(ctx, q, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	for id, want := range map[string]string{"orphan": "failed", "young": "running", "alive-run": "running", "resumable": "running"} {
		if got := state("SELECT status FROM runs WHERE id = $1", id); got != want {
			t.Errorf("run %s: %s, want %s", id, got, want)
		}
	}
	if got := state("SELECT status FROM spans WHERE span_id = $1", "sp-orphan"); got != "error" {
		t.Errorf("span of the orphaned run: %s", got)
	}
	if got := state("SELECT state FROM approvals WHERE id = $1", "ap-orphan"); got != "expired" {
		t.Errorf("approval of the orphaned run: %s", got)
	}
	if got := state("SELECT state FROM approvals WHERE id = $1", "ap-young"); got != "pending" {
		t.Errorf("approval of a young run: %s", got)
	}
}
