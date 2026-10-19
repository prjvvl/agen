package workspace

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newWS(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "src", "pkg"), 0o755)
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n"), 0o644)
	os.WriteFile(filepath.Join(root, "src", "pkg", "util.go"), []byte("package pkg\n\nfunc Add(a, b int) int { return a + b }\n"), 0o644)
	os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: main\n"), 0o644)
	os.WriteFile(filepath.Join(root, "README.md"), []byte("# demo\n"), 0o644)
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	return s, root
}

func call(t *testing.T, s *Server, name string, args map[string]any) (string, error) {
	t.Helper()
	b, _ := json.Marshal(args)
	return s.Call(context.Background(), name, b)
}

func TestPathsStayInsideTheRoot(t *testing.T) {
	s, root := newWS(t)
	outside := filepath.Join(filepath.Dir(root), "secret.txt")
	os.WriteFile(outside, []byte("nope"), 0o644)
	defer os.Remove(outside)
	for _, p := range []string{"../secret.txt", "src/../../secret.txt", outside} {
		if out, err := call(t, s, "read", map[string]any{"path": p}); err == nil && strings.Contains(out, "nope") {
			t.Fatalf("read %s escaped the root: %q", p, out)
		}
	}
	if _, err := call(t, s, "write", map[string]any{"path": "../x.txt", "content": "x"}); err == nil {
		if _, statErr := os.Stat(filepath.Join(filepath.Dir(root), "x.txt")); statErr == nil {
			t.Fatal("write escaped the root")
		}
	}
	// A symlink pointing outside is refused.
	if runtime.GOOS != "windows" {
		os.Symlink(filepath.Dir(root), filepath.Join(root, "up"))
		if _, err := call(t, s, "read", map[string]any{"path": "up/secret.txt"}); err == nil {
			t.Fatal("read through a symlink escaped the root")
		}
	}
	// grep does not follow a symlinked file out of the root.
	if err := os.Symlink(outside, filepath.Join(root, "notes.txt")); err == nil {
		if out, err := call(t, s, "grep", map[string]any{"pattern": "nope"}); err != nil || strings.Contains(out, "nope") {
			t.Fatalf("grep through a symlink: %q %v", out, err)
		}
	}
	// An absolute path inside the root is fine.
	if out, err := call(t, s, "read", map[string]any{"path": filepath.Join(root, "README.md")}); err != nil || !strings.Contains(out, "# demo") {
		t.Fatalf("absolute path inside: %q %v", out, err)
	}
}

func TestReadListGrepEditWrite(t *testing.T) {
	s, root := newWS(t)
	out, err := call(t, s, "read", map[string]any{"path": "src/main.go", "offset": 3, "limit": 2})
	if err != nil || !strings.Contains(out, "     3\tfunc main() {") || !strings.Contains(out, "read on with offset 5") {
		t.Fatalf("read: %q %v", out, err)
	}
	out, _ = call(t, s, "list", map[string]any{"pattern": "**/*.go"})
	if out != "src/main.go\nsrc/pkg/util.go" {
		t.Fatalf("list: %q", out)
	}
	if out, _ = call(t, s, "list", map[string]any{}); strings.Contains(out, ".git") {
		t.Fatalf("list shows .git: %q", out)
	}
	out, _ = call(t, s, "grep", map[string]any{"pattern": "func \\w+", "glob": "src/**"})
	if !strings.Contains(out, "src/main.go:3: func main() {") || !strings.Contains(out, "src/pkg/util.go:3:") {
		t.Fatalf("grep: %q", out)
	}
	if _, err := call(t, s, "edit", map[string]any{"path": "src/main.go", "old": "\"hi\"", "new": "\"hello\""}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "src", "main.go"))
	if !strings.Contains(string(b), "\"hello\"") {
		t.Fatalf("edit: %s", b)
	}
	if _, err := call(t, s, "edit", map[string]any{"path": "src/main.go", "old": "missing", "new": "x"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("edit missing: %v", err)
	}
	os.WriteFile(filepath.Join(root, "dup.txt"), []byte("a a"), 0o644)
	if _, err := call(t, s, "edit", map[string]any{"path": "dup.txt", "old": "a", "new": "b"}); err == nil || !strings.Contains(err.Error(), "occurs 2 times") {
		t.Fatalf("edit ambiguous: %v", err)
	}
	if out, err := call(t, s, "write", map[string]any{"path": "new/dir/f.txt", "content": "x"}); err != nil || !strings.HasPrefix(out, "created") {
		t.Fatalf("write: %q %v", out, err)
	}
}

func TestRun(t *testing.T) {
	s, _ := newWS(t)
	cmd := "echo hello && exit 3"
	out, err := call(t, s, "run", map[string]any{"command": cmd})
	if err != nil || !strings.HasPrefix(out, "exit code 3") || !strings.Contains(out, "hello") {
		t.Fatalf("run: %q %v", out, err)
	}
	slow := "sleep 5"
	if runtime.GOOS == "windows" {
		slow = "ping -n 6 127.0.0.1 >NUL"
	}
	if out, _ := call(t, s, "run", map[string]any{"command": slow, "timeout_seconds": 1}); !strings.HasPrefix(out, "timed out") {
		t.Fatalf("timeout: %q", out)
	}
	s.AllowRun = false
	if _, err := call(t, s, "run", map[string]any{"command": "echo x"}); err == nil {
		t.Fatal("run while off")
	}
	for _, tl := range s.Tools() {
		if tl.Name == "run" {
			t.Fatal("run listed while off")
		}
	}
}

func TestMCPStdio(t *testing.T) {
	s, _ := newWS(t)
	in, inW := io.Pipe()
	outR, out := io.Pipe()
	go func() { _ = s.Serve(context.Background(), in, out); out.Close() }()
	rd := bufio.NewReader(outR)
	send := func(v string) map[string]any {
		t.Helper()
		io.WriteString(inW, v+"\n")
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		json.Unmarshal([]byte(line), &m)
		return m
	}
	init := send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	if init["result"].(map[string]any)["protocolVersion"] != "2025-03-26" {
		t.Fatalf("initialize: %v", init)
	}
	io.WriteString(inW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	list := send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 6 {
		t.Fatalf("tools: %v", tools)
	}
	res := send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read","arguments":{"path":"nope.txt"}}}`)
	if res["result"].(map[string]any)["isError"] != true {
		t.Fatalf("error result: %v", res)
	}
	inW.Close()
}
