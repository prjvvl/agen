package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prjvvl/agen/platform/internal/apidesc"
)

// Scenarios (spec/scenarios/*.json) are written once as Hub API calls and
// played through every driver: the API (Connect JSON over HTTP), MCP (the
// tool generated from the same method) and the CLI (the command that does
// the same thing, with --json). A method the CLI has no command for fails
// the scenario on the CLI unless the step or scenario skips it with a
// reason, so a feature cannot land on one surface and quietly miss another.
// See docs/development.md (Feature coverage).

type scenario struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Features    []string          `json:"features"`
	Skip        map[string]string `json:"skip"` // driver -> why the scenario does not apply there
	Bundles     map[string]struct {
		From  string                     `json:"from"`
		Patch map[string]json.RawMessage `json:"patch"` // file -> JSON merge patch, or null to drop the file
	} `json:"bundles"`
	Steps []scenarioStep `json:"steps"`
}

type scenarioStep struct {
	Call   string            `json:"call"`
	Args   map[string]any    `json:"args"`
	Expect map[string]any    `json:"expect"`
	Error  string            `json:"error"` // the call fails with this text
	Save   map[string]string `json:"save"`  // variable -> path in the response
	Retry  int               `json:"retry"` // seconds to retry until the expectations hold
	Skip   map[string]string `json:"skip"`  // driver -> why this step does not apply there
}

// driver calls one Hub API method its own way and returns the response in
// the API's JSON shape.
type driver interface {
	name() string
	call(t *testing.T, method string, args map[string]any) (map[string]any, error)
}

type apiDriver struct{ hub, token string }

func (apiDriver) name() string { return "api" }

func (d apiDriver) call(t *testing.T, method string, args map[string]any) (map[string]any, error) {
	status, out := rest(t, d.hub, d.token, method, args)
	if status != 200 {
		return nil, fmt.Errorf("%v: %v", out["code"], out["message"])
	}
	return out, nil
}

type mcpDriver struct{ hub, token string }

func (mcpDriver) name() string { return "mcp" }

func (d mcpDriver) call(t *testing.T, method string, args map[string]any) (map[string]any, error) {
	r := mcpCall(t, d.hub, d.token, "tools/call", map[string]any{"name": apidesc.SnakeCase(method), "arguments": args})
	text := ""
	if c, ok := r["content"].([]any); ok && len(c) > 0 {
		text, _ = c[0].(map[string]any)["text"].(string)
	}
	if isErr, _ := r["isError"].(bool); isErr {
		return nil, fmt.Errorf("%s", text)
	}
	out, _ := r["structuredContent"].(map[string]any)
	if out == nil {
		out = map[string]any{}
		_ = json.Unmarshal([]byte(text), &out)
	}
	return out, nil
}

type cliDriver struct{}

func (cliDriver) name() string { return "cli" }

// cliCommand is how the CLI does one API method: its arguments, the key the
// printed JSON sits under in the API response ("" when it is the response),
// and stdin.
type cliCommand struct {
	args  []string
	wrap  string
	stdin string
	text  bool // prints a message, not JSON
}

func (d cliDriver) call(t *testing.T, method string, a map[string]any) (map[string]any, error) {
	cmd, err := cliFor(t, method, a)
	if err != nil {
		return nil, err
	}
	var out, errb bytes.Buffer
	if cmd.stdin != "" {
		r, w, _ := os.Pipe()
		go func() { w.Write([]byte(cmd.stdin)); w.Close() }()
		old := os.Stdin
		os.Stdin = r
		defer func() { os.Stdin = old }()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if code := Main(ctx, cmd.args, &out, &errb); code != 0 {
		return nil, fmt.Errorf("agen %s: exit %d: %s", strings.Join(cmd.args, " "), code, strings.TrimSpace(errb.String()))
	}
	res := map[string]any{}
	if cmd.text {
		return res, nil
	}
	if s := strings.TrimSpace(out.String()); s != "" {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			return nil, fmt.Errorf("agen %s: not JSON: %s", strings.Join(cmd.args, " "), s)
		}
		if cmd.wrap != "" {
			res[cmd.wrap] = v
		} else if m, ok := v.(map[string]any); ok {
			res = m
		}
	}
	return res, nil
}

