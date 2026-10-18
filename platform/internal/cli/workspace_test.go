package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// agenBinary builds the agen command for tests that run it as a tool server.
func agenBinary(t *testing.T) string {
	t.Helper()
	name := "agen"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	p := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", p, "./cmd/agen")
	cmd.Dir = filepath.Join(repoRoot(), "platform")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build agen: %v\n%s", err, out)
	}
	return p
}

// The coder template's tool server: an agent on a real host reads, edits
// and tests a project through `agen mcp workspace`, confined to its folder.
func TestCoderTemplateWorksInItsFolder(t *testing.T) {
	bin := hostBin(t)
	t.Setenv("AGEN_BIN", agenBinary(t))
	home := t.TempDir()
	t.Setenv("AGEN_HOME", home)
	t.Setenv("AGEN_HUB", "")
	t.Setenv("AGEN_TOKEN", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	upOut := &safeBuf{}
	go Main(context.Background(), []string{"up", "--listen", "127.0.0.1:0", "--gateway-listen", "127.0.0.1:0", "--host-bin", bin}, upOut, &safeBuf{})
	waitUntil(t, "agen up", 30*time.Second, func() bool { return strings.Contains(upOut.String(), "agen is up") })

	project := t.TempDir()
	os.WriteFile(filepath.Join(project, "greet.txt"), []byte("hello world\n"), 0o644)
	check := "findstr /C:\"hello agen\" greet.txt"
	if runtime.GOOS != "windows" {
		check = "grep -q 'hello agen' greet.txt && echo found"
	}
	dir := filepath.Join(t.TempDir(), "coder")
	copyDir(t, filepath.Join(repoRoot(), "examples", "bundles", "coder"), dir)
	// A scripted model instead of a router, the run tool allowed, the
	// project folder as the root.
	os.WriteFile(filepath.Join(dir, "x-agen", "harness.json"), []byte(`{"provider":"fake","model":"fake","script":"script.json"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "x-agen", "secrets.json"), []byte(`{}`), 0o644)
	mcp, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"workspace": map[string]any{"command": "agen", "args": []string{"mcp", "workspace", "--root", project}}}})
	os.WriteFile(filepath.Join(dir, "mcp.json"), mcp, 0o644)
	cfg, _ := os.ReadFile(filepath.Join(dir, "x-agen", "config.json"))
	os.WriteFile(filepath.Join(dir, "x-agen", "config.json"), []byte(strings.Replace(string(cfg), `"workspace.run",
        "action": "ask"`, `"workspace.run",
        "action": "allow"`, 1)), 0o644)
	script, _ := json.Marshal(map[string]any{"responses": []map[string]any{
		{"toolCalls": []map[string]any{{"name": "workspace.read", "arguments": map[string]any{"path": "greet.txt"}}}},
		{"expect": "hello world", "toolCalls": []map[string]any{{"name": "workspace.edit", "arguments": map[string]any{"path": "greet.txt", "old": "world", "new": "agen"}}}},
		{"expect": "edited greet.txt", "toolCalls": []map[string]any{{"name": "workspace.read", "arguments": map[string]any{"path": "../outside.txt"}}}},
		{"expect": "outside the workspace", "toolCalls": []map[string]any{{"name": "workspace.run", "arguments": map[string]any{"command": check}}}},
		{"expect": "exit code 0", "text": "Changed greet.txt and checked it."},
	}})
	os.WriteFile(filepath.Join(dir, "script.json"), script, 0o644)
	agen(t, 0, "deploy", dir)
	t.Cleanup(func() {
		if t.Failed() {
			var out bytes.Buffer
			Main(context.Background(), []string{"logs", "coder"}, &out, &out)
			t.Logf("logs:\n%s", out.String())
		}
	})
	task := asJSON(t, agen(t, 0, "run", "coder", "say hello to agen", "--json"))
	if task["state"] != "TASK_STATE_SUCCEEDED" || task["output"] != "Changed greet.txt and checked it." {
		t.Fatalf("task: %v", task)
	}
	if b, _ := os.ReadFile(filepath.Join(project, "greet.txt")); string(b) != "hello agen\n" {
		t.Fatalf("file: %q", b)
	}
	agen(t, 0, "down")
}
