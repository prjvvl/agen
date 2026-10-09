package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prjvvl/agen/platform/internal/store"
)

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func hostBin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("AGEN_HOST_BIN"); p != "" {
		return p
	}
	name := "agen-host"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	p := filepath.Join(repoRoot(), "target", "debug", name)
	if _, err := exec.LookPath("cargo"); err != nil {
		if _, statErr := os.Stat(p); statErr == nil {
			return p
		}
		if os.Getenv("AGEN_REQUIRE_HOST") == "1" {
			t.Fatal("AGEN_REQUIRE_HOST=1 but agen-host is not built and cargo is not available")
		}
		t.Skip("agen-host is not built and cargo is not available (set AGEN_HOST_BIN)")
	}
	cmd := exec.Command("cargo", "build", "-q", "-p", "agen-host")
	cmd.Dir = repoRoot()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build agen-host: %v\n%s", err, out)
	}
	return p
}

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// agen runs one CLI command and returns stdout, failing on a non-zero exit
// unless wantCode says otherwise.
func agen(t *testing.T, wantCode int, args ...string) string {
	t.Helper()
	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if code := Main(ctx, args, &out, &errb); code != wantCode {
		t.Fatalf("agen %s: exit %d (want %d)\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, wantCode, out.String(), errb.String())
	}
	return out.String()
}

func waitUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLocalFleetThroughTheCLI(t *testing.T) {
	bin := hostBin(t)
	home := t.TempDir()
	t.Setenv("AGEN_HOME", home)
	t.Setenv("AGEN_HUB", "")
	t.Setenv("AGEN_TOKEN", "")

	// Without a Hub, client commands explain what to do.
	var errb bytes.Buffer
	if code := Main(context.Background(), []string{"ps"}, &bytes.Buffer{}, &errb); code != 1 || !strings.Contains(errb.String(), "agen up") {
		t.Fatalf("no hub: %d %s", code, errb.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	upOut, upErr := &safeBuf{}, &safeBuf{}
	upDone := make(chan int, 1)
	go func() {
		upDone <- Main(ctx, []string{"up", "--listen", "127.0.0.1:0", "--gateway-listen", "127.0.0.1:0", "--host-bin", bin}, upOut, upErr)
	}()
	defer func() {
		cancel()
		select {
		case code := <-upDone:
			if code != 0 {
				t.Errorf("agen up exited %d: %s", code, upErr.String())
			}
		case <-time.After(60 * time.Second):
			t.Error("agen up did not stop")
		}
		if t.Failed() {
			t.Logf("agen up stderr:\n%s", upErr.String())
		}
	}()
	waitUntil(t, "agen up", 30*time.Second, func() bool { return strings.Contains(upOut.String(), "agen is up") })
	var lc localConfig
	b, err := os.ReadFile(filepath.Join(home, "local.json"))
	if err != nil || json.Unmarshal(b, &lc) != nil || lc.Token == "" {
		t.Fatalf("local config: %s %v", b, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(filepath.Join(home, "local.json")); st.Mode().Perm() != 0o600 {
			t.Fatalf("local.json mode %v", st.Mode().Perm())
		}
	}
	if resp, err := http.Get(lc.Hub + "/healthz"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: %v", err)
	}

	waitUntil(t, "local nest", 30*time.Second, func() bool { return strings.Contains(agen(t, 0, "nests"), "local") })

	bundle := filepath.Join(t.TempDir(), "hello")
	copyDir(t, filepath.Join(repoRoot(), "examples", "bundles", "hello"), bundle)
	out := agen(t, 0, "deploy", bundle, "--replicas", "1")
	if !strings.Contains(out, "deployed default/hello") || !strings.Contains(out, "desired 1") {
		t.Fatalf("deploy: %s", out)
	}
	waitUntil(t, "ready instance", 60*time.Second, func() bool {
		return strings.Contains(agen(t, 0, "ps"), "default/hello  pool  1      1")
	})
	if out := agen(t, 0, "run", "hello", "hi there"); strings.TrimSpace(out) != "Hello! Nice to meet you." {
		t.Fatalf("run: %q", out)
	}
	all := agen(t, 0, "ps", "--all")
	if !strings.Contains(all, "default/hello") || !strings.Contains(all, "local") || !strings.Contains(all, "http://127.0.0.1:") {
		t.Fatalf("ps --all: %s", all)
	}
	if out := agen(t, 0, "tasks"); !strings.Contains(out, "succeeded") {
		t.Fatalf("tasks: %s", out)
	}
	waitUntil(t, "run logged", 10*time.Second, func() bool {
		out := agen(t, 0, "logs", "hello")
		return strings.Contains(out, " succeeded in ") && strings.Contains(out, "started (task ")
	})

	// JSON output is the API response.
	var ps struct {
		Deployments []struct {
			Name    string `json:"name"`
			Desired int    `json:"desired"`
		} `json:"deployments"`
	}
	if err := json.Unmarshal([]byte(agen(t, 0, "ps", "--json")), &ps); err != nil || len(ps.Deployments) != 1 || ps.Deployments[0].Desired != 1 {
		t.Fatalf("ps --json: %+v %v", ps, err)
	}

	// Redeploying a changed bundle updates the deployment in place.
	os.WriteFile(filepath.Join(bundle, "x-agen", "fake-script.json"), []byte(`{"cycle":true,"responses":[{"text":"Hi from v2."}]}`), 0o644)
	if out := agen(t, 0, "deploy", bundle); !strings.Contains(out, "deployed default/hello") {
		t.Fatalf("redeploy: %s", out)
	}
	waitUntil(t, "v2 answers", 60*time.Second, func() bool {
		var o, e bytes.Buffer
		Main(context.Background(), []string{"run", "hello", "v2?", "--timeout", "20s"}, &o, &e)
		return strings.TrimSpace(o.String()) == "Hi from v2."
	})

	// A second nest joins with a one-time token; scaling spreads the
	// deployment over both nests and ps --all shows the whole fleet.
	join := strings.TrimSpace(agen(t, 0, "join-token", "--ttl", "5m"))
	nestCtx, nestCancel := context.WithCancel(context.Background())
	nestErr := &safeBuf{}
	nestDone := make(chan int, 1)
	go func() {
		nestDone <- Main(nestCtx, []string{"nest", "run", "--hub", lc.Hub, "--join-token", join, "--name", "second", "--capacity", "8",
			"--label", "zone=b", "--gateway-listen", "127.0.0.1:0", "--store", defaultStoreURL(home), "--data-dir", filepath.Join(t.TempDir(), "second"), "--host-bin", bin}, &safeBuf{}, nestErr)
	}()
	defer func() {
		nestCancel()
		if code := <-nestDone; code != 0 {
			t.Errorf("nest run exited %d: %s", code, nestErr.String())
		}
	}()
	waitUntil(t, "second nest", 30*time.Second, func() bool { return strings.Contains(agen(t, 0, "nests"), "second") })
	agen(t, 0, "scale", "hello", "2")
	waitUntil(t, "2 ready", 60*time.Second, func() bool { return strings.Contains(agen(t, 0, "ps"), "default/hello  pool  2      2") })
	fleet := agen(t, 0, "ps", "--all")
	if !strings.Contains(fleet, "  local  ") || !strings.Contains(fleet, "  second  ") {
		t.Fatalf("instances not spread over both nests:\n%s", fleet)
	}
	for i := 0; i < 4; i++ {
		if out := agen(t, 0, "run", "hello", "fleet"); strings.TrimSpace(out) != "Hi from v2." {
			t.Fatalf("fleet run: %q", out)
		}
	}
	// stop drains and holds at zero even with work waiting; start resumes.
	busyID := strings.TrimSpace(agen(t, 0, "run", "hello", "in flight", "--no-wait"))
	if out := agen(t, 0, "stop", "hello"); !strings.Contains(out, "stopped (paused)") {
		t.Fatalf("stop: %s", out)
	}
	waitUntil(t, "instances gone", 30*time.Second, func() bool {
		return !strings.Contains(agen(t, 0, "ps", "--all"), "default/hello")
	})
	queuedID := strings.TrimSpace(agen(t, 0, "run", "hello", "while stopped", "--no-wait"))
	time.Sleep(2 * time.Second) // several autoscaler ticks
	if out := agen(t, 0, "ps", "--all"); strings.Contains(out, "default/hello") {
		t.Fatalf("stopped deployment was restarted by queued work:\n%s", out)
	}
	if out := agen(t, 0, "tasks", "hello"); !strings.Contains(out, queuedID) || !strings.Contains(out, queuedID+"  default/hello  queued") {
		t.Fatalf("task should wait while stopped:\n%s", out)
	}
	agen(t, 1, "scale", "hello", "1") // scaling a stopped deployment is refused
	if out := agen(t, 0, "start", "hello"); !strings.Contains(out, "started") {
		t.Fatalf("start: %s", out)
	}
	waitUntil(t, "queued task runs after start", 60*time.Second, func() bool {
		out := agen(t, 0, "tasks", "hello")
		return strings.Contains(out, queuedID+"  default/hello  succeeded")
	})
	_ = busyID
	// The local admin token is never sent to another Hub.
	var eb bytes.Buffer
	if code := Main(context.Background(), []string{"ps", "--hub", "http://127.0.0.1:9"}, &bytes.Buffer{}, &eb); code != 1 || !strings.Contains(eb.String(), "--token") {
		t.Fatalf("foreign hub without token: %d %s", code, eb.String())
	}
	agen(t, 0, "stop", "hello")
	agen(t, 0, "rm", "hello")
	if out := agen(t, 0, "ps"); strings.Contains(out, "hello") {
		t.Fatalf("after rm: %s", out)
	}
	// Usage errors exit 2; API errors exit 1.
	agen(t, 2, "scale", "hello")
	agen(t, 1, "scale", "hello", "1")
	agen(t, 2, "frobnicate")
}

// startUp runs `agen up` in-process with a fresh AGEN_HOME until the test
// ends and returns the local config.
func startUp(t *testing.T) (localConfig, string) {
	t.Helper()
	bin := hostBin(t)
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
				t.Errorf("agen up exited %d", code)
			}
		case <-time.After(60 * time.Second):
			t.Error("agen up did not stop")
		}
		if t.Failed() {
			t.Logf("agen up stderr:\n%s", upErr.String())
		}
	})
	waitUntil(t, "agen up", 30*time.Second, func() bool { return strings.Contains(upOut.String(), "agen is up") })
	var lc localConfig
	b, _ := os.ReadFile(filepath.Join(home, "local.json"))
	if err := json.Unmarshal(b, &lc); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "local nest", 30*time.Second, func() bool { return strings.Contains(agen(t, 0, "nests"), "local") })
	return lc, home
}

