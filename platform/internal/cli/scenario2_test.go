package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Scenario 2: agen up → 3 agents delegating over A2A (boss → researcher →
// writer), all started from zero by one task → one linked trace → agen down
// leaves nothing running.
func TestScenario2LocalFleetDelegationTraceAndDown(t *testing.T) {
	bin := hostBin(t)
	home := t.TempDir()
	t.Setenv("AGEN_HOME", home)
	t.Setenv("AGEN_HUB", "")
	t.Setenv("AGEN_TOKEN", "")
	upOut := &safeBuf{}
	upDone := make(chan int, 1)
	go func() {
		upDone <- Main(context.Background(), []string{"up", "--listen", "127.0.0.1:0", "--gateway-listen", "127.0.0.1:0", "--host-bin", bin}, upOut, &safeBuf{})
	}()
	waitUntil(t, "agen up", 30*time.Second, func() bool { return strings.Contains(upOut.String(), "agen is up") })
	waitUntil(t, "local nest", 30*time.Second, func() bool { return strings.Contains(agen(t, 0, "nests"), "local") })

	allow := `"permissions":{"default":"deny","rules":[{"tool":"call_agent","action":"allow"}]}`
	agents := []struct{ name, config, script string }{
		{"writer", `{"kind":"pool","scale":{"min":0,"max":1}}`, `{"cycle":true,"responses":[{"text":"Draft ready."}]}`},
		{"researcher", `{"kind":"pool","scale":{"min":0,"max":1},"delegates":[{"name":"writer"}],` + allow + `}`,
			`{"cycle":true,"responses":[{"toolCalls":[{"name":"call_agent","arguments":{"agent":"writer","message":"write it up"}}]},{"text":"Research done: Draft ready.","expect":"Draft ready."}]}`},
		{"boss", `{"kind":"pool","scale":{"min":0,"max":1},"delegates":[{"name":"researcher"}],` + allow + `}`,
			`{"cycle":true,"responses":[{"toolCalls":[{"name":"call_agent","arguments":{"agent":"researcher","message":"research it"}}]},{"text":"Boss: all done","expect":"Research done"}]}`},
	}
	for _, a := range agents {
		dir := filepath.Join(t.TempDir(), a.name)
		copyDir(t, filepath.Join(repoRoot(), "examples", "bundles", "hello"), dir)
		os.WriteFile(filepath.Join(dir, "x-agen", "config.json"), []byte(a.config), 0o644)
		os.WriteFile(filepath.Join(dir, "x-agen", "fake-script.json"), []byte(a.script), 0o644)
		agen(t, 0, "deploy", dir, "--name", a.name)
	}
	if out := agen(t, 0, "ps", "--all"); strings.Contains(out, "default/") {
		t.Fatalf("agents running before any work:\n%s", out)
	}

	// One task for boss wakes the whole chain.
	task := asJSON(t, agen(t, 0, "run", "boss", "plan the launch", "--json"))
	if task["state"] != "TASK_STATE_SUCCEEDED" || task["output"] != "Boss: all done" {
		t.Fatalf("task: %v", task)
	}
	trace := agen(t, 0, "trace", task["id"].(string))
	t.Logf("trace:\n%s", trace)
	if !strings.Contains(trace, ": 3 runs,") {
		t.Fatalf("trace should link 3 runs:\n%s", trace)
	}
	indent := func(dep string) int {
		for _, l := range strings.Split(trace, "\n") {
			if strings.Contains(l, "[default/"+dep+" run ") {
				return len(l) - len(strings.TrimLeft(l, " "))
			}
		}
		t.Fatalf("no run of %s in trace", dep)
		return -1
	}
	if !(indent("boss") < indent("researcher") && indent("researcher") < indent("writer")) {
		t.Fatalf("delegated runs must nest under their caller's tool span:\n%s", trace)
	}
	tr := asJSON(t, agen(t, 0, "trace", task["id"].(string), "--json"))
	byDep := map[string]map[string]any{}
	for _, r := range tr["runs"].([]any) {
		m := r.(map[string]any)
		byDep[m["deployment"].(string)] = m
	}
	boss, res, wr := byDep["boss"], byDep["researcher"], byDep["writer"]
	if res["parentRunId"] != boss["id"] || wr["parentRunId"] != res["id"] || wr["rootRunId"] != boss["id"] || res["rootRunId"] != boss["id"] {
		t.Fatalf("lineage: %v", byDep)
	}

	// agen down: every process of the fleet is gone.
	var lc localConfig
	b, _ := os.ReadFile(filepath.Join(home, "local.json"))
	if err := json.Unmarshal(b, &lc); err != nil {
		t.Fatal(err)
	}
	var endpoints []string
	waitUntil(t, "instances listed", 10*time.Second, func() bool {
		endpoints = endpoints[:0]
		for _, l := range strings.Split(agen(t, 0, "ps", "--all"), "\n") {
			if i := strings.Index(l, "http://"); i >= 0 {
				endpoints = append(endpoints, strings.TrimSpace(l[i:]))
			}
		}
		return len(endpoints) >= 1
	})
	gw := resolve(t, "boss")[0]
	if out := agen(t, 0, "down"); !strings.Contains(out, "agen is down") {
		t.Fatalf("down: %s", out)
	}
	select {
	case code := <-upDone:
		if code != 0 {
			t.Fatalf("agen up exited %d", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("agen up still running")
	}
	client := &http.Client{Timeout: 2 * time.Second}
	for _, u := range append(endpoints, gw, lc.Hub+"/healthz") {
		if resp, err := client.Post(strings.TrimSuffix(u, "/")+"/agen.v1.HostService/Health", "application/json", strings.NewReader("{}")); err == nil {
			resp.Body.Close()
			t.Fatalf("%s still answers after agen down", u)
		}
	}
}
