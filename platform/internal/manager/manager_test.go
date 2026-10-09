package manager

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/gen/agen/v1/agenv1connect"
	"github.com/prjvvl/agen/platform/internal/hub"
	"github.com/prjvvl/agen/platform/internal/store"
)

const admin = "admin-secret-for-tests"

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

// hostBin returns the agen-host binary: AGEN_HOST_BIN, or the workspace
// debug build (built on demand).
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

func helloFiles(t *testing.T) map[string][]byte {
	root := filepath.Join(repoRoot(), "examples", "bundles", "hello")
	files := map[string][]byte{}
	if err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		b, err := os.ReadFile(p)
		files[filepath.ToSlash(rel)] = b
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

// syncBuf is a goroutine-safe log sink dumped when a test fails.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type cluster struct {
	url, storeURL, dir, bin string
	srv                     *httptest.Server
	store                   *store.Store
	client                  agenv1connect.HubServiceClient
	mgr                     *Manager
	stop                    func()
	logs                    *syncBuf
}

// clusterOpts tunes a test cluster.
type clusterOpts struct {
	autoscale bool
}

func startCluster(t *testing.T, capacity int, opts ...func(*Config, *clusterOpts)) *cluster {
	t.Helper()
	bin := hostBin(t)
	dir := t.TempDir()
	storeURL := "sqlite:" + filepath.ToSlash(filepath.Join(dir, "agen.db"))
	s, err := store.Open(context.Background(), storeURL)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := hub.New(s, admin)
	h.Poll = 20 * time.Millisecond
	mux := http.NewServeMux()
	p, hh := h.Handler()
	mux.Handle(p, hh)
	nest := h.Nest()
	nest.WatchPoll = 50 * time.Millisecond
	p, nh := nest.Handler()
	mux.Handle(p, nh)
	srv := httptest.NewServer(mux)

	ctx, cancel := context.WithCancel(context.Background())
	sched := h.NewScheduler("test-hub")
	sched.Tick, sched.Log = 100*time.Millisecond, log
	var co clusterOpts
	cfgHooks := opts

	client := agenv1connect.NewHubServiceClient(http.DefaultClient, srv.URL, connect.WithProtoJSON(), connect.WithInterceptors(connect.UnaryInterceptorFunc(
		func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				req.Header().Set("Authorization", "Bearer "+admin)
				return next(ctx, req)
			}
		})))
	jt, err := client.CreateJoinToken(ctx, connect.NewRequest(&agenv1.CreateJoinTokenRequest{TtlSeconds: 60}))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{HubURL: srv.URL, JoinToken: jt.Msg.Token, Name: "local", Capacity: capacity, HostBin: bin, StoreURL: storeURL,
		DataDir: filepath.Join(dir, "nest"), Heartbeat: 200 * time.Millisecond, DispatchPoll: 50 * time.Millisecond, LeaseSeconds: 3, Log: log}
	for _, o := range cfgHooks {
		o(&cfg, &co)
	}
	// Most tests drive desired counts by hand. Settings are final before
	// the scheduler starts (it reads them without a lock).
	sched.Autoscaling = co.autoscale
	schedDone := make(chan struct{})
	go func() { sched.Run(ctx); close(schedDone) }()
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- m.Run(ctx) }()
	c := &cluster{srv: srv, url: srv.URL, storeURL: storeURL, dir: dir, bin: bin, store: s, client: client, mgr: m, logs: logs}
	var once sync.Once
	c.stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-mgrDone:
			case <-time.After(60 * time.Second):
				t.Error("manager did not stop")
			}
			<-schedDone
			srv.Close()
			s.Close()
		})
	}
	t.Cleanup(func() {
		c.stop()
		if t.Failed() {
			t.Logf("cluster logs:\n%s", logs.String())
		}
	})
	return c
}

// waitFor polls cond until it holds or the timeout passes.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (c *cluster) readyInstances(t *testing.T) []*agenv1.Instance { return c.ready(t, "hello") }