func postJSON(t *testing.T, url string, body any) (int, map[string]any, string) {
	t.Helper()
	return postJSONAuth(t, url, "", body)
}

// postJSONAuth posts with a bearer token (none when empty).
func postJSONAuth(t *testing.T, url, token string, body any) (int, map[string]any, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, string(raw)
}

// An idle deployment sleeps (0 instances); an A2A call through the
// Gateway wakes it and is answered; it sleeps again after the idle timeout.
func TestSleepWakeThroughGateway(t *testing.T) {
	_, home := startUp(t)
	bundle := filepath.Join(t.TempDir(), "hello")
	copyDir(t, filepath.Join(repoRoot(), "examples", "bundles", "hello"), bundle)
	os.WriteFile(filepath.Join(bundle, "x-agen", "config.json"),
		[]byte(`{"kind":"pool","scale":{"min":0,"max":2,"idleTimeout":"2s"},"limits":{"maxDelegationDepth":2}}`), 0o644)
	agen(t, 0, "deploy", bundle)
	if out := agen(t, 0, "ps", "--all"); strings.Contains(out, "default/hello") {
		t.Fatalf("instances before any call:\n%s", out)
	}
	start := time.Now()
	if out := agen(t, 0, "call", "hello", "wake up"); strings.TrimSpace(out) != "Hello! Nice to meet you." {
		t.Fatalf("call: %q", out)
	}
	t.Logf("cold call answered in %s", time.Since(start))
	// The Hub's instance list follows the Manager's heartbeat.
	waitUntil(t, "woken instance listed", 10*time.Second, func() bool { return strings.Contains(agen(t, 0, "ps", "--all"), "default/hello") })

	// The same Gateway serves the agent card and streaming.
	var ep struct{ Endpoints []string }
	ep.Endpoints = resolve(t, "hello")
	resp, err := http.Get(ep.Endpoints[0] + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	var card map[string]any
	json.NewDecoder(resp.Body).Decode(&card)
	resp.Body.Close()
	skills, _ := card["skills"].([]any)
	if card["name"] != "hello" || card["url"] != ep.Endpoints[0] || len(skills) != 1 {
		t.Fatalf("agent card: %v", card)
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "s1", "method": "message/stream", "params": map[string]any{
		"message": map[string]any{"kind": "message", "role": "user", "messageId": "stream-1", "parts": []map[string]string{{"kind": "text", "text": "hi"}}}}})
	sresp, err := http.Post(ep.Endpoints[0], "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	sse, _ := io.ReadAll(sresp.Body)
	sresp.Body.Close()
	if sresp.Header.Get("Content-Type") != "text/event-stream" || !strings.Contains(string(sse), `"state":"working"`) ||
		!strings.Contains(string(sse), `"artifact-update"`) || !strings.Contains(string(sse), `"final":true`) || !strings.Contains(string(sse), "Nice to meet you") {
		t.Fatalf("stream:\n%s", sse)
	}
	// Retrying a message id returns the same task (no second run).
	send := func(id string, depth int) (int, map[string]any, string) {
		return postJSON(t, ep.Endpoints[0], map[string]any{"jsonrpc": "2.0", "id": 1, "method": "message/send", "params": map[string]any{
			"message":  map[string]any{"kind": "message", "role": "user", "messageId": id, "parts": []map[string]string{{"kind": "text", "text": "hi"}}},
			"metadata": map[string]any{"agen.depth": depth}}})
	}
	_, first, _ := send("dup-1", 1)
	_, again, _ := send("dup-1", 1)
	fr, _ := first["result"].(map[string]any)
	ar, _ := again["result"].(map[string]any)
	fm, _ := fr["metadata"].(map[string]any)
	am, _ := ar["metadata"].(map[string]any)
	taskID, _ := fr["id"].(string)
	if !strings.HasPrefix(taskID, "a2a-") || ar["id"] != taskID || fm["agen.run_id"] == nil || fm["agen.run_id"] != am["agen.run_id"] {
		t.Fatalf("idempotent send: %v / %v", first, again)
	}
	_, got, _ := postJSON(t, ep.Endpoints[0], map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tasks/get", "params": map[string]any{"id": taskID}})
	if gr, _ := got["result"].(map[string]any); gr["id"] != taskID {
		t.Fatalf("tasks/get: %v", got)
	}
	// Lineage claims (depth, parent/root run) of an anonymous caller are
	// ignored: a deep claim is just a new root call...
	_, anon, raw := send("deep-anon", 3)
	if r, _ := anon["result"].(map[string]any); r == nil {
		t.Fatalf("anonymous caller's depth claim should be ignored: %s", raw)
	}
	// ...while an agent's (Hub-signed call token) are recorded and enforced:
	// depth beyond the deployment's limit is refused.
	agentTok := signCallToken(t, home, "agent:default/boss", "default/hello")
	helloTok := signCallToken(t, home, "agent:default/hello", "default/hello")
	firstRun, _ := fm["agen.run_id"].(string)
	_, deep, raw := postJSONAuth(t, ep.Endpoints[0], helloTok, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "message/send", "params": map[string]any{
		"message":  map[string]any{"kind": "message", "role": "user", "messageId": "deep-1", "parts": []map[string]string{{"kind": "text", "text": "hi"}}},
		"metadata": map[string]any{"agen.depth": 3, "agen.parent_run_id": firstRun}}})
	if e, _ := deep["error"].(map[string]any); e == nil || e["code"] != float64(-32051) {
		t.Fatalf("depth limit: %s", raw)
	}
	// An agent's lineage is checked against the Store by the callee host:
	// boss cannot claim one of hello's runs as its parent...
	helloRun, _ := fm["agen.run_id"].(string)
	lineage := func(tok, msgID string, meta map[string]any) (map[string]any, string) {
		_, out, raw := postJSONAuth(t, ep.Endpoints[0], tok, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "message/send", "params": map[string]any{
			"message":  map[string]any{"kind": "message", "role": "user", "messageId": msgID, "parts": []map[string]string{{"kind": "text", "text": "hi"}}},
			"metadata": meta}})
		r, _ := out["result"].(map[string]any)
		return r, raw
	}
	forged, raw := lineage(signCallToken(t, home, "agent:default/boss", "default/hello"), "forged-1",
		map[string]any{"agen.parent_run_id": helloRun, "agen.root_run_id": "someone-elses-root", "agen.depth": 1})
	if st, _ := forged["status"].(map[string]any); st["state"] != "failed" || !strings.Contains(raw, "is not a run of default/boss") {
		t.Fatalf("forged lineage: %s", raw)
	}
	// ...while hello's own run as parent is accepted, with the root taken
	// from the Store (not the claim).
	good, raw := lineage(signCallToken(t, home, "agent:default/hello", "default/hello"), "lineage-ok",
		map[string]any{"agen.parent_run_id": helloRun, "agen.root_run_id": "claimed-root", "agen.depth": 0})
	gm, _ := good["metadata"].(map[string]any)
	child, _ := gm["agen.run_id"].(string)
	if child == "" {
		t.Fatalf("genuine lineage: %s", raw)
	}
	var parent, root string
	st, err := store.Open(context.Background(), defaultStoreURL(home))
	if err != nil {
		t.Fatal(err)
	}
	st.DB().QueryRow("SELECT parent_run_id, root_run_id FROM runs WHERE id = $1", child).Scan(&parent, &root)
	st.Close()
	if parent != helloRun || root != helloRun {
		t.Fatalf("child lineage parent=%q root=%q, want %q", parent, root, helloRun)
	}
	// A token for another deployment, or a forged one, is refused.
	for _, bad := range []string{signCallToken(t, home, "agent:default/boss", "default/other"), agentTok + "x"} {
		if code, out, raw := postJSONAuth(t, ep.Endpoints[0], bad, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "message/send", "params": map[string]any{
			"message": map[string]any{"kind": "message", "role": "user", "messageId": "bad-tok", "parts": []map[string]string{{"kind": "text", "text": "hi"}}}}}); code != http.StatusUnauthorized || out["error"] == nil {
			t.Fatalf("bad token accepted: %d %s", code, raw)
		}
	}

	// Idle for longer than idleTimeout: back to zero instances.
	waitUntil(t, "deployment to sleep", 30*time.Second, func() bool {
		return !strings.Contains(agen(t, 0, "ps", "--all"), "default/hello") && strings.Contains(agen(t, 0, "ps"), "default/hello  pool  0      0")
	})
	// The next call wakes it again.
	if out := agen(t, 0, "call", "hello", "again"); strings.TrimSpace(out) != "Hello! Nice to meet you." {
		t.Fatalf("second wake: %q", out)
	}
	// Unknown deployments are an error, not a hang.
	agen(t, 1, "call", "nobody", "hi")
}

