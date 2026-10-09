package hub

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/gen/agen/v1/agenv1connect"
	"github.com/prjvvl/agen/platform/internal/calltoken"
	"github.com/prjvvl/agen/platform/internal/store"
)

type bearerRT struct{ token string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (e env) nestClient(token string) agenv1connect.NestServiceClient {
	return agenv1connect.NewNestServiceClient(&http.Client{Transport: bearerRT{token}}, e.url, connect.WithProtoJSON())
}

// enroll creates a join token and enrols a Nest with it.
func (e env) enroll(t *testing.T, name string, capacity int32, labels map[string]string) (string, agenv1connect.NestServiceClient) {
	t.Helper()
	ctx := context.Background()
	jt, err := e.client(admin).CreateJoinToken(ctx, connect.NewRequest(&agenv1.CreateJoinTokenRequest{TtlSeconds: 60}))
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.nestClient("").Enroll(ctx, connect.NewRequest(&agenv1.EnrollRequest{JoinToken: jt.Msg.Token, Name: name, Capacity: capacity, Labels: labels}))
	if err != nil {
		t.Fatal(err)
	}
	if r.Msg.NestToken == "" {
		t.Fatal("no nest token")
	}
	return r.Msg.NestId, e.nestClient(r.Msg.NestToken)
}

func (e env) scheduler() *Scheduler {
	s := e.hub.NewScheduler("test-hub")
	s.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return s
}

func TestNestEnrollmentAndTokenIsolation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.nestClient("").Enroll(ctx, connect.NewRequest(&agenv1.EnrollRequest{JoinToken: "join_bogus", Name: "n"})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("bogus join token: %v", err)
	}
	jt, _ := e.client(admin).CreateJoinToken(ctx, connect.NewRequest(&agenv1.CreateJoinTokenRequest{TtlSeconds: 60}))
	r, err := e.nestClient("").Enroll(ctx, connect.NewRequest(&agenv1.EnrollRequest{JoinToken: jt.Msg.Token, Name: "a", Capacity: 4}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.nestClient("").Enroll(ctx, connect.NewRequest(&agenv1.EnrollRequest{JoinToken: jt.Msg.Token, Name: "b"})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("join token reused: %v", err)
	}
	idA, a := r.Msg.NestId, e.nestClient(r.Msg.NestToken)
	idB, _ := e.enroll(t, "b", 4, nil)

	// A nest token only works on NestService, and only for its own nest.
	if _, err := e.client(r.Msg.NestToken).ListDeployments(ctx, connect.NewRequest(&agenv1.ListDeploymentsRequest{})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("nest token on HubService: %v", err)
	}
	if _, err := a.ReportStatus(ctx, connect.NewRequest(&agenv1.ReportStatusRequest{NestId: idB})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("report as another nest: %v", err)
	}
	if _, err := e.nestClient("").ReportStatus(ctx, connect.NewRequest(&agenv1.ReportStatusRequest{NestId: idA})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("no token: %v", err)
	}
	// A user API token (even operator) is not a nest token.
	op, _ := e.client(admin).CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "op", Scopes: []string{ScopeOperator}}))
	if _, err := e.nestClient(op.Msg.Secret).ReportStatus(ctx, connect.NewRequest(&agenv1.ReportStatusRequest{NestId: idA})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("operator token on NestService: %v", err)
	}
	// Tokens with the nest scope cannot be minted through the API.
	if _, err := e.client(admin).CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "nest:" + idA, Scopes: []string{ScopeNest}})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("mint nest token: %v", err)
	}
	if _, err := a.ReportStatus(ctx, connect.NewRequest(&agenv1.ReportStatusRequest{NestId: idA, Capacity: 4, Instances: []*agenv1.Instance{
		{Id: "i1", Deployment: "hello", State: agenv1.InstanceState_INSTANCE_STATE_READY}}})); err != nil {
		t.Fatal(err)
	}
	list, _ := e.client(admin).ListInstances(ctx, connect.NewRequest(&agenv1.ListInstancesRequest{}))
	if len(list.Msg.Instances) != 1 || list.Msg.Instances[0].NestId != idA || list.Msg.Instances[0].Namespace != "default" {
		t.Fatalf("instances: %v", list.Msg.Instances)
	}
}