func (c *cluster) ready(t *testing.T, deployment string) []*agenv1.Instance {
	r, err := c.client.ListInstances(context.Background(), connect.NewRequest(&agenv1.ListInstancesRequest{Namespace: "default", Deployment: deployment}))
	if err != nil {
		t.Fatal(err)
	}
	var out []*agenv1.Instance
	for _, in := range r.Msg.Instances {
		if in.State == agenv1.InstanceState_INSTANCE_STATE_READY || in.State == agenv1.InstanceState_INSTANCE_STATE_BUSY {
			out = append(out, in)
		}
	}
	return out
}

func (c *cluster) allInstances(t *testing.T) int {
	r, err := c.client.ListInstances(context.Background(), connect.NewRequest(&agenv1.ListInstancesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return len(r.Msg.Instances)
}

func TestManagerRunsDeploymentTasksEndToEnd(t *testing.T) {
	c := startCluster(t, 4)
	ctx := context.Background()
	ref := &agenv1.DeploymentRef{Namespace: "default", Name: "hello"}
	if _, err := c.client.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	// Tasks submitted while nothing runs wait durably in the queue.
	var ids []string
	for i := 0; i < 4; i++ {
		r, err := c.client.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref, Input: "hi"}))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.Msg.Task.Id)
	}
	if _, err := c.client.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref, Desired: 2})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "2 ready instances", 60*time.Second, func() bool { return len(c.readyInstances(t)) == 2 })

	usedInstances := map[string]bool{}
	for _, id := range ids {
		r, err := c.client.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: id, WaitSeconds: 30}))
		if err != nil {
			t.Fatal(err)
		}
		tk := r.Msg.Task
		if tk.State != agenv1.TaskState_TASK_STATE_SUCCEEDED || tk.Output != "Hello! Nice to meet you." || tk.RunId == "" || tk.InstanceId == "" {
			t.Fatalf("task: %v", tk)
		}
		usedInstances[tk.InstanceId] = true
	}
	runs, err := c.client.ListRuns(ctx, connect.NewRequest(&agenv1.ListRunsRequest{Namespace: "default", Deployment: "hello"}))
	if err != nil || len(runs.Msg.Runs) != 4 {
		t.Fatalf("runs recorded by hosts: %v %v", runs, err)
	}
	for _, r := range runs.Msg.Runs {
		if r.Status != "succeeded" || r.DefinitionDigest == "" {
			t.Fatalf("run: %v", r)
		}
	}
	t.Logf("tasks ran on %d instance(s)", len(usedInstances))

	// A crashed instance is replaced.
	before := c.readyInstances(t)
	if !c.mgr.KillInstance(before[0].Id) {
		t.Fatal("kill")
	}
	waitFor(t, "crashed instance replaced", 60*time.Second, func() bool {
		now := c.readyInstances(t)
		if len(now) != 2 {
			return false
		}
		for _, in := range now {
			if in.Id == before[0].Id {
				return false
			}
		}
		return true
	})
	// The replacement serves tasks too.
	r, err := c.client.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref, Input: "again"}))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := c.client.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: r.Msg.Task.Id, WaitSeconds: 30}))
	if got.Msg.Task.State != agenv1.TaskState_TASK_STATE_SUCCEEDED {
		t.Fatalf("after replace: %v", got.Msg.Task)
	}

	// Scale to zero: instances drain and stop.
	if _, err := c.client.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref, Desired: 0})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "instances stopped", 30*time.Second, func() bool { return c.allInstances(t) == 0 && len(c.mgr.Instances()) == 0 })

	// A new definition version rolls instances onto the new digest.
	c.client.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref, Desired: 1}))
	waitFor(t, "1 ready instance", 60*time.Second, func() bool { return len(c.readyInstances(t)) == 1 })
	v2 := helloFiles(t)
	v2["x-agen/fake-script.json"] = []byte(`{"cycle":true,"responses":[{"text":"Hello from v2."}]}`)
	u, err := c.client.UpdateDeployment(ctx, connect.NewRequest(&agenv1.UpdateDeploymentRequest{Ref: ref, BundleFiles: v2}))
	if err != nil {
		t.Fatal(err)
	}
	digest := u.Msg.Deployment.DefinitionDigest
	waitFor(t, "instance on new digest", 60*time.Second, func() bool {
		rs := c.readyInstances(t)
		return len(rs) == 1 && rs[0].DefinitionDigest == digest
	})
	r, _ = c.client.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref, Input: "v2?"}))
	got, _ = c.client.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: r.Msg.Task.Id, WaitSeconds: 30}))
	if got.Msg.Task.Output != "Hello from v2." {
		t.Fatalf("v2 output: %v", got.Msg.Task)
	}

	// Stopping the manager stops every host process.
	c.stop()
	if n := len(c.mgr.Instances()); n != 0 {
		t.Fatalf("instances left after stop: %d", n)
	}
}

