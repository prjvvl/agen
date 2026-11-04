package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"os/exec"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// cmdEval runs a bundle against every scenario file in a directory, with
// its tools mocked, scoring each run with rule checks and one LLM-judge
// call per run when the scenario has a rubric (§6 learning and testing).
// The real work (mocking tools, scoring, pass@1/pass^k per scenario) is
// `agen-host eval`; this command loops it over the scenario files and
// aggregates the reports, the same shape `agen replay` already uses to
// drive `agen-host` directly with no Hub involved. `agen deploy
// --require-eval` (2.43, part) runs the exact same aggregation via
// runEvalScenarios, then records the report against the deployment and
// compares it with the last one; `agen eval history <deployment>` reads
// those recorded reports back.
func (e *env) cmdEval(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "history" {
		return e.cmdEvalHistory(ctx, args[1:])
	}
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	scenarios := fs.String("scenarios", "", "directory of scenario JSON files (required)")
	runs := fs.Int("runs", 1, "runs per scenario")
	hostBin := fs.String("host-bin", "", "agen-host executable (default: next to agen, $AGEN_HOST_BIN, or on PATH)")
	store := fs.String("store", "", "store URL for the eval runs (default: a scratch SQLite database, removed after)")
	asJSON := fs.Bool("json", false, "print the full JSON report instead of a table")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *scenarios == "" {
		return usageErr("agen eval <bundle-dir> --scenarios DIR [--runs N] [--json]")
	}
	if *runs < 1 {
		return usageErr("agen eval: --runs must be at least 1")
	}
	bundle := pos[0]
	bin, err := findHostBin(*hostBin)
	if err != nil {
		return err
	}
	files, err := scenarioFiles(*scenarios)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no *.json scenario files in %s", *scenarios)
	}

	reports, allPass, err := runEvalScenarios(ctx, bin, bundle, files, *runs, *store)
	if err != nil {
		return err
	}

	if *asJSON {
		b, err := json.MarshalIndent(reports, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(e.stdout, string(b))
	} else {
		w := e.table()
		fmt.Fprintln(w, "SCENARIO\tRUNS\tPASS@1\tPASS^K")
		for _, r := range reports {
			runsList, _ := r["runs"].([]any)
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", r["scenario"], len(runsList),
				yesNo(r["pass1"]), yesNo(r["passK"]))
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	if !allPass {
		return fmt.Errorf("%d/%d scenarios did not pass^%d", countFails(reports), len(reports), *runs)
	}
	return nil
}

func yesNo(v any) string {
	if b, _ := v.(bool); b {
		return "yes"
	}
	return "no"
}

func countFails(reports []map[string]any) int {
	n := 0
	for _, r := range reports {
		if passK, _ := r["passK"].(bool); !passK {
			n++
		}
	}
	return n
}

// runEvalScenarios runs every scenario file against bundle with its tools
// mocked (shelling out to `agen-host eval` once per file, same as cmdEval),
// aggregating a report per scenario. storeURL "" uses a scratch SQLite
// database, removed once done. Shared by `agen eval` and `agen deploy
// --require-eval` (2.43, part), so the two commands can never drift.
func runEvalScenarios(ctx context.Context, hostBin, bundle string, files []string, runs int, storeURL string) ([]map[string]any, bool, error) {
	if storeURL == "" {
		dir, err := os.MkdirTemp("", "agen-eval-*")
		if err != nil {
			return nil, false, err
		}
		defer os.RemoveAll(dir)
		storeURL = "sqlite:" + filepath.ToSlash(filepath.Join(dir, "eval.db"))
	}
	var reports []map[string]any
	allPass := true
	for _, f := range files {
		report, err := runEvalScenario(ctx, hostBin, bundle, f, runs, storeURL)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		if passK, _ := report["passK"].(bool); !passK {
			allPass = false
		}
		reports = append(reports, report)
	}
	return reports, allPass, nil
}

// scenarioFiles lists the *.json files directly inside dir, sorted by name
// so a report's scenario order is stable across runs.
func scenarioFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, ent := range entries {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		files = append(files, filepath.Join(dir, ent.Name()))
	}
	sort.Strings(files)
	return files, nil
}

// runEvalScenario shells out to `agen-host eval`, which always prints its
// JSON report to stdout before exiting — 0 when the scenario passed^k, 1
// when it did not. Only the absence of a report on stdout (a crash, a bad
// bundle or scenario file) is treated as this command's own error.
func runEvalScenario(ctx context.Context, hostBin, bundle, scenarioFile string, runs int, storeURL string) (map[string]any, error) {
	cmd := exec.CommandContext(ctx, hostBin, "eval", bundle, "--scenario", scenarioFile, "--runs", strconv.Itoa(runs), "--store", storeURL)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	_ = cmd.Run()
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("%s", bytes.TrimSpace(stderr.Bytes()))
	}
	var report map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		return nil, fmt.Errorf("%w (stderr: %s)", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return report, nil
}

// cmdEvalHistory lists the eval reports `agen deploy --require-eval` has
// recorded against a deployment, newest first (2.43, part: eval history).
func (e *env) cmdEvalHistory(ctx context.Context, args []string) error {
	var cf clientFlags
	fs := e.flags("eval history", &cf)
	limit := fs.Int("limit", 0, "most recent N reports (default 20)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("agen eval history <deployment> [--limit N] [-n NS]")
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	r, err := c.ListEvalReports(ctx, connect.NewRequest(&agenv1.ListEvalReportsRequest{Ref: ref(cf, pos[0]), Limit: int32(*limit)}))
	if err != nil {
		return err
	}
	if cf.json {
		return e.printJSON(r.Msg)
	}
	w := e.table()
	fmt.Fprintln(w, "ID\tDEFINITION\tCREATED")
	for _, rep := range r.Msg.Reports {
		fmt.Fprintf(w, "%s\t%s\t%s\n", rep.Id, shortDigest(rep.DefinitionDigest), rep.CreatedAt.AsTime().Format(time.RFC3339))
	}
	return w.Flush()
}

// evalComparisonLines compares a new eval report against the previous one
// recorded for a deployment (2.43, part), one line per scenario that
// changed: a scenario passing before and failing now ("regressed"), or
// failing before and passing now ("fixed"). A scenario missing from
// either side, or unchanged, prints nothing — only what's worth a look.
func evalComparisonLines(oldReportJSON string, newReports []map[string]any) []string {
	if oldReportJSON == "" {
		return nil
	}
	var old []map[string]any
	if err := json.Unmarshal([]byte(oldReportJSON), &old); err != nil {
		return nil
	}
	oldPassK := map[string]bool{}
	for _, r := range old {
		name, _ := r["scenario"].(string)
		passK, _ := r["passK"].(bool)
		oldPassK[name] = passK
	}
	var lines []string
	for _, r := range newReports {
		name, _ := r["scenario"].(string)
		passK, _ := r["passK"].(bool)
		was, known := oldPassK[name]
		switch {
		case !known:
		case was && !passK:
			lines = append(lines, fmt.Sprintf("regressed: %s now fails (passed last eval)", name))
		case !was && passK:
			lines = append(lines, fmt.Sprintf("fixed: %s now passes (failed last eval)", name))
		}
	}
	return lines
}