func resolve(t *testing.T, name string) []string {
	t.Helper()
	var out struct {
		Endpoints []string `json:"endpoints"`
	}
	raw := agen(t, 0, "resolve", name, "--json")
	if err := json.Unmarshal([]byte(raw), &out); err != nil || len(out.Endpoints) == 0 {
		t.Fatalf("resolve: %s %v", raw, err)
	}
	return out.Endpoints
}

// agen down stops agen up; a restart after the store was wiped re-enrols the
// local nest instead of running unheard with a stale credential.
func TestDownAndRestartWithWipedStore(t *testing.T) {
	bin := hostBin(t)
	home := t.TempDir()
	t.Setenv("AGEN_HOME", home)
	t.Setenv("AGEN_HUB", "")
	t.Setenv("AGEN_TOKEN", "")
	// A fixed Hub address, so the restarted agen up finds the saved nest
	// credential for "its" Hub and has to detect that it is stale.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hubAddr := ln.Addr().String()
	ln.Close()
	var upErr *safeBuf
	up := func() chan int {
		done := make(chan int, 1)
		out := &safeBuf{}
		upErr = &safeBuf{}
		errb := upErr
		go func() {
			done <- Main(context.Background(), []string{"up", "--listen", hubAddr, "--gateway-listen", "127.0.0.1:0", "--host-bin", bin}, out, errb)
		}()
		waitUntil(t, "agen up", 30*time.Second, func() bool { return strings.Contains(out.String(), "agen is up") })
		waitUntil(t, "local nest", 30*time.Second, func() bool { return strings.Contains(agen(t, 0, "nests"), "local  native   active") })
		return done
	}
	down := func(done chan int) {
		if out := agen(t, 0, "down"); !strings.Contains(out, "agen is down") {
			t.Fatalf("down: %s", out)
		}
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("agen up exited %d", code)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("agen up still running after agen down")
		}
	}
	bundle := filepath.Join(repoRoot(), "examples", "bundles", "hello")
	done := up()
	agen(t, 0, "deploy", bundle, "--replicas", "1")
	if out := agen(t, 0, "run", "hello", "hi"); strings.TrimSpace(out) != "Hello! Nice to meet you." {
		t.Fatalf("run: %q", out)
	}
	down(done)
	if out := agen(t, 0, "down"); !strings.Contains(out, "not running") {
		t.Fatalf("second down: %s", out)
	}
	// Wipe the store but keep AGEN_HOME/nest/nest.json.
	for _, f := range []string{"agen.db", "agen.db-wal", "agen.db-shm"} {
		os.Remove(filepath.Join(home, f))
	}
	done = up()
	if !strings.Contains(upErr.String(), "saved nest credential rejected by the hub; enrolling again") {
		t.Fatalf("stale credential not detected:\n%s", upErr.String())
	}
	agen(t, 0, "deploy", bundle, "--replicas", "1")
	if out := agen(t, 0, "run", "hello", "hi again", "--timeout", "60s"); strings.TrimSpace(out) != "Hello! Nice to meet you." {
		t.Fatalf("run after wipe: %q", out)
	}
	down(done)
}

