package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// deploymentReady reports whether name has at least one ready instance,
// read from `ps --json` rather than the tabular `ps` output (whose
// column widths shift with the deployment name's length).
func deploymentReady(t *testing.T, name string) bool {
	t.Helper()
	var ps struct {
		Deployments []struct {
			Name  string `json:"name"`
			Ready int    `json:"ready"`
		} `json:"deployments"`
	}
	if err := json.Unmarshal([]byte(agen(t, 0, "ps", "--json")), &ps); err != nil {
		t.Fatal(err)
	}
	for _, d := range ps.Deployments {
		if d.Name == name && d.Ready > 0 {
			return true
		}
	}
	return false
}

// A run of a deployment with recordInteractions on saves a cassette;
// `agen replay <run>` re-runs it offline from that cassette, reproducing
// the exact same output, with no live model involved (3.6, part).
// features: replay
func TestReplayReproducesARecordedRunOffline(t *testing.T) {
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
	waitUntil(t, "local nest", 30*time.Second, func() bool { return strings.Contains(agen(t, 0, "nests"), "local") })

	bundle := filepath.Join(t.TempDir(), "recorder")
	copyDir(t, filepath.Join(repoRoot(), "examples", "bundles", "hello"), bundle)
	if err := os.WriteFile(filepath.Join(bundle, "x-agen", "config.json"),
		[]byte(`{"permissions":{"default":"allow"},"recordInteractions":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out := agen(t, 0, "deploy", bundle, "--name", "recorder", "--replicas", "1")
	if !strings.Contains(out, "deployed default/recorder") {
		t.Fatalf("deploy: %s", out)
	}
	waitUntil(t, "ready instance", 60*time.Second, func() bool { return deploymentReady(t, "recorder") })

	runOut := agen(t, 0, "run", "recorder", "hi there", "--json")
	var sub struct {
		RunId string `json:"runId"`
	}
	if err := json.Unmarshal([]byte(runOut), &sub); err != nil || sub.RunId == "" {
		t.Fatalf("run --json: %s %v", runOut, err)
	}

	var replayOut, replayErr bytes.Buffer
	if code := Main(context.Background(), []string{"replay", sub.RunId, "--host-bin", bin}, &replayOut, &replayErr); code != 0 {
		t.Fatalf("replay: exit %d\nstdout: %s\nstderr: %s", code, replayOut.String(), replayErr.String())
	}
	if !strings.Contains(replayOut.String(), "Hello! Nice to meet you.") {
		t.Fatalf("replay did not reproduce the recorded output: %s", replayOut.String())
	}
}

// Replaying a run of a deployment that never recorded one fails clearly.
// features: replay
func TestReplayOfAnUnrecordedRunFailsClearly(t *testing.T) {
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
	defer func() {
		cancel()
		select {
		case <-upDone:
		case <-time.After(60 * time.Second):
			t.Error("agen up did not stop")
		}
	}()
	waitUntil(t, "agen up", 30*time.Second, func() bool { return strings.Contains(upOut.String(), "agen is up") })
	waitUntil(t, "local nest", 30*time.Second, func() bool { return strings.Contains(agen(t, 0, "nests"), "local") })

	bundle := filepath.Join(t.TempDir(), "hello")
	copyDir(t, filepath.Join(repoRoot(), "examples", "bundles", "hello"), bundle)
	agen(t, 0, "deploy", bundle, "--replicas", "1")
	waitUntil(t, "ready instance", 60*time.Second, func() bool { return deploymentReady(t, "hello") })
	runOut := agen(t, 0, "run", "hello", "hi there", "--json")
	var sub struct {
		RunId string `json:"runId"`
	}
	if err := json.Unmarshal([]byte(runOut), &sub); err != nil || sub.RunId == "" {
		t.Fatalf("run --json: %s %v", runOut, err)
	}

	var replayErr bytes.Buffer
	if code := Main(context.Background(), []string{"replay", sub.RunId, "--host-bin", bin}, &bytes.Buffer{}, &replayErr); code == 0 {
		t.Fatalf("replay of an unrecorded run should fail, got exit 0: %s", replayErr.String())
	}
}
