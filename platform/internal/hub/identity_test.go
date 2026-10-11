package hub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

func sign(key, body string, b64 bool) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(body))
	if b64 {
		return base64.StdEncoding.EncodeToString(m.Sum(nil))
	}
	return hex.EncodeToString(m.Sum(nil))
}

func TestWebhookSignaturesAndKeys(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: withTriggers(t, `[
		{"type":"webhook","name":"gh","labels":{"source":"github"},
		 "auth":{"type":"hmac","header":"X-Hub-Signature-256","prefix":"sha256=","secret":"GH_SECRET"},
		 "idempotencyKey":{"header":"X-GitHub-Delivery"},"conversationKey":{"field":"issue.number"}},
		{"type":"webhook","name":"b64","auth":{"type":"hmac","header":"X-Sig","encoding":"base64","secret":"GH_SECRET"}}]`)})); err != nil {
		t.Fatal(err)
	}
	post := func(path string, hdr map[string]string, body string) (int, map[string]string) {
		req, _ := http.NewRequest(http.MethodPost, e.url+path, bytes.NewBufferString(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]string
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	body := `{"action":"opened","issue":{"number":42}}`
	// The secret is not set yet: a configuration error, not shown to callers.
	st, out := post("/hooks/default/hello/gh", map[string]string{"X-Hub-Signature-256": "sha256=" + sign("k", body, false)}, body)
	if st != http.StatusInternalServerError || strings.Contains(out["error"], "GH_SECRET") {
		t.Fatalf("missing secret: %d %v", st, out)
	}
	// A secret limited to another deployment is refused.
	c.SetSecret(ctx, connect.NewRequest(&agenv1.SetSecretRequest{Name: "GH_SECRET", Value: "s3cret", Deployments: []string{"other"}}))
	if st, _ := post("/hooks/default/hello/gh", map[string]string{"X-Hub-Signature-256": "sha256=" + sign("s3cret", body, false)}, body); st != http.StatusInternalServerError {
		t.Fatalf("secret of another deployment: %d", st)
	}
	c.SetSecret(ctx, connect.NewRequest(&agenv1.SetSecretRequest{Name: "GH_SECRET", Value: "s3cret", Deployments: []string{"hello"}}))
	if _, err := c.CreateWebhookSecret(ctx, connect.NewRequest(&agenv1.CreateWebhookSecretRequest{Ref: ref("", "hello"), Trigger: "gh"})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("bearer secret for an hmac trigger: %v", err)
	}
	for _, sig := range []string{"", "sha256=zz", "sha256=" + sign("wrong", body, false), sign("s3cret", body, false)} {
		if st, _ := post("/hooks/default/hello/gh", map[string]string{"X-Hub-Signature-256": sig}, body); st != http.StatusUnauthorized {
			t.Fatalf("signature %q: %d", sig, st)
		}
	}
	good := map[string]string{"X-Hub-Signature-256": "sha256=" + sign("s3cret", body, false), "X-GitHub-Delivery": "d-1"}
	st, out = post("/hooks/default/hello/gh", good, body)
	if st != http.StatusAccepted {
		t.Fatalf("signed: %d %v", st, out)
	}
	if _, again := post("/hooks/default/hello/gh", good, body); again["task_id"] != out["task_id"] {
		t.Fatalf("redelivery made a new task: %v vs %v", again, out)
	}
	task, _ := e.store.GetTask(ctx, out["task_id"])
	if task.ConversationKey != "webhook:gh:42" || task.Labels["source"] != "github" {
		t.Fatalf("task: %+v", task)
	}
	if st, _ := post("/hooks/default/hello/b64", map[string]string{"X-Sig": sign("s3cret", "hi", true)}, "hi"); st != http.StatusAccepted {
		t.Fatalf("base64 signature: %d", st)
	}
	if ev := events(t, c, "hello"); ev["gh:rejected"] != 1 || ev["gh:fired"] != 2 {
		t.Fatalf("events: %v", ev)
	}
	// A bad auth configuration is refused at deploy time.
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "bad", BundleFiles: withTriggers(t,
		`[{"type":"webhook","name":"x","auth":{"type":"hmac","header":"X-Sig","secret":"S","algorithm":"md5"}}]`)})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("md5: %v", err)
	}
}