// TestMain lets the test binary act as a bare Manager process for
// TestHostsDieWithTheManager.
func TestMain(m *testing.M) {
	if os.Getenv("AGEN_TEST_MANAGER_CHILD") == "1" {
		cfg := Config{HubURL: os.Getenv("HUB"), JoinToken: os.Getenv("JOIN"), Name: "child", Capacity: 2, HostBin: os.Getenv("HOST_BIN"),
			StoreURL: os.Getenv("STORE"), DataDir: os.Getenv("DATA"), Heartbeat: 200 * time.Millisecond, DispatchPoll: 50 * time.Millisecond,
			Labels: map[string]string{"role": "child"}}
		mgr, err := New(cfg)
		if err == nil {
			err = mgr.Run(context.Background())
		}
		if err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// A Manager killed without cleanup takes its hosts with it (Job Object on
// Windows, parent-death signal on Linux): no orphaned agents.
func TestHostsDieWithTheManager(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("process-tree cleanup is implemented for Windows and Linux")
	}
	c := startCluster(t, 4)
	ctx := context.Background()
	ref := &agenv1.DeploymentRef{Namespace: "default", Name: "hello"}
	// Only the child nest (labelled role=child) may run this deployment.
	if _, err := c.client.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t),
		Placement: &agenv1.Placement{Labels: map[string]string{"role": "child"}}})); err != nil {
		t.Fatal(err)
	}
	jt, err := c.client.CreateJoinToken(ctx, connect.NewRequest(&agenv1.CreateJoinTokenRequest{TtlSeconds: 60}))
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), "AGEN_TEST_MANAGER_CHILD=1", "HUB="+c.url, "JOIN="+jt.Msg.Token, "HOST_BIN="+c.bin,
		"STORE="+c.storeURL, "DATA="+filepath.Join(c.dir, "child"))
	var childErr syncBuf
	child.Stderr = &childErr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("child manager stderr:\n%s", childErr.String())
		}
	})
	if _, err := c.client.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref, Desired: 1})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "child instance ready", 60*time.Second, func() bool { return len(c.readyInstances(t)) == 1 })
	in := c.readyInstances(t)[0]
	if len(c.mgr.Instances()) != 0 {
		t.Fatal("in-process nest took an instance despite placement labels")
	}
	// Without the host token the host answers 401: any HTTP answer means
	// the process is alive; a transport error means it is gone.
	host := &HostClient{BaseURL: in.Endpoint, HTTP: &http.Client{Timeout: 2 * time.Second}}
	alive := func() bool {
		_, err := host.Health(ctx)
		var he *HostError
		return err == nil || errors.As(err, &he)
	}
	if !alive() {
		t.Fatal("host not answering before kill")
	}
	var he *HostError
	if _, err := host.Health(ctx); !errors.As(err, &he) || he.Status != http.StatusUnauthorized {
		t.Fatalf("host answered a caller without its host token: %v", err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	waitFor(t, "host to die with its manager", 15*time.Second, func() bool { return !alive() })
}

func TestSafeJoinRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{"../x", "a/../../x", "/etc/passwd", "C:/x", "a\\b", "", "a//b", "./a"} {
		if _, err := safeJoin(root, bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if p, err := safeJoin(root, "skills/greeting/SKILL.md"); err != nil || filepath.Dir(filepath.Dir(filepath.Dir(p))) != root {
		t.Fatalf("valid path: %s %v", p, err)
	}
}

// slowFiles is the hello bundle whose model answers after delayMs.
func slowFiles(t *testing.T, delayMs int) map[string][]byte {
	f := helloFiles(t)
	f["x-agen/fake-script.json"] = []byte(`{"cycle":true,"responses":[{"text":"slow done","delayMs":` + strconv.Itoa(delayMs) + `}]}`)
	return f
}

func (c *cluster) waitRunning(t *testing.T, id string) *agenv1.Task {
	t.Helper()
	var tk *agenv1.Task
	waitFor(t, "task running", 60*time.Second, func() bool {
		r, err := c.client.GetTask(context.Background(), connect.NewRequest(&agenv1.GetTaskRequest{Id: id}))
		if err != nil {
			t.Fatal(err)
		}
		tk = r.Msg.Task
		return tk.State == agenv1.TaskState_TASK_STATE_RUNNING && tk.InstanceId != ""
	})
	return tk
}

// A host that dies mid-task: the Manager requeues the task and another
// instance resumes the same run (tasks map to one run by task id).
func TestCrashMidTaskResumesOnAnotherInstance(t *testing.T) {
	c := startCluster(t, 4)
	ctx := context.Background()
	ref := &agenv1.DeploymentRef{Namespace: "default", Name: "slow"}
	if _, err := c.client.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "slow", BundleFiles: slowFiles(t, 2500)})); err != nil {
		t.Fatal(err)
	}
	c.client.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref, Desired: 2}))
	waitFor(t, "2 ready", 60*time.Second, func() bool { return len(c.ready(t, "slow")) == 2 })
	s, err := c.client.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref, Input: "work"}))
	if err != nil {
		t.Fatal(err)
	}
	first := c.waitRunning(t, s.Msg.Task.Id)
	time.Sleep(500 * time.Millisecond) // mid model call
	if !c.mgr.KillInstance(first.InstanceId) {
		t.Fatal("kill")
	}
	r, err := c.client.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: s.Msg.Task.Id, WaitSeconds: 30}))
	if err != nil {
		t.Fatal(err)
	}
	tk := r.Msg.Task
	if tk.State != agenv1.TaskState_TASK_STATE_SUCCEEDED || tk.Output != "slow done" || tk.InstanceId == first.InstanceId || tk.Attempts != 2 {
		t.Fatalf("after crash: %v (first instance %s)", tk, first.InstanceId)
	}
	runs, _ := c.client.ListRuns(ctx, connect.NewRequest(&agenv1.ListRunsRequest{Namespace: "default", Deployment: "slow"}))
	if len(runs.Msg.Runs) != 1 || runs.Msg.Runs[0].Id != tk.RunId || runs.Msg.Runs[0].Status != "succeeded" {
		t.Fatalf("one resumed run expected: %v", runs.Msg.Runs)
	}
}

