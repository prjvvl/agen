// Package features checks that every user-facing feature is tested on every
// surface it is offered on (SDKs, API, MCP, CLI, console, installs), using
// spec/features.yaml and the `features:` tags tests carry, and renders the
// support matrix (docs/reference/support.md).
package features

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Spec is spec/features.yaml.
type Spec struct {
	Surfaces []Surface         `yaml:"surfaces"`
	Ignore   map[string]any    `yaml:"ignore"` // kind -> {item: reason}
	Features []Feature         `yaml:"features"`
	Notes    map[string]string `yaml:"notes"`
}

// Surface is one way people use agen: an SDK, an interface of the fleet, or
// a way to install and run it.
type Surface struct {
	ID    string `yaml:"id"`
	Group string `yaml:"group"`
	Title string `yaml:"title"`
	// Paths are the test locations (repo-relative prefixes) whose tags count
	// for this surface.
	Paths []string `yaml:"paths"`
}

// Feature is one capability and where it must work.
type Feature struct {
	ID       string            `yaml:"id"`
	Title    string            `yaml:"title"`
	Contract []string          `yaml:"contract"`
	On       []string          `yaml:"on"`
	Not      map[string]string `yaml:"not"` // surface -> why it is not offered there
	Gap      map[string]string `yaml:"gap"` // surface -> what is missing ("" when only the test is)
}

// Tag is one `features:` declaration in a test file.
type Tag struct {
	File     string
	Line     int
	Features []string
	Surfaces []string // where the tagged test exercises the features
}

// Load reads spec/features.yaml.
func Load(root string) (*Spec, error) {
	b, err := os.ReadFile(filepath.Join(root, "spec", "features.yaml"))
	if err != nil {
		return nil, err
	}
	var s Spec
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("spec/features.yaml: %w", err)
	}
	return &s, nil
}

// `// features: a.b, c.d` or `# features[api,cli]: a.b` (explicit surfaces).
var tagLine = regexp.MustCompile(`^\s*(?://|#)\s*features(?:\[([a-z0-9, -]+)\])?:\s*(.+?)\s*$`)

// testFile reports whether a repo-relative path holds tests.
func testFile(rel string) bool {
	base := filepath.Base(rel)
	switch {
	case strings.HasSuffix(base, "_test.go"),
		strings.HasSuffix(base, ".spec.ts"),
		strings.HasSuffix(base, ".test.js"),
		strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"),
		strings.HasPrefix(base, "test-install."):
		return true
	case strings.HasSuffix(base, ".rs"):
		return strings.Contains(rel, "/tests/")
	case strings.HasSuffix(base, ".json"):
		return strings.HasPrefix(rel, "spec/conformance/cases/") || strings.HasPrefix(rel, "spec/scenarios/")
	}
	return false
}

var skipDirs = map[string]bool{"node_modules": true, "target": true, ".git": true, ".venv": true, "dist": true, "test-results": true, "gen": true}

// Scan finds the `features:` tags in the repo's tests. A tag's surfaces are
// the ones it names, else those whose paths contain the file.
func (s *Spec) Scan(root string) ([]Tag, error) {
	var tags []Tag
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if !testFile(rel) {
			return nil
		}
		found, err := scanFile(p, rel)
		if err != nil {
			return err
		}
		for _, t := range found {
			if t.Surfaces == nil {
				t.Surfaces = s.surfacesFor(rel)
			}
			tags = append(tags, t)
		}
		return nil
	})
	return tags, err
}

