package manager

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/gen/agen/v1/agenv1connect"
	"github.com/prjvvl/agen/platform/internal/definition"
	"github.com/prjvvl/agen/platform/internal/kube"
)

// fakeKube is an in-memory Kubernetes API for pods, secrets and config maps.
type fakeKube struct {
	mu      sync.Mutex
	objects map[string]map[string]any // "pods/name" -> object
	deletes []string                  // "pods/name grace=N"
	down    bool
}

func newFakeKube(t *testing.T) (*fakeKube, *kube.Client) {
	f := &fakeKube{objects: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, &kube.Client{BaseURL: srv.URL, Namespace: "ns", HTTP: srv.Client()}
}

func (f *fakeKube) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		http.Error(w, `{"message":"unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/ns/")
	kind, name, _ := strings.Cut(rest, "/")
	switch {
	case r.Method == http.MethodPost:
		var obj map[string]any
		json.NewDecoder(r.Body).Decode(&obj)
		n := obj["metadata"].(map[string]any)["name"].(string)
		if _, ok := f.objects[kind+"/"+n]; ok {
			http.Error(w, `{"message":"exists"}`, http.StatusConflict)
			return
		}
		f.objects[kind+"/"+n] = obj
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && name == "":
		var items []map[string]any
		for k, o := range f.objects {
			if strings.HasPrefix(k, kind+"/") {
				items = append(items, o)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	case r.Method == http.MethodGet:
		o, ok := f.objects[kind+"/"+name]
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(o)
	case r.Method == http.MethodDelete:
		var body struct{ GracePeriodSeconds int64 }
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		f.deletes = append(f.deletes, kind+"/"+name)
		if _, ok := f.objects[kind+"/"+name]; !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		delete(f.objects, kind+"/"+name)
	}
}

func (f *fakeKube) get(key string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objects[key]
}

func (f *fakeKube) setStatus(pod string, status map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o := f.objects["pods/"+pod]; o != nil {
		o["status"] = status
	}
}

func (f *fakeKube) keys(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}

// defNest serves GetDefinition for one definition.
type defNest struct {
	agenv1connect.UnimplementedNestServiceHandler
	files map[string][]byte
}

func (d *defNest) GetDefinition(context.Context, *connect.Request[agenv1.GetDefinitionRequest]) (*connect.Response[agenv1.GetDefinitionResponse], error) {
	return connect.NewResponse(&agenv1.GetDefinitionResponse{Definition: &agenv1.Definition{Files: d.files}}), nil
}

func kubeTestManager(t *testing.T, files map[string][]byte) (*fakeKube, *kubeBackend) {
	t.Helper()
	fk, kc := newFakeKube(t)
	path, h := agenv1connect.NewNestServiceHandler(&defNest{files: files})
	mux := http.NewServeMux()
	mux.Handle(path, h)
	hub := httptest.NewServer(mux)
	t.Cleanup(hub.Close)
	m, err := New(Config{HubURL: hub.URL, StoreURL: "postgres://user:SECRETPW@db/agen", DataDir: t.TempDir(), NestID: "N1", NestToken: "x",
		HostEnv: []string{"PROVIDER_KEY=sk-test-123"}, KillGrace: 5 * time.Second,
		Kube: &KubeConfig{Client: kc, Image: "agen:test", OwnerPod: "nest-0", OwnerUID: "0123456789abcdef", Poll: 20 * time.Millisecond},
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return fk, m.backend.(*kubeBackend)
}

func helloDef() (map[string][]byte, string) {
	files := map[string][]byte{"agent.md": []byte("hi"), "x-agen/run.sh": []byte("#!/bin/sh\necho hi\n"), "bin/blob": {0xff, 0xfe, 0x00}}
	return files, definition.Digest(files)
}

func startSpec(id, digest string, endpoint chan string) hostSpec {
	return hostSpec{id: id, ns: "default", dep: "hello", digest: digest, args: []string{"--max-concurrency", "1"},
		env:      []string{"AGEN_STORE=postgres://user:SECRETPW@db/agen", "AGEN_HOST_TOKEN=host-tok"},
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		endpoint: endpoint}
}

func TestKubeBackendPodLifecycle(t *testing.T) {
	files, digest := helloDef()
	fk, b := kubeTestManager(t, files)
	ep := make(chan string, 1)
	proc, err := b.start(context.Background(), startSpec("01ABC", digest, ep))
	if err != nil {
		t.Fatal(err)
	}

	// Definition: one config map per digest and Nest pod, owned by it,
	// scripts executable, binary files as binaryData.
	cms := fk.keys("configmaps/")
	if len(cms) != 1 || !strings.HasPrefix(cms[0], "configmaps/agen-def-01234567-sha256-") {
		t.Fatalf("config maps: %v", cms)
	}
	cm := fk.get(cms[0])
	if cm["metadata"].(map[string]any)["ownerReferences"] == nil || cm["binaryData"] == nil || cm["immutable"] != true {
		t.Fatalf("config map: %v", cm)
	}
	// Settings only in the secret; the pod spec carries no secret values.
	pod := fk.get("pods/agen-01abc")
	raw, _ := json.Marshal(pod)
	for _, leak := range []string{"SECRETPW", "sk-test-123", "host-tok"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("pod spec contains %q: %s", leak, raw)
		}
	}
	secret := fk.get("secrets/agen-01abc")
	sd, _ := json.Marshal(secret["stringData"])
	for _, want := range []string{"SECRETPW", "sk-test-123", "host-tok"} {
		if !strings.Contains(string(sd), want) {
			t.Fatalf("secret lacks %q: %s", want, sd)
		}
	}
	for _, want := range []string{`"RuntimeDefault"`, `"limits"`, `"restartPolicy":"Never"`, `"automountServiceAccountToken":false`, `"agen.dev/deployment":"hello"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("pod spec lacks %s: %s", want, raw)
		}
	}

	// Running with an IP: the endpoint is reported.
	fk.setStatus("agen-01abc", map[string]any{"phase": "Running", "podIP": "10.1.2.3"})
	select {
	case url := <-ep:
		if url != "http://10.1.2.3:7080" {
			t.Fatal(url)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no endpoint")
	}

	// The container crashes: the instance ends with the reason, and the pod
	// and its secret are removed.
	fk.setStatus("agen-01abc", map[string]any{"phase": "Failed", "containerStatuses": []any{
		map[string]any{"state": map[string]any{"terminated": map[string]any{"exitCode": 3, "reason": "Error"}}}}})
	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "exit 3") {
			t.Fatalf("wait: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failed pod not noticed")
	}
	if fk.get("pods/agen-01abc") != nil || fk.get("secrets/agen-01abc") != nil {
		t.Fatal("failed pod or its secret left behind")
	}
}