// Cancelling a task at the Hub reaches the running host through the lease
// keepalive, and the instance's slot is freed.
func TestCancelledTaskCancelsHostRun(t *testing.T) {
	c := startCluster(t, 4)
	ctx := context.Background()
	ref := &agenv1.DeploymentRef{Namespace: "default", Name: "slow"}
	if _, err := c.client.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "slow", BundleFiles: slowFiles(t, 20000)})); err != nil {
		t.Fatal(err)
	}
	c.client.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref, Desired: 1}))
	waitFor(t, "1 ready", 60*time.Second, func() bool { return len(c.ready(t, "slow")) == 1 })
	s, _ := c.client.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref, Input: "long"}))
	c.waitRunning(t, s.Msg.Task.Id)
	start := time.Now()
	if _, err := c.client.CancelTask(ctx, connect.NewRequest(&agenv1.CancelTaskRequest{Id: s.Msg.Task.Id})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "host run cancelled", 10*time.Second, func() bool {
		runs, _ := c.client.ListRuns(ctx, connect.NewRequest(&agenv1.ListRunsRequest{Namespace: "default", Deployment: "slow"}))
		return len(runs.Msg.Runs) == 1 && runs.Msg.Runs[0].Status == "cancelled"
	})
	if el := time.Since(start); el > 8*time.Second {
		t.Fatalf("cancel took %s", el)
	}
	waitFor(t, "slot freed", 10*time.Second, func() bool {
		in := c.ready(t, "slow")
		return len(in) == 1 && in[0].RunningTasks == 0
	})
	got, _ := c.client.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: s.Msg.Task.Id}))
	if got.Msg.Task.State != agenv1.TaskState_TASK_STATE_CANCELLED {
		t.Fatalf("task: %v", got.Msg.Task)
	}
}

