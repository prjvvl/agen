package hub

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/gen/agen/v1/agenv1connect"
	"github.com/prjvvl/agen/platform/internal/store"
)

// runTree records a root run (r1 on hello) that delegated to r2 (on
// helper), with a short conversation and spans, as the engine would.
func runTree(t *testing.T, e env, now int64) {
	t.Helper()
	db := e.store.DB()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ('s1','hello','default','hello','{}',$1,$1), ('s2','helper','default','helper','{}',$1,$1)", now)
	exec("INSERT INTO conversations (id, session_id, created_ms) VALUES ('c1','s1',$1), ('c2','s2',$1)", now)
	exec("INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, trace_id, started_ms, ended_ms, input_tokens, output_tokens, cost_usd, root_run_id, labels, step) "+
		"VALUES ('r1','s1','c1','default','hello','succeeded','hi','t1',$1,$2,10,5,0.01,'r1','{\"team\":\"ops\"}',2)", now, now+900)
	exec("INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, trace_id, started_ms, ended_ms, input_tokens, output_tokens, cost_usd, root_run_id, parent_run_id, error) "+
		"VALUES ('r2','s2','c2','default','helper','failed','sub','t1',$1,$2,4,1,0.02,'r1','r1','boom')", now+100, now+300)
	exec("INSERT INTO messages (conversation_id, seq, run_id, body, created_ms) VALUES "+
		"('c1',1,'r0','{\"role\":\"user\",\"content\":\"earlier\"}',$1),"+
		"('c1',2,'r1','{\"role\":\"user\",\"content\":\"hi\"}',$1),"+
		"('c1',3,'r1','{\"role\":\"assistant\",\"content\":\"\",\"tool_calls\":[{\"id\":\"k1\",\"name\":\"files.read\",\"arguments\":{\"path\":\"a.txt\"}}]}',$1),"+
		"('c1',4,'r1','{\"role\":\"tool\",\"content\":\"contents\",\"tool_call_id\":\"k1\"}',$1),"+
		"('c1',5,'r1','{\"role\":\"assistant\",\"content\":\"Hello!\"}',$1)", now)
}