func TestKeyFrom(t *testing.T) {
	h := http.Header{}
	h.Set("X-Id", " abc ")
	body := []byte(`{"a":{"b":7,"s":"x"},"top":"t"}`)
	for _, c := range []struct {
		src  *agenv1.KeySource
		want string
	}{
		{nil, ""},
		{&agenv1.KeySource{Header: "X-Id"}, "abc"},
		{&agenv1.KeySource{Header: "X-Missing", Field: "top"}, "t"},
		{&agenv1.KeySource{Field: "a.b"}, "7"},
		{&agenv1.KeySource{Field: "a.s"}, "x"},
		{&agenv1.KeySource{Field: "a"}, ""},
		{&agenv1.KeySource{Field: "a.b.c"}, ""},
	} {
		if got := keyFrom(c.src, h, body); got != c.want {
			t.Errorf("%v: %q, want %q", c.src, got, c.want)
		}
	}
	if got := keyFrom(&agenv1.KeySource{Field: "x"}, h, []byte("not json")); got != "" {
		t.Errorf("non-JSON body: %q", got)
	}
}

func TestConversationKeysLabelsAndLeasingOrder(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	submit := func(key string, labels map[string]string) (*agenv1.Task, error) {
		r, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "hi", ConversationKey: key, Labels: labels}))
		if err != nil {
			return nil, err
		}
		return r.Msg.Task, nil
	}
	if _, err := submit("k", map[string]string{"bad key!": "x"}); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad label: %v", err)
	}
	if _, err := submit(strings.Repeat("k", 257), nil); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("long key: %v", err)
	}
	a1, err := submit("chat", map[string]string{"project": "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if a1.ConversationKey != "api:chat" || a1.Labels["project"] != "p1" {
		t.Fatalf("task: %v", a1)
	}
	a2, _ := submit("chat", nil)
	b1, _ := submit("other", nil)
	free, _ := submit("", nil)
	// One task per conversation is in flight at a time, oldest first.
	got, err := e.store.LeaseTasks(ctx, "n1", "default", "hello", 10, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, tk := range got {
		ids[tk.ID] = true
	}
	if len(got) != 3 || !ids[a1.Id] || !ids[b1.Id] || !ids[free.Id] {
		t.Fatalf("first lease: %v", ids)
	}
	if more, _ := e.store.LeaseTasks(ctx, "n1", "default", "hello", 10, 60_000); len(more) != 0 {
		t.Fatalf("leased %s while %s is in flight", more[0].ID, a1.Id)
	}
	// Cancelling the first task ends its run, so the conversation is free.
	if _, err := e.store.DB().ExecContext(ctx, "INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ('s1','hello','default','hello','{}',1,1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.DB().ExecContext(ctx, "INSERT INTO conversations (id, session_id, created_ms) VALUES ('c1','s1',1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.DB().ExecContext(ctx, "INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, started_ms, task_id, owner, epoch) "+
		"VALUES ('r1','s1','c1','default','hello','running','hi',1,$1,'host-1',1)", a1.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CancelTask(ctx, connect.NewRequest(&agenv1.CancelTaskRequest{Id: a1.Id})); err != nil {
		t.Fatal(err)
	}
	run, err := e.store.GetRun(ctx, "r1")
	if err != nil || run.Status != "cancelled" || run.EndedMs == 0 {
		t.Fatalf("run after cancel: %+v %v", run, err)
	}
	next, _ := e.store.LeaseTasks(ctx, "n1", "default", "hello", 10, 60_000)
	if len(next) != 1 || next[0].ID != a2.Id || next[0].Labels != nil && len(next[0].Labels) != 0 {
		t.Fatalf("after cancel: %v", next)
	}
	// A task that fails ends its run too.
	if _, err := e.store.DB().ExecContext(ctx, "INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, started_ms, task_id, owner, epoch) "+
		"VALUES ('r2','s1','c1','default','hello','running','hi',2,$1,'host-1',1)", a2.Id); err != nil {
		t.Fatal(err)
	}
	if err := e.store.CompleteTask(ctx, a2.Id, next[0].LeaseID, false, "", "store unavailable", "r2", "i1"); err != nil {
		t.Fatal(err)
	}
	if run, err := e.store.GetRun(ctx, "r2"); err != nil || run.Status != "failed" || run.EndedMs == 0 {
		t.Fatalf("run after a failed task: %+v %v", run, err)
	}
}

func TestValidateOnlyGuideAndWhoAmI(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	text := map[string]string{}
	for p, b := range helloFiles(t) {
		text[p] = string(b)
	}
	r, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleText: text, ValidateOnly: true}))
	if err != nil || r.Msg.Deployment != nil {
		t.Fatalf("validate: %v %v", r, err)
	}
	joined := strings.Join(r.Msg.Warnings, "\n")
	if !strings.Contains(joined, "never calls a model") {
		t.Fatalf("warnings: %v", r.Msg.Warnings)
	}
	if l, _ := c.ListDeployments(ctx, connect.NewRequest(&agenv1.ListDeploymentsRequest{})); len(l.Msg.Deployments) != 0 {
		t.Fatal("validate_only deployed")
	}
	text["x-agen/config.json"] = `{"permissions":{"rules":[{"tool":"nothere.tool","action":"allow"}]},"delegates":[{"name":"ghost"}]}`
	r, err = c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleText: text, ValidateOnly: true}))
	if err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(r.Msg.Warnings, "\n")
	for _, want := range []string{`"nothere.tool" names no server`, `delegate "ghost" is not deployed`} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing warning %q in %v", want, r.Msg.Warnings)
		}
	}
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleText: map[string]string{"plugin.json": "{}"},
		BundleFiles: map[string][]byte{"plugin.json": []byte("{}")}})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("duplicate path: %v", err)
	}
	created, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleText: text}))
	if err != nil {
		t.Fatal(err)
	}
	def, err := c.GetDefinition(ctx, connect.NewRequest(&agenv1.GetDefinitionRequest{Digest: created.Msg.Deployment.DefinitionDigest}))
	if err != nil || def.Msg.TextFiles["x-agen/config.json"] != text["x-agen/config.json"] {
		t.Fatalf("text files: %v %v", def, err)
	}

	g, err := c.GetBundleGuide(ctx, connect.NewRequest(&agenv1.GetBundleGuideRequest{}))
	if err != nil || g.Msg.Guide == "" || g.Msg.Schemas["x-agen/config.json"] == "" {
		t.Fatalf("guide: %v", err)
	}
	example := map[string]string{}
	for p, s := range g.Msg.Example {
		example[p] = s
	}
	ex, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleText: example, ValidateOnly: true}))
	if err != nil || len(ex.Msg.Warnings) != 0 {
		t.Fatalf("the example bundle should validate cleanly: %v %v", ex, err)
	}

	tok, _ := c.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "ci", Scopes: []string{"viewer"}, Namespaces: []string{"team-a"}}))
	me, err := e.client(tok.Msg.Secret).WhoAmI(ctx, connect.NewRequest(&agenv1.WhoAmIRequest{}))
	if err != nil || me.Msg.Name != "ci" || me.Msg.Id != "token:"+tok.Msg.Token.Id || strings.Join(me.Msg.Scopes, ",") != "viewer" || me.Msg.Namespaces[0] != "team-a" {
		t.Fatalf("whoami: %v %v", me, err)
	}
	_, err = e.client(tok.Msg.Secret).SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("team-a", "hello"), Input: "x"}))
	if code(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "SubmitTask requires the operator scope; this token has: viewer") {
		t.Fatalf("scope error: %v", err)
	}
}