func TestSchedulerPlacesWatchesAndReschedulesLostNest(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	idA, a := e.enroll(t, "a", 2, nil)
	idB, b := e.enroll(t, "b", 2, nil)

	stream, err := a.WatchAssignments(ctx, connect.NewRequest(&agenv1.WatchAssignmentsRequest{NestId: idA}))
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() || len(stream.Msg().Assignments) != 0 {
		t.Fatalf("initial assignments: %v %v", stream.Msg(), stream.Err())
	}
	if _, err := b.WatchAssignments(ctx, connect.NewRequest(&agenv1.WatchAssignmentsRequest{NestId: idA})); err != nil {
		t.Fatal(err) // the error arrives on Receive for server streams
	}

	sched := e.scheduler()
	if _, err := c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "hello"), Desired: 3})); err != nil {
		t.Fatal(err)
	}
	if epoch, err := sched.Step(ctx); err != nil || epoch == 0 {
		t.Fatalf("step: %d %v", epoch, err)
	}
	all, _ := e.store.AllAssignments(ctx)
	total := map[string]int{}
	for _, x := range all {
		total[x.NestID] += x.Count
	}
	if total[idA]+total[idB] != 3 || total[idA] == 0 || total[idB] == 0 {
		t.Fatalf("spread: %v", total)
	}
	if !stream.Receive() {
		t.Fatal(stream.Err())
	}
	got := stream.Msg()
	if len(got.Assignments) != 1 || int(got.Assignments[0].Count) != total[idA] || got.Assignments[0].Kind != "pool" || got.Revision != 2 {
		t.Fatalf("watched: %v", got)
	}
	// Stable: another step changes nothing.
	sched.Step(ctx)
	again, _ := e.store.AllAssignments(ctx)
	if len(again) != len(all) {
		t.Fatalf("unstable placement: %v -> %v", all, again)
	}

	// Nest b goes silent: it is marked lost and its instances move to a
	// (up to a's capacity of 2).
	store.NowMs = func() int64 { return time.Now().Add(20 * time.Second).UnixMilli() }
	if _, err := a.ReportStatus(ctx, connect.NewRequest(&agenv1.ReportStatusRequest{NestId: idA, Capacity: 2})); err != nil {
		t.Fatal(err)
	}
	sched.Step(ctx)
	store.NowMs = func() int64 { return time.Now().UnixMilli() }
	nb, _ := e.store.GetNest(ctx, idB)
	if nb.State != "lost" {
		t.Fatalf("nest b: %s", nb.State)
	}
	all, _ = e.store.AllAssignments(ctx)
	if len(all) != 1 || all[0].NestID != idA || all[0].Count != 2 {
		t.Fatalf("after loss: %v", all)
	}
	// b comes back: the missing instance is placed on it.
	if _, err := b.ReportStatus(ctx, connect.NewRequest(&agenv1.ReportStatusRequest{NestId: idB, Capacity: 2})); err != nil {
		t.Fatal(err)
	}
	sched.Step(ctx)
	all, _ = e.store.AllAssignments(ctx)
	if len(all) != 2 {
		t.Fatalf("after return: %v", all)
	}

	// Only one leader: a second replica is fenced out.
	other := e.hub.NewScheduler("other-hub")
	if epoch, err := other.Step(ctx); err != nil || epoch != 0 {
		t.Fatalf("second leader: %d %v", epoch, err)
	}
}

func TestPlacementLabelsAndCapacity(t *testing.T) {
	nests := []store.Nest{
		{ID: "n1", State: "active", Capacity: 1, Labels: map[string]string{"gpu": "yes"}},
		{ID: "n2", State: "active", Capacity: 3},
		{ID: "n3", State: "lost", Capacity: 10, Labels: map[string]string{"gpu": "yes"}},
	}
	d := store.Deployment{Namespace: "default", Name: "x", DefinitionDigest: "sha256:1", Placement: []byte(`{"labels":{"gpu":"yes"}}`)}
	got := Placement(d, 5, nests, nil, map[string]int{})
	if len(got) != 1 || got[0].NestID != "n1" || got[0].Count != 1 {
		t.Fatalf("labels+capacity: %v", got)
	}
	d.Placement = nil
	got = Placement(d, 5, nests, nil, map[string]int{"n2": 2})
	if len(got) != 2 || got[0].Count+got[1].Count != 2 {
		t.Fatalf("capacity used by others: %v", got)
	}
	// Scale down removes from the most loaded nest.
	cur := []store.Assignment{{NestID: "n1", Count: 1}, {NestID: "n2", Count: 3}}
	got = Placement(d, 2, nests, cur, map[string]int{})
	if len(got) != 2 || got[0].Count != 1 || got[1].Count != 1 {
		t.Fatalf("scale down: %v", got)
	}
}

