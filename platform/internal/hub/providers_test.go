package hub

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// An account error holds every deployment on the same provider key: no
// leases, no wake-ups, queued work waits. After the backoff one probe task
// goes through; a run that succeeds ends the hold. Operators can end it too.
// features: models.holds
func TestProviderAccountErrorsHoldTheAccount(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	for _, name := range []string{"alpha", "beta", "other"} {
		f := helloFiles(t)
		key := "OPENROUTER_API_KEY"
		if name == "other" {
			key = "OTHER_KEY"
		}
		f["x-agen/harness.json"] = []byte(`{"provider":"openrouter","model":"deepseek/deepseek-v4-flash","apiKeySecret":"` + key + `"}`)
		if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: name, BundleFiles: f})); err != nil {
			t.Fatal(err)
		}
		c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", name), Desired: 1}))
		for i := 0; i < 2; i++ {
			if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", name), Input: "work"})); err != nil {
				t.Fatal(err)
			}
		}
	}
	id, n := e.enroll(t, "n", 8, nil)
	e.scheduler().Step(ctx)
	lease := func(name string) []*agenv1.Task {
		t.Helper()
		l, err := n.LeaseTasks(ctx, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: id, Ref: ref("", name), Max: 1}))
		if err != nil {
			t.Fatal(err)
		}
		return l.Msg.Tasks
	}
	first := lease("alpha")
	if len(first) != 1 {
		t.Fatal("no task leased")
	}
	if _, err := n.CompleteTask(ctx, connect.NewRequest(&agenv1.CompleteTaskRequest{NestId: id, TaskId: first[0].Id, LeaseId: first[0].LeaseId,
		Error: "provider account error: HTTP 401 Unauthorized: key expired"})); err != nil {
		t.Fatal(err)
	}
	acct := "default/openrouter:OPENROUTER_API_KEY"
	var hold store.ProviderHold
	deadline := time.Now().Add(5 * time.Second)
	for {
		h, err := e.store.GetProviderHold(ctx, acct)
		if err == nil {
			hold = h
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("account not held after an account error")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if hold.Reason != "HTTP 401 Unauthorized: key expired" {
		t.Fatalf("reason %q", hold.Reason)
	}
	if got := lease("alpha"); len(got) != 0 {
		t.Fatalf("held account leased: %v", got)
	}
	if got := lease("beta"); len(got) != 0 {
		t.Fatalf("other deployment on the held key leased: %v", got)
	}
	if got := lease("other"); len(got) != 1 {
		t.Fatal("a deployment on another key is held too")
	}
	if _, err := c.RequestWake(ctx, connect.NewRequest(&agenv1.RequestWakeRequest{Ref: ref("", "beta")})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("wake while held: %v", err)
	}
	d, _ := c.GetDeployment(ctx, connect.NewRequest(&agenv1.GetDeploymentRequest{Ref: ref("", "beta")}))
	if d.Msg.Deployment.ProviderHold.GetAccount() != acct {
		t.Fatalf("deployment does not show the hold: %v", d.Msg.Deployment)
	}
	holds, err := e.client(e.token(t, ScopeViewer)).ListProviderHolds(ctx, connect.NewRequest(&agenv1.ListProviderHoldsRequest{}))
	if err != nil || len(holds.Msg.Holds) != 1 {
		t.Fatalf("list: %v %v", holds, err)
	}
	// The retry time comes: exactly one probe task goes through.
	if _, err := e.store.DB().ExecContext(ctx, "UPDATE provider_holds SET retry_ms = $1", store.NowMs()-1); err != nil {
		t.Fatal(err)
	}
	probe := lease("beta")
	if len(probe) != 1 {
		t.Fatal("no probe task once due")
	}
	if got := lease("alpha"); len(got) != 0 {
		t.Fatalf("second task during the probe: %v", got)
	}
	// The probe goes back to the queue (its instance was busy): another
	// probe may go through, but never to a paused deployment.
	if _, err := n.ReleaseTask(ctx, connect.NewRequest(&agenv1.ReleaseTaskRequest{NestId: id, TaskId: probe[0].Id, LeaseId: probe[0].LeaseId, NotStarted: true})); err != nil {
		t.Fatal(err)
	}
	c.PauseDeployment(ctx, connect.NewRequest(&agenv1.PauseDeploymentRequest{Ref: ref("", "alpha"), Paused: true}))
	if got := lease("alpha"); len(got) != 0 {
		t.Fatalf("paused deployment got a probe: %v", got)
	}
	c.PauseDeployment(ctx, connect.NewRequest(&agenv1.PauseDeploymentRequest{Ref: ref("", "alpha"), Paused: false}))
	if probe = lease("beta"); len(probe) != 1 {
		t.Fatal("no new probe after the first went back to the queue")
	}
	// The probe succeeds: a run that started after the hold.
	now := store.NowMs()
	for _, q := range []string{
		"INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ('s1','beta','default','beta','{}',$1,$1)",
		"INSERT INTO conversations (id, session_id, created_ms, closed_ms) VALUES ('c1','s1',$1,$1)",
		"INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, started_ms, ended_ms) VALUES ('r1','s1','c1','default','beta','succeeded','x',$1,$1)",
	} {
		if _, err := e.store.DB().ExecContext(ctx, q, now+1); err != nil {
			t.Fatal(err)
		}
	}
	e.scheduler().Step(ctx)
	if _, err := e.store.GetProviderHold(ctx, acct); err == nil {
		t.Fatal("hold not ended by a successful run")
	}
	if got := lease("alpha"); len(got) != 1 {
		t.Fatal("work does not resume after the hold")
	}
	// Operators end a hold at once; viewers cannot.
	if _, _, err := e.store.HoldProvider(ctx, acct, "no credit"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.client(e.token(t, ScopeViewer)).ResumeProvider(ctx, connect.NewRequest(&agenv1.ResumeProviderRequest{Account: acct})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer resumed: %v", err)
	}
	if _, err := c.ResumeProvider(ctx, connect.NewRequest(&agenv1.ResumeProviderRequest{Account: acct})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ResumeProvider(ctx, connect.NewRequest(&agenv1.ResumeProviderRequest{Account: acct})); code(err) != connect.CodeNotFound {
		t.Fatalf("resume without a hold: %v", err)
	}
}

// features: models.holds
func TestHoldBackoff(t *testing.T) {
	for n, want := range map[int]time.Duration{1: 5 * time.Minute, 2: 10 * time.Minute, 4: 40 * time.Minute, 5: time.Hour, 9: time.Hour} {
		if got := store.HoldBackoff(n); got != want {
			t.Errorf("HoldBackoff(%d) = %s, want %s", n, got, want)
		}
	}
}

// token mints an API token with one scope.
func (e env) token(t *testing.T, scope string) string {
	t.Helper()
	r, err := e.client(admin).CreateApiToken(context.Background(), connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "t-" + scope + "-" + store.NewID(), Scopes: []string{scope}}))
	if err != nil {
		t.Fatal(err)
	}
	return r.Msg.Secret
}
