package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/gen/agen/v1/agenv1connect"
	"github.com/prjvvl/agen/platform/internal/store"
)

const admin = "admin-secret-for-tests"

type env struct {
	store *store.Store
	url   string
	hub   *Hub
}

func setup(t *testing.T) env {
	t.Helper()
	s, err := store.Open(context.Background(), "sqlite:"+filepath.ToSlash(filepath.Join(t.TempDir(), "hub.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	h := New(s, admin)
	h.Poll = 10 * time.Millisecond
	mux := http.NewServeMux()
	path, handler := h.Handler()
	mux.Handle(path, handler)
	nest := h.Nest()
	nest.WatchPoll = 20 * time.Millisecond
	path, handler = nest.Handler()
	mux.Handle(path, handler)
	path, handler = h.WebhookHandler()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return env{store: s, url: srv.URL, hub: h}
}

func (e env) client(token string) agenv1connect.HubServiceClient {
	return agenv1connect.NewHubServiceClient(http.DefaultClient, e.url, connect.WithInterceptors(connect.UnaryInterceptorFunc(
		func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				if token != "" {
					req.Header().Set("Authorization", "Bearer "+token)
				}
				return next(ctx, req)
			}
		})), connect.WithProtoJSON())
}

func helloFiles(t *testing.T) map[string][]byte {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..", "..", "examples", "bundles", "hello")
	files := map[string][]byte{}
	if err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		b, err := os.ReadFile(p)
		files[filepath.ToSlash(rel)] = b
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func code(err error) connect.Code {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Code()
	}
	return connect.CodeUnknown
}

func ref(ns, name string) *agenv1.DeploymentRef {
	return &agenv1.DeploymentRef{Namespace: ns, Name: name}
}

func TestAuthScopesAndNamespaces(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.client("").ListDeployments(ctx, connect.NewRequest(&agenv1.ListDeploymentsRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("no token: %v", err)
	}
	if _, err := e.client("wrong").ListDeployments(ctx, connect.NewRequest(&agenv1.ListDeploymentsRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("bad token: %v", err)
	}
	adm := e.client(admin)
	viewer, err := adm.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "v", Scopes: []string{"viewer"}}))
	if err != nil {
		t.Fatal(err)
	}
	v := e.client(viewer.Msg.Secret)
	if _, err := v.ListDeployments(ctx, connect.NewRequest(&agenv1.ListDeploymentsRequest{})); err != nil {
		t.Fatalf("viewer can list: %v", err)
	}
	if _, err := v.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer cannot create: %v", err)
	}
	if _, err := v.CreateJoinToken(ctx, connect.NewRequest(&agenv1.CreateJoinTokenRequest{})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer cannot mint join tokens: %v", err)
	}
	// An operator limited to namespace "team-a".
	op, _ := adm.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "op", Scopes: []string{"operator"}, Namespaces: []string{"team-a"}}))
	o := e.client(op.Msg.Secret)
	if _, err := o.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Namespace: "team-a", BundleFiles: helloFiles(t)})); err != nil {
		t.Fatalf("in-namespace create: %v", err)
	}
	if _, err := o.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Namespace: "team-b", BundleFiles: helloFiles(t)})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("cross-namespace create: %v", err)
	}
	if _, err := adm.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Namespace: "team-b", BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	list, err := o.ListDeployments(ctx, connect.NewRequest(&agenv1.ListDeploymentsRequest{}))
	if err != nil || len(list.Msg.Deployments) != 1 || list.Msg.Deployments[0].Namespace != "team-a" {
		t.Fatalf("namespace-filtered list: %v %v", list, err)
	}
	// Revoked tokens stop working.
	if _, err := adm.RevokeApiToken(ctx, connect.NewRequest(&agenv1.RevokeApiTokenRequest{Id: viewer.Msg.Token.Id})); err != nil {
		t.Fatal(err)
	}
	if _, err := v.ListDeployments(ctx, connect.NewRequest(&agenv1.ListDeploymentsRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked: %v", err)
	}
	tokens, _ := adm.ListApiTokens(ctx, connect.NewRequest(&agenv1.ListApiTokensRequest{}))
	if len(tokens.Msg.Tokens) != 2 {
		t.Fatalf("%v", tokens.Msg.Tokens)
	}
}

