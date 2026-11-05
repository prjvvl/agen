package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeScenario(t *testing.T, dir, name string, scenario map[string]any) {
	t.Helper()
	b, err := json.Marshal(scenario)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// `agen eval` loops `agen-host eval` over every scenario file and reports
// pass@1/pass^k per scenario, failing overall (non-zero) when any scenario
// does not pass^k, without needing a Hub or any deployment — it drives
// `agen-host` directly, the same way `agen replay` already does.
// features: eval
func TestEvalFailsOverallWhenOneScenarioDoesNotPass(t *testing.T) {
	bin := hostBin(t)
	bundle := filepath.Join(t.TempDir(), "hello")
	copyDir(t, filepath.Join(repoRoot(), "examples", "bundles", "hello"), bundle)

	scenarios := t.TempDir()
	writeScenario(t, scenarios, "greets.json", map[string]any{
		"input": "hi",
		"rules": map[string]any{"mustContain": []string{"Hello"}},
	})
	writeScenario(t, scenarios, "wrong.json", map[string]any{
		"input": "hi",
		"rules": map[string]any{"mustContain": []string{"this text never appears"}},
	})

	out := agen(t, 1, "eval", bundle, "--scenarios", scenarios, "--host-bin", bin)
	if !strings.Contains(out, "greets") || !strings.Contains(out, "wrong") {
		t.Fatalf("report should list both scenarios: %s", out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var greets, wrong []string
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "greets":
			greets = f
		case "wrong":
			wrong = f
		}
	}
	if len(greets) < 4 || greets[2] != "yes" || greets[3] != "yes" {
		t.Errorf("greets should pass@1 and pass^k: %q", greets)
	}
	if len(wrong) < 4 || wrong[2] == "yes" && wrong[3] == "yes" {
		t.Errorf("wrong should not pass: %q", wrong)
	}
}

// Every scenario passing exits 0; --json prints the full per-run detail.
// features: eval
func TestEvalAllScenariosPassingExitsZeroWithJSON(t *testing.T) {
	bin := hostBin(t)
	bundle := filepath.Join(t.TempDir(), "hello")
	copyDir(t, filepath.Join(repoRoot(), "examples", "bundles", "hello"), bundle)

	scenarios := t.TempDir()
	writeScenario(t, scenarios, "greets.json", map[string]any{
		"input": "hi",
		"rules": map[string]any{"mustContain": []string{"Hello"}},
	})

	out := agen(t, 0, "eval", bundle, "--scenarios", scenarios, "--runs", "2", "--host-bin", bin, "--json")
	var reports []map[string]any
	if err := json.Unmarshal([]byte(out), &reports); err != nil {
		t.Fatalf("--json output did not parse: %v\n%s", err, out)
	}
	if len(reports) != 1 {
		t.Fatalf("want 1 report, got %d: %s", len(reports), out)
	}
	r := reports[0]
	if r["scenario"] != "greets" || r["pass1"] != true || r["passK"] != true {
		t.Fatalf("unexpected report: %v", r)
	}
	runs, _ := r["runs"].([]any)
	if len(runs) != 2 {
		t.Fatalf("want 2 runs, got %d: %v", len(runs), r)
	}
}

// evalComparisonLines (2.43, part: comparison against the last eval)
// reports only what changed since the last recorded report: a regression
// (passed before, fails now), a fix (failed before, passes now), nothing
// for a scenario that's unchanged or new.
func TestEvalComparisonLinesReportsOnlyWhatChanged(t *testing.T) {
	old := `[{"scenario":"a","passK":true},{"scenario":"b","passK":false},{"scenario":"c","passK":true}]`
	new_ := []map[string]any{
		{"scenario": "a", "passK": false}, // regressed
		{"scenario": "b", "passK": true},  // fixed
		{"scenario": "c", "passK": true},  // unchanged: no line
		{"scenario": "d", "passK": false}, // new: no line (nothing to compare against)
	}
	lines := evalComparisonLines(old, new_)
	if len(lines) != 2 {
		t.Fatalf("want exactly 2 lines, got %v", lines)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "regressed: a") || !strings.Contains(joined, "fixed: b") {
		t.Fatalf("lines: %v", lines)
	}
	if empty := evalComparisonLines("", new_); empty != nil {
		t.Fatalf("no previous report should produce no lines: %v", empty)
	}
}
