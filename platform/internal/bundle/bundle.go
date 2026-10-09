// Package bundle validates agent bundles uploaded to the Hub and extracts the
// deployment policy from x-agen/config.json. It uses the same JSON Schemas as
// the engine (spec/bundle, copied into ./schemas by scripts/gen.sh); the engine
// re-validates everything when an instance starts.
package bundle

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"gopkg.in/yaml.v3"

	"github.com/prjvvl/agen/platform/internal/definition"
)

//go:embed schemas/*.json
var schemaFS embed.FS

var (
	compileOnce sync.Once
	schemas     map[string]*jsonschema.Schema
	compileErr  error
)

func compiled() (map[string]*jsonschema.Schema, error) {
	compileOnce.Do(func() {
		c := jsonschema.NewCompiler()
		names := []string{"plugin", "mcp", "agent", "harness", "config", "secrets"}
		for _, n := range names {
			b, err := schemaFS.ReadFile("schemas/" + n + ".schema.json")
			if err != nil {
				compileErr = err
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
			if err != nil {
				compileErr = err
				return
			}
			if err := c.AddResource(n+".json", doc); err != nil {
				compileErr = err
				return
			}
		}
		schemas = map[string]*jsonschema.Schema{}
		for _, n := range names {
			s, err := c.Compile(n + ".json")
			if err != nil {
				compileErr = err
				return
			}
			schemas[n] = s
		}
	})
	return schemas, compileErr
}

// Scale is the deployment scale policy from config.json.
type Scale struct {
	Min                    int    `json:"min"`
	Max                    int    `json:"max"`
	TargetQueuePerInstance int    `json:"targetQueuePerInstance,omitempty"`
	IdleTimeout            string `json:"idleTimeout,omitempty"`
	MaxConcurrency         int    `json:"maxConcurrency,omitempty"`
}

// IdleSeconds parses IdleTimeout ("30s", "5m", ...); 0 if unset.
func (s Scale) IdleSeconds() int {
	if s.IdleTimeout == "" {
		return 0
	}
	d, err := time.ParseDuration(s.IdleTimeout)
	if err != nil {
		return 0
	}
	return int(d.Seconds())
}

// Trigger from config.json.
type Trigger struct {
	Type     string `json:"type"`
	Name     string `json:"name,omitempty"`
	Schedule string `json:"schedule,omitempty"`
	Input    string `json:"input,omitempty"`
	CatchUp  bool   `json:"catchUp,omitempty"`
}

// Bundle is a validated upload.
type Bundle struct {
	Name     string
	Digest   string
	Kind     string
	Scale    Scale
	Budget   json.RawMessage
	Limits   json.RawMessage
	Triggers []Trigger
	Files    map[string][]byte
}

// Error lists every problem found.
type Error struct{ Issues []string }

func (e *Error) Error() string { return "invalid bundle:\n  - " + strings.Join(e.Issues, "\n  - ") }

var frontmatter = regexp.MustCompile(`(?s)\A\x{feff}?---[ \t]*\r?\n(.*?)\r?\n---[ \t]*\r?\n(.*)\z`)

// Parse validates bundle files (path -> bytes) and extracts its policy.
func Parse(files map[string][]byte) (*Bundle, error) {
	sch, err := compiled()
	if err != nil {
		return nil, fmt.Errorf("bundle schemas: %w", err)
	}
	var issues []string
	add := func(format string, a ...any) { issues = append(issues, fmt.Sprintf(format, a...)) }
	clean := map[string][]byte{}
	for p, b := range files {
		p = strings.TrimPrefix(path.Clean(strings.ReplaceAll(p, `\`, "/")), "./")
		if p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") {
			add("%s: path escapes the bundle", p)
			continue
		}
		// Nests refuse ':' (a Windows drive or stream), so the Hub does too.
		if strings.Contains(p, ":") {
			add("%s: ':' is not allowed in bundle paths", p)
			continue
		}
		if !definition.Ignored(p) {
			clean[p] = b
		}
	}
	check := func(file, schema string, required bool) map[string]any {
		b, ok := clean[file]
		if !ok {
			if required {
				add("%s: required file is missing", file)
			}
			return nil
		}
		v, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			add("%s: invalid JSON: %v", file, err)
			return nil
		}
		if err := sch[schema].Validate(v); err != nil {
			for _, line := range schemaErrors(err) {
				add("%s: %s", file, line)
			}
			return nil
		}
		m, _ := v.(map[string]any)
		return m
	}
	plugin := check("plugin.json", "plugin", true)
	check("mcp.json", "mcp", false)
	check("x-agen/harness.json", "harness", true)
	config := check("x-agen/config.json", "config", false)
	check("x-agen/secrets.json", "secrets", false)

	b := &Bundle{Files: clean, Kind: "pool", Scale: Scale{Min: 0, Max: 1}, Budget: json.RawMessage("{}"), Limits: json.RawMessage("{}")}
	if md, ok := clean["x-agen/agent.md"]; !ok {
		add("x-agen/agent.md: required file is missing")
	} else if m := frontmatter.FindSubmatch(md); m == nil {
		add("x-agen/agent.md: missing YAML frontmatter (--- ... ---)")
	} else {
		var front map[string]any
		if err := yaml.Unmarshal(m[1], &front); err != nil {
			add("x-agen/agent.md: invalid frontmatter YAML: %v", err)
		} else {
			fj, _ := json.Marshal(front)
			v, _ := jsonschema.UnmarshalJSON(bytes.NewReader(fj))
			if err := sch["agent"].Validate(v); err != nil {
				for _, line := range schemaErrors(err) {
					add("x-agen/agent.md: %s", line)
				}
			}
			if name, _ := front["name"].(string); name != "" {
				b.Name = name
			}
		}
		if strings.TrimSpace(string(m[2])) == "" {
			add("x-agen/agent.md: system prompt body is empty")
		}
	}
	if plugin != nil {
		if pn, _ := plugin["name"].(string); b.Name != "" && pn != b.Name {
			add("x-agen/agent.md: name %q must match plugin.json name %q", b.Name, pn)
		}
	}
	if config != nil {
		raw, _ := json.Marshal(config)
		var c struct {
			Kind     string          `json:"kind"`
			Scale    *Scale          `json:"scale"`
			Budget   json.RawMessage `json:"budget"`
			Limits   json.RawMessage `json:"limits"`
			Triggers []Trigger       `json:"triggers"`
		}
		_ = json.Unmarshal(raw, &c)
		if c.Kind != "" {
			b.Kind = c.Kind
		}
		if c.Scale != nil {
			b.Scale = *c.Scale
			if b.Scale.Max == 0 {
				b.Scale.Max = 1
			}
		}
		if len(c.Budget) > 0 {
			b.Budget = c.Budget
		}
		if len(c.Limits) > 0 {
			b.Limits = c.Limits
		}
		b.Triggers = c.Triggers
		seen := map[string]bool{}
		for i, t := range b.Triggers {
			if t.Name == "" {
				b.Triggers[i].Name = fmt.Sprintf("%s-%d", t.Type, i)
			}
			if seen[b.Triggers[i].Name] {
				add("x-agen/config.json: trigger name %q is used twice", b.Triggers[i].Name)
			}
			seen[b.Triggers[i].Name] = true
			if t.Type == "cron" {
				if _, err := cron.ParseStandard(t.Schedule); err != nil {
					add("x-agen/config.json: trigger %q: invalid cron schedule %q: %v", b.Triggers[i].Name, t.Schedule, err)
				}
			}
		}
	}
	switch b.Kind {
	case "singleton":
		if b.Scale.Max > 1 {
			add("x-agen/config.json: a singleton runs at most one instance (scale.max must be 1)")
		}
		b.Scale.Max = 1
	}
	if b.Scale.Min > b.Scale.Max {
		add("x-agen/config.json: scale.min (%d) exceeds scale.max (%d)", b.Scale.Min, b.Scale.Max)
	}
	if len(issues) > 0 {
		sort.Strings(issues)
		return nil, &Error{Issues: issues}
	}
	b.Digest = definition.Digest(clean)
	return b, nil
}

var printer = message.NewPrinter(language.English)

func schemaErrors(err error) []string {
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []string{err.Error()}
	}
	var out []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			loc := "/" + strings.Join(e.InstanceLocation, "/")
			out = append(out, fmt.Sprintf("at %s: %s", loc, e.ErrorKind.LocalizedString(printer)))
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	return out
}
