// Package workspace is a small MCP server (stdio) that gives an agent files
// and a shell inside one folder: read, list, grep, edit, write and run. It is
// what coding templates use (`agen mcp workspace --root DIR`). Every path is
// confined to the root (symlinks included); outputs are clipped so a big
// file or log cannot flood the model's context.
package workspace

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/prjvvl/agen/platform/internal/mcpstdio"
)

// Limits on what one call returns.
const (
	readLines    = 400    // lines read returns without a limit
	maxLineChars = 2000   // longer lines are cut
	maxOutput    = 20_000 // characters of command output kept (the end)
	maxMatches   = 200
	maxFiles     = 500
)

// Server serves the workspace tools for one root.
type Server struct {
	Root string
	// AllowRun enables the run tool.
	AllowRun bool
	// RunTimeout bounds one command (default 2 min; a call may ask for less
	// or more, up to MaxRunTimeout).
	RunTimeout, MaxRunTimeout time.Duration
}

// New checks the root and returns a server for it.
func New(root string) (*Server, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("workspace root %s is not a folder", root)
	}
	return &Server{Root: abs, AllowRun: true, RunTimeout: 2 * time.Minute, MaxRunTimeout: 15 * time.Minute}, nil
}

// resolve turns a path relative to the root into an absolute one, refusing
// anything outside the root (also through symlinks).
func (s *Server) resolve(p string) (string, error) {
	if p == "" {
		p = "."
	}
	if filepath.IsAbs(p) || filepath.VolumeName(p) != "" {
		// The root is stored resolved (/private/var on macOS, long names on
		// Windows); resolve the given path the same way before comparing.
		rel, err := filepath.Rel(s.Root, resolvedPrefix(p))
		if err != nil {
			return "", fmt.Errorf("%s is outside the workspace", p)
		}
		p = rel
	}
	if c := filepath.ToSlash(filepath.Clean(p)); c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%s is outside the workspace", p)
	}
	full := filepath.Join(s.Root, filepath.Clean("/" + filepath.ToSlash(p))[1:])
	// The deepest existing ancestor must still be inside the root.
	check := full
	for {
		if real, err := filepath.EvalSymlinks(check); err == nil {
			if !inside(s.Root, real) {
				return "", fmt.Errorf("%s is outside the workspace", p)
			}
			break
		}
		parent := filepath.Dir(check)
		if parent == check {
			break
		}
		check = parent
	}
	if !inside(s.Root, full) {
		return "", fmt.Errorf("%s is outside the workspace", p)
	}
	return full, nil
}

// resolvedPrefix resolves symlinks (and, on Windows, short names) in the
// deepest part of p that exists, keeping the rest as given.
func resolvedPrefix(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func inside(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *Server) rel(full string) string {
	r, err := filepath.Rel(s.Root, full)
	if err != nil {
		return full
	}
	return filepath.ToSlash(r)
}

// skipDir: folders list and grep do not descend into.
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "target", ".venv", "__pycache__", "dist", ".next":
		return true
	}
	return false
}

func tool(name, desc string, schema, ann map[string]any) mcpstdio.Tool {
	return mcpstdio.Tool{Name: name, Description: desc, InputSchema: schema, Annotations: ann}
}

var (
	obj     = mcpstdio.Object
	str     = mcpstdio.Str
	integer = mcpstdio.Int
)

