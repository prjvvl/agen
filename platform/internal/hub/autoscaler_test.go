package hub

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

func dep(kind string, desired int, lastActivityMs int64, scale string) store.Deployment {
	return store.Deployment{Kind: kind, Desired: desired, LastActivityMs: lastActivityMs, Scale: []byte(scale)}
}

func TestDesiredFor(t *testing.T) {
	now := time.Now().UnixMilli()
	recent, old := now-1000, now-10*60*1000
	cases := []struct {
		name string
		d    store.Deployment
		load int
		want int
	}{
		{"pool scales up at once", dep("pool", 0, old, `{"min":0,"max":5,"targetQueuePerInstance":2}`), 7, 4},
		{"pool capped at max", dep("pool", 1, recent, `{"min":0,"max":3}`), 50, 3},
		{"pool holds while recently active", dep("pool", 3, recent, `{"min":0,"max":5,"idleTimeoutSeconds":60}`), 0, 3},
		{"pool scales to min after idle", dep("pool", 3, old, `{"min":1,"max":5,"idleTimeoutSeconds":60}`), 0, 1},
		{"pool without idle timeout uses the default", dep("pool", 2, now-60*1000, `{"min":0,"max":5}`), 0, 2},
		{"pool partial scale down after idle", dep("pool", 4, old, `{"min":0,"max":5,"idleTimeoutSeconds":60}`), 1, 1},
		{"task stops as soon as the queue is empty", dep("task", 2, recent, `{"min":0,"max":5}`), 0, 0},
		{"task follows load", dep("task", 0, recent, `{"min":0,"max":5}`), 3, 3},
		{"desired below min is raised", dep("pool", 0, old, `{"min":2,"max":5}`), 0, 2},
		{"singleton max one", dep("singleton", 0, old, `{"min":0,"max":1}`), 9, 1},
	}
	for _, c := range cases {
		if got := DesiredFor(c.d, c.load, now); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

// A full Nest: an idle deployment holding its only slot gives it up after
// PressureIdle (not its idle timeout) when another deployment waits for room;
// without that pressure it keeps its slot.
func TestIdleCapacityGoesToWaitingWork(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	for _, name := range []string{"idle", "waiting"} {
		if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: name, BundleFiles: helloFiles(t),
			Scale: &agenv1.ScalePolicy{Min: 0, Max: 1, IdleTimeoutSeconds: 3600}})); err != nil {
			t.Fatal(err)
		}
	}
	e.enroll(t, "small", 1, nil)
	sched := e.scheduler()
	setActivity := func(name string, ago time.Duration) {
		t.Helper()
		if _, err := e.store.UpdateDeployment(ctx, "default", name, func(d *store.Deployment) error {
			d.LastActivityMs = store.NowMs() - ago.Milliseconds()
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	desired := func(name string) int32 {
		t.Helper()
		r, err := c.GetDeployment(ctx, connect.NewRequest(&agenv1.GetDeploymentRequest{Ref: ref("", name)}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg.Deployment.Desired
	}
	if _, err := c.ScaleDeployment(ctx, connect.NewRequest(&agenv1.ScaleDeploymentRequest{Ref: ref("", "idle"), Desired: 1})); err != nil {
		t.Fatal(err)
	}
	if _, err := sched.Step(ctx); err != nil {
		t.Fatal(err)
	}
	setActivity("idle", 2*time.Minute)
	if _, err := sched.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if desired("idle") != 1 {
		t.Fatal("idle deployment scaled down with no one waiting")
	}
	// Work arrives for the other deployment; the Nest is full.
	if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "waiting"), Input: "x"})); err != nil {
		t.Fatal(err)
	}
	setActivity("idle", 2*time.Minute)
	for i := 0; i < 3; i++ {
		if _, err := sched.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if desired("idle") != 0 || desired("waiting") != 1 {
		t.Fatalf("idle=%d waiting=%d, want the slot handed over", desired("idle"), desired("waiting"))
	}
	all, _ := e.store.AllAssignments(ctx)
	if len(all) != 1 || all[0].Deployment != "waiting" {
		t.Fatalf("assignments: %+v", all)
	}
}