// cliFor maps an API method and its arguments to the CLI command that does
// the same. Arguments it cannot express are an error, so a missing flag
// shows up as a failing scenario.
func cliFor(t *testing.T, method string, a map[string]any) (cliCommand, error) {
	used := map[string]bool{}
	str := func(k string) string { used[k] = true; s, _ := a[k].(string); return s }
	ref := func() (ns, name string) {
		used["ref"] = true
		r, _ := a["ref"].(map[string]any)
		ns, _ = r["namespace"].(string)
		name, _ = r["name"].(string)
		return
	}
	ns := func() []string { return []string{"-n", str("namespace")} }
	var c cliCommand
	switch method {
	case "CreateDeployment":
		dir := t.TempDir()
		used["bundleText"] = true
		files, _ := a["bundleText"].(map[string]any)
		for p, body := range files {
			path := filepath.Join(dir, filepath.FromSlash(p))
			os.MkdirAll(filepath.Dir(path), 0o755)
			os.WriteFile(path, []byte(body.(string)), 0o644)
		}
		c.args = append([]string{"deploy", dir, "--name", str("name"), "--json"}, ns()...)
		if v, _ := a["validateOnly"].(bool); v {
			used["validateOnly"] = true
			c.args = append(c.args, "--validate")
		} else {
			c.wrap = "deployment"
		}
	case "SubmitTask":
		n, name := ref()
		c.args = []string{"run", name, str("input"), "-n", n, "--no-wait", "--json"}
		c.wrap = "task"
		if v := str("modelTier"); v != "" {
			c.args = append(c.args, "--tier", v)
		}
		if v := str("notBefore"); v != "" {
			c.args = append(c.args, "--at", v)
		}
		if v := str("conversationKey"); v != "" {
			c.args = append(c.args, "--conversation", v)
		}
		if v := str("idempotencyKey"); v != "" {
			c.args = append(c.args, "--idempotency-key", v)
		}
		if l, ok := a["labels"].(map[string]any); ok {
			used["labels"] = true
			keys := make([]string, 0, len(l))
			for k := range l {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				c.args = append(c.args, "--label", fmt.Sprintf("%s=%v", k, l[k]))
			}
		}
	case "GetTask":
		used["waitSeconds"] = true
		wait, _ := a["waitSeconds"].(float64)
		c.args = []string{"wait", str("id"), "--json", "--timeout", fmt.Sprintf("%ds", int(wait)+1)}
		c.wrap = "task"
	case "ListTasks":
		c.args = append([]string{"tasks", str("deployment"), "--json"}, ns()...)
		if v, _ := a["scheduled"].(bool); v {
			used["scheduled"] = true
			c.args = append(c.args, "--scheduled")
		}
		if v := str("state"); v != "" {
			c.args = append(c.args, "--state", strings.ToLower(strings.TrimPrefix(v, "TASK_STATE_")))
		}
	case "CancelTask":
		c.args = []string{"cancel", str("id"), "--json"}
	case "GetLogs":
		n, name := ref()
		c.args = []string{"logs", name, "-n", n, "--json"}
	case "SetSecret":
		c.args = append([]string{"secret", "set", str("name")}, ns()...)
		used["deployments"] = true
		for _, d := range a["deployments"].([]any) {
			c.args = append(c.args, "--for", d.(string))
		}
		c.stdin = str("value")
		c.text = true
	case "ListSecrets":
		c.args = append([]string{"secret", "ls", "--json"}, ns()...)
	case "DeleteSecret":
		c.args = append([]string{"secret", "rm", str("name")}, ns()...)
		c.text = true
	case "SetNotificationTarget":
		used["target"] = true
		tg := a["target"].(map[string]any)
		c.args = []string{"notify", "set", tg["name"].(string), tg["url"].(string), "-n", tg["namespace"].(string), "--json"}
		for _, ev := range tg["events"].([]any) {
			c.args = append(c.args, "--event", ev.(string))
		}
	case "ListNotificationTargets":
		c.args = append([]string{"notify", "ls", "--json"}, ns()...)
	case "DeleteNotificationTarget":
		c.args = append([]string{"notify", "rm", str("name")}, ns()...)
		c.text = true
	case "ListDeployments":
		c.args = append([]string{"ps", "--json"}, ns()...)
	case "ListTriggerEvents":
		n, name := ref()
		c.args = []string{"triggers", name, "-n", n, "--json"}
	case "CreateWebhookSecret":
		n, name := ref()
		c.args = []string{"webhook-secret", name, str("trigger"), "-n", n, "--json"}
	case "ListNests":
		c.args = []string{"nests", "--json"}
	case "CreateJoinToken":
		used["ttlSeconds"] = true
		ttl, _ := a["ttlSeconds"].(float64)
		c.args = []string{"join-token", "--json", "--ttl", fmt.Sprintf("%ds", int(ttl))}
	case "ListProviderHolds":
		c.args = []string{"providers", "--json"}
	case "ListTemplates":
		c.args = []string{"init", "--list", "--json"}
	default:
		return c, fmt.Errorf("the CLI has no command for %s (map it in cliFor, or skip the step on cli with a reason)", method)
	}
	for k := range a {
		if !used[k] {
			return c, fmt.Errorf("the CLI command for %s does not take %q", method, k)
		}
	}
	return c, nil
}