func TestListRunsFiltersPagesAndTreeUsage(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	runTree(t, e, now)
	c := e.client(admin)
	roots, err := c.ListRuns(ctx, connect.NewRequest(&agenv1.ListRunsRequest{RootsOnly: true}))
	if err != nil || len(roots.Msg.Runs) != 1 {
		t.Fatalf("roots: %v %v", roots, err)
	}
	r := roots.Msg.Runs[0]
	if r.Id != "r1" || r.TreeRuns != 2 || r.TreeUsage.InputTokens != 14 || r.TreeUsage.CostUsd < 0.029 || r.Steps != 2 {
		t.Fatalf("root run: %+v", r)
	}
	failed, err := c.ListRuns(ctx, connect.NewRequest(&agenv1.ListRunsRequest{Status: "failed"}))
	if err != nil || len(failed.Msg.Runs) != 1 || failed.Msg.Runs[0].Error != "boom" {
		t.Fatalf("failed: %v %v", failed, err)
	}
	labelled, err := c.ListRuns(ctx, connect.NewRequest(&agenv1.ListRunsRequest{Labels: map[string]string{"team": "ops"}}))
	if err != nil || len(labelled.Msg.Runs) != 1 || labelled.Msg.Runs[0].Id != "r1" {
		t.Fatalf("labels: %v %v", labelled, err)
	}
	none, _ := c.ListRuns(ctx, connect.NewRequest(&agenv1.ListRunsRequest{Labels: map[string]string{"team": "o%"}}))
	if len(none.Msg.Runs) != 0 {
		t.Fatalf("a LIKE wildcard in a label value matched: %v", none.Msg.Runs)
	}
	later, _ := c.ListRuns(ctx, connect.NewRequest(&agenv1.ListRunsRequest{Since: timestamppb.New(time.UnixMilli(now + 50))}))
	if len(later.Msg.Runs) != 1 || later.Msg.Runs[0].Id != "r2" {
		t.Fatalf("since: %v", later.Msg.Runs)
	}
	page1, err := c.ListRuns(ctx, connect.NewRequest(&agenv1.ListRunsRequest{Limit: 1}))
	if err != nil || len(page1.Msg.Runs) != 1 || page1.Msg.Runs[0].Id != "r2" || page1.Msg.NextPageToken == "" {
		t.Fatalf("page 1: %v %v", page1, err)
	}
	page2, err := c.ListRuns(ctx, connect.NewRequest(&agenv1.ListRunsRequest{Limit: 1, PageToken: page1.Msg.NextPageToken}))
	if err != nil || len(page2.Msg.Runs) != 1 || page2.Msg.Runs[0].Id != "r1" || page2.Msg.NextPageToken != "" {
		t.Fatalf("page 2: %v %v", page2, err)
	}
	if _, err := c.ListRuns(ctx, connect.NewRequest(&agenv1.ListRunsRequest{Status: "weird"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad status: %v", err)
	}
}

func TestTranscriptAndSystemPrompt(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	d, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)}))
	if err != nil {
		t.Fatal(err)
	}
	runTree(t, e, time.Now().UnixMilli())
	if _, err := e.store.DB().Exec("UPDATE runs SET definition_digest = $1 WHERE id = 'r1'", d.Msg.Deployment.DefinitionDigest); err != nil {
		t.Fatal(err)
	}
	tr, err := c.GetTranscript(ctx, connect.NewRequest(&agenv1.GetTranscriptRequest{RunId: "r1"}))
	if err != nil {
		t.Fatal(err)
	}
	m := tr.Msg.Messages
	if len(m) != 4 || m[0].Role != "user" || m[1].ToolCalls[0].Name != "files.read" ||
		m[1].ToolCalls[0].Arguments.GetStructValue().Fields["path"].GetStringValue() != "a.txt" || m[2].ToolCallId != "k1" || m[3].Seq != 5 {
		t.Fatalf("transcript: %v", m)
	}
	if !strings.Contains(tr.Msg.SystemPrompt, "friendly assistant") || strings.Contains(tr.Msg.SystemPrompt, "---") {
		t.Fatalf("system prompt: %q", tr.Msg.SystemPrompt)
	}
	hist, err := c.GetTranscript(ctx, connect.NewRequest(&agenv1.GetTranscriptRequest{RunId: "r1", IncludeHistory: true}))
	if err != nil || len(hist.Msg.Messages) != 5 || hist.Msg.Messages[0].Content != "earlier" {
		t.Fatalf("history: %v %v", hist, err)
	}
	other, err := c.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "other", Scopes: []string{"viewer"}, Namespaces: []string{"other"}}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.client(other.Msg.Secret).GetTranscript(ctx, connect.NewRequest(&agenv1.GetTranscriptRequest{RunId: "r1"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("another namespace's transcript: %v", err)
	}
}

func TestMetricsAndCallEdges(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	runTree(t, e, time.Now().UnixMilli()-1000)
	m, err := e.client(admin).GetMetrics(ctx, connect.NewRequest(&agenv1.GetMetricsRequest{WindowSeconds: 3600, Buckets: 6}))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Msg.Deployments) != 2 || m.Msg.BucketSeconds != 600 {
		t.Fatalf("metrics: %v", m.Msg)
	}
	hello, helper := m.Msg.Deployments[0], m.Msg.Deployments[1]
	if hello.Ref.Name != "hello" || hello.Runs != 1 || hello.P50Ms != 900 || len(hello.Buckets) != 6 || hello.Buckets[5].Runs != 1 {
		t.Fatalf("hello: %v", hello)
	}
	if helper.Failed != 1 || helper.Usage.CostUsd < 0.019 {
		t.Fatalf("helper: %v", helper)
	}
	if len(m.Msg.Edges) != 1 || m.Msg.Edges[0].From.Name != "hello" || m.Msg.Edges[0].To.Name != "helper" || m.Msg.Edges[0].Failed != 1 {
		t.Fatalf("edges: %v", m.Msg.Edges)
	}
}

func TestQueueCapRefusesTasks(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t),
		Limits: &agenv1.Limits{MaxQueuedTasks: 2}})); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x"})); err != nil {
			t.Fatal(err)
		}
	}
	_, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x"}))
	if code(err) != connect.CodeResourceExhausted {
		t.Fatalf("third task: %v", err)
	}
	d, _ := c.GetDeployment(ctx, connect.NewRequest(&agenv1.GetDeploymentRequest{Ref: ref("", "hello")}))
	if l := d.Msg.Deployment.Limits; l.MaxQueuedTasks != 2 || l.MaxDelegationDepth != 3 || l.MaxFanOut != 20 {
		t.Fatalf("limits: %v", l)
	}
}