func TestApprovalRecordsAndNotifications(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	var mu sync.Mutex
	var got []*http.Request
	var bodies []string
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got, bodies = append(got, r), append(bodies, string(b))
		mu.Unlock()
	}))
	defer recv.Close()
	files := helloFiles(t)
	files["x-agen/config.json"] = []byte(`{"permissions":{"default":"ask","notify":{"url":"` + recv.URL + `","secret":"HOOK_KEY"}}}`)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: files})); err != nil {
		t.Fatal(err)
	}
	c.SetSecret(ctx, connect.NewRequest(&agenv1.SetSecretRequest{Name: "HOOK_KEY", Value: "hook-key-1", Deployments: []string{"hello"}}))
	alice, _ := c.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "alice", Scopes: []string{"operator", "approver"}}))
	bob, _ := c.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "bob", Scopes: []string{"approver"}}))
	a := store.Approval{ID: store.NewID(), Namespace: "default", Deployment: "hello", Tool: "pay", RequestedBy: "token:" + alice.Msg.Token.Id}
	a, err := e.store.CreateApproval(ctx, a, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	e.hub.notifyApproval(ctx, a)
	mu.Lock()
	if len(got) != 1 || got[0].Header.Get("X-Agen-Signature") != "sha256="+sign("hook-key-1", bodies[0], false) || !strings.Contains(bodies[0], `"requestedByName":"alice"`) {
		t.Fatalf("notification: %d %v", len(got), bodies)
	}
	mu.Unlock()

	_, err = e.client(alice.Msg.Secret).DecideApproval(ctx, connect.NewRequest(&agenv1.DecideApprovalRequest{Id: a.ID, Approve: true}))
	if code(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "nobody may approve their own request") || !strings.Contains(err.Error(), "alice") {
		t.Fatalf("self-approval: %v", err)
	}
	d, err := e.client(bob.Msg.Secret).DecideApproval(ctx, connect.NewRequest(&agenv1.DecideApprovalRequest{Id: a.ID, Approve: false}))
	if err != nil || d.Msg.Approval.DecidedByName != "bob" || d.Msg.Approval.RequestedByName != "alice" || d.Msg.Approval.DecidedAt == nil {
		t.Fatalf("decided: %v %v", d, err)
	}
	if _, err := e.client(bob.Msg.Secret).DecideApproval(ctx, connect.NewRequest(&agenv1.DecideApprovalRequest{Id: a.ID, Approve: true})); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "no longer pending (it is denied)") {
		t.Fatalf("decided twice: %v", err)
	}
	e.hub.notifyApproval(ctx, store.Approval{ID: "x", Namespace: "default", Deployment: "missing"})
}