func TestDeploymentLifecycle(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	bad := helloFiles(t)
	bad["x-agen/harness.json"] = []byte(`{"provider":"nope","model":"m"}`)
	_, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: bad}))
	if code(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "harness.json") {
		t.Fatalf("invalid bundle: %v", err)
	}
	created, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)}))
	if err != nil {
		t.Fatal(err)
	}
	d := created.Msg.Deployment
	if d.Namespace != "default" || d.Name != "hello" || d.Kind != agenv1.DeploymentKind_DEPLOYMENT_KIND_POOL || d.Scale.Max != 3 ||
		d.Scale.IdleTimeoutSeconds != 300 || d.Desired != 0 || !strings.HasPrefix(d.DefinitionDigest, "sha256:") {
		t.Fatalf("%v", d)
	}
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); code(err) != connect.CodeAlreadyExists {
		t.Fatalf("duplicate: %v", err)
	}
	def, err := c.GetDefinition(ctx, connect.NewRequest(&agenv1.GetDefinitionRequest{Digest: d.DefinitionDigest}))
	if err != nil || len(def.Msg.Definition.Files) != len(helloFiles(t)) {
		t.Fatalf("definition: %v", err)
	}
	s, err := c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "hello"), Desired: 2}))
	if err != nil || s.Msg.Deployment.Desired != 2 {
		t.Fatalf("scale: %v", err)
	}
	if _, err := c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "hello"), Desired: 9})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("scale beyond max: %v", err)
	}
	// New bundle version + narrower scale clamps desired.
	v2 := helloFiles(t)
	v2["x-agen/agent.md"] = []byte("---\nname: hello\ndescription: Greets people\nskills: [greeting]\n---\nBe brief.\n")
	u, err := c.UpdateDeployment(ctx, connect.NewRequest(&agenv1.UpdateDeploymentRequest{Ref: ref("", "hello"), BundleFiles: v2,
		Scale: &agenv1.ScalePolicy{Min: 0, Max: 1}}))
	if err != nil {
		t.Fatal(err)
	}
	if u.Msg.Deployment.DefinitionDigest == d.DefinitionDigest || u.Msg.Deployment.Desired != 1 || u.Msg.Deployment.Generation <= d.Generation {
		t.Fatalf("update: %v", u.Msg.Deployment)
	}
	defs, _ := c.ListDefinitions(ctx, connect.NewRequest(&agenv1.ListDefinitionsRequest{Name: "hello"}))
	if len(defs.Msg.Definitions) != 2 {
		t.Fatalf("definitions: %d", len(defs.Msg.Definitions))
	}
	// Singletons cannot scale beyond one.
	single := helloFiles(t)
	single["x-agen/config.json"] = []byte(`{"kind":"singleton"}`)
	sd, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "solo", BundleFiles: single}))
	if err != nil || sd.Msg.Deployment.Kind != agenv1.DeploymentKind_DEPLOYMENT_KIND_SINGLETON || sd.Msg.Deployment.Scale.Max != 1 {
		t.Fatalf("singleton: %v %v", sd, err)
	}
	if _, err := c.DeleteDeployment(ctx, connect.NewRequest(&agenv1.DeleteDeploymentRequest{Ref: ref("", "hello")})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetDeployment(ctx, connect.NewRequest(&agenv1.GetDeploymentRequest{Ref: ref("", "hello")})); code(err) != connect.CodeNotFound {
		t.Fatalf("deleted: %v", err)
	}
}

