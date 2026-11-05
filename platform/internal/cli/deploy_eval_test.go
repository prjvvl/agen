package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// deployAgenUp starts a local `agen up` for one test, returning its stdout
// buffer (for waitUntil "agen is up"/"local nest" checks already used by
// the replay tests) and a cleanup the caller defers.
func deployAgenUp(t *testing.T, bin string) *safeBuf {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGEN_HOME", home)
	t.Setenv("AGEN_HUB", "")
	t.Setenv("AGEN_TOKEN", "")
	ctx, cancel := context.WithCancel(context.Background())
	upOut, upErr := &safeBuf{}, &safeBuf{}
	upDone := make(chan int, 1)
	go func() {
		upDone <- Main(ctx, []string{"up", "--listen", "127.0.0.1:0", "--gateway-listen", "127.0.0.1:0", "--host-bin", bin}, upOut, upErr)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-upDone:
			if code != 0 {
				t.Errorf("agen up exited %d: %s", code, upErr.String())
			}
		case <-time.After(60 * time.Second):
			t.Error("agen up did not stop")
		}
	})
	waitUntil(t, "agen up", 30*time.Second, func() bool { return strings.Contains(upOut.String(), "agen is up") })
	waitUntil(t, "local nest", 30*time.Second, func() bool { return strings.Contains(agen(t, 0, "nests"), "local") })
	return upOut
}

func writeEvalsDirScenario(t *testing.T, dir, name, mustContain string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeScenario(t, dir, name, map[string]any{
		"input": "hi there",
		"rules": map[string]any{"mustContain": []string{mustContain}},
	})
}

// `agen deploy --require-eval` refuses to create the deployment at all
// when a bundled scenario does not pass, discovering x-agen/evals inside
// the bundle without a --scenarios flag (2.43, part).
// features: eval
func TestDeployRequireEvalRefusesToDeployWhenAScenarioFails(t *testing.T) {
	bin := hostBin(t)
	deployAgenUp(t, bin)

	bundle := filepath.Join(t.TempDir(), "hello")
	copyDir(t, filepath.Join(repoRoot(), "examples", "bundles", "hello"), bundle)
	writeEvalsDirScenario(t, filepath.Join(bundle, "x-agen", "evals"), "greets.json", "this text never appears in the reply")

	agen(t, 1, "deploy", bundle, "--name", "willfail", "--require-eval", "--host-bin", bin)
	ps := agen(t, 0, "ps", "--json")
	if strings.Contains(ps, `"willfail"`) {
		t.Fatalf("the deployment must not have been created: %s", ps)
	}
}

// `agen deploy --require-eval` deploys once every bundled scenario
// passes, and records the report against the deployment (2.43, part).
// features: eval
func TestDeployRequireEvalPassesAndRecordsTheReport(t *testing.T) {
	bin := hostBin(t)
	deployAgenUp(t, bin)

	bundle := filepath.Join(t.TempDir(), "hello")
	copyDir(t, filepath.Join(repoRoot(), "examples", "bundles", "hello"), bundle)
	writeEvalsDirScenario(t, filepath.Join(bundle, "x-agen", "evals"), "greets.json", "Nice to meet you")

	out := agen(t, 0, "deploy", bundle, "--name", "willpass", "--require-eval", "--host-bin", bin)
	if !strings.Contains(out, "passed^1") || !strings.Contains(out, "deployed default/willpass") {
		t.Fatalf("deploy: %s", out)
	}

	// Deploying again with the same bundle/scenario should find the
	// prior report and print no regression/fixed lines (nothing changed).
	out2 := agen(t, 0, "deploy", bundle, "--name", "willpass", "--require-eval", "--host-bin", bin)
	if strings.Contains(out2, "regressed:") || strings.Contains(out2, "fixed:") {
		t.Fatalf("an unchanged scenario should print no comparison lines: %s", out2)
	}

	// Both recorded reports are readable back, newest first.
	hist := agen(t, 0, "eval", "history", "willpass", "--json")
	var histOut struct {
		Reports []map[string]any `json:"reports"`
	}
	if err := json.Unmarshal([]byte(hist), &histOut); err != nil {
		t.Fatalf("eval history --json did not parse: %v\n%s", err, hist)
	}
	if len(histOut.Reports) != 2 {
		t.Fatalf("want 2 recorded reports, got %d: %s", len(histOut.Reports), hist)
	}
}