// A task waiting for an approval shows it, and GetTask returns early when
// the wait begins.
func TestGetTaskReturnsWhileWaitingForApproval(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	task, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	db := e.store.DB()
	for _, q := range []string{
		"INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ('s1','hello','default','hello','{}',$1,$1)",
		"INSERT INTO conversations (id, session_id, created_ms) VALUES ('c1','s1',$1)",
	} {
		if _, err := db.Exec(q, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, started_ms, task_id) VALUES ('r1','s1','c1','default','hello','waiting_approval','x',$1,$2)",
		now, task.Msg.Task.Id); err != nil {
		t.Fatal(err)
	}
	created := make(chan store.Approval, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		a, _ := e.store.CreateApproval(ctx, store.Approval{ID: store.NewID(), Namespace: "default", Deployment: "hello", RunID: "r1", Tool: "files.write", Arguments: []byte("{}")}, time.Hour.Milliseconds())
		created <- a
	}()
	start := time.Now()
	got, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: task.Msg.Task.Id, WaitSeconds: 30}))
	a := <-created
	if err != nil || got.Msg.Task.PendingApprovalId == "" || got.Msg.Task.PendingApprovalId != a.ID || time.Since(start) > 5*time.Second {
		t.Fatalf("get task: %v %v after %v", got, err, time.Since(start))
	}
	// Already waiting: a new call waits as usual (and still shows the approval).
	start = time.Now()
	got, err = c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: task.Msg.Task.Id, WaitSeconds: 1}))
	if err != nil || got.Msg.Task.PendingApprovalId != a.ID || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("second wait: %v %v after %v", got, err, time.Since(start))
	}
	list, err := c.ListTasks(ctx, connect.NewRequest(&agenv1.ListTasksRequest{}))
	if err != nil || list.Msg.Tasks[0].PendingApprovalId != a.ID {
		t.Fatalf("list tasks: %v %v", list, err)
	}
}

// An on-behalf token acts for the submitter of a running task, with the
// scopes both hold, and can never decide approvals.
func TestOnBehalfToken(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adm := e.client(admin)
	if _, err := adm.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, scopes []string, onBehalf bool) string {
		r, err := adm.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: name, Scopes: scopes, OnBehalf: onBehalf}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg.Secret
	}
	assistant := mk("assistant", []string{"operator", "approver"}, true)
	viewer := mk("ana", []string{"viewer"}, false)
	operator := mk("olu", []string{"operator", "approver"}, false)
	task := func(tok string) string {
		t.Helper()
		// Submitting needs operator; a viewer's chat task is written directly.
		if tok == viewer {
			id := store.NewID()
			if _, err := e.store.SubmitTask(ctx, store.Task{ID: id, Namespace: "default", Deployment: "hello", Input: "x", SubmittedBy: "token:" + tokenID(t, e, "ana")}); err != nil {
				t.Fatal(err)
			}
			return id
		}
		r, err := e.client(tok).SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x"}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg.Task.Id
	}
	as := func(taskID string) agenv1HubClient {
		return e.clientWith(assistant, map[string]string{TaskHeader: taskID})
	}
	if _, err := e.client(assistant).ListDeployments(ctx, connect.NewRequest(&agenv1.ListDeploymentsRequest{})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("without a task: %v", err)
	}
	vt := task(viewer)
	if _, err := as(vt).ListDeployments(ctx, connect.NewRequest(&agenv1.ListDeploymentsRequest{})); err != nil {
		t.Fatalf("read for a viewer: %v", err)
	}
	if _, err := as(vt).ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "hello"), Desired: 1})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("a viewer's assistant changed something: %v", err)
	}
	ot := task(operator)
	if _, err := as(ot).ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "hello"), Desired: 1})); err != nil {
		t.Fatalf("operator's assistant: %v", err)
	}
	me, err := as(ot).WhoAmI(ctx, connect.NewRequest(&agenv1.WhoAmIRequest{}))
	if err != nil || me.Msg.Name != "olu" {
		t.Fatalf("who am i: %v %v", me, err)
	}
	if _, err := as(ot).DecideApproval(ctx, connect.NewRequest(&agenv1.DecideApprovalRequest{Id: "x", Approve: true})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("decide approval: %v", err)
	}
	if _, err := as(ot).CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "x", Scopes: []string{"viewer"}})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("admin call: %v", err)
	}
	if _, err := adm.CancelTask(ctx, connect.NewRequest(&agenv1.CancelTaskRequest{Id: ot})); err != nil {
		t.Fatal(err)
	}
	if _, err := as(ot).ListDeployments(ctx, connect.NewRequest(&agenv1.ListDeploymentsRequest{})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("finished task: %v", err)
	}
}