func TestTasksWakeAndResolve(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "missing"), Input: "x"})); code(err) != connect.CodeNotFound {
		t.Fatalf("unknown deployment: %v", err)
	}
	sub, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "hi", IdempotencyKey: "req-1"}))
	if err != nil || sub.Msg.Task.State != agenv1.TaskState_TASK_STATE_QUEUED {
		t.Fatal(err)
	}
	again, _ := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "hi", IdempotencyKey: "req-1"}))
	if again.Msg.Task.Id != sub.Msg.Task.Id {
		t.Fatal("idempotent submit")
	}
	// A nest finishes the task while the client waits for it.
	go func() {
		time.Sleep(100 * time.Millisecond)
		leased, _ := e.store.LeaseTasks(ctx, "nest-1", "default", "hello", 1, 60_000)
		_ = e.store.CompleteTask(ctx, leased[0].ID, leased[0].LeaseID, true, "Hello!", "", "run-1", "inst-1")
	}()
	got, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: sub.Msg.Task.Id, WaitSeconds: 5}))
	if err != nil || got.Msg.Task.State != agenv1.TaskState_TASK_STATE_SUCCEEDED || got.Msg.Task.Output != "Hello!" {
		t.Fatalf("wait: %v %v", got, err)
	}
	second, _ := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "later"}))
	cancelled, _ := c.CancelTask(ctx, connect.NewRequest(&agenv1.CancelTaskRequest{Id: second.Msg.Task.Id}))
	if cancelled.Msg.Task.State != agenv1.TaskState_TASK_STATE_CANCELLED {
		t.Fatal(cancelled.Msg.Task.State)
	}
	list, _ := c.ListTasks(ctx, connect.NewRequest(&agenv1.ListTasksRequest{Deployment: "hello"}))
	if len(list.Msg.Tasks) != 2 {
		t.Fatalf("tasks: %d", len(list.Msg.Tasks))
	}

	w, err := c.RequestWake(ctx, connect.NewRequest(&agenv1.RequestWakeRequest{Ref: ref("", "hello")}))
	if err != nil || w.Msg.Deployment.Desired != 1 {
		t.Fatalf("wake: %v", err)
	}
	nest, _ := e.store.UpsertNest(ctx, store.Nest{Name: "n1", Backend: "native", Capacity: 4, GatewayURL: "http://127.0.0.1:7801"})
	epoch, _ := e.store.AcquireLease(ctx, e.store.LeaderLeaseName(), "test", 60_000)
	_ = e.store.SetAssignments(ctx, epoch, "default", "hello", []store.Assignment{{NestID: nest.ID, Count: 1, DefinitionDigest: "d"}})
	_ = e.store.ReportInstances(ctx, nest.ID, []store.Instance{{ID: "i1", Namespace: "default", Deployment: "hello", State: "ready"}})
	r, err := c.Resolve(ctx, connect.NewRequest(&agenv1.ResolveRequest{Ref: ref("", "hello")}))
	if err != nil || r.Msg.Ready != 1 || len(r.Msg.Endpoints) != 1 || r.Msg.Endpoints[0] != "http://127.0.0.1:7801/a2a/default/hello" {
		t.Fatalf("resolve: %v %v", r, err)
	}
	nests, _ := c.ListNests(ctx, connect.NewRequest(&agenv1.ListNestsRequest{}))
	if len(nests.Msg.Nests) != 1 || nests.Msg.Nests[0].Used != 1 {
		t.Fatalf("nests: %v", nests.Msg.Nests)
	}
	insts, _ := c.ListInstances(ctx, connect.NewRequest(&agenv1.ListInstancesRequest{Deployment: "hello"}))
	if len(insts.Msg.Instances) != 1 || insts.Msg.Instances[0].State != agenv1.InstanceState_INSTANCE_STATE_READY {
		t.Fatalf("instances: %v", insts.Msg.Instances)
	}
}