func TestKubeBackendKillAndUnreachableAPI(t *testing.T) {
	files, digest := helloDef()
	fk, b := kubeTestManager(t, files)

	// Kill: the pod is deleted, then the instance is gone.
	p1, err := b.start(context.Background(), startSpec("01K1", digest, make(chan string, 1)))
	if err != nil {
		t.Fatal(err)
	}
	p1.Kill()
	if err := waitErr(p1, 5*time.Second); err == nil || !strings.Contains(err.Error(), "pod deleted") {
		t.Fatalf("after kill: %v", err)
	}

	// The API server goes away while an instance is being stopped: the
	// Manager is not left waiting forever.
	defer func(d time.Duration) { giveUpAfter = d }(giveUpAfter)
	giveUpAfter = 300 * time.Millisecond
	p2, err := b.start(context.Background(), startSpec("01K2", digest, make(chan string, 1)))
	if err != nil {
		t.Fatal(err)
	}
	fk.mu.Lock()
	fk.down = true
	fk.mu.Unlock()
	p2.Kill()
	if err := waitErr(p2, 10*time.Second); err == nil || !strings.Contains(err.Error(), "state unknown") {
		t.Fatalf("unreachable API: %v", err)
	}
}

func TestKubeBackendRejectsOversizedDefinitionAndCleansUp(t *testing.T) {
	big := map[string][]byte{"agent.md": []byte(strings.Repeat("x", maxConfigMapBytes+1))}
	fk, b := kubeTestManager(t, big)
	if _, err := b.start(context.Background(), startSpec("01BIG", definition.Digest(big), make(chan string, 1))); err == nil ||
		!strings.Contains(err.Error(), "config map") {
		t.Fatalf("oversized definition: %v", err)
	}
	if len(fk.keys("pods/")) != 0 || len(fk.keys("secrets/")) != 0 {
		t.Fatal("objects created for a definition that cannot be published")
	}

	// cleanup removes instance pods (and their secrets) left under this Nest.
	fk.mu.Lock()
	fk.objects["pods/agen-old"] = map[string]any{"metadata": map[string]any{"name": "agen-old"}}
	fk.objects["secrets/agen-old"] = map[string]any{"metadata": map[string]any{"name": "agen-old"}}
	fk.mu.Unlock()
	if err := b.cleanup(context.Background(), "N1"); err != nil {
		t.Fatal(err)
	}
	if fk.get("pods/agen-old") != nil || fk.get("secrets/agen-old") != nil {
		t.Fatal("left-over instance not removed")
	}
}

func TestResourceFraction(t *testing.T) {
	for in, want := range map[string]string{"1": "100m", "2": "200m", "500m": "50m", "1Gi": "102Mi", "512Mi": "51Mi", "weird": "weird"} {
		if got := fraction(in); got != want {
			t.Errorf("fraction(%q) = %q, want %q", in, got, want)
		}
	}
}

func waitErr(p hostProc, d time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		return context.DeadlineExceeded
	}
}