func TestNestTaskLeasingIsScopedToAssignments(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	idA, a := e.enroll(t, "a", 1, nil)
	idB, b := e.enroll(t, "b", 1, map[string]string{"zone": "other"})
	if _, err := c.UpdateDeployment(ctx, connect.NewRequest(&agenv1.UpdateDeploymentRequest{Ref: ref("", "hello"),
		Placement: &agenv1.Placement{Labels: map[string]string{"zone": "other"}}})); err != nil {
		t.Fatal(err)
	}
	c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "hello"), Desired: 1}))
	e.scheduler().Step(ctx)
	task, _ := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "hi"}))

	if _, err := a.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idA, Ref: ref("", "hello"), Max: 1})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("unassigned nest leased: %v", err)
	}
	l, err := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "hello"), Max: 5, LeaseSeconds: 30}))
	if err != nil || len(l.Msg.Tasks) != 1 {
		t.Fatalf("lease: %v %v", l, err)
	}
	lt := l.Msg.Tasks[0]
	if _, err := a.CompleteTask(ctx, connect.NewRequest(&agenv1.CompleteTaskRequest{NestId: idA, TaskId: lt.Id, LeaseId: lt.LeaseId, Success: true})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("other nest completed: %v", err)
	}
	if _, err := b.ExtendLease(ctx, connect.NewRequest(&agenv1.ExtendLeaseRequest{NestId: idB, TaskId: lt.Id, LeaseId: lt.LeaseId, InstanceId: "inst-1", LeaseSeconds: 30})); err != nil {
		t.Fatal(err)
	}
	got, _ := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: task.Msg.Task.Id}))
	if got.Msg.Task.State != agenv1.TaskState_TASK_STATE_RUNNING || got.Msg.Task.InstanceId != "inst-1" {
		t.Fatalf("running: %v", got.Msg.Task)
	}
	if _, err := b.CompleteTask(ctx, connect.NewRequest(&agenv1.CompleteTaskRequest{NestId: idB, TaskId: lt.Id, LeaseId: "stale", Success: true})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale lease: %v", err)
	}
	if _, err := b.CompleteTask(ctx, connect.NewRequest(&agenv1.CompleteTaskRequest{NestId: idB, TaskId: lt.Id, LeaseId: lt.LeaseId, Success: true, Output: "ok", RunId: "r1", InstanceId: "inst-1"})); err != nil {
		t.Fatal(err)
	}
	got, _ = c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: task.Msg.Task.Id}))
	if got.Msg.Task.State != agenv1.TaskState_TASK_STATE_SUCCEEDED || got.Msg.Task.Output != "ok" {
		t.Fatalf("done: %v", got.Msg.Task)
	}

	// Child tasks: lineage comes from a parent task the nest holds, never
	// from the caller, so max_delegation_depth cannot be bypassed.
	c.UpdateDeployment(ctx, connect.NewRequest(&agenv1.UpdateDeploymentRequest{Ref: ref("", "hello"), Limits: &agenv1.Limits{MaxDelegationDepth: 1}}))
	c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "parent"}))
	pl, err := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "hello"), Max: 1, LeaseSeconds: 30}))
	if err != nil || len(pl.Msg.Tasks) != 1 {
		t.Fatalf("lease parent: %v %v", pl, err)
	}
	parent := pl.Msg.Tasks[0]
	if _, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "hello"), Input: "x"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("no parent: %v", err)
	}
	if _, err := a.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idA, Ref: ref("", "hello"), Input: "x", ParentTaskId: parent.Id})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("parent held by another nest: %v", err)
	}
	if _, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("other", "hello"), Input: "x", ParentTaskId: parent.Id})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("cross-namespace: %v", err)
	}
	ch, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "hello"), Input: "child",
		ParentTaskId: parent.Id, Depth: 0, RootRunId: "forged"}))
	if err != nil || ch.Msg.Task.Depth != 1 || ch.Msg.Task.Source != "a2a" || ch.Msg.Task.ParentTaskId != parent.Id || ch.Msg.Task.RootRunId == "forged" {
		t.Fatalf("child: %v %v", ch, err)
	}
	// The child (depth 1) delegating again is depth 2 > 1, whatever depth it claims.
	cl, err := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "hello"), Max: 1, LeaseSeconds: 30}))
	if err != nil || len(cl.Msg.Tasks) != 1 || cl.Msg.Tasks[0].Id != ch.Msg.Task.Id {
		t.Fatalf("lease child: %v %v", cl, err)
	}
	if _, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "hello"), Input: "x",
		ParentTaskId: ch.Msg.Task.Id, Depth: 0})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("too deep: %v", err)
	}
	// Released before starting: back in the queue without using an attempt.
	if _, err := b.ReleaseTask(ctx, connect.NewRequest(&agenv1.ReleaseTaskRequest{NestId: idB, TaskId: ch.Msg.Task.Id, LeaseId: cl.Msg.Tasks[0].LeaseId, NotStarted: true})); err != nil {
		t.Fatal(err)
	}
	rel, _ := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: ch.Msg.Task.Id}))
	if rel.Msg.Task.State != agenv1.TaskState_TASK_STATE_QUEUED || rel.Msg.Task.Attempts != 0 {
		t.Fatalf("released: %v", rel.Msg.Task)
	}

	// Definitions and wake-ups are limited to what the nest may run.
	dep, _ := c.GetDeployment(ctx, connect.NewRequest(&agenv1.GetDeploymentRequest{Ref: ref("", "hello")}))
	digest := dep.Msg.Deployment.DefinitionDigest
	if _, err := a.GetDefinition(ctx, connect.NewRequest(&agenv1.GetDefinitionRequest{Digest: digest})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("unassigned nest read definition: %v", err)
	}
	if _, err := b.GetDefinition(ctx, connect.NewRequest(&agenv1.GetDefinitionRequest{Digest: digest})); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ReportActivity(ctx, connect.NewRequest(&agenv1.ReportActivityRequest{Ref: ref("", "hello")})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("ineligible nest woke deployment: %v", err)
	}
	if _, err := b.ReportActivity(ctx, connect.NewRequest(&agenv1.ReportActivityRequest{Ref: ref("", "hello")})); err != nil {
		t.Fatal(err)
	}

	// Durable approvals raised through the nest are decided by a human. They
	// must name a real run of the deployment, and requested_by is set by the
	// Hub (a nest cannot name the admin to block them from deciding).
	if _, err := b.CreateApproval(ctx, connect.NewRequest(&agenv1.CreateApprovalRequest{Approval: &agenv1.Approval{Deployment: "hello", Tool: "write_note", RunId: "nope"}})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("approval for unknown run: %v", err)
	}
	now := time.Now().UnixMilli()
	for _, q := range []string{
		"INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ('s1','hello','default','hello','{}',$1,$1)",
		"INSERT INTO conversations (id, session_id, created_ms) VALUES ('c1','s1',$1)",
		"INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, started_ms, owner) VALUES ('r1','s1','c1','default','hello','running','hi',$1,'inst-1')",
	} {
		if _, err := e.store.DB().ExecContext(ctx, q, now); err != nil {
			t.Fatal(err)
		}
	}
	// Only the instance that owns the run may raise its asks.
	for _, inst := range []string{"", "inst-other"} {
		if _, err := b.CreateApproval(ctx, connect.NewRequest(&agenv1.CreateApprovalRequest{Approval: &agenv1.Approval{Deployment: "hello", Tool: "write_note", RunId: "r1"},
			InstanceId: inst})); code(err) != connect.CodePermissionDenied {
			t.Fatalf("ask by instance %q on another instance's run: %v", inst, err)
		}
	}
	ap, err := b.CreateApproval(ctx, connect.NewRequest(&agenv1.CreateApprovalRequest{Approval: &agenv1.Approval{Deployment: "hello", Tool: "write_note", RunId: "r1",
		RequestedBy: "admin"}, TtlSeconds: 60, InstanceId: "inst-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if ap.Msg.Approval.RequestedBy != "nest:"+idB {
		t.Fatalf("requested_by: %q", ap.Msg.Approval.RequestedBy)
	}
	// A resumed run asking the same question gets the same pending approval
	// (argument key order does not matter); a different question does not.
	args1, _ := structpb.NewStruct(map[string]any{"path": "a.txt", "text": "hi"})
	args2, _ := structpb.NewStruct(map[string]any{"text": "hi", "path": "a.txt"})
	q1, err := b.CreateApproval(ctx, connect.NewRequest(&agenv1.CreateApprovalRequest{Approval: &agenv1.Approval{Deployment: "hello", Tool: "write_note", RunId: "r1", Arguments: args1}, InstanceId: "inst-1"}))
	if err != nil {
		t.Fatal(err)
	}
	q2, _ := b.CreateApproval(ctx, connect.NewRequest(&agenv1.CreateApprovalRequest{Approval: &agenv1.Approval{Deployment: "hello", Tool: "write_note", RunId: "r1", Arguments: args2}, InstanceId: "inst-1"}))
	other, _ := structpb.NewStruct(map[string]any{"path": "b.txt"})
	q3, _ := b.CreateApproval(ctx, connect.NewRequest(&agenv1.CreateApprovalRequest{Approval: &agenv1.Approval{Deployment: "hello", Tool: "write_note", RunId: "r1", Arguments: other}, InstanceId: "inst-1"}))
	if q1.Msg.Approval.Id != q2.Msg.Approval.Id || q3.Msg.Approval.Id == q1.Msg.Approval.Id {
		t.Fatalf("pending approval reuse: %s %s %s", q1.Msg.Approval.Id, q2.Msg.Approval.Id, q3.Msg.Approval.Id)
	}
	if _, err := a.GetApproval(ctx, connect.NewRequest(&agenv1.GetApprovalRequest{Id: ap.Msg.Approval.Id})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("unassigned nest read approval: %v", err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		c.DecideApproval(ctx, connect.NewRequest(&agenv1.DecideApprovalRequest{Id: ap.Msg.Approval.Id, Approve: true}))
	}()
	w, err := b.GetApproval(ctx, connect.NewRequest(&agenv1.GetApprovalRequest{Id: ap.Msg.Approval.Id, WaitSeconds: 5}))
	if err != nil || w.Msg.Approval.State != agenv1.ApprovalState_APPROVAL_STATE_APPROVED || w.Msg.Approval.DecidedBy != "admin" {
		t.Fatalf("approval wait: %v %v", w, err)
	}
}

func TestNamespaceScopedDefinitionsAndListLimits(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	da, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Namespace: "a", BundleFiles: helloFiles(t)}))
	if err != nil {
		t.Fatal(err)
	}
	other := helloFiles(t)
	other["x-agen/agent.md"] = []byte("---\nname: hello\ndescription: Other\n---\nSecret prompt.\n")
	db, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Namespace: "b", BundleFiles: other}))
	if err != nil {
		t.Fatal(err)
	}
	c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("a", "hello"), Input: "old"}))
	time.Sleep(5 * time.Millisecond)
	for i := 0; i < 3; i++ {
		c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("b", "hello"), Input: "newer"}))
	}
	tok, err := c.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "a-viewer", Scopes: []string{ScopeViewer}, Namespaces: []string{"a"}}))
	if err != nil {
		t.Fatal(err)
	}
	v := e.client(tok.Msg.Secret)
	defs, err := v.ListDefinitions(ctx, connect.NewRequest(&agenv1.ListDefinitionsRequest{}))
	if err != nil || len(defs.Msg.Definitions) != 1 || defs.Msg.Definitions[0].Digest != da.Msg.Deployment.DefinitionDigest {
		t.Fatalf("definitions: %v %v", defs, err)
	}
	if _, err := v.GetDefinition(ctx, connect.NewRequest(&agenv1.GetDefinitionRequest{Digest: db.Msg.Deployment.DefinitionDigest})); code(err) != connect.CodeNotFound {
		t.Fatalf("other namespace's definition: %v", err)
	}
	// Namespace filtering happens before LIMIT: the scoped token still sees
	// its (older) task.
	ts, err := v.ListTasks(ctx, connect.NewRequest(&agenv1.ListTasksRequest{Limit: 1}))
	if err != nil || len(ts.Msg.Tasks) != 1 || ts.Msg.Tasks[0].Namespace != "a" {
		t.Fatalf("tasks: %v %v", ts, err)
	}
	if _, err := v.ListTasks(ctx, connect.NewRequest(&agenv1.ListTasksRequest{Namespace: "b"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("explicit other namespace: %v", err)
	}
	all, _ := c.ListTasks(ctx, connect.NewRequest(&agenv1.ListTasksRequest{Limit: 2}))
	if len(all.Msg.Tasks) != 2 || all.Msg.Tasks[0].Namespace != "b" {
		t.Fatalf("admin tasks: %v", all.Msg.Tasks)
	}
}

func TestPauseAndRedeployPolicy(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	e.enroll(t, "a", 4, nil)
	c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "hello"), Desired: 2}))
	p, err := c.PauseDeployment(ctx, connect.NewRequest(&agenv1.PauseDeploymentRequest{Ref: ref("", "hello"), Paused: true}))
	if err != nil || !p.Msg.Deployment.Paused || p.Msg.Deployment.Desired != 0 {
		t.Fatalf("pause: %v %v", p, err)
	}
	if _, err := c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "hello"), Desired: 1})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("scale while paused: %v", err)
	}
	if _, err := c.RequestWake(ctx, connect.NewRequest(&agenv1.RequestWakeRequest{Ref: ref("", "hello")})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("wake while paused: %v", err)
	}
	// Queued work does not bring a paused deployment back.
	c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "waits"}))
	sched := e.scheduler()
	sched.Step(ctx)
	all, _ := e.store.AllAssignments(ctx)
	d, _ := c.GetDeployment(ctx, connect.NewRequest(&agenv1.GetDeploymentRequest{Ref: ref("", "hello")}))
	if len(all) != 0 || d.Msg.Deployment.Desired != 0 {
		t.Fatalf("paused deployment scheduled: %v %v", all, d.Msg.Deployment)
	}
	r, err := c.PauseDeployment(ctx, connect.NewRequest(&agenv1.PauseDeploymentRequest{Ref: ref("", "hello"), Paused: false}))
	if err != nil || r.Msg.Deployment.Paused {
		t.Fatalf("resume: %v %v", r, err)
	}
	sched.Step(ctx)
	d, _ = c.GetDeployment(ctx, connect.NewRequest(&agenv1.GetDeploymentRequest{Ref: ref("", "hello")}))
	if d.Msg.Deployment.Desired != 1 {
		t.Fatalf("resumed with queued task: desired %d", d.Msg.Deployment.Desired)
	}

	// A new bundle version brings its own policy; placement is kept.
	c.UpdateDeployment(ctx, connect.NewRequest(&agenv1.UpdateDeploymentRequest{Ref: ref("", "hello"), Placement: &agenv1.Placement{Labels: map[string]string{"x": "y"}}}))
	v2 := helloFiles(t)
	v2["x-agen/config.json"] = []byte(`{"kind":"pool","scale":{"min":1,"max":5,"targetQueuePerInstance":4},"limits":{"maxDelegationDepth":3}}`)
	u, err := c.UpdateDeployment(ctx, connect.NewRequest(&agenv1.UpdateDeploymentRequest{Ref: ref("", "hello"), BundleFiles: v2}))
	if err != nil {
		t.Fatal(err)
	}
	ud := u.Msg.Deployment
	if ud.Scale.Max != 5 || ud.Scale.Min != 1 || ud.Scale.TargetQueuePerInstance != 4 || ud.Limits.MaxDelegationDepth != 3 || ud.Placement.Labels["x"] != "y" {
		t.Fatalf("redeploy policy: %v", ud)
	}
}

