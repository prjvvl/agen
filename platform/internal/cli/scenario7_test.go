package cli

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func bundleB64(t *testing.T, name string, override map[string]string) map[string]string {
	t.Helper()
	root := filepath.Join(repoRoot(), "examples", "bundles", name)
	files := map[string]string{}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			b, _ := os.ReadFile(p)
			files[filepath.ToSlash(rel)] = base64.StdEncoding.EncodeToString(b)
		}
		return nil
	})
	for k, v := range override {
		files[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	return files
}

// Scenario 7 (scripted): an external AI client, holding a token scoped to one
// namespace with operator + approver scopes, drives the fleet over MCP only:
// deploy, list, scale, and approve an agent's pending "ask"; it is refused
// outside its namespace and cannot approve its own requests.
func TestScenario7ExternalAIOverMCP(t *testing.T) {
	lc, _ := startUp(t)
	hub := lc.Hub
	tok := asJSON(t, agen(t, 0, "token", "create", "--name", "ai", "--scope", "operator", "--scope", "approver", "--namespace", "team", "--json"))
	ai := tok["secret"].(string)
	ref := func(name string) map[string]any { return map[string]any{"namespace": "team", "name": name} }

	// Deploy two agents into its namespace; one must ask before delegating.
	writer := bundleB64(t, "hello", map[string]string{"x-agen/fake-script.json": `{"cycle":true,"responses":[{"text":"Draft ready."}]}`})
	gated := bundleB64(t, "hello", map[string]string{
		"x-agen/config.json": `{"kind":"pool","scale":{"min":0,"max":1},"delegates":[{"name":"writer"}],
			"permissions":{"default":"deny","rules":[{"tool":"call_agent","action":"ask"}]}}`,
		"x-agen/fake-script.json": `{"cycle":true,"responses":[{"toolCalls":[{"name":"call_agent","arguments":{"agent":"writer","message":"publish"}}]},{"text":"published"}]}`,
	})
	for name, files := range map[string]map[string]string{"writer": writer, "gated": gated} {
		if r, isErr := tool(t, hub, ai, "create_deployment", map[string]any{"namespace": "team", "name": name, "bundleFiles": files}); isErr {
			t.Fatalf("deploy %s: %v", name, r)
		}
		if r, isErr := tool(t, hub, ai, "scale_deployment", map[string]any{"ref": ref(name), "desired": 1}); isErr {
			t.Fatalf("scale %s: %v", name, r)
		}
	}
	// Outside its namespace: refused.
	if _, isErr := tool(t, hub, ai, "create_deployment", map[string]any{"namespace": "default", "bundleFiles": writer}); !isErr {
		t.Fatal("deploy outside the token's namespace succeeded")
	}
	if _, isErr := tool(t, hub, ai, "list_deployments", map[string]any{"namespace": "default"}); !isErr {
		t.Fatal("list outside the token's namespace succeeded")
	}
	list, _ := tool(t, hub, ai, "list_deployments", map[string]any{})
	if deps, _ := list["deployments"].([]any); len(deps) != 2 {
		t.Fatalf("list: %v", list)
	}
	waitUntil(t, "instances ready", 60*time.Second, func() bool {
		r, _ := tool(t, hub, ai, "list_instances", map[string]any{"namespace": "team"})
		ins, _ := r["instances"].([]any)
		ready := 0
		for _, i := range ins {
			if i.(map[string]any)["state"] == "INSTANCE_STATE_READY" {
				ready++
			}
		}
		return ready == 2
	})

	// A person (admin) gives the gated agent work; it asks; the AI approves.
	task := asJSON(t, agen(t, 0, "run", "gated", "go", "-n", "team", "--no-wait", "--json"))
	var approval map[string]any
	waitUntil(t, "pending approval", 30*time.Second, func() bool {
		r, _ := tool(t, hub, ai, "list_approvals", map[string]any{"namespace": "team", "state": "APPROVAL_STATE_PENDING"})
		if as, _ := r["approvals"].([]any); len(as) == 1 {
			approval = as[0].(map[string]any)
		}
		return approval != nil
	})
	if approval["tool"] != "call_agent" || approval["requestedBy"] != "admin" {
		t.Fatalf("approval: %v", approval)
	}
	if r, isErr := tool(t, hub, ai, "decide_approval", map[string]any{"id": approval["id"], "approve": true}); isErr {
		t.Fatalf("approve via MCP: %v", r)
	}
	var final map[string]any
	waitUntil(t, "task done", 30*time.Second, func() bool {
		r, _ := tool(t, hub, ai, "get_task", map[string]any{"id": task["id"], "waitSeconds": 5})
		final, _ = r["task"].(map[string]any)
		return final != nil && final["state"] == "TASK_STATE_SUCCEEDED"
	})
	if final["output"] != "published" {
		t.Fatalf("task: %v", final)
	}
	// The AI's own work cannot be approved by the AI.
	own, isErr := tool(t, hub, ai, "submit_task", map[string]any{"ref": ref("gated"), "input": "again"})
	if isErr {
		t.Fatalf("submit: %v", own)
	}
	var mine map[string]any
	waitUntil(t, "second approval", 30*time.Second, func() bool {
		r, _ := tool(t, hub, ai, "list_approvals", map[string]any{"namespace": "team", "state": "APPROVAL_STATE_PENDING"})
		if as, _ := r["approvals"].([]any); len(as) == 1 {
			mine = as[0].(map[string]any)
		}
		return mine != nil
	})
	if r, isErr := tool(t, hub, ai, "decide_approval", map[string]any{"id": mine["id"], "approve": true}); !isErr || !strings.Contains(strings.ToLower(tostring(r)), "permission") {
		t.Fatalf("self-approval via MCP: %v", r)
	}
	agen(t, 0, "deny", mine["id"].(string)) // a person declines it
}

func tostring(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}