// TestScenariosOnEverySurface plays every spec/scenarios case through the
// API, MCP and CLI drivers against one local fleet; each driver works in
// its own namespace.
func TestScenariosOnEverySurface(t *testing.T) {
	lc, _ := startUp(t)
	root := repoRoot()
	paths, _ := filepath.Glob(filepath.Join(root, "spec", "scenarios", "*.json"))
	if len(paths) == 0 {
		t.Fatal("no scenarios in spec/scenarios")
	}
	drivers := []driver{apiDriver{lc.Hub, lc.Token}, mcpDriver{lc.Hub, lc.Token}, cliDriver{}}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var sc scenario
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&sc); err != nil {
			t.Fatalf("%s: %v", filepath.Base(p), err)
		}
		bundles := map[string]map[string]any{}
		for name, spec := range sc.Bundles {
			files, err := patchedBundle(filepath.Join(root, filepath.FromSlash(spec.From)), spec.Patch)
			if err != nil {
				t.Fatalf("%s: bundle %s: %v", sc.Name, name, err)
			}
			bundles[name] = files
		}
		for _, d := range drivers {
			t.Run(sc.Name+"/"+d.name(), func(t *testing.T) {
				if why := sc.Skip[d.name()]; why != "" {
					t.Skip(why)
				}
				vars := map[string]any{"ns": "sc-" + d.name() + "-" + sc.Name}
				for i, st := range sc.Steps {
					if why := st.Skip[d.name()]; why != "" {
						continue
					}
					runStep(t, d, i, st, vars, bundles)
				}
			})
		}
	}
}

