package cli

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// docFiles are the user-facing docs whose shell examples must stay true.
var docFiles = []string{"README.md", "docs/index.md", "docs/deploy.md", "docs/security.md", "docs/getting-started.md", "docs/troubleshooting.md", "docs/install.md", "docs/embed.md", "docs/development.md",
	"docs/guides/tools.md", "docs/guides/agents.md", "docs/guides/permissions.md", "docs/guides/budgets.md", "docs/guides/triggers.md",
	"docs/guides/memory.md", "docs/guides/mcp.md", "docs/guides/console.md", "docs/guides/traces.md", "docs/guides/templates.md",
	"docs/guides/notifications.md", "examples/go-app/README.md", "examples/node-app/README.md", "examples/python-app/README.md"}

// shellLines returns the lines of ```sh blocks in a Markdown file, joined
// across trailing-backslash continuations, without comments.
func shellLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	in, cont := false, ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			in = strings.TrimSpace(line) == "```sh" && !in
			continue
		}
		if !in {
			continue
		}
		if i := strings.Index(line, " #"); i >= 0 {
			line = line[:i]
		}
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, "\\") {
			cont += strings.TrimSuffix(line, "\\") + " "
			continue
		}
		if line = strings.TrimSpace(cont + line); line != "" {
			out = append(out, line)
		}
		cont = ""
	}
	return out
}

var envPrefix = regexp.MustCompile(`^([A-Z_][A-Z0-9_]*=\S*\s+)+`)

// agenArgs returns the agen arguments of a shell line ("" prefix env
// assignments and pipes stripped), or nil when the line is not an agen call.
func agenArgs(line string) []string {
	if i := strings.LastIndex(line, "|"); i >= 0 {
		line = strings.TrimSpace(line[i+1:])
	}
	line = envPrefix.ReplaceAllString(line, "")
	f := strings.Fields(line)
	if len(f) < 2 || f[0] != "agen" {
		return nil
	}
	return f[1:]
}

// Every agen command in the docs names a real command, and every flag
// it uses is one that command defines (checked against its -h output).
func TestDocsUseRealCommandsAndFlags(t *testing.T) {
	root := repoRoot()
	checked := 0
	for _, doc := range docFiles {
		for _, line := range shellLines(t, filepath.Join(root, doc)) {
			args := agenArgs(line)
			if args == nil {
				continue
			}
			// Known flags: the union of the -h output of the command and of
			// its first word (subcommands like "hub serve").
			known := map[string]bool{}
			for _, prefix := range [][]string{args[:1], args[:min(2, len(args))]} {
				var out, errOut safeBuf
				Main(context.Background(), append(append([]string{}, prefix...), "-h"), &out, &errOut)
				help := out.String() + errOut.String()
				if strings.Contains(help, "unknown command") {
					t.Errorf("%s: unknown command in %q", doc, line)
				}
				for _, m := range regexp.MustCompile(`(?m)^\s+-([a-z][a-z0-9-]*)`).FindAllStringSubmatch(help, -1) {
					known[m[1]] = true
				}
			}
			for _, a := range args {
				if !strings.HasPrefix(a, "-") {
					continue
				}
				name := strings.TrimLeft(strings.SplitN(a, "=", 2)[0], "-")
				if name != "" && !known[name] && name != "h" {
					t.Errorf("%s: %q uses flag --%s, which `agen %s` does not define", doc, line, name, args[0])
				}
			}
			checked++
		}
	}
	// The checker itself: it knows deploy's real flags and not made-up ones.
	var out, errOut safeBuf
	Main(context.Background(), []string{"deploy", "-h"}, &out, &errOut)
	help := out.String() + errOut.String()
	if !regexp.MustCompile(`(?m)^\s+-replicas\b`).MatchString(help) || regexp.MustCompile(`(?m)^\s+-no-such-flag\b`).MatchString(help) {
		t.Fatalf("help parsing is broken:\n%s", help)
	}
	if checked < 15 {
		t.Fatalf("only %d agen commands found in the docs", checked)
	}
	t.Logf("%d agen commands checked", checked)
}

// The "Local: one process" walkthrough in docs/deploy.md runs as
// written (through the CLI, in a temporary AGEN_HOME).
func TestDocsLocalWalkthrough(t *testing.T) {
	bin := hostBin(t)
	root := repoRoot()
	b, err := os.ReadFile(filepath.Join(root, "docs", "deploy.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	i := strings.Index(doc, "## Local: one process")
	j := strings.Index(doc[i:], "\n## ")
	section := doc[i : i+j+1]
	tmp := filepath.Join(t.TempDir(), "local.md")
	os.WriteFile(tmp, []byte(section), 0o600)
	lines := shellLines(t, tmp)
	if len(lines) < 5 {
		t.Fatalf("walkthrough has %d commands", len(lines))
	}
	home := t.TempDir()
	t.Setenv("AGEN_HOME", home)
	t.Setenv("AGEN_HUB", "")
	t.Setenv("AGEN_TOKEN", "")
	t.Chdir(t.TempDir()) // the walkthrough must not need a clone
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upDone := make(chan int, 1)
	for _, line := range lines {
		args := agenArgs(line)
		if args == nil {
			t.Fatalf("not an agen command: %q", line)
		}
		switch args[0] {
		case "up":
			upOut := &safeBuf{}
			go func() {
				upDone <- Main(ctx, append(args, "--host-bin", bin, "--listen", "127.0.0.1:0", "--gateway-listen", "127.0.0.1:0"), upOut, &safeBuf{})
			}()
			waitUntil(t, "agen up", 60*time.Second, func() bool { return strings.Contains(upOut.String(), "agen is up") })
			waitUntil(t, "local nest", 30*time.Second, func() bool { return strings.Contains(agen(t, 0, "nests"), "local") })
		case "down":
			out := agen(t, 0, args...)
			if !strings.Contains(out, "down") {
				t.Fatalf("agen down: %q", out)
			}
			select {
			case code := <-upDone:
				if code != 0 {
					t.Fatalf("agen up exited %d", code)
				}
			case <-time.After(60 * time.Second):
				t.Fatal("agen up did not stop")
			}
		case "run":
			if out := agen(t, 0, args...); strings.TrimSpace(out) != "Hello! Nice to meet you." {
				t.Fatalf("%q: %q", line, out)
			}
		default:
			agen(t, 0, args...)
		}
	}
}