// A host that cannot start is restarted with backoff, not in a tight loop.
func TestCrashLoopBacksOff(t *testing.T) {
	bad := "sqlite:" + filepath.ToSlash(filepath.Join(t.TempDir(), "missing-dir", "nested", "x.db"))
	c := startCluster(t, 4, func(cfg *Config, _ *clusterOpts) { cfg.StoreURL = bad })
	ctx := context.Background()
	if _, err := c.client.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	c.client.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: &agenv1.DeploymentRef{Namespace: "default", Name: "hello"}, Desired: 1}))
	time.Sleep(7 * time.Second)
	starts := strings.Count(c.logs.String(), `msg="instance starting"`)
	exits := strings.Count(c.logs.String(), `msg="instance exited unexpectedly"`)
	// 250 ms reconcile ticks would give ~28 starts; backoff 2s, 4s, 8s... gives 3-4.
	if exits == 0 || starts < 2 || starts > 5 {
		t.Fatalf("starts %d exits %d\n%s", starts, exits, c.logs.String())
	}
}

// When the Hub is unreachable, singletons are killed at once, even busy ones
// (the Hub will reschedule them elsewhere), while pools keep serving.
func TestPartitionedNestStopsSingletonsButKeepsPools(t *testing.T) {
	c := startCluster(t, 4, func(cfg *Config, _ *clusterOpts) { cfg.HubLostAfter = time.Second })
	ctx := context.Background()
	single := slowFiles(t, 60000)
	single["x-agen/config.json"] = []byte(`{"kind":"singleton"}`)
	if _, err := c.client.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "solo", BundleFiles: single})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.client.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	c.client.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: &agenv1.DeploymentRef{Namespace: "default", Name: "solo"}, Desired: 1}))
	c.client.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: &agenv1.DeploymentRef{Namespace: "default", Name: "hello"}, Desired: 1}))
	waitFor(t, "both ready", 60*time.Second, func() bool { return len(c.ready(t, "solo")) == 1 && len(c.ready(t, "hello")) == 1 })
	busy, _ := c.client.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: &agenv1.DeploymentRef{Namespace: "default", Name: "solo"}, Input: "long"}))
	c.waitRunning(t, busy.Msg.Task.Id)
	cut := time.Now()
	c.srv.CloseClientConnections()
	c.srv.Config.SetKeepAlivesEnabled(false)
	c.srv.Listener.Close() // the Hub disappears
	waitFor(t, "singleton stopped", 15*time.Second, func() bool {
		in := c.mgr.Instances()
		return len(in["default/solo"]) == 0 && len(in["default/hello"]) == 1
	})
	c.mgr.mu.Lock()
	tracked := 0
	for _, in := range c.mgr.instances {
		if in.dep == "solo" {
			tracked++
		}
	}
	c.mgr.mu.Unlock()
	// Killed, not drained: the process is gone within a few seconds of the
	// Hub disappearing although its task had a minute left.
	waitFor(t, "busy singleton process gone", 5*time.Second, func() bool {
		c.mgr.mu.Lock()
		defer c.mgr.mu.Unlock()
		for _, in := range c.mgr.instances {
			if in.dep == "solo" {
				return false
			}
		}
		return true
	})
	if el := time.Since(cut); el > 8*time.Second {
		t.Fatalf("singleton stopped after %s (tracked %d)", el, tracked)
	}
	time.Sleep(time.Second) // and it is not restarted while the Hub is away
	if in := c.mgr.Instances(); len(in["default/solo"]) != 0 || len(in["default/hello"]) != 1 {
		t.Fatalf("instances: %v", in)
	}
}

