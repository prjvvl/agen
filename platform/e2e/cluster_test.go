// Package e2e drives distributed Agen fleets: TestDistributedCluster in Docker
// (deploy/compose/cluster.yml): Postgres, two Hubs behind a load balancer,
// three Nests, all with mTLS. Run with AGEN_CLUSTER_E2E=1 after building
// the image (docker build -f deploy/docker/Dockerfile -t agen:dev .).
package e2e

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
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/gen/agen/v1/agenv1connect"
	"github.com/prjvvl/agen/platform/internal/pki"
)

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

type cluster struct {
	t      *testing.T
	env    []string
	admin  string
	caHash string
	c      agenv1connect.HubServiceClient
}

func (k *cluster) compose(args ...string) (string, error) {
	cmd := exec.Command("docker", append([]string{"compose", "-p", "agen-e2e", "-f", filepath.Join(repoRoot(), "deploy", "compose", "cluster.yml")}, args...)...)
	cmd.Env = append(os.Environ(), k.env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func (k *cluster) must(args ...string) string {
	k.t.Helper()
	out, err := k.compose(args...)
	if err != nil {
		k.t.Fatalf("docker compose %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Second)
	}
}

// psql runs a query in the cluster's Postgres and returns trimmed output.
func (k *cluster) psql(q string) string {
	k.t.Helper()
	return strings.TrimSpace(k.must("exec", "-T", "postgres", "psql", "-U", "agen", "-d", "agen", "-tAc", q))
}

func files(t *testing.T, override map[string]string) map[string][]byte {
	root := filepath.Join(repoRoot(), "examples", "bundles", "hello")
	out := map[string][]byte{}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			b, _ := os.ReadFile(p)
			out[filepath.ToSlash(rel)] = b
		}
		return nil
	})
	for k, v := range override {
		out[k] = []byte(v)
	}
	return out
}

func (k *cluster) deploy(name string, f map[string][]byte, desired int32) {
	k.deployOn(name, f, desired, "")
}

// deployOn deploys onto one nest (by its node label) when node is set.
func (k *cluster) deployOn(name string, f map[string][]byte, desired int32, node string) {
	k.t.Helper()
	ctx := context.Background()
	req := &agenv1.CreateDeploymentRequest{Name: name, BundleFiles: f}
	if node != "" {
		req.Placement = &agenv1.Placement{Labels: map[string]string{"node": node}}
	}
	if _, err := k.c.CreateDeployment(ctx, connect.NewRequest(req)); err != nil {
		k.t.Fatal(err)
	}
	if _, err := k.c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: &agenv1.DeploymentRef{Name: name}, Desired: desired})); err != nil {
		k.t.Fatal(err)
	}
}

func (k *cluster) instances(dep string) []*agenv1.Instance {
	r, err := k.c.ListInstances(context.Background(), connect.NewRequest(&agenv1.ListInstancesRequest{Deployment: dep}))
	if err != nil {
		return nil
	}
	var ready []*agenv1.Instance
	for _, in := range r.Msg.Instances {
		if in.State == agenv1.InstanceState_INSTANCE_STATE_READY || in.State == agenv1.InstanceState_INSTANCE_STATE_BUSY {
			ready = append(ready, in)
		}
	}
	return ready
}

func (k *cluster) nestName(id string) string {
	r, err := k.c.ListNests(context.Background(), connect.NewRequest(&agenv1.ListNestsRequest{}))
	if err != nil {
		k.t.Fatal(err)
	}
	for _, n := range r.Msg.Nests {
		if n.Id == id {
			return n.Name
		}
	}
	return ""
}