func TestLifecycleLogs(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	in := store.Instance{ID: "i1", Namespace: "default", Deployment: "hello", State: "ready"}
	if err := e.store.ReportInstances(ctx, "n1", []store.Instance{in}); err != nil {
		t.Fatal(err)
	}
	in.State, in.Message = "failed", "not ready: mcp server fetch is disconnected"
	e.store.ReportInstances(ctx, "n1", []store.Instance{in})
	e.store.ReportInstances(ctx, "n1", nil)
	task, _ := e.store.SubmitTask(ctx, store.Task{Namespace: "default", Deployment: "hello", Input: "x"})
	leased, _ := e.store.LeaseTasks(ctx, "n1", "default", "hello", 1, 1)
	time.Sleep(5 * time.Millisecond)
	if _, _, err := e.store.RequeueExpiredLeases(ctx, 3); err != nil {
		t.Fatal(err)
	}
	leased, _ = e.store.LeaseTasks(ctx, "n1", "default", "hello", 1, 60_000)
	if err := e.store.ReleaseTask(ctx, task.ID, leased[0].LeaseID, false); err != nil {
		t.Fatal(err)
	}
	logs, err := e.store.Logs(ctx, "default", "hello", "", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range logs {
		lines = append(lines, l.Level+" "+l.Message)
	}
	all := strings.Join(lines, "\n")
	for _, want := range []string{
		"info instance i1 started on nest n1",
		"error instance i1 failed: not ready: mcp server fetch is disconnected",
		"info instance i1 exited",
		"warn task " + task.ID + " requeued: its lease on nest n1 expired on attempt 1",
		"warn task " + task.ID + " returned to the queue after attempt 2",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
}
