package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// mcpCall is a minimal MCP client call (stateless streamable HTTP).
func mcpCall(t *testing.T, hub, token, method string, params any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, hub+"/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("mcp %s: %v", method, err)
	}
	if e, ok := out["error"]; ok {
		t.Fatalf("mcp %s error: %v", method, e)
	}
	return out["result"].(map[string]any)
}

// tool calls an MCP tool and returns (structured result, isError).
func tool(t *testing.T, hub, token, name string, args any) (map[string]any, bool) {
	t.Helper()
	r := mcpCall(t, hub, token, "tools/call", map[string]any{"name": name, "arguments": args})
	isErr, _ := r["isError"].(bool)
	sc, _ := r["structuredContent"].(map[string]any)
	if sc == nil {
		content := r["content"].([]any)[0].(map[string]any)["text"].(string)
		_ = json.Unmarshal([]byte(content), &sc)
	}
	return sc, isErr
}

func rest(t *testing.T, hub, token, method string, args any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(args)
	req, _ := http.NewRequest(http.MethodPost, hub+"/agen.v1.HubService/"+method, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func asJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	return m
}

// dropVolatile removes fields that legitimately differ between two reads
// (timestamps of activity/updates) before comparing.
func dropVolatile(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			if k == "updatedAt" || k == "lastActivityUnix" {
				continue
			}
			out[k] = dropVolatile(val)
		}
		return out
	case []any:
		for i := range x {
			x[i] = dropVolatile(x[i])
		}
	}
	return v
}

// The same operation via CLI, REST and MCP gives the same result, with
// the same authorization.
func TestCLIRESTAndMCPParity(t *testing.T) {
	lc, _ := startUp(t)
	hub, admin := lc.Hub, lc.Token

	init := mcpCall(t, hub, admin, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "0"}})
	if init["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize: %v", init)
	}
	tools := mcpCall(t, hub, admin, "tools/list", map[string]any{})["tools"].([]any)
	names := map[string]bool{}
	for _, x := range tools {
		names[x.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"list_deployments", "create_deployment", "scale_deployment", "pause_deployment", "decide_approval", "submit_task", "get_trace"} {
		if !names[want] {
			t.Fatalf("tool %s missing from %v", want, names)
		}
	}

	// Deploy through MCP (bundle files are base64, as in protojson).
	files := map[string]string{}
	root := filepath.Join(repoRoot(), "examples", "bundles", "hello")
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			b, _ := os.ReadFile(p)
			files[filepath.ToSlash(rel)] = base64.StdEncoding.EncodeToString(b)
		}
		return nil
	})
	created, isErr := tool(t, hub, admin, "create_deployment", map[string]any{"bundleFiles": files})
	if isErr || created["deployment"].(map[string]any)["name"] != "hello" {
		t.Fatalf("create via MCP: %v", created)
	}

	// Reads are identical three ways.
	cli := asJSON(t, agen(t, 0, "ps", "--json"))
	st, viaREST := rest(t, hub, admin, "ListDeployments", map[string]any{"namespace": "default"})
	viaMCP, isErr := tool(t, hub, admin, "list_deployments", map[string]any{"namespace": "default"})
	if st != 200 || isErr {
		t.Fatalf("list: %d %v", st, isErr)
	}
	if !reflect.DeepEqual(cli, viaREST) || !reflect.DeepEqual(viaREST, viaMCP) {
		t.Fatalf("list differs:\ncli:  %v\nrest: %v\nmcp:  %v", cli, viaREST, viaMCP)
	}

	// Writes: each path produces the same deployment state.
	scaled := map[string]map[string]any{}
	scaled["cli"] = asJSON(t, agen(t, 0, "scale", "hello", "2", "--json"))["deployment"].(map[string]any)
	agen(t, 0, "scale", "hello", "0")
	_, r := rest(t, hub, admin, "ScaleDeployment", map[string]any{"ref": map[string]any{"name": "hello"}, "desired": 2})
	scaled["rest"] = r["deployment"].(map[string]any)
	agen(t, 0, "scale", "hello", "0")
	m, _ := tool(t, hub, admin, "scale_deployment", map[string]any{"ref": map[string]any{"name": "hello"}, "desired": 2})
	scaled["mcp"] = m["deployment"].(map[string]any)
	for _, k := range []string{"cli", "rest", "mcp"} {
		d := scaled[k]
		if d["desired"] != float64(2) || d["name"] != "hello" {
			t.Fatalf("%s scale: %v", k, d)
		}
		delete(d, "generation") // each write bumps it
		delete(d, "ready")
		scaled[k] = dropVolatile(d).(map[string]any)
	}
	if !reflect.DeepEqual(scaled["cli"], scaled["rest"]) || !reflect.DeepEqual(scaled["rest"], scaled["mcp"]) {
		t.Fatalf("scale results differ: %v", scaled)
	}
	paused, isErr := tool(t, hub, admin, "pause_deployment", map[string]any{"ref": map[string]any{"name": "hello"}, "paused": true})
	psAfter := asJSON(t, agen(t, 0, "ps", "--json"))["deployments"].([]any)[0].(map[string]any)
	if isErr || paused["deployment"].(map[string]any)["paused"] != true || psAfter["paused"] != true {
		t.Fatalf("pause via MCP: %v", paused)
	}

	// Authorization is the same: a viewer may read but not scale, on REST
	// and MCP alike (and the error is the API's).
	tok := asJSON(t, agen(t, 0, "token", "create", "--name", "v", "--scope", "viewer", "--json"))
	viewer := tok["secret"].(string)
	if _, isErr := tool(t, hub, viewer, "list_deployments", map[string]any{}); isErr {
		t.Fatal("viewer cannot list via MCP")
	}
	st, restErr := rest(t, hub, viewer, "ScaleDeployment", map[string]any{"ref": map[string]any{"name": "hello"}, "desired": 1})
	mcpErr := mcpCall(t, hub, viewer, "tools/call", map[string]any{"name": "scale_deployment", "arguments": map[string]any{"ref": map[string]any{"name": "hello"}, "desired": 1}})
	text := mcpErr["content"].([]any)[0].(map[string]any)["text"].(string)
	if st != 403 || mcpErr["isError"] != true || !reflect.DeepEqual(asJSON(t, text), restErr) || restErr["code"] != "permission_denied" {
		t.Fatalf("viewer scale: rest %d %v / mcp %v", st, restErr, mcpErr)
	}
	// Unauthenticated MCP calls are refused like the API.
	if _, isErr := tool(t, hub, "", "list_deployments", map[string]any{}); !isErr {
		t.Fatal("unauthenticated MCP call succeeded")
	}
	_ = time.Second
}
