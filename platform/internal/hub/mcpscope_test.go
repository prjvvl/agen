package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/apidesc"
	"github.com/prjvvl/agen/platform/internal/mcpserver"
	"github.com/prjvvl/agen/platform/internal/store"
)

// expectedScope is the reviewed scope of every HubService method. A new
// method fails TestMCPScopeMatrix until it is classified here.
var expectedScope = map[string]string{
	"ListDeployments": ScopeViewer, "GetDeployment": ScopeViewer, "ListInstances": ScopeViewer, "ListNests": ScopeViewer,
	"GetTask": ScopeViewer, "ListTasks": ScopeViewer, "Resolve": ScopeViewer, "ListDefinitions": ScopeViewer,
	"GetDefinition": ScopeViewer, "ListTriggerEvents": ScopeViewer, "ListRuns": ScopeViewer, "GetTrace": ScopeViewer, "GetLogs": ScopeViewer,
	"WhoAmI": ScopeViewer, "GetBundleGuide": ScopeViewer,
	"CreateDeployment": ScopeOperator, "UpdateDeployment": ScopeOperator, "ScaleDeployment": ScopeOperator, "DeleteDeployment": ScopeOperator,
	"PauseDeployment": ScopeOperator, "SubmitTask": ScopeOperator, "CancelTask": ScopeOperator, "RequestWake": ScopeOperator,
	"CreateWebhookSecret": ScopeOperator,
	"ListApprovals":       ScopeApprover, "DecideApproval": ScopeApprover,
	"SetSecret": ScopeAdmin, "ListSecrets": ScopeAdmin, "DeleteSecret": ScopeAdmin, "CreateJoinToken": ScopeAdmin,
	"CreateApiToken": ScopeAdmin, "ListApiTokens": ScopeAdmin, "RevokeApiToken": ScopeAdmin, "RevokeNest": ScopeAdmin,
}