func TestDistributedCluster(t *testing.T) {
	if os.Getenv("AGEN_CLUSTER_E2E") != "1" {
		t.Skip("set AGEN_CLUSTER_E2E=1 (needs Docker and the agen:dev image)")
	}
	b := make([]byte, 16)
	rand.Read(b)
	k := &cluster{t: t, admin: "agen_admin_" + hex.EncodeToString(b)}
	k.env = []string{"AGEN_ADMIN_TOKEN=" + k.admin, "AGEN_HUB_PORT=17443", "AGEN_HUB_KEK=kek-" + hex.EncodeToString(b)}
	k.compose("down", "-v", "--remove-orphans")
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := k.compose("logs", "--no-color", "--tail", "80")
			t.Logf("cluster logs:\n%s", out)
		}
		if os.Getenv("AGEN_CLUSTER_KEEP") != "1" {
			k.compose("down", "-v", "--remove-orphans")
		}
	})
	ctx := context.Background()

	// ---- Postgres + 2 Hubs + LB, then 3 Nests joining with mTLS ----
	k.must("up", "-d", "postgres", "hub1", "hub2", "hub")
	re := regexp.MustCompile(`CA (sha256:[0-9a-f]{64})`)
	waitFor(t, "hub CA", 90*time.Second, func() bool {
		out, _ := k.compose("logs", "hub1")
		m := re.FindStringSubmatch(out)
		if m != nil {
			k.caHash = m[1]
		}
		return m != nil
	})
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig, tr.ForceAttemptHTTP2 = pki.PinnedTLS(k.caHash, "127.0.0.1"), true
	hc := &http.Client{Transport: tr, Timeout: 60 * time.Second}
	k.c = agenv1connect.NewHubServiceClient(hc, "https://127.0.0.1:17443", connect.WithProtoJSON(), connect.WithInterceptors(connect.UnaryInterceptorFunc(
		func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				req.Header().Set("Authorization", "Bearer "+k.admin)
				return next(ctx, req)
			}
		})))
	waitFor(t, "hub API through the load balancer", 60*time.Second, func() bool {
		_, err := k.c.ListNests(ctx, connect.NewRequest(&agenv1.ListNestsRequest{}))
		return err == nil
	})
	for i := 1; i <= 3; i++ {
		r, err := k.c.CreateJoinToken(ctx, connect.NewRequest(&agenv1.CreateJoinTokenRequest{TtlSeconds: 600}))
		if err != nil || r.Msg.CaHash != k.caHash {
			t.Fatalf("join token: %v %v", r, err)
		}
		k.env = append(k.env, fmt.Sprintf("JOIN_TOKEN_%d=%s", i, r.Msg.Token))
	}
	k.env = append(k.env, "AGEN_CA_HASH="+k.caHash)
	// Nests (and their hosts) get a least-privilege Store role: run-data
	// tables only, no access to the Hub's keys or tokens.
	hostPW := "host_" + hex.EncodeToString(b)
	k.must("run", "--rm", "--no-deps", "-e", "AGEN_HOST_DB_PASSWORD="+hostPW, "hub1",
		"store", "host-role", "--role", "agen_host") // admin store from the Hub's AGEN_STORE
	k.env = append(k.env, "AGEN_NEST_STORE=postgres://agen_host:"+hostPW+"@postgres:5432/agen")
	if out, _ := k.compose("exec", "-T", "postgres", "psql", "postgres://agen_host:"+hostPW+"@localhost/agen", "-tAc", "SELECT key_pem FROM hub_ca"); !strings.Contains(out, "permission denied") {
		t.Fatalf("host role read the CA key: %s", out)
	}
	k.must("up", "-d", "nest1", "nest2", "nest3")
	waitFor(t, "3 active nests", 90*time.Second, func() bool {
		r, err := k.c.ListNests(ctx, connect.NewRequest(&agenv1.ListNestsRequest{}))
		if err != nil {
			return false
		}
		active := 0
		for _, n := range r.Msg.Nests {
			if n.State == agenv1.NestState_NEST_STATE_ACTIVE {
				active++
			}
		}
		return active == 3
	})
	// No credentials on process command lines (visible to anyone on the
	// host via ps / docker inspect): store URLs and join tokens come from the
	// environment.
	for _, svc := range []string{"hub1", "nest1"} {
		cmdline := k.must("exec", "-T", svc, "sh", "-c", "tr '\\0' ' ' < /proc/1/cmdline")
		if strings.Contains(cmdline, "postgres://") || strings.Contains(cmdline, "join_") || strings.Contains(cmdline, "--token") {
			t.Fatalf("%s command line carries a credential: %s", svc, cmdline)
		}
	}
	// The Hub's keys are sealed in the Store (AGEN_HUB_KEK).
	for _, q := range []string{"SELECT key_pem FROM hub_ca", "SELECT private_key FROM hub_token_key"} {
		if v := k.psql(q); !strings.HasPrefix(v, "enc1:") {
			t.Fatalf("%s: not sealed: %.20s", q, v)
		}
	}
	if n := k.psql("SELECT COUNT(*) FROM nests WHERE cert_fingerprint <> ''"); n != "3" {
		t.Fatalf("nests with certificates: %s", n)
	}
	leader := k.psql("SELECT holder FROM leases WHERE name = 'hub-leader'")
	t.Logf("hub leader: %s", leader)

	// Work spreads over the nests.
	k.deploy("hello", files(t, nil), 3)
	waitFor(t, "3 hello instances", 120*time.Second, func() bool { return len(k.instances("hello")) == 3 })
	spread := map[string]bool{}
	for _, in := range k.instances("hello") {
		spread[in.NestId] = true
	}
	if len(spread) < 2 {
		t.Fatalf("instances not spread over nests: %v", spread)
	}
	sub, err := k.c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: &agenv1.DeploymentRef{Name: "hello"}, Input: "hi"}))
	if err != nil {
		t.Fatal(err)
	}
	got, err := k.c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: sub.Msg.Task.Id, WaitSeconds: 30}))
	if err != nil || got.Msg.Task.Output != "Hello! Nice to meet you." {
		t.Fatalf("task on the cluster: %v %v", got, err)
	}
	t.Log("cluster up, 3 nests enrolled with certificates, work spread and answered")

	// nest3 runs its hosts under --host-memory-limit 2GiB (Linux RLIMIT_AS):
	// an agent placed there carries the limit and still answers.
	k.deployOn("limited", files(t, nil), 1, "nest3")
	waitFor(t, "limited on nest3", 120*time.Second, func() bool { return len(k.instances("limited")) == 1 })
	lim := k.must("exec", "-T", "nest3", "sh", "-c",
		"for d in /proc/[0-9]*; do if grep -q agen-host $d/cmdline 2>/dev/null; then grep 'Max address space' $d/limits; fi; done")
	if !strings.Contains(lim, "2147483648") {
		t.Fatalf("nest3 host without the memory limit: %q", lim)
	}
	lt, err := k.c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: &agenv1.DeploymentRef{Name: "limited"}, Input: "hi"}))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "task on the memory-limited host", 60*time.Second, func() bool {
		g, err := k.c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: lt.Msg.Task.Id, WaitSeconds: 5}))
		return err == nil && g.Msg.Task.State == agenv1.TaskState_TASK_STATE_SUCCEEDED
	})
	t.Log("native memory limit: nest3 host has RLIMIT_AS 2GiB and answered")

	// ---- Kill a nest mid side effect ----
	bank := files(t, map[string]string{
		"mcp.json":           `{"mcpServers":{"bank":{"command":"mcp-test-server","env":{"NOTES_FILE":"/shared/notes.txt"}}}}`,
		"x-agen/config.json": `{"kind":"pool","scale":{"min":0,"max":1},"permissions":{"default":"deny","rules":[{"tool":"bank.*","action":"allow"}]}}`,
		"x-agen/fake-script.json": `{"perRun":true,"responses":[
			{"toolCalls":[{"name":"bank.write_note_slow","arguments":{"text":"pay bob 100","ms":20000}}]},
			{"text":"payment handled"}]}`,
	})
	k.deploy("bank", bank, 1)
	waitFor(t, "bank instance", 120*time.Second, func() bool { return len(k.instances("bank")) == 1 })
	pay, _ := k.c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: &agenv1.DeploymentRef{Name: "bank"}, Input: "pay bob"}))
	waitFor(t, "side effect started", 60*time.Second, func() bool {
		return k.psql("SELECT COUNT(*) FROM effects e JOIN runs r ON r.id = e.run_id WHERE r.task_id = '"+pay.Msg.Task.Id+"'") == "1"
	})
	victim := k.nestName(k.instances("bank")[0].NestId)
	// The tool writes the note, then sleeps: kill during the sleep, when the
	// effect has happened but its result is not recorded.
	waitFor(t, "note written", 30*time.Second, func() bool {
		out, _ := k.compose("exec", "-T", victim, "sh", "-c", "cat /shared/notes.txt 2>/dev/null | wc -l")
		return strings.TrimSpace(out) == "1"
	})
	firstOwner := k.psql("SELECT owner FROM runs WHERE task_id = '" + pay.Msg.Task.Id + "'")
	t.Logf("killing %s while it runs the payment", victim)
	k.must("kill", victim)
	done, err := k.c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: pay.Msg.Task.Id, WaitSeconds: 1}))
	waitFor(t, "payment task to finish elsewhere", 180*time.Second, func() bool {
		done, err = k.c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: pay.Msg.Task.Id, WaitSeconds: 5}))
		return err == nil && done.Msg.Task.State == agenv1.TaskState_TASK_STATE_SUCCEEDED
	})
	runs := k.psql("SELECT COUNT(*) FROM runs WHERE task_id = '" + pay.Msg.Task.Id + "'")
	owner := k.psql("SELECT owner FROM runs WHERE task_id = '" + pay.Msg.Task.Id + "'")
	notes := strings.TrimSpace(k.must("exec", "-T", k.aliveNest(victim), "sh", "-c", "cat /shared/notes.txt | wc -l"))
	msgs := k.psql("SELECT COUNT(*) FROM messages m JOIN runs r ON r.conversation_id = m.conversation_id WHERE r.task_id = '" + pay.Msg.Task.Id + "' AND m.body LIKE '%effect_unknown%'")
	if runs != "1" || owner == firstOwner || notes != "1" || msgs == "0" || done.Msg.Task.Output != "payment handled" {
		t.Fatalf("resume after nest loss: runs=%s owner %s->%s notes=%s effect_unknown msgs=%s task=%v", runs, firstOwner, owner, notes, msgs, done.Msg.Task)
	}
	if state := k.psql("SELECT state FROM nests WHERE name = '" + victim + "'"); state != "lost" {
		t.Fatalf("%s state %s", victim, state)
	}
	t.Logf("nest failure: %s lost, run resumed on another nest, side effect written once (effect_unknown reported, not repeated)", victim)

	// ---- D5 / scenario 6 (part 1): both Hubs down, A2A between agents keeps working ----
	allow := `"permissions":{"default":"deny","rules":[{"tool":"call_agent","action":"allow"}]}`
	var alive []string
	for _, n := range []string{"nest1", "nest2", "nest3"} {
		if n != victim {
			alive = append(alive, n)
		}
	}
	k.deployOn("writer", files(t, map[string]string{"x-agen/fake-script.json": `{"cycle":true,"responses":[{"text":"Draft ready."}]}`}), 1, alive[0])
	k.deployOn("boss", files(t, map[string]string{
		"x-agen/config.json":      `{"kind":"pool","scale":{"min":0,"max":1},"delegates":[{"name":"writer"}],` + allow + `}`,
		"x-agen/fake-script.json": `{"perRun":true,"responses":[{"toolCalls":[{"name":"call_agent","arguments":{"agent":"writer","message":"draft"}}]},{"text":"Boss done."}]}`,
	}), 1, alive[1])
	waitFor(t, "boss and writer", 120*time.Second, func() bool { return len(k.instances("boss")) == 1 && len(k.instances("writer")) == 1 })
	bossNest := k.nestName(k.instances("boss")[0].NestId)
	if writerNest := k.nestName(k.instances("writer")[0].NestId); writerNest == bossNest {
		t.Fatalf("boss and writer both on %s", bossNest)
	}
	// Nest Gateways require a Hub-signed call token; an operator gets one
	// for boss from Resolve (valid while the Hubs are down).
	res, err := k.c.Resolve(ctx, connect.NewRequest(&agenv1.ResolveRequest{Ref: &agenv1.DeploymentRef{Name: "boss"}}))
	if err != nil || res.Msg.Token == "" {
		t.Fatalf("resolve boss: %v %v", res, err)
	}
	a2aWith := func(id, token string) string {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "message/send", "params": map[string]any{
			"message": map[string]any{"kind": "message", "role": "user", "messageId": id, "parts": []map[string]string{{"kind": "text", "text": "go"}}}}})
		args := []string{"exec", "-T", bossNest, "curl", "-s", "--max-time", "60", "-X", "POST", "-H", "Content-Type: application/json"}
		if token != "" {
			args = append(args, "-H", "Authorization: Bearer "+token)
		}
		out, _ := k.compose(append(args, "--data", string(body), "http://"+bossNest+":7071/a2a/default/boss")...)
		return out
	}
	a2a := func(id string) string { return a2aWith(id, res.Msg.Token) }
	if out := a2aWith("anon-1", ""); !strings.Contains(out, "-32052") {
		t.Fatalf("anonymous A2A call to a nest gateway: %s", out)
	}
	if out := a2a("warm-1"); !strings.Contains(out, "Boss done.") { // resolution cached while the Hubs are up
		t.Fatalf("A2A with Hubs up: %s", out)
	}
	k.must("stop", "hub1", "hub2")
	if out := a2a("hub-down-1"); !strings.Contains(out, "Boss done.") || !strings.Contains(out, `"completed"`) {
		t.Fatalf("A2A with both Hubs down: %s", out)
	}
	// "Boss done." is scripted: prove the delegated run itself succeeded and
	// its answer reached the boss, for both calls.
	if n := atoi(t, k.psql("SELECT COUNT(*) FROM runs WHERE deployment = 'writer' AND status = 'succeeded'")); n < 2 {
		t.Fatalf("writer runs succeeded: %d, want 2 (one while the Hubs were down)", n)
	}
	if n := atoi(t, k.psql("SELECT COUNT(*) FROM messages m JOIN runs r ON r.conversation_id = m.conversation_id WHERE r.deployment = 'boss' AND m.body LIKE '%Draft ready.%'")); n < 2 {
		t.Fatalf("boss saw the writer's answer %d times, want 2", n)
	}
	t.Logf("D5: both Hubs down, boss (%s) -> writer (%s, another nest) over A2A still answered", bossNest, alive[0])

	// ---- Leader failover ----
	k.must("start", "hub1", "hub2")
	waitFor(t, "hub API back", 90*time.Second, func() bool {
		_, err := k.c.ListNests(ctx, connect.NewRequest(&agenv1.ListNestsRequest{}))
		return err == nil
	})
	var leaderSvc string
	waitFor(t, "a leader", 60*time.Second, func() bool {
		leader = k.psql("SELECT holder FROM leases WHERE name = 'hub-leader' AND expires_ms > (extract(epoch from now()) * 1000)::bigint")
		leaderSvc = k.hubOf(leader)
		return leaderSvc != ""
	})
	epoch := k.psql("SELECT epoch FROM leases WHERE name = 'hub-leader'")
	t.Logf("killing leader %s (%s, epoch %s)", leaderSvc, leader, epoch)
	k.must("kill", leaderSvc)
	var newLeader string
	waitFor(t, "the other Hub to take over", 60*time.Second, func() bool {
		newLeader = k.psql("SELECT holder FROM leases WHERE name = 'hub-leader'")
		return newLeader != leader && k.hubOf(newLeader) != ""
	})
	newEpoch := k.psql("SELECT epoch FROM leases WHERE name = 'hub-leader'")
	if atoi(t, newEpoch) <= atoi(t, epoch) {
		t.Fatalf("epoch did not advance: %s -> %s", epoch, newEpoch)
	}
	// The new leader schedules: scaling works through the load balancer.
	if _, err := k.c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: &agenv1.DeploymentRef{Name: "hello"}, Desired: 1})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "rescheduled by the new leader", 90*time.Second, func() bool { return len(k.instances("hello")) == 1 })
	t.Logf("leader %s killed, %s took over (epoch %s -> %s) and schedules", leaderSvc, k.hubOf(newLeader), epoch, newEpoch)
	_ = base64.StdEncoding
}

// hubOf maps a leader lease holder (hostname/pid/id) to its compose service.
func (k *cluster) hubOf(holder string) string {
	host := strings.SplitN(holder, "/", 2)[0]
	for _, svc := range []string{"hub1", "hub2"} {
		out, err := k.compose("ps", "-q", svc)
		id := strings.TrimSpace(out)
		if err == nil && id != "" && strings.HasPrefix(id, host) {
			return svc
		}
	}
	return ""
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		t.Fatalf("not a number: %q", s)
	}
	return n
}

// aliveNest returns a nest service other than the killed one.
func (k *cluster) aliveNest(killed string) string {
	for _, n := range []string{"nest1", "nest2", "nest3"} {
		if n != killed {
			return n
		}
	}
	return ""
}