// Tools lists the workspace tools.
func (s *Server) Tools() []mcpstdio.Tool {
	ro := mcpstdio.ReadOnly
	out := []mcpstdio.Tool{
		tool("read", "Read a text file, with line numbers. Long files come a page at a time: pass offset (first line, 1-based) and limit.",
			obj(map[string]any{"path": str("File path, relative to the workspace"), "offset": integer("First line (default 1)"), "limit": integer(fmt.Sprintf("Lines to read (default %d)", readLines))}, "path"), ro),
		tool("list", "List files whose path matches a glob (relative to the workspace; ** matches any folders), e.g. \"src/**/*.go\". Skips .git, node_modules, target and similar.",
			obj(map[string]any{"pattern": str("Glob (default **/*)")}), ro),
		tool("grep", "Search file contents with a regular expression. Returns path:line: text for each match.",
			obj(map[string]any{"pattern": str("Regular expression (RE2)"), "glob": str("Only files matching this glob (default **/*)"), "ignore_case": map[string]any{"type": "boolean"}}, "pattern"), ro),
		tool("edit", "Replace one exact piece of text in a file. old must occur exactly once (add surrounding lines to make it unique) unless replace_all is set.",
			obj(map[string]any{"path": str("File path"), "old": str("Text to replace, exactly as in the file"), "new": str("Replacement text"), "replace_all": map[string]any{"type": "boolean"}}, "path", "old", "new"), nil),
		tool("write", "Create a file, or replace a whole file, with the given content (folders are created).",
			obj(map[string]any{"path": str("File path"), "content": str("The full file content")}, "path", "content"), nil),
	}
	if s.AllowRun {
		shell := "sh -c"
		if runtime.GOOS == "windows" {
			shell = "cmd /C"
		}
		out = append(out, tool("run", fmt.Sprintf("Run a shell command (%s) in the workspace folder, e.g. tests or a build. Returns the exit code and the end of the output.", shell),
			obj(map[string]any{"command": str("The command line"), "timeout_seconds": integer(fmt.Sprintf("Default %d, at most %d", int(s.RunTimeout.Seconds()), int(s.MaxRunTimeout.Seconds())))}, "command"), nil))
	}
	return out
}