func tokenID(t *testing.T, e env, name string) string {
	t.Helper()
	list, err := e.store.ListAPITokens(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range list {
		if tok.Name == name {
			return tok.ID
		}
	}
	t.Fatalf("no token %s", name)
	return ""
}

func TestNotificationTargets(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	var mu sync.Mutex
	var got []map[string]any
	var sigs []string
	var bodies [][]byte
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var v map[string]any
		_ = json.Unmarshal(b, &v)
		mu.Lock()
		got, sigs, bodies = append(got, v), append(sigs, r.Header.Get("X-Agen-Signature")), append(bodies, b)
		mu.Unlock()
	}))
	defer recv.Close()
	if _, err := c.SetNotificationTarget(ctx, connect.NewRequest(&agenv1.SetNotificationTargetRequest{Target: &agenv1.NotificationTarget{Name: "Bad Name", Url: recv.URL}})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad name: %v", err)
	}
	if _, err := c.SetNotificationTarget(ctx, connect.NewRequest(&agenv1.SetNotificationTargetRequest{Target: &agenv1.NotificationTarget{Name: "ops", Url: recv.URL, Events: []string{"nope"}}})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad event: %v", err)
	}
	set, err := c.SetNotificationTarget(ctx, connect.NewRequest(&agenv1.SetNotificationTargetRequest{Target: &agenv1.NotificationTarget{Name: "ops", Url: recv.URL, Events: []string{"task.failed"}}}))
	if err != nil || set.Msg.Secret == "" {
		t.Fatalf("set: %v %v", set, err)
	}
	list, err := c.ListNotificationTargets(ctx, connect.NewRequest(&agenv1.ListNotificationTargetsRequest{}))
	if err != nil || len(list.Msg.Targets) != 1 || list.Msg.Targets[0].Namespace != "default" {
		t.Fatalf("list: %v %v", list, err)
	}
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	task, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.DB().Exec("UPDATE tasks SET state = 'failed', error = 'boom' WHERE id = $1", task.Msg.Task.Id); err != nil {
		t.Fatal(err)
	}
	e.hub.afterTaskEnded(ctx, task.Msg.Task.Id)
	e.hub.notifyApproval(ctx, store.Approval{ID: "a1", Namespace: "default", Deployment: "hello", Tool: "x", Arguments: []byte("{}"), State: "pending"})
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0]["type"] != "task.failed" || got[0]["task"].(map[string]any)["error"] != "boom" {
		t.Fatalf("notifications: %v", got)
	}
	mac := hmac.New(sha256.New, []byte(set.Msg.Secret))
	mac.Write(bodies[0])
	if sigs[0] != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("signature %q", sigs[0])
	}
	if _, err := c.DeleteNotificationTarget(ctx, connect.NewRequest(&agenv1.DeleteNotificationTargetRequest{Name: "ops"})); err != nil {
		t.Fatal(err)
	}
}

func TestEventStream(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	e.hub.feed().interval = 50 * time.Millisecond
	path, h := e.hub.EventsHandler()
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	get := func(tok string) *http.Response {
		req, _ := http.NewRequest("GET", srv.URL+"/api/v1/events", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if r := get("wrong"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", r.StatusCode)
	}
	resp := get(admin)
	defer resp.Body.Close()
	lines := bufio.NewScanner(resp.Body)
	next := func() string {
		t.Helper()
		for lines.Scan() {
			if l := lines.Text(); strings.HasPrefix(l, "data: ") {
				return strings.TrimPrefix(l, "data: ")
			}
		}
		t.Fatal("stream ended")
		return ""
	}
	if first := next(); first != "{}" {
		t.Fatalf("ready: %s", first)
	}
	time.Sleep(200 * time.Millisecond) // the watcher has taken its first look
	task, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for {
		ch := make(chan string, 1)
		go func() { ch <- next() }()
		select {
		case data := <-ch:
			var ev Event
			_ = json.Unmarshal([]byte(data), &ev)
			if ev.Kind == "task" && ev.ID == task.Msg.Task.Id && ev.State == "queued" && ev.Deployment == "hello" {
				return
			}
		case <-deadline:
			t.Fatal("no task event")
		}
	}
}

type agenv1HubClient = agenv1connect.HubServiceClient

// clientWith is client with extra request headers.
func (e env) clientWith(token string, hdr map[string]string) agenv1HubClient {
	return agenv1connect.NewHubServiceClient(http.DefaultClient, e.url, connect.WithInterceptors(connect.UnaryInterceptorFunc(
		func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				req.Header().Set("Authorization", "Bearer "+token)
				for k, v := range hdr {
					req.Header().Set(k, v)
				}
				return next(ctx, req)
			}
		})), connect.WithProtoJSON())
}