func scanFile(path, rel string) ([]Tag, error) {
	if strings.HasSuffix(rel, ".json") {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var c struct {
			Features []string `json:"features"`
		}
		if err := json.Unmarshal(b, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		if len(c.Features) == 0 {
			return nil, nil
		}
		return []Tag{{File: rel, Line: 1, Features: c.Features}}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Tag
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for n := 1; sc.Scan(); n++ {
		m := tagLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		t := Tag{File: rel, Line: n, Features: split(m[2])}
		if m[1] != "" {
			t.Surfaces = split(m[1])
		}
		out = append(out, t)
	}
	return out, sc.Err()
}

func split(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// surfacesFor returns the surfaces whose paths hold the file (longest
// matching prefix wins, so a narrower path can override a folder's).
func (s *Spec) surfacesFor(rel string) []string {
	best, out := -1, []string(nil)
	for _, sf := range s.Surfaces {
		for _, p := range sf.Paths {
			if !strings.HasPrefix(rel, p) {
				continue
			}
			switch {
			case len(p) > best:
				best, out = len(p), []string{sf.ID}
			case len(p) == best:
				out = append(out, sf.ID)
			}
		}
	}
	return out
}

// Coverage maps feature -> surface -> the tests that exercise it there.
type Coverage map[string]map[string][]string

// Cover groups tags by feature and surface.
func Cover(tags []Tag) Coverage {
	c := Coverage{}
	for _, t := range tags {
		for _, f := range t.Features {
			if c[f] == nil {
				c[f] = map[string][]string{}
			}
			for _, s := range t.Surfaces {
				c[f][s] = append(c[f][s], fmt.Sprintf("%s:%d", t.File, t.Line))
			}
		}
	}
	return c
}

// Check reports problems: unknown ids, features not tested where they are
// offered (and not marked as a gap), gaps that are in fact tested, and tags
// naming unknown features.
func (s *Spec) Check(tags []Tag) []string {
	var out []string
	surfaces := map[string]bool{}
	for _, sf := range s.Surfaces {
		surfaces[sf.ID] = true
	}
	ids := map[string]bool{}
	validID := regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z][a-z0-9-]*)*$`)
	for _, f := range s.Features {
		if !validID.MatchString(f.ID) {
			out = append(out, fmt.Sprintf("feature %q: ids are lower-case words joined by dots (area.name)", f.ID))
		}
		if ids[f.ID] {
			out = append(out, fmt.Sprintf("feature %s is listed twice", f.ID))
		}
		ids[f.ID] = true
		on := map[string]bool{}
		for _, x := range f.On {
			if !surfaces[x] {
				out = append(out, fmt.Sprintf("feature %s: unknown surface %q in on", f.ID, x))
			}
			on[x] = true
		}
		for x := range f.Not {
			if !surfaces[x] {
				out = append(out, fmt.Sprintf("feature %s: unknown surface %q in not", f.ID, x))
			}
			if on[x] {
				out = append(out, fmt.Sprintf("feature %s: %s is both in on and not", f.ID, x))
			}
		}
		for x := range f.Gap {
			if !on[x] {
				out = append(out, fmt.Sprintf("feature %s: gap on %s, which is not in on", f.ID, x))
			}
		}
	}
	cov := Cover(tags)
	for _, t := range tags {
		for _, f := range t.Features {
			if !ids[f] {
				out = append(out, fmt.Sprintf("%s:%d: unknown feature %q (add it to spec/features.yaml)", t.File, t.Line, f))
			}
		}
		if len(t.Surfaces) == 0 {
			out = append(out, fmt.Sprintf("%s:%d: no surface covers this file; name them: features[api,cli]: ...", t.File, t.Line))
		}
		for _, x := range t.Surfaces {
			if !surfaces[x] {
				out = append(out, fmt.Sprintf("%s:%d: unknown surface %q", t.File, t.Line, x))
			}
		}
	}
	for _, f := range s.Features {
		for _, x := range f.On {
			tested := len(cov[f.ID][x]) > 0
			_, gap := f.Gap[x]
			switch {
			case !tested && !gap:
				out = append(out, fmt.Sprintf("feature %s: no %s test; tag one with `features: %s`, or record it under gap or not", f.ID, x, f.ID))
			case tested && gap:
				out = append(out, fmt.Sprintf("feature %s: %s is tested (%s); remove it from gap", f.ID, x, cov[f.ID][x][0]))
			}
		}
	}
	sort.Strings(out)
	return out
}

// Inventory is everything users can touch that a feature must account for,
// keyed "kind:item" (rpc:SubmitTask, field:SubmitTask.model_tier,
// harness:router, config:triggers, agent:skills, cli:run, sdk:modelTier).
type Inventory map[string]bool

// CheckContracts reports inventory items no feature's contract (or the
// ignore list) accounts for, and contract items that do not exist.
func (s *Spec) CheckContracts(inv Inventory) []string {
	var out []string
	claimed := map[string]bool{}
	for _, f := range s.Features {
		for _, c := range f.Contract {
			if !inv[c] {
				out = append(out, fmt.Sprintf("feature %s: contract item %q does not exist", f.ID, c))
			}
			claimed[c] = true
		}
	}
	for item := range inv {
		if claimed[item] || s.ignored(item) {
			continue
		}
		out = append(out, fmt.Sprintf("%s belongs to no feature: add it to a feature's contract in spec/features.yaml", item))
	}
	sort.Strings(out)
	return out
}

// ignored: spec ignore lists name items by kind; for fields a bare field
// name (e.g. namespace) covers it on every request.
func (s *Spec) ignored(item string) bool {
	kind, name, _ := strings.Cut(item, ":")
	m, ok := s.Ignore[kind].(map[string]any)
	if !ok {
		return false
	}
	if _, ok := m[name]; ok {
		return true
	}
	if kind == "field" {
		_, f, _ := strings.Cut(name, ".")
		_, ok := m[f]
		return ok
	}
	return false
}

// SDKSources are the files where each SDK spells out its spec and run
// option keys; a key a feature offers on that SDK must appear there.
var SDKSources = map[string][]string{
	"python": {"sdks/python/python/agen/__init__.py"},
	"node":   {"sdks/node/index.js", "sdks/node/index.d.ts"},
	"go":     {"sdks/go/agen/agen.go"},
}

// CheckSDKKeys reports sdk:<key> contract items missing from an SDK the
// feature is offered on.
func (s *Spec) CheckSDKKeys(root string) []string {
	var out []string
	src := map[string]string{}
	for sdk, files := range SDKSources {
		var b strings.Builder
		for _, f := range files {
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
			if err != nil {
				out = append(out, err.Error())
				continue
			}
			b.Write(data)
		}
		src[sdk] = b.String()
	}
	for _, f := range s.Features {
		for _, c := range f.Contract {
			key, ok := strings.CutPrefix(c, "sdk:")
			if !ok {
				continue
			}
			word := regexp.MustCompile(`\b` + regexp.QuoteMeta(key) + `\b`)
			for _, x := range f.On {
				body, isSDK := src[x]
				if !isSDK || word.MatchString(body) {
					continue
				}
				if _, gap := f.Gap[x]; gap {
					continue
				}
				out = append(out, fmt.Sprintf("feature %s: the %s SDK does not expose %q (%s)", f.ID, x, key, strings.Join(SDKSources[x], ", ")))
			}
		}
	}
	sort.Strings(out)
	return out
}

// Markdown renders the support matrix.
func (s *Spec) Markdown(tags []Tag) string {
	cov := Cover(tags)
	var b strings.Builder
	b.WriteString("# Support matrix\n\n")
	b.WriteString("<!-- Generated from spec/features.yaml and the `features:` tags in tests: AGEN_UPDATE_DOCS=1 go test ./internal/features -->\n\n")
	b.WriteString("What works where, and what proves it. ✓ is tested on that surface, ◐ is offered there but not tested yet (or, with a note, not fully offered yet), – is deliberately not offered there (see the note), and an empty cell does not apply.\n\n")
	if s.Notes["intro"] != "" {
		b.WriteString(strings.TrimSpace(s.Notes["intro"]) + "\n\n")
	}
	var groups []string
	byGroup := map[string][]Surface{}
	for _, sf := range s.Surfaces {
		if byGroup[sf.Group] == nil {
			groups = append(groups, sf.Group)
		}
		byGroup[sf.Group] = append(byGroup[sf.Group], sf)
	}
	var notes []string
	for _, g := range groups {
		cols := byGroup[g]
		fmt.Fprintf(&b, "## %s\n\n| Feature |", s.groupTitle(g))
		for _, c := range cols {
			fmt.Fprintf(&b, " %s |", c.Title)
		}
		b.WriteString("\n|---|")
		for range cols {
			b.WriteString(":-:|")
		}
		b.WriteString("\n")
		for _, f := range s.Features {
			if !touches(f, cols) {
				continue
			}
			fmt.Fprintf(&b, "| %s |", f.Title)
			for _, c := range cols {
				cell := ""
				on := contains(f.On, c.ID)
				switch {
				case on && isGap(f, c.ID):
					cell = "◐"
					if note := f.Gap[c.ID]; note != "" {
						notes = append(notes, fmt.Sprintf("%s on %s: %s", f.Title, c.Title, note))
						cell = fmt.Sprintf("◐[^%d]", len(notes))
					}
				case on && len(cov[f.ID][c.ID]) > 0:
					cell = "✓"
				case f.Not[c.ID] != "":
					notes = append(notes, fmt.Sprintf("%s on %s: %s", f.Title, c.Title, f.Not[c.ID]))
					cell = fmt.Sprintf("–[^%d]", len(notes))
				}
				fmt.Fprintf(&b, " %s |", cell)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	for i, n := range notes {
		fmt.Fprintf(&b, "[^%d]: %s\n", i+1, n)
	}
	return b.String()
}

func (s *Spec) groupTitle(g string) string {
	if t := s.Notes["group."+g]; t != "" {
		return t
	}
	return g
}

func isGap(f Feature, surface string) bool {
	_, ok := f.Gap[surface]
	return ok
}

func touches(f Feature, cols []Surface) bool {
	for _, c := range cols {
		if contains(f.On, c.ID) || f.Not[c.ID] != "" {
			return true
		}
	}
	return false
}

func contains(list []string, x string) bool {
	for _, y := range list {
		if y == x {
			return true
		}
	}
	return false
}
