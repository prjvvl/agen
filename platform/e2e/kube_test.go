package e2e

// The Kubernetes Nest backend on kind. The Hub, Postgres and one Nest
// run in the cluster (deploy/kube); the Nest starts every agent instance as
// its own pod. Run with AGEN_KIND_E2E=1 after building agen:dev; needs kind
// and kubectl. AGEN_KIND_KEEP=1 keeps the cluster afterwards.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/gen/agen/v1/agenv1connect"
	"github.com/prjvvl/agen/platform/internal/pki"
)

const kindCluster = "agen-e2e"

type kindEnv struct {
	t    *testing.T
	kind string
	c    agenv1connect.HubServiceClient
}

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// Namespaces of deploy/kube: the Hub (and dev Postgres) in agen, the Nest
// and its instance pods in agen-nests.
const (
	hubNS  = "agen"
	nestNS = "agen-nests"
)

func (k *kindEnv) kubectlIn(ns string, args ...string) (string, error) {
	return run("kubectl", append([]string{"--context", "kind-" + kindCluster, "-n", ns}, args...)...)
}

func (k *kindEnv) mustIn(ns string, args ...string) string {
	k.t.Helper()
	out, err := k.kubectlIn(ns, args...)
	if err != nil {
		k.t.Fatalf("kubectl -n %s %s: %v\n%s", ns, strings.Join(args, " "), err, out)
	}
	return out
}

// kubectl / mustKubectl act on the Nest namespace.
func (k *kindEnv) kubectl(args ...string) (string, error) { return k.kubectlIn(nestNS, args...) }

func (k *kindEnv) mustKubectl(args ...string) string {
	k.t.Helper()
	return k.mustIn(nestNS, args...)
}

// pods lists live (not terminating) instance pods of a deployment.
func (k *kindEnv) pods(dep string) []string {
	out := k.mustKubectl("get", "pods", "-l", "app.kubernetes.io/managed-by=agen,agen.dev/deployment="+dep,
		"-o", `jsonpath={range .items[*]}{.metadata.name} {.status.phase} {.metadata.deletionTimestamp}{"\n"}{end}`)
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && (f[1] == "Running" || f[1] == "Pending") {
			names = append(names, f[0])
		}
	}
	return names
}

func (k *kindEnv) ask(dep, input string) string {
	k.t.Helper()
	ctx := context.Background()
	r, err := k.c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: &agenv1.DeploymentRef{Name: dep}, Input: input}))
	if err != nil {
		k.t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		g, err := k.c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: r.Msg.Task.Id, WaitSeconds: 10}))
		if err == nil && g.Msg.Task.State == agenv1.TaskState_TASK_STATE_SUCCEEDED {
			return g.Msg.Task.Output
		}
		if err == nil && g.Msg.Task.State == agenv1.TaskState_TASK_STATE_FAILED {
			k.t.Fatalf("task failed: %v", g.Msg.Task)
		}
	}
	k.t.Fatalf("task %s did not finish", r.Msg.Task.Id)
	return ""
}

// loadImage copies a local image into the kind node. `kind load` imports
// with --all-platforms, which fails for images whose other platforms Docker
// never pulled (Docker Desktop's containerd store); importing just the
// local platform works everywhere.
func loadImage(img string) error {
	save := exec.Command("docker", "save", img)
	imp := exec.Command("docker", "exec", "-i", kindCluster+"-control-plane", "ctr", "--namespace=k8s.io", "images", "import", "--snapshotter=overlayfs", "-")
	pipe, err := save.StdoutPipe()
	if err != nil {
		return err
	}
	var out bytes.Buffer
	imp.Stdin, imp.Stdout, imp.Stderr = pipe, &out, &out
	if err := save.Start(); err != nil {
		return err
	}
	if err := imp.Run(); err != nil {
		save.Wait()
		return fmt.Errorf("%v: %s", err, out.String())
	}
	return save.Wait()
}