func TestApprovalsCannotBeSelfDecided(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adm := e.client(admin)
	alice, _ := adm.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "alice", Scopes: []string{"operator", "approver"}}))
	bob, _ := adm.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "bob", Scopes: []string{"approver"}}))
	// Alice's task caused the approval (requested_by is her principal id).
	a, err := e.store.CreateApproval(ctx, store.Approval{Namespace: "default", Deployment: "d", Tool: "pay",
		RequestedBy: "token:" + alice.Msg.Token.Id, Arguments: json.RawMessage(`{"to":"bob"}`)}, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.client(alice.Msg.Secret).DecideApproval(ctx, connect.NewRequest(&agenv1.DecideApprovalRequest{Id: a.ID, Approve: true})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("self-approval: %v", err)
	}
	pending, _ := e.client(bob.Msg.Secret).ListApprovals(ctx, connect.NewRequest(&agenv1.ListApprovalsRequest{State: agenv1.ApprovalState_APPROVAL_STATE_PENDING}))
	if len(pending.Msg.Approvals) != 1 || pending.Msg.Approvals[0].Arguments.Fields["to"].GetStringValue() != "bob" {
		t.Fatalf("%v", pending.Msg.Approvals)
	}
	d, err := e.client(bob.Msg.Secret).DecideApproval(ctx, connect.NewRequest(&agenv1.DecideApprovalRequest{Id: a.ID, Approve: true}))
	if err != nil || d.Msg.Approval.State != agenv1.ApprovalState_APPROVAL_STATE_APPROVED || d.Msg.Approval.DecidedBy != "token:"+bob.Msg.Token.Id {
		t.Fatalf("decide: %v %v", d, err)
	}
}

func TestRunsTracesAndLogsFromEngineTables(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	db := e.store.DB()
	now := time.Now().UnixMilli()
	mustExec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec("INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ('s1','hello','default','hello','{}',$1,$1)", now)
	mustExec("INSERT INTO conversations (id, session_id, created_ms) VALUES ('c1','s1',$1)", now)
	mustExec("INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, output, trace_id, started_ms, ended_ms, input_tokens, output_tokens) VALUES ('r1','s1','c1','default','hello','succeeded','hi','Hello!','t1',$1,$2,10,3)", now, now+5)
	mustExec("INSERT INTO spans (span_id, trace_id, parent_span_id, run_id, name, start_ms, end_ms, status, attributes, seq) VALUES ('a','t1','','r1','agen.run',$1,$2,'ok','{}',0)", now, now+5)
	mustExec("INSERT INTO spans (span_id, trace_id, parent_span_id, run_id, name, start_ms, end_ms, status, attributes, seq) VALUES ('b','t1','a','r1','gen_ai.chat',$1,$2,'ok','{\"gen_ai.request.model\":\"fake-1\"}',1)", now, now+2)
	mustExec("INSERT INTO logs (instance_id, namespace, deployment, time_ms, level, message) VALUES ('i1','default','hello',$1,'warn','retrying')", now)
	c := e.client(admin)
	runs, err := c.ListRuns(ctx, connect.NewRequest(&agenv1.ListRunsRequest{Deployment: "hello"}))
	if err != nil || len(runs.Msg.Runs) != 1 || runs.Msg.Runs[0].Usage.InputTokens != 10 || runs.Msg.Runs[0].EndedAt == nil {
		t.Fatalf("runs: %v %v", runs, err)
	}
	tr, err := c.GetTrace(ctx, connect.NewRequest(&agenv1.GetTraceRequest{TraceId: "t1"}))
	if err != nil || len(tr.Msg.Spans) != 2 || tr.Msg.Spans[0].Name != "agen.run" || tr.Msg.Spans[1].Attributes.Fields["gen_ai.request.model"].GetStringValue() != "fake-1" {
		t.Fatalf("trace: %v %v", tr, err)
	}
	logs, err := c.GetLogs(ctx, connect.NewRequest(&agenv1.GetLogsRequest{Ref: ref("", "hello")}))
	if err != nil || len(logs.Msg.Lines) != 1 || logs.Msg.Lines[0].Message != "retrying" {
		t.Fatalf("logs: %v %v", logs, err)
	}
}

// The same API answers plain JSON over HTTP (what curl, the UI and the CLI use).
func TestPlainJSONOverHTTP(t *testing.T) {
	e := setup(t)
	body, _ := json.Marshal(map[string]any{"bundleFiles": encodeFiles(helloFiles(t))})
	req, _ := http.NewRequest("POST", e.url+"/agen.v1.HubService/CreateDeployment", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"name":"hello"`) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	req, _ = http.NewRequest("POST", e.url+"/agen.v1.HubService/ListDeployments", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated JSON call: %d", resp.StatusCode)
	}
}

func encodeFiles(files map[string][]byte) map[string]string {
	out := map[string]string{}
	for k, v := range files {
		out[k] = b64(v)
	}
	return out
}
