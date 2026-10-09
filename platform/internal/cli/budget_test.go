package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// End to end: runs report their model cost; once today's spend reaches the
// deployment's max_usd_per_day, queued work stops being started (clean stop:
// no run is interrupted) and `agen ps` says why.
func TestDailyBudgetEndToEnd(t *testing.T) {
	startUp(t)
	bundle := filepath.Join(t.TempDir(), "spender")
	copyDir(t, filepath.Join(repoRoot(), "examples", "bundles", "hello"), bundle)
	os.WriteFile(filepath.Join(bundle, "x-agen", "config.json"),
		[]byte(`{"kind":"pool","scale":{"min":0,"max":1,"maxConcurrency":1},"budget":{"maxUsdPerDay":1.0}}`), 0o644)
	os.WriteFile(filepath.Join(bundle, "x-agen", "fake-script.json"),
		[]byte(`{"cycle":true,"responses":[{"text":"spent","usage":{"input_tokens":100,"output_tokens":10,"cost_usd":0.4}}]}`), 0o644)
	agen(t, 0, "deploy", bundle, "--name", "spender")
	for i := 0; i < 6; i++ {
		agen(t, 0, "run", "spender", "work", "--no-wait")
	}
	waitUntil(t, "budget exhausted", 60*time.Second, func() bool {
		return strings.Contains(agen(t, 0, "ps"), "daily budget used ($1.20)")
	})
	time.Sleep(3 * time.Second) // several scheduler ticks and dispatch polls
	tasks := agen(t, 0, "tasks", "spender")
	if n := strings.Count(tasks, "  succeeded  "); n != 3 {
		t.Fatalf("%d tasks ran; 3 x $0.40 reach the $1.00 budget:\n%s", n, tasks)
	}
	if n := strings.Count(tasks, "  queued  "); n != 3 {
		t.Fatalf("remaining tasks must wait queued:\n%s", tasks)
	}
	if strings.Contains(tasks, "  failed  ") {
		t.Fatalf("no task may fail from the budget stop:\n%s", tasks)
	}
	agen(t, 1, "call", "spender", "more") // refused: daily budget used
}
