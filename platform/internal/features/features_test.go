package features

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/prjvvl/agen/platform/internal/apidesc"
	"github.com/prjvvl/agen/platform/internal/bundle"
	"github.com/prjvvl/agen/platform/internal/cli"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

// TestFeaturesAreTestedEverywhereTheyAreOffered is the cross-surface gate:
// see docs/contributing (Feature coverage) and spec/features.yaml.
func TestFeaturesAreTestedEverywhereTheyAreOffered(t *testing.T) {
	root := repoRoot(t)
	spec, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := spec.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	var problems []string
	problems = append(problems, spec.Check(tags)...)
	problems = append(problems, spec.CheckContracts(inventory(t, root))...)
	problems = append(problems, spec.CheckSDKKeys(root)...)
	for _, p := range problems {
		t.Error(p)
	}

	path := filepath.Join(root, "docs", "reference", "support.md")
	want := spec.Markdown(tags)
	if os.Getenv("AGEN_UPDATE_DOCS") == "1" {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, _ := os.ReadFile(path)
	if strings.ReplaceAll(string(got), "\r\n", "\n") != want {
		t.Error("docs/reference/support.md is out of date: run AGEN_UPDATE_DOCS=1 go test ./internal/features")
	}
}

// inventory lists what users can touch: Hub API methods and their request
// fields (MCP tools are generated from the same methods), bundle schema
// keys, CLI commands and SDK spec and run option keys.
func inventory(t *testing.T, root string) Inventory {
	t.Helper()
	inv := Inventory{}
	sd, err := apidesc.Service("agen.v1.HubService")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < sd.Methods().Len(); i++ {
		m := sd.Methods().Get(i)
		inv["rpc:"+string(m.Name())] = true
		fields := m.Input().Fields()
		for j := 0; j < fields.Len(); j++ {
			inv["field:"+string(m.Name())+"."+string(fields.Get(j).Name())] = true
		}
	}
	for _, kind := range []string{"harness", "config", "agent"} {
		s, err := bundle.Schema(kind)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal([]byte(s), &schema); err != nil {
			t.Fatal(err)
		}
		for k := range schema.Properties {
			inv[kind+":"+k] = true
		}
	}
	t.Setenv("AGEN_HOME", t.TempDir())
	var out bytes.Buffer
	cli.Main(context.Background(), []string{"help"}, &out, &out)
	cmd := regexp.MustCompile(`(?m)^  agen ([a-z][a-z-]*)`)
	for _, m := range cmd.FindAllStringSubmatch(out.String(), -1) {
		inv["cli:"+m[1]] = true
	}
	for _, k := range sdkKeys(t, root) {
		inv["sdk:"+k] = true
	}
	return inv
}

// sdkKeys reads the agent spec and run option keys every SDK passes to the
// shared core (agen-sdk-core's AgentSpec and RunSpec, camelCase in JSON).
func sdkKeys(t *testing.T, root string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "engine", "crates", "agen-sdk-core", "src", "lib.rs"))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, name := range []string{"AgentSpec", "RunSpec"} {
		start := strings.Index(string(b), "pub struct "+name+" {")
		if start < 0 {
			t.Fatalf("agen-sdk-core: no struct %s", name)
		}
		body := string(b[start:])
		body = body[:strings.Index(body, "\n}")]
		for _, m := range regexp.MustCompile(`(?m)^    pub ([a-z_]+):`).FindAllStringSubmatch(body, -1) {
			keys = append(keys, camel(m[1]))
		}
	}
	return keys
}

func camel(s string) string {
	parts := strings.Split(s, "_")
	for i := 1; i < len(parts); i++ {
		parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
	}
	return strings.Join(parts, "")
}