// An approval raised by a delegated run (no Hub task of its own) names the
// submitter of its root run's task, so that person cannot approve it; a child
// task's parent run must be the parent task's run.
func TestDelegatedApprovalsNameTheRootSubmitter(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	idB, b := e.enroll(t, "b", 2, nil)
	c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "hello"), Desired: 1}))
	e.scheduler().Step(ctx)
	tok, _ := c.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "alice", Scopes: []string{ScopeOperator, ScopeApprover}}))
	alice := e.client(tok.Msg.Secret)
	task, err := alice.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "go"}))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for _, q := range []string{
		"INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ('s1','hello','default','hello','{}',$1,$1)",
		"INSERT INTO conversations (id, session_id, created_ms) VALUES ('c1','s1',$1)",
		"INSERT INTO conversations (id, session_id, created_ms, closed_ms) VALUES ('c0','s1',$1,$1)",
		// root run of alice's task, and a delegated run under it (A2A, no Hub task)
		"INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, started_ms, task_id, root_run_id) VALUES ('root','s1','c0','default','hello','running','go',$1,'" + task.Msg.Task.Id + "','root')",
		"INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, started_ms, task_id, root_run_id, parent_run_id, owner) VALUES ('child','s1','c1','default','hello','running','sub',$1,'a2a-x','root','root','inst-1')",
	} {
		if _, err := e.store.DB().ExecContext(ctx, q, now); err != nil {
			t.Fatal(err)
		}
	}
	// Runs started over A2A (no Hub task): the requester is the user of the
	// Hub-signed call token the Gateway recorded. A host writing a plain
	// principal, or a token for another deployment, is ignored.
	key, err := e.hub.tokenKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bobTok, _ := calltoken.Sign(key, calltoken.Claims{Sub: "user:token:bob", Aud: "default/hello"}, time.Minute, time.Now().Add(-time.Hour))
	otherTok, _ := calltoken.Sign(key, calltoken.Claims{Sub: "user:token:carol", Aud: "default/other"}, time.Minute, time.Now())
	// want "" = the ask is refused: a record that does not verify never
	// falls back to the Nest (the real requester could then approve it).
	for i, c := range []struct{ requestedBy, want string }{
		{bobTok, "token:bob"}, // expired long ago: still a valid record
		{"token:mallory", ""},
		{otherTok, ""},
	} {
		id := fmt.Sprintf("a2a-run-%d", i)
		conv := fmt.Sprintf("ca%d", i)
		for _, q := range []string{
			"INSERT INTO conversations (id, session_id, created_ms, closed_ms) VALUES ('" + conv + "','s1',$1,$1)",
			"INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, started_ms, task_id, root_run_id, owner, requested_by) VALUES ('" + id + "','s1','" + conv + "','default','hello','running','hi',$1,'a2a-" + id + "','" + id + "','inst-1','" + c.requestedBy + "')",
		} {
			if _, err := e.store.DB().ExecContext(ctx, q, now); err != nil {
				t.Fatal(err)
			}
		}
		got, err := b.CreateApproval(ctx, connect.NewRequest(&agenv1.CreateApprovalRequest{Approval: &agenv1.Approval{Deployment: "hello", Tool: "pay", RunId: id}, InstanceId: "inst-1"}))
		if c.want == "" {
			if code(err) != connect.CodeFailedPrecondition {
				t.Fatalf("requested_by %q: ask not refused: %v %v", c.requestedBy, got, err)
			}
			continue
		}
		if err != nil || got.Msg.Approval.RequestedBy != c.want {
			t.Fatalf("requested_by %q: got %v %v, want %s", c.requestedBy, got, err, c.want)
		}
	}
	ap, err := b.CreateApproval(ctx, connect.NewRequest(&agenv1.CreateApprovalRequest{Approval: &agenv1.Approval{Deployment: "hello", Tool: "pay", RunId: "child"}, InstanceId: "inst-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if ap.Msg.Approval.RequestedBy != "token:"+tok.Msg.Token.Id {
		t.Fatalf("requested_by: %q", ap.Msg.Approval.RequestedBy)
	}
	if _, err := alice.DecideApproval(ctx, connect.NewRequest(&agenv1.DecideApprovalRequest{Id: ap.Msg.Approval.Id, Approve: true})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("alice approved her delegated run's ask: %v", err)
	}

	// Child tasks: parent_run_id must be the parent task's run.
	l, _ := b.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: idB, Ref: ref("", "hello"), Max: 1, LeaseSeconds: 30}))
	if len(l.Msg.Tasks) != 1 {
		t.Fatal("lease")
	}
	if _, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "hello"), Input: "x",
		ParentTaskId: l.Msg.Tasks[0].Id, ParentRunId: "child"})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("foreign parent run: %v", err)
	}
	ch, err := b.SubmitChildTask(ctx, connect.NewRequest(&agenv1.SubmitChildTaskRequest{NestId: idB, Ref: ref("", "hello"), Input: "x",
		ParentTaskId: l.Msg.Tasks[0].Id, ParentRunId: "root"}))
	if err != nil || ch.Msg.Task.ParentRunId != "root" {
		t.Fatalf("parent run of the parent task: %v %v", ch, err)
	}
}
