package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Scenario 3: a 50-task burst on a pool at zero scales it up to max
// within the target time, every task completes, the pool returns to zero
// after the idle timeout, and the next call is still answered.
func TestScenario3AutoscaleBurst(t *testing.T) {
	startUp(t)
	bundle := filepath.Join(t.TempDir(), "workers")
	copyDir(t, filepath.Join(repoRoot(), "examples", "bundles", "hello"), bundle)
	os.WriteFile(filepath.Join(bundle, "x-agen", "config.json"),
		[]byte(`{"kind":"pool","scale":{"min":0,"max":5,"targetQueuePerInstance":2,"idleTimeout":"2s","maxConcurrency":1}}`), 0o644)
	os.WriteFile(filepath.Join(bundle, "x-agen", "fake-script.json"), []byte(`{"cycle":true,"responses":[{"text":"done","delayMs":200}]}`), 0o644)
	agen(t, 0, "deploy", bundle, "--name", "workers")
	if !strings.Contains(agen(t, 0, "ps"), "default/workers  pool  0      0") {
		t.Fatal("pool should start at zero")
	}

	start := time.Now()
	var ids []string
	for i := 0; i < 50; i++ {
		ids = append(ids, strings.TrimSpace(agen(t, 0, "run", "workers", "job", "--no-wait")))
	}
	submitted := time.Since(start)
	// Scales 0 → 5 ready instances within the target time.
	const target = 30 * time.Second
	var scaledIn time.Duration
	waitUntil(t, "5 ready instances", target, func() bool {
		if strings.Contains(agen(t, 0, "ps"), "default/workers  pool  5      5") {
			scaledIn = time.Since(start)
			return true
		}
		return false
	})
	// Every task completes.
	waitUntil(t, "all 50 tasks done", 120*time.Second, func() bool {
		out := agen(t, 0, "tasks", "workers", "--limit", "100")
		return strings.Count(out, "  succeeded  ") == 50
	})
	allDone := time.Since(start)
	// Back to zero after the idle timeout; the next call still works.
	waitUntil(t, "back to zero", 60*time.Second, func() bool {
		return strings.Contains(agen(t, 0, "ps"), "default/workers  pool  0      0") && !strings.Contains(agen(t, 0, "ps", "--all"), "default/workers")
	})
	atZero := time.Since(start)
	if out := agen(t, 0, "call", "workers", "one more"); strings.TrimSpace(out) != "done" {
		t.Fatalf("call after scale-to-zero: %q", out)
	}
	t.Logf("50 tasks submitted in %s; 5 ready after %s; all done after %s; back to 0 after %s", submitted.Round(time.Millisecond),
		scaledIn.Round(time.Millisecond), allDone.Round(time.Millisecond), atZero.Round(time.Millisecond))
	_ = ids
}