func findKind() string {
	if p, err := exec.LookPath("kind"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	name := "kind"
	if os.PathSeparator == '\\' {
		name += ".exe"
	}
	return filepath.Join(home, "go", "bin", name)
}

func TestKubernetesBackend(t *testing.T) {
	if os.Getenv("AGEN_KIND_E2E") != "1" {
		t.Skip("set AGEN_KIND_E2E=1 (needs Docker, kind, kubectl and the agen:dev image)")
	}
	k := &kindEnv{t: t, kind: findKind()}
	kube := filepath.Join(repoRoot(), "deploy", "kube")
	run(k.kind, "delete", "cluster", "--name", kindCluster)
	cfg := filepath.Join(kube, "kind.yaml")
	if v, _ := run("docker", "info", "--format", "{{.CgroupVersion}}"); strings.TrimSpace(v) == "1" {
		// Docker on a cgroup v1 host (e.g. older WSL2 kernels): recent
		// kubelets refuse to start there unless told otherwise.
		b, _ := os.ReadFile(cfg)
		cfg = filepath.Join(t.TempDir(), "kind.yaml")
		patch := "kubeadmConfigPatches:\n  - |\n    kind: KubeletConfiguration\n    failCgroupV1: false\n"
		os.WriteFile(cfg, append(b, []byte(patch)...), 0o644)
	}
	if out, err := run(k.kind, "create", "cluster", "--name", kindCluster, "--config", cfg, "--wait", "120s"); err != nil {
		t.Fatalf("kind create: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if t.Failed() {
			for ns, sel := range map[string]string{hubNS: "app=agen-hub", nestNS: "app=agen-nest"} {
				out, _ := k.kubectlIn(ns, "logs", "-l", sel, "--tail", "60", "--all-containers", "--prefix")
				t.Logf("%s logs:\n%s", sel, out)
			}
			out, _ := run("kubectl", "--context", "kind-"+kindCluster, "get", "pods", "-A", "-o", "wide")
			t.Logf("pods:\n%s", out)
		}
		if os.Getenv("AGEN_KIND_KEEP") != "1" {
			run(k.kind, "delete", "cluster", "--name", kindCluster)
		}
	})
	if out, err := run("docker", "image", "inspect", "postgres:17-alpine"); err != nil {
		if out, err := run("docker", "pull", "postgres:17-alpine"); err != nil {
			t.Fatalf("pull postgres: %v\n%s", err, out)
		}
		_ = out
	}
	for _, img := range []string{"agen:dev", "postgres:17-alpine"} {
		if err := loadImage(img); err != nil {
			t.Fatalf("load %s into kind: %v", img, err)
		}
	}

	// Hub (2 replicas, mTLS) + dev Postgres.
	b := make([]byte, 16)
	rand.Read(b)
	admin := "agen_admin_" + hex.EncodeToString(b)
	const storeURL = "postgres://agen:agen@postgres.agen.svc:5432/agen?sslmode=disable"
	k.mustIn(hubNS, "apply", "-f", filepath.Join(kube, "hub.yaml"))
	k.mustIn(hubNS, "apply", "-f", filepath.Join(kube, "postgres-dev.yaml"))
	k.mustIn(hubNS, "create", "secret", "generic", "agen-admin", "--from-literal=token="+admin)
	k.mustIn(hubNS, "create", "secret", "generic", "agen-hub-kek", "--from-literal=kek=kek-"+hex.EncodeToString(b))
	k.mustIn(hubNS, "create", "secret", "generic", "agen-store", "--from-literal=url="+storeURL)
	var caHash string
	re := regexp.MustCompile(`CA (sha256:[0-9a-f]{64})`)
	waitFor(t, "hub CA", 180*time.Second, func() bool {
		out, _ := k.kubectlIn(hubNS, "logs", "-l", "app=agen-hub", "--tail", "-1")
		if m := re.FindStringSubmatch(out); m != nil {
			caHash = m[1]
		}
		return caHash != ""
	})
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig, tr.ForceAttemptHTTP2 = pki.PinnedTLS(caHash, "127.0.0.1"), true
	k.c = agenv1connect.NewHubServiceClient(&http.Client{Transport: tr, Timeout: 60 * time.Second}, "https://127.0.0.1:17444",
		connect.WithProtoJSON(), connect.WithInterceptors(connect.UnaryInterceptorFunc(
			func(next connect.UnaryFunc) connect.UnaryFunc {
				return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
					req.Header().Set("Authorization", "Bearer "+admin)
					return next(ctx, req)
				}
			})))
	ctx := context.Background()
	waitFor(t, "hub API", 90*time.Second, func() bool {
		_, err := k.c.ListNests(ctx, connect.NewRequest(&agenv1.ListNestsRequest{}))
		return err == nil
	})

	// A Nest with the Kubernetes backend joins.
	jt, err := k.c.CreateJoinToken(ctx, connect.NewRequest(&agenv1.CreateJoinTokenRequest{TtlSeconds: 600}))
	if err != nil {
		t.Fatal(err)
	}
	run("kubectl", "--context", "kind-"+kindCluster, "create", "namespace", nestNS)
	// The Nest namespace gets a least-privilege Store role (run-data tables
	// only), created from a Hub pod, which holds the admin DSN.
	hostPW := "host_" + hex.EncodeToString(b)
	k.mustIn(hubNS, "exec", "deploy/agen-hub", "--", "env", "AGEN_HOST_DB_PASSWORD="+hostPW, "agen", "store", "host-role", "--role", "agen_host")
	hostURL := "postgres://agen_host:" + hostPW + "@postgres.agen.svc:5432/agen?sslmode=disable"
	k.mustKubectl("create", "secret", "generic", "agen-store", "--from-literal=url="+hostURL)
	k.mustKubectl("create", "secret", "generic", "agen-join", "--from-literal=token="+jt.Msg.Token, "--from-literal=ca-hash="+caHash)
	// A provider-style key every instance should receive (--host-env-dir).
	k.mustKubectl("create", "secret", "generic", "agen-host-env", "--from-literal=DEMO_PROVIDER_KEY=demo-value-42")
	k.mustKubectl("apply", "-f", filepath.Join(kube, "nest.yaml"))
	// The Hub's secrets are not in the Nest's namespace.
	if out, _ := k.kubectl("get", "secret", "agen-admin"); !strings.Contains(out, "NotFound") {
		t.Fatalf("admin secret visible in the nest namespace: %s", out)
	}
	waitFor(t, "kubernetes nest active", 120*time.Second, func() bool {
		r, err := k.c.ListNests(ctx, connect.NewRequest(&agenv1.ListNestsRequest{}))
		return err == nil && len(r.Msg.Nests) == 1 && r.Msg.Nests[0].Backend == "kubernetes" && r.Msg.Nests[0].State == agenv1.NestState_NEST_STATE_ACTIVE
	})

	// ---- deploy + scale: one pod per instance ----
	if _, err := k.c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "hello", BundleFiles: files(t, nil)})); err != nil {
		t.Fatal(err)
	}
	scale := func(n int32) {
		t.Helper()
		if _, err := k.c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: &agenv1.DeploymentRef{Name: "hello"}, Desired: n})); err != nil {
			t.Fatal(err)
		}
	}
	readyInstances := func(dep string) int {
		r, err := k.c.ListInstances(ctx, connect.NewRequest(&agenv1.ListInstancesRequest{Deployment: dep}))
		if err != nil {
			return -1
		}
		n := 0
		for _, in := range r.Msg.Instances {
			if in.State == agenv1.InstanceState_INSTANCE_STATE_READY || in.State == agenv1.InstanceState_INSTANCE_STATE_BUSY {
				n++
			}
		}
		return n
	}
	scale(2)
	waitFor(t, "2 hello pods ready", 180*time.Second, func() bool { return len(k.pods("hello")) == 2 && readyInstances("hello") == 2 })
	if out := k.ask("hello", "hi"); out != "Hello! Nice to meet you." {
		t.Fatalf("answer from a pod: %q", out)
	}
	t.Logf("deploy/scale: 2 instance pods %v answered", k.pods("hello"))

	// Host settings: the provider key reached the instance's secret (not
	// its pod spec), and the instance refuses callers without its host
	// token (asked from a Hub pod). Where the CNI enforces the
	// NetworkPolicy the call times out instead ("000"); either stops it.
	inst := k.pods("hello")[0]
	if v := k.mustKubectl("get", "secret", inst, "-o", "jsonpath={.data.DEMO_PROVIDER_KEY}"); v != base64.StdEncoding.EncodeToString([]byte("demo-value-42")) {
		t.Fatalf("provider key in instance secret: %q", v)
	}
	if spec := k.mustKubectl("get", "pod", inst, "-o", "json"); strings.Contains(spec, "demo-value-42") || strings.Contains(spec, "agen:agen@") {
		t.Fatal("secret value in the pod spec")
	}
	ip := strings.TrimSpace(k.mustKubectl("get", "pod", inst, "-o", "jsonpath={.status.podIP}"))
	code, _ := k.kubectlIn(hubNS, "exec", "deploy/agen-hub", "--", "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "5",
		"-X", "POST", "-H", "Content-Type: application/json", "-d", "{}", "http://"+ip+":7080/agen.v1.HostService/Health")
	// curl prints the code first; kubectl may append "command terminated
	// with exit code 28" when curl times out.
	c := strings.TrimSpace(code)
	if len(c) > 3 {
		c = c[:3]
	}
	if c != "401" && c != "000" {
		t.Fatalf("instance answered a foreign pod without the host token: %q", code)
	}
	t.Logf("host isolation: provider key only in the instance secret; foreign caller got %s", c)

	// ---- pod failure: a deleted instance pod is replaced ----
	before := k.pods("hello")
	k.mustKubectl("delete", "pod", before[0], "--wait=false")
	waitFor(t, "replacement pod", 120*time.Second, func() bool {
		now := k.pods("hello")
		if len(now) != 2 || readyInstances("hello") != 2 {
			return false
		}
		for _, p := range now {
			if p == before[0] {
				return false
			}
		}
		return true
	})
	if out := k.ask("hello", "hi again"); out != "Hello! Nice to meet you." {
		t.Fatalf("after pod failure: %q", out)
	}
	t.Logf("pod failure: %s deleted, replaced (%v)", before[0], k.pods("hello"))

	// ---- scale to zero ----
	scale(0)
	waitFor(t, "no hello pods", 120*time.Second, func() bool { return len(k.pods("hello")) == 0 })
	t.Log("scale to zero: no instance pods left")

	// ---- asleep deployment woken by an A2A call through the Gateway ----
	if _, err := k.c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "sleeper", BundleFiles: files(t, map[string]string{
		"x-agen/config.json": `{"kind":"pool","scale":{"min":0,"max":1,"idleTimeout":"5s"}}`,
	})})); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Second)
	if n := len(k.pods("sleeper")); n != 0 {
		t.Fatalf("sleeper should start asleep, has %d pods", n)
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "message/send", "params": map[string]any{
		"message": map[string]any{"kind": "message", "role": "user", "messageId": "wake-1", "parts": []map[string]string{{"kind": "text", "text": "hi"}}}}})
	res, err := k.c.Resolve(ctx, connect.NewRequest(&agenv1.ResolveRequest{Ref: &agenv1.DeploymentRef{Name: "sleeper"}}))
	if err != nil || res.Msg.Token == "" {
		t.Fatalf("resolve sleeper: %v %v", res, err)
	}
	wake, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:17445/a2a/default/sleeper", bytes.NewReader(body))
	wake.Header.Set("Content-Type", "application/json")
	wake.Header.Set("Authorization", "Bearer "+res.Msg.Token)
	resp, err := (&http.Client{Timeout: 3 * time.Minute}).Do(wake)
	if err != nil {
		t.Fatal(err)
	}
	var a2a bytes.Buffer
	a2a.ReadFrom(resp.Body)
	resp.Body.Close()
	if !strings.Contains(a2a.String(), "Hello! Nice to meet you.") {
		t.Fatalf("A2A wake: %s", a2a.String())
	}
	waitFor(t, "sleeper back to zero pods after idle", 120*time.Second, func() bool { return len(k.pods("sleeper")) == 0 })
	t.Log("wake: A2A call woke the asleep deployment (a pod was started), answered, then it scaled back to zero")

	// ---- scenario 8 (scenario 3 on kind): a 50-task burst scales a pool
	// from zero to 5 pods, every task completes, then it is back to zero ----
	if _, err := k.c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "workers", BundleFiles: files(t, map[string]string{
		"x-agen/config.json":      `{"kind":"pool","scale":{"min":0,"max":5,"targetQueuePerInstance":2,"idleTimeout":"5s","maxConcurrency":1}}`,
		"x-agen/fake-script.json": `{"cycle":true,"responses":[{"text":"done","delayMs":500}]}`,
	})})); err != nil {
		t.Fatal(err)
	}
	burst := time.Now()
	var ids []string
	for i := 0; i < 50; i++ {
		r, err := k.c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: &agenv1.DeploymentRef{Name: "workers"}, Input: "job"}))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.Msg.Task.Id)
	}
	maxPods := 0
	var fiveReady time.Duration
	waitFor(t, "all 50 burst tasks done", 5*time.Minute, func() bool {
		maxPods = max(maxPods, len(k.pods("workers")))
		if fiveReady == 0 && readyInstances("workers") >= 5 {
			fiveReady = time.Since(burst)
		}
		for _, id := range ids {
			g, err := k.c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: id}))
			if err != nil || g.Msg.Task.State != agenv1.TaskState_TASK_STATE_SUCCEEDED {
				return false
			}
		}
		return true
	})
	done := time.Since(burst)
	if maxPods != 5 {
		t.Fatalf("burst scaled to %d pods at most, want 5", maxPods)
	}
	// Scenario 3 targets 5 ready in 30 s on one machine; pods also need
	// scheduling and container start, so kind gets 60 s.
	if fiveReady == 0 || fiveReady > 60*time.Second {
		t.Fatalf("5 ready instances after %s, want <= 60s", fiveReady)
	}
	waitFor(t, "workers back to zero pods", 2*time.Minute, func() bool { return len(k.pods("workers")) == 0 })
	t.Logf("scenario 8: 50 tasks -> 5 ready after %s -> all succeeded after %s -> 0 pods after %s", fiveReady.Round(time.Second), done.Round(time.Second), time.Since(burst).Round(time.Second))

	// ---- the Nest pod itself dies: its instance pods go with it, the
	// restarted Nest keeps its identity and runs the instances again ----
	scale(1)
	waitFor(t, "1 hello pod", 180*time.Second, func() bool { return len(k.pods("hello")) == 1 && readyInstances("hello") == 1 })
	old := k.pods("hello")[0]
	nests, _ := k.c.ListNests(ctx, connect.NewRequest(&agenv1.ListNestsRequest{}))
	nestID := nests.Msg.Nests[0].Id
	k.mustKubectl("delete", "pod", "agen-nest-0", "--grace-period=0", "--force")
	waitFor(t, "instances back after nest restart", 240*time.Second, func() bool {
		now := k.pods("hello")
		return len(now) == 1 && now[0] != old && readyInstances("hello") == 1
	})
	nests, _ = k.c.ListNests(ctx, connect.NewRequest(&agenv1.ListNestsRequest{}))
	if len(nests.Msg.Nests) != 1 || nests.Msg.Nests[0].Id != nestID {
		t.Fatalf("nest identity after restart: %v (was %s)", nests.Msg.Nests, nestID)
	}
	if out := k.ask("hello", "after nest restart"); out != "Hello! Nice to meet you." {
		t.Fatalf("after nest restart: %q", out)
	}
	t.Logf("nest pod failure: nest %s restarted with the same identity; instance pod %s replaced", nestID, old)
}
