package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Async delegation: a run starts another agent with start_agent and ends
// its turn; when the started task ends, its result arrives as a new task in
// the caller's conversation, which continues with it. Without a
// conversation key the caller's session gets one for the reply.
// features[a2a]: delegation.async
func TestAsyncDelegationRepliesIntoTheConversation(t *testing.T) {
	bin := hostBin(t)
	home := t.TempDir()
	t.Setenv("AGEN_HOME", home)
	t.Setenv("AGEN_HUB", "")
	t.Setenv("AGEN_TOKEN", "")
	upOut := &safeBuf{}
	go Main(context.Background(), []string{"up", "--listen", "127.0.0.1:0", "--gateway-listen", "127.0.0.1:0", "--host-bin", bin}, upOut, &safeBuf{})
	waitUntil(t, "agen up", 30*time.Second, func() bool { return strings.Contains(upOut.String(), "agen is up") })
	waitUntil(t, "local nest", 30*time.Second, func() bool { return strings.Contains(agen(t, 0, "nests"), "local") })

	agents := []struct{ name, config, script string }{
		{"writer", `{"kind":"pool","scale":{"min":0,"max":1}}`, `{"cycle":true,"responses":[{"text":"Report: 42 bugs fixed."}]}`},
		{"lead", `{"kind":"pool","scale":{"min":0,"max":1,"idleTimeout":"10m"},"delegates":[{"name":"writer"}],` +
			`"permissions":{"default":"deny","rules":[{"tool":"start_agent","action":"allow"}]}}`,
			`{"cycle":true,"responses":[` +
				`{"toolCalls":[{"name":"start_agent","arguments":{"agent":"writer","message":"fix the bugs"}}]},` +
				`{"text":"Started; I will report back.","expect":"Started task"},` +
				`{"text":"Final: 42 bugs fixed.","expect":"[Result of the task you started on writer"}]}`},
	}
	for _, a := range agents {
		dir := filepath.Join(t.TempDir(), a.name)
		copyDir(t, filepath.Join(repoRoot(), "examples", "bundles", "hello"), dir)
		os.WriteFile(filepath.Join(dir, "x-agen", "config.json"), []byte(a.config), 0o644)
		os.WriteFile(filepath.Join(dir, "x-agen", "fake-script.json"), []byte(a.script), 0o644)
		agen(t, 0, "deploy", dir, "--name", a.name)
	}
	replies := func() []map[string]any {
		var out []map[string]any
		for _, x := range asJSON(t, agen(t, 0, "tasks", "lead", "--json"))["tasks"].([]any) {
			if m := x.(map[string]any); m["source"] == "reply:writer" {
				out = append(out, m)
			}
		}
		return out
	}
	for round, key := range []string{"c1", ""} {
		args := []string{"run", "lead", "fix everything", "--json"}
		if key != "" {
			args = append(args, "--conversation", key)
		}
		first := asJSON(t, agen(t, 0, args...))
		if first["output"] != "Started; I will report back." {
			t.Fatalf("round %d: first run: %v", round, first)
		}
		var reply map[string]any
		waitUntil(t, "reply task done", 60*time.Second, func() bool {
			r := replies()
			if len(r) <= round || r[0]["state"] != "TASK_STATE_SUCCEEDED" {
				return false
			}
			reply = r[0] // newest first
			return true
		})
		if reply["output"] != "Final: 42 bugs fixed." {
			t.Fatalf("round %d: reply: %v", round, reply)
		}
		ck, _ := reply["conversationKey"].(string)
		if (key != "" && ck != "api:"+key) || (key == "" && !strings.HasPrefix(ck, "async:")) {
			t.Fatalf("round %d: reply conversation key %q", round, ck)
		}
		// The reply continued the caller's conversation.
		runs := asJSON(t, agen(t, 0, "runs", "lead", "--json"))["runs"].([]any)
		byTask := map[string]string{}
		for _, r := range runs {
			m := r.(map[string]any)
			if id, _ := m["taskId"].(string); id != "" {
				byTask[id], _ = m["conversationId"].(string)
			}
		}
		if byTask[first["id"].(string)] == "" || byTask[first["id"].(string)] != byTask[reply["id"].(string)] {
			t.Fatalf("round %d: reply ran in another conversation: %v", round, byTask)
		}
	}
	agen(t, 0, "down")
}