// Call runs a tool; the error is shown to the model as the tool's error.
func (s *Server) Call(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	var a struct {
		Path        string `json:"path"`
		Offset      int    `json:"offset"`
		Limit       int    `json:"limit"`
		Pattern     string `json:"pattern"`
		Glob        string `json:"glob"`
		IgnoreCase  bool   `json:"ignore_case"`
		Old         string `json:"old"`
		New         string `json:"new"`
		ReplaceAll  bool   `json:"replace_all"`
		Content     string `json:"content"`
		Command     string `json:"command"`
		TimeoutSecs int    `json:"timeout_seconds"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("arguments: %v", err)
		}
	}
	switch name {
	case "read":
		return s.read(a.Path, a.Offset, a.Limit)
	case "list":
		return s.list(a.Pattern)
	case "grep":
		return s.grep(a.Pattern, a.Glob, a.IgnoreCase)
	case "edit":
		return s.edit(a.Path, a.Old, a.New, a.ReplaceAll)
	case "write":
		return s.write(a.Path, a.Content)
	case "run":
		if !s.AllowRun {
			return "", errors.New("run is turned off for this workspace")
		}
		return s.run(ctx, a.Command, a.TimeoutSecs)
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

func (s *Server) read(p string, offset, limit int) (string, error) {
	full, err := s.resolve(p)
	if err != nil {
		return "", err
	}
	f, err := os.Open(full)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %v", p, errors.Unwrap(err))
	}
	defer f.Close()
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = readLines
	}
	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	n, shown, more := 0, 0, false
	for sc.Scan() {
		n++
		if n < offset {
			continue
		}
		if shown == limit {
			more = true
			continue
		}
		line := sc.Text()
		if strings.ContainsRune(line, 0) {
			return "", fmt.Errorf("%s looks like a binary file", p)
		}
		if len(line) > maxLineChars {
			line = line[:maxLineChars] + " …(line cut)"
		}
		fmt.Fprintf(&b, "%6d\t%s\n", n, line)
		shown++
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if n == 0 {
		return fmt.Sprintf("(%s is empty)", p), nil
	}
	if more {
		fmt.Fprintf(&b, "… %d lines in all; read on with offset %d\n", n, offset+shown)
	}
	return b.String(), nil
}

// globRE turns a glob with ** into a regular expression over slash paths.
func globRE(glob string) (*regexp.Regexp, error) {
	if glob == "" {
		glob = "**/*"
	}
	var b strings.Builder
	b.WriteString("^")
	g := filepath.ToSlash(glob)
	for i := 0; i < len(g); i++ {
		switch c := g[i]; {
		case strings.HasPrefix(g[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(g[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// walk visits files matching a glob, sorted, up to max.
func (s *Server) walk(glob string, max int, visit func(full, rel string) bool) error {
	re, err := globRE(glob)
	if err != nil {
		return fmt.Errorf("glob %q: %v", glob, err)
	}
	var files []string
	err = filepath.WalkDir(s.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != s.Root && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if re.MatchString(s.rel(p)) {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	for i, f := range files {
		if i >= max || !visit(f, s.rel(f)) {
			break
		}
	}
	return err
}

func (s *Server) list(pattern string) (string, error) {
	var out []string
	total := 0
	err := s.walk(pattern, 1<<30, func(_, rel string) bool {
		total++
		if len(out) < maxFiles {
			out = append(out, rel)
		}
		return true
	})
	if err != nil {
		return "", err
	}
	if total == 0 {
		return "no files match", nil
	}
	res := strings.Join(out, "\n")
	if total > len(out) {
		res += fmt.Sprintf("\n… %d files in all; narrow the pattern", total)
	}
	return res, nil
}

func (s *Server) grep(pattern, glob string, ignoreCase bool) (string, error) {
	if ignoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("pattern: %v", err)
	}
	var b strings.Builder
	matches := 0
	err = s.walk(glob, 1<<30, func(full, rel string) bool {
		if _, err := s.resolve(rel); err != nil {
			return true // a symlink to a file outside the root
		}
		data, err := os.ReadFile(full)
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			return true // unreadable or binary
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				if len(line) > 300 {
					line = line[:300] + "…"
				}
				fmt.Fprintf(&b, "%s:%d: %s\n", rel, i+1, strings.TrimRight(line, "\r"))
				if matches++; matches >= maxMatches {
					return false
				}
			}
		}
		return true
	})
	if err != nil {
		return "", err
	}
	if matches == 0 {
		return "no matches", nil
	}
	if matches >= maxMatches {
		fmt.Fprintf(&b, "… stopped at %d matches; narrow the pattern or glob\n", maxMatches)
	}
	return b.String(), nil
}

func (s *Server) edit(p, old, new string, all bool) (string, error) {
	full, err := s.resolve(p)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %v", p, errors.Unwrap(err))
	}
	text := string(data)
	if old == "" {
		return "", errors.New("old is empty; use write to create a file")
	}
	n := strings.Count(text, old)
	switch {
	case n == 0:
		return "", fmt.Errorf("old text not found in %s (it must match exactly, including spaces and line breaks)", p)
	case n > 1 && !all:
		return "", fmt.Errorf("old text occurs %d times in %s; include more surrounding text to make it unique, or set replace_all", n, p)
	}
	if all {
		text = strings.ReplaceAll(text, old, new)
	} else {
		text = strings.Replace(text, old, new, 1)
	}
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		return "", err
	}
	if all {
		return fmt.Sprintf("replaced %d occurrences in %s", n, p), nil
	}
	return fmt.Sprintf("edited %s", p), nil
}

func (s *Server) write(p, content string) (string, error) {
	full, err := s.resolve(p)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}
	_, statErr := os.Stat(full)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return "", err
	}
	if statErr == nil {
		return fmt.Sprintf("replaced %s (%d bytes)", p, len(content)), nil
	}
	return fmt.Sprintf("created %s (%d bytes)", p, len(content)), nil
}

func (s *Server) run(ctx context.Context, command string, secs int) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", errors.New("command is empty")
	}
	timeout := s.RunTimeout
	if secs > 0 {
		timeout = min(time.Duration(secs)*time.Second, s.MaxRunTimeout)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := shell(ctx, command)
	cmd.Dir = s.Root
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	text := out.String()
	if len(text) > maxOutput {
		text = "…(start of output cut)\n" + text[len(text)-maxOutput:]
	}
	code := 0
	var ee *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return fmt.Sprintf("timed out after %s\n%s", timeout, text), nil
	case errors.As(err, &ee):
		code = ee.ExitCode()
	case err != nil:
		return "", err
	}
	return fmt.Sprintf("exit code %d\n%s", code, text), nil
}

// Serve answers MCP requests on r with responses on w until r ends.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	return mcpstdio.Serve(ctx, s, "agen-workspace", "Files and a shell inside one folder. Paths are relative to it.", r, w)
}