func runStep(t *testing.T, d driver, i int, st scenarioStep, vars map[string]any, bundles map[string]map[string]any) {
	t.Helper()
	args := substitute(st.Args, vars, bundles).(map[string]any)
	deadline := time.Now().Add(time.Duration(st.Retry) * time.Second)
	for {
		out, err := d.call(t, st.Call, args)
		problem := ""
		switch {
		case st.Error != "" && err == nil:
			problem = fmt.Sprintf("want an error containing %q, got %v", st.Error, out)
		case st.Error != "" && !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(st.Error)):
			problem = fmt.Sprintf("want an error containing %q, got %v", st.Error, err)
		case st.Error == "" && err != nil:
			problem = err.Error()
		case err == nil:
			problem = expectAll(out, substitute(st.Expect, vars, bundles))
		}
		if problem == "" {
			for v, path := range st.Save {
				vars[v] = lookup(out, path)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("step %d %s: %s", i+1, st.Call, problem)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// substitute replaces "$var" strings with saved values, "$bundle:name" with
// a bundle's files and "$in:30m" with that time from now (RFC 3339).
func substitute(v any, vars map[string]any, bundles map[string]map[string]any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			out[k] = substitute(val, vars, bundles)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = substitute(x[i], vars, bundles)
		}
		return out
	case string:
		switch {
		case strings.HasPrefix(x, "$bundle:"):
			return bundles[strings.TrimPrefix(x, "$bundle:")]
		case strings.HasPrefix(x, "$in:"):
			d, _ := time.ParseDuration(strings.TrimPrefix(x, "$in:"))
			return time.Now().Add(d).UTC().Format(time.RFC3339)
		case strings.HasPrefix(x, "$"):
			if val, ok := vars[x[1:]]; ok {
				return val
			}
		}
	case nil:
		return map[string]any{}
	}
	return v
}

// expectAll checks "path": want pairs. want is a value (compared as text),
// or {"contains": x}, {"has": {field: value}} (an array element with those
// fields), {"exists": bool} or {"len": n}.
func expectAll(out map[string]any, expect any) string {
	m, _ := expect.(map[string]any)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, path := range keys {
		got := lookup(out, path)
		if p := expectOne(got, m[path]); p != "" {
			return fmt.Sprintf("%s: %s (response %v)", path, p, out)
		}
	}
	return ""
}

func expectOne(got, want any) string {
	op, isOp := want.(map[string]any)
	if !isOp || len(op) != 1 {
		if fmt.Sprint(got) != fmt.Sprint(want) {
			return fmt.Sprintf("got %v, want %v", got, want)
		}
		return ""
	}
	for k, w := range op {
		switch k {
		case "contains":
			if !strings.Contains(fmt.Sprint(got), fmt.Sprint(w)) {
				return fmt.Sprintf("%v does not contain %v", got, w)
			}
		case "exists":
			if (got != nil) != w.(bool) {
				return fmt.Sprintf("exists = %v, want %v", got != nil, w)
			}
		case "len":
			list, _ := got.([]any)
			if fmt.Sprint(len(list)) != fmt.Sprint(w) {
				return fmt.Sprintf("len %d, want %v", len(list), w)
			}
		case "has":
			list, _ := got.([]any)
			for _, el := range list {
				if expectAll(asMap(el), w) == "" {
					return ""
				}
			}
			return fmt.Sprintf("no element with %v in %v", w, got)
		default:
			if fmt.Sprint(got) != fmt.Sprint(want) {
				return fmt.Sprintf("got %v, want %v", got, want)
			}
		}
	}
	return ""
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

// lookup follows a dotted path ("task.state", "tasks.0.id").
func lookup(v any, path string) any {
	if path == "" {
		return v
	}
	for _, part := range strings.Split(path, ".") {
		switch x := v.(type) {
		case map[string]any:
			v = x[part]
		case []any:
			var i int
			if _, err := fmt.Sscan(part, &i); err != nil || i >= len(x) {
				return nil
			}
			v = x[i]
		default:
			return nil
		}
	}
	return v
}

// patchedBundle reads a bundle folder as text files and applies JSON merge
// patches (RFC 7396) per file; a null patch drops the file.
func patchedBundle(dir string, patches map[string]json.RawMessage) (map[string]any, error) {
	files := map[string]any{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		files[filepath.ToSlash(rel)] = string(b)
		return err
	})
	if err != nil {
		return nil, err
	}
	for file, raw := range patches {
		if string(raw) == "null" {
			delete(files, file)
			continue
		}
		var base, patch any
		if s, ok := files[file].(string); ok {
			if err := json.Unmarshal([]byte(s), &base); err != nil {
				return nil, fmt.Errorf("%s: %w", file, err)
			}
		}
		if err := json.Unmarshal(raw, &patch); err != nil {
			return nil, err
		}
		b, _ := json.MarshalIndent(mergePatch(base, patch), "", "  ")
		files[file] = string(b) + "\n"
	}
	return files, nil
}

func mergePatch(base, patch any) any {
	p, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	b, ok := base.(map[string]any)
	if !ok {
		b = map[string]any{}
	}
	out := map[string]any{}
	for k, v := range b {
		out[k] = v
	}
	for k, v := range p {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = mergePatch(out[k], v)
	}
	return out
}