// Task agents: queued work starts instances, every task completes, then the
// deployment returns to zero and nothing is left running or recorded as live.
func TestTaskAgentsExitAndLeaveNothingBehind(t *testing.T) {
	c := startCluster(t, 8, func(_ *Config, o *clusterOpts) { o.autoscale = true })
	ctx := context.Background()
	files := slowFiles(t, 300)
	files["x-agen/config.json"] = []byte(`{"kind":"task","scale":{"min":0,"max":3}}`)
	if _, err := c.client.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "jobs", BundleFiles: files})); err != nil {
		t.Fatal(err)
	}
	ref := &agenv1.DeploymentRef{Namespace: "default", Name: "jobs"}
	if len(c.mgr.Instances()) != 0 {
		t.Fatal("instances before any work")
	}
	var ids []string
	for i := 0; i < 6; i++ {
		r, err := c.client.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref, Input: "job"}))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.Msg.Task.Id)
	}
	// Each task gets its own instance, which exits when the task is done.
	instances := map[string]bool{}
	for _, id := range ids {
		r, err := c.client.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: id, WaitSeconds: 60}))
		if err != nil {
			t.Fatal(err)
		}
		if r.Msg.Task.State != agenv1.TaskState_TASK_STATE_SUCCEEDED || r.Msg.Task.Output != "slow done" {
			t.Fatalf("task: %v", r.Msg.Task)
		}
		instances[r.Msg.Task.InstanceId] = true
	}
	if len(instances) != len(ids) {
		t.Fatalf("%d tasks ran on %d instances; task instances must not be reused", len(ids), len(instances))
	}
	waitFor(t, "task deployment back to zero", 30*time.Second, func() bool {
		d, err := c.client.GetDeployment(ctx, connect.NewRequest(&agenv1.GetDeploymentRequest{Ref: ref}))
		if err != nil {
			t.Fatal(err)
		}
		return d.Msg.Deployment.Desired == 0 && len(c.mgr.Instances()) == 0 && c.allInstances(t) == 0
	})
	// No host processes left for the deployment and no unfinished runs.
	c.mgr.mu.Lock()
	left := len(c.mgr.instances)
	c.mgr.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d instance processes still tracked", left)
	}
	var succeeded, other int
	if err := c.store.DB().QueryRowContext(ctx,
		"SELECT COALESCE(SUM(CASE WHEN status = 'succeeded' THEN 1 ELSE 0 END), 0), COALESCE(SUM(CASE WHEN status <> 'succeeded' THEN 1 ELSE 0 END), 0) FROM runs WHERE deployment = 'jobs'").
		Scan(&succeeded, &other); err != nil {
		t.Fatal(err)
	}
	if succeeded != len(ids) || other != 0 {
		t.Fatalf("runs: %d succeeded, %d other", succeeded, other)
	}
}