// End to end: a cron trigger wakes a sleeping deployment and its task
// runs; a webhook call (with the trigger's secret) becomes a task too.
func TestCronAndWebhookTriggersEndToEnd(t *testing.T) {
	lc, _ := startUp(t)
	bundle := filepath.Join(t.TempDir(), "hello")
	copyDir(t, filepath.Join(repoRoot(), "examples", "bundles", "hello"), bundle)
	os.WriteFile(filepath.Join(bundle, "x-agen", "config.json"), []byte(`{"kind":"pool","scale":{"min":0,"max":1,"idleTimeout":"30s"},
		"triggers":[{"type":"cron","name":"tick","schedule":"@every 3s","input":"cron says hi"},{"type":"webhook","name":"hook","input":"Webhook:"}]}`), 0o644)
	agen(t, 0, "deploy", bundle)
	waitUntil(t, "a cron task to run", 60*time.Second, func() bool {
		out := agen(t, 0, "tasks", "hello")
		return strings.Contains(out, "succeeded  cron:tick")
	})
	secretOut := agen(t, 0, "webhook-secret", "hello", "hook")
	secret := strings.SplitN(secretOut, "\n", 2)[0]
	req, _ := http.NewRequest(http.MethodPost, lc.Hub+"/hooks/default/hello/hook", strings.NewReader(`{"event":"push"}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var accepted map[string]string
	json.NewDecoder(resp.Body).Decode(&accepted)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || accepted["task_id"] == "" {
		t.Fatalf("webhook: %d %v", resp.StatusCode, accepted)
	}
	waitUntil(t, "webhook task to run", 60*time.Second, func() bool {
		return strings.Contains(agen(t, 0, "tasks", "hello", "--limit", "200"), accepted["task_id"]+"  default/hello  succeeded  webhook:hook")
	})
	trig := agen(t, 0, "triggers", "hello")
	if !strings.Contains(trig, "tick     fired") || !strings.Contains(trig, "hook     fired") {
		t.Fatalf("trigger events:\n%s", trig)
	}
	agen(t, 0, "stop", "hello") // no more cron runs
}