// Every MCP tool enforces exactly the scope its API call needs (MCP is a
// thin layer over the authenticated API), namespace-limited tokens stay in
// their namespaces, and nothing listed or returned carries secret values.
func TestMCPScopeMatrix(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.SetSecret(ctx, connect.NewRequest(&agenv1.SetSecretRequest{Name: "API_KEY", Value: "sk-mcp-canary-value", Deployments: []string{"hello"}})); err != nil {
		t.Fatal(err)
	}
	path, api := e.hub.Handler()
	mux := http.NewServeMux()
	mux.Handle(path, api)
	mcpH, err := mcpserver.Handler(mux)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mcpH)
	defer srv.Close()

	token := func(scopes []string, namespaces []string) string {
		r, err := c.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "mcp-" + strings.Join(scopes, "-"), Scopes: scopes, Namespaces: namespaces}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg.Secret
	}
	rpc := func(tok, method string, params any) map[string]any {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(raw), "sk-mcp-canary-value") {
			t.Fatalf("secret value in an MCP response: %s", raw)
		}
		var out map[string]any
		json.Unmarshal(raw, &out)
		return out
	}

	list := rpc("", "tools/list", map[string]any{})
	tools, _ := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) < 20 {
		t.Fatalf("tools/list: %d tools", len(tools))
	}
	principals := map[string]struct {
		tok    string
		scopes []string
	}{
		"none":     {"", nil},
		"viewer":   {token([]string{ScopeViewer}, nil), []string{ScopeViewer}},
		"operator": {token([]string{ScopeOperator}, nil), []string{ScopeOperator}},
		"approver": {token([]string{ScopeApprover}, nil), []string{ScopeApprover}},
		"admin":    {admin, []string{ScopeAdmin}},
	}
	procs := procedures(t)
	checked := 0
	for _, raw := range tools {
		tool := raw.(map[string]any)
		name := tool["name"].(string)
		procedure, ok := procs[name]
		if !ok {
			t.Fatalf("tool %s has no API procedure", name)
		}
		method := procedure[strings.LastIndex(procedure, "/")+1:]
		need, reviewed := expectedScope[method]
		if !reviewed {
			t.Errorf("tool %s (%s) has no reviewed scope in expectedScope", name, method)
			continue
		}
		if got := procedureScope(procedure); got != need {
			t.Errorf("%s needs %s, but the API enforces %s", method, need, got)
		}
		for who, p := range principals {
			res := rpc(p.tok, "tools/call", map[string]any{"name": name, "arguments": map[string]any{}})
			text := toolText(res)
			allowed := p.tok != "" && (Principal{Scopes: set(p.scopes)}).Can(need)
			denied := strings.Contains(text, `"code":"permission_denied"`) || strings.Contains(text, `"code":"unauthenticated"`)
			if allowed && denied {
				t.Errorf("%s as %s (needs %s): refused: %s", name, who, need, text)
			}
			if !allowed && !denied {
				t.Errorf("%s as %s (needs %s): not refused: %.200s", name, who, need, text)
			}
			checked++
		}
	}
	t.Logf("%d tools x %d principals checked", len(tools), len(principals))

	// Namespace-limited tokens stay in their namespace.
	teamA := token([]string{ScopeOperator}, []string{"team-a"})
	if text := toolText(rpc(teamA, "tools/call", map[string]any{"name": toolName(t, tools, "ListDeployments"), "arguments": map[string]any{"namespace": "team-b"}})); !strings.Contains(text, "permission_denied") {
		t.Fatalf("team-a token listed team-b: %s", text)
	}
	if text := toolText(rpc(teamA, "tools/call", map[string]any{"name": toolName(t, tools, "ListDeployments"), "arguments": map[string]any{"namespace": "team-a"}})); strings.Contains(text, "permission_denied") {
		t.Fatalf("team-a token refused in team-a: %s", text)
	}
	// Id-addressed calls check the namespace of what they name: a team-a
	// operator+approver cannot read, cancel, decide or trace team-b things.
	files := helloFiles(t)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Namespace: "team-b", Name: "hello", BundleFiles: files})); err != nil {
		t.Fatal(err)
	}
	task, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: &agenv1.DeploymentRef{Namespace: "team-b", Name: "hello"}, Input: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for _, q := range []string{
		"INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ('sb','hello','team-b','hello','{}',$1,$1)",
		"INSERT INTO conversations (id, session_id, created_ms) VALUES ('cb','sb',$1)",
		"INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, started_ms, trace_id) VALUES ('rb','sb','cb','team-b','hello','running','x',$1,'trace-team-b')",
	} {
		if _, err := e.store.DB().ExecContext(ctx, q, now); err != nil {
			t.Fatal(err)
		}
	}
	ap, err := e.store.CreateApproval(ctx, store.Approval{Namespace: "team-b", Deployment: "hello", RunID: "rb", Tool: "pay", RequestedBy: "token:someone"}, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	teamAOps := token([]string{ScopeOperator, ScopeApprover}, []string{"team-a"})
	for method, args := range map[string]map[string]any{
		"GetTask":         {"id": task.Msg.Task.Id},
		"CancelTask":      {"id": task.Msg.Task.Id},
		"DecideApproval":  {"id": ap.ID, "approve": true},
		"GetLogs":         {"ref": map[string]any{"namespace": "team-b", "name": "hello"}},
		"ScaleDeployment": {"ref": map[string]any{"namespace": "team-b", "name": "hello"}, "desired": 3},
	} {
		text := toolText(rpc(teamAOps, "tools/call", map[string]any{"name": toolName(t, tools, method), "arguments": args}))
		if !strings.Contains(text, "permission_denied") && !strings.Contains(text, "not_found") {
			t.Errorf("team-a token %s on a team-b object was not refused: %.200s", method, text)
		}
	}
	// A trace spanning namespaces is filtered: team-a sees none of team-b's
	// runs in it, while an admin does.
	traceArgs := map[string]any{"name": toolName(t, tools, "GetTrace"), "arguments": map[string]any{"traceId": "trace-team-b"}}
	if text := toolText(rpc(teamAOps, "tools/call", traceArgs)); strings.Contains(text, `"rb"`) {
		t.Errorf("team-a token saw a team-b run in a trace: %.200s", text)
	}
	if text := toolText(rpc(admin, "tools/call", traceArgs)); !strings.Contains(text, `"rb"`) {
		t.Errorf("admin trace lacks the team-b run (the filter check would be vacuous): %.200s", text)
	}
	// Secret metadata is admin-only, and even admins never get values back.
	if text := toolText(rpc(admin, "tools/call", map[string]any{"name": toolName(t, tools, "ListSecrets"), "arguments": map[string]any{}})); !strings.Contains(text, "API_KEY") {
		t.Fatalf("admin secret list: %s", text)
	}
}

func set(xs []string) map[string]bool {
	out := map[string]bool{}
	for _, x := range xs {
		out[x] = true
	}
	return out
}

func toolText(res map[string]any) string {
	r, _ := res["result"].(map[string]any)
	content, _ := r["content"].([]any)
	var b strings.Builder
	for _, c := range content {
		if m, ok := c.(map[string]any); ok {
			b.WriteString(m["text"].(string))
		}
	}
	if b.Len() == 0 {
		raw, _ := json.Marshal(res)
		return string(raw)
	}
	return b.String()
}

// procedures maps MCP tool names to their API procedures.
func procedures(t *testing.T) map[string]string {
	t.Helper()
	sd, err := apidesc.Service("agen.v1.HubService")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, tool := range apidesc.Tools(sd) {
		out[tool.Name] = tool.Procedure
	}
	return out
}

func toolName(t *testing.T, tools []any, method string) string {
	t.Helper()
	for name, proc := range procedures(t) {
		if strings.HasSuffix(proc, "/"+method) {
			return name
		}
	}
	t.Fatalf("no tool for %s", method)
	return ""
}
