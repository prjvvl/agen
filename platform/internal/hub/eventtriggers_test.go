package hub

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// finishTask leases and completes a queued task of a deployment.
func finishTask(t *testing.T, e env, dep, id string, ok bool) {
	t.Helper()
	ctx := context.Background()
	leased, err := e.store.LeaseTasks(ctx, "n1", "default", dep, 50, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range leased {
		if l.ID == id {
			if err := e.store.CompleteTask(ctx, l.ID, l.LeaseID, ok, "done", map[bool]string{true: "", false: "boom"}[ok], "", ""); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("task %s was not leased", id)
}

func tasksOf(t *testing.T, e env, dep string) []store.Task {
	t.Helper()
	list, err := e.store.ListTasks(context.Background(), "default", dep, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// An event trigger submits a task per matching event, with the event in its
// input, its labels, and a conversation key taken from the event; only
// events after the trigger appeared count.
// features: triggers
func TestEventTriggersFire(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	sched := e.scheduler()
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "watcher", BundleFiles: withTriggers(t,
		`[{"type":"event","name":"bad","on":{"types":["task.nope"]}}]`)})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("unknown event type accepted: %v", err)
	}
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "watcher", BundleFiles: withTriggers(t,
		`[{"type":"event","name":"bad","on":{"types":["task.finished"],"count":3}}]`)})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("count without within accepted: %v", err)
	}
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	early, _ := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "before the trigger"}))
	finishTask(t, e, "hello", early.Msg.Task.Id, false)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "watcher", BundleFiles: withTriggers(t,
		`[{"type":"event","name":"failures","input":"Look into this failure.","on":{"types":["task.finished"],"deployment":"hello"},
		  "conversation":"{{event.labels.ticket}}","labels":{"via":"watcher"}}]`)})); err != nil {
		t.Fatal(err)
	}
	if _, err := sched.Step(ctx); err != nil { // the trigger starts here
		t.Fatal(err)
	}
	sub, _ := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x", Labels: map[string]string{"ticket": "T-7"}}))
	finishTask(t, e, "hello", sub.Msg.Task.Id, false)
	for range 3 {
		if _, err := sched.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got := tasksOf(t, e, "watcher")
	if len(got) != 1 {
		t.Fatalf("watcher tasks: %d, want 1 (the earlier failure is not replayed, repeats fire once)", len(got))
	}
	w := got[0]
	if w.Source != "event:failures" || !strings.HasPrefix(w.Input, "Look into this failure.\n\n[Events from trigger failures]\n") ||
		!strings.Contains(w.Input, sub.Msg.Task.Id) || !strings.Contains(w.Input, `"task.finished"`) {
		t.Fatalf("watcher task: %+v", w)
	}
	if w.Labels["ticket"] != "T-7" || w.Labels["via"] != "watcher" || w.ConversationKey != "event:failures:T-7" {
		t.Fatalf("labels/conversation: %v %q", w.Labels, w.ConversationKey)
	}
	if ev := events(t, c, "watcher"); ev["failures:fired"] != 1 {
		t.Fatalf("trigger events: %v", ev)
	}
}

// on.count fires once with all the events that arrived within the window.
// features: triggers
func TestEventTriggerCountsWithinAWindow(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	sched := e.scheduler()
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "watcher", BundleFiles: withTriggers(t,
		`[{"type":"event","name":"burst","on":{"types":["task.queued"],"deployment":"hello","count":3,"within":"10m"}}]`)})); err != nil {
		t.Fatal(err)
	}
	sched.Step(ctx)
	submit := func() {
		if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x"})); err != nil {
			t.Fatal(err)
		}
	}
	submit()
	submit()
	sched.Step(ctx)
	if n := len(tasksOf(t, e, "watcher")); n != 0 {
		t.Fatalf("fired after 2 of 3: %d", n)
	}
	submit()
	sched.Step(ctx)
	got := tasksOf(t, e, "watcher")
	if len(got) != 1 || strings.Count(got[0].Input, `"task.queued"`) != 3 {
		t.Fatalf("one task with three events: %v", got)
	}
	submit()
	sched.Step(ctx)
	if n := len(tasksOf(t, e, "watcher")); n != 1 {
		t.Fatalf("the count starts again: %d", n)
	}
}

// Coalescing (M4, scenario S13): a trigger whose "coalesce" is set merges a
// matching event that arrives while it already has a task queued in the
// target conversation into that task, instead of firing a separate one.
// features: triggers
func TestEventTriggerCoalescesABurstIntoOneTask(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	sched := e.scheduler()
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "watcher", BundleFiles: withTriggers(t,
		`[{"type":"event","name":"burst","on":{"types":["task.queued"],"deployment":"hello"},"conversation":"fixed","coalesce":"10m"}]`)})); err != nil {
		t.Fatal(err)
	}
	sched.Step(ctx)
	submit := func() {
		if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x"})); err != nil {
			t.Fatal(err)
		}
	}

	submit()
	sched.Step(ctx)
	got := tasksOf(t, e, "watcher")
	if len(got) != 1 {
		t.Fatalf("after the first event: %d watcher tasks, want 1", len(got))
	}
	first := got[0]

	// Two more events arrive while the first watcher task is still queued:
	// both merge into it rather than firing their own.
	submit()
	submit()
	sched.Step(ctx)
	got = tasksOf(t, e, "watcher")
	if len(got) != 1 || got[0].ID != first.ID {
		t.Fatalf("a burst did not coalesce into the first task: %v", got)
	}
	if n := strings.Count(got[0].Input, `"task.queued"`); n != 3 {
		t.Fatalf("merged task should list all 3 events, found %d", n)
	}
	if ev := events(t, c, "watcher"); ev["burst:fired"] != 3 {
		t.Fatalf("trigger events (merged or not, each still fires): %v", ev)
	}
}

// A trigger never reacts to its own tasks, and a loop between triggers of
// two deployments stops after maxEventChain tasks.
// features: triggers
func TestEventTriggerLoopsStop(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	sched := e.scheduler()
	for _, pair := range [][2]string{{"ping", "pong"}, {"pong", "ping"}} {
		if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: pair[0], BundleFiles: withTriggers(t,
			`[{"type":"event","name":"bounce","on":{"types":["task.finished"],"deployment":"`+pair[1]+`"}},
			  {"type":"event","name":"self","on":{"types":["task.finished"],"deployment":"`+pair[0]+`"}}]`)})); err != nil {
			t.Fatal(err)
		}
	}
	sched.Step(ctx)
	first, _ := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "ping"), Input: "go"}))
	finishTask(t, e, "ping", first.Msg.Task.Id, true)
	// Keep finishing whatever the triggers queue.
	for range 30 {
		sched.Step(ctx)
		for _, dep := range []string{"ping", "pong"} {
			leased, err := e.store.LeaseTasks(ctx, "n1", "default", dep, 50, 60_000)
			if err != nil {
				t.Fatal(err)
			}
			for _, l := range leased {
				if err := e.store.CompleteTask(ctx, l.ID, l.LeaseID, true, "done", "", "", ""); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	total := len(tasksOf(t, e, "ping")) + len(tasksOf(t, e, "pong"))
	// Each finished task fires two (one per deployment) until the chain
	// limit: at most 2^(maxEventChain+1) tasks, and it ends.
	if total > 1<<(maxEventChain+1) || total < maxEventChain {
		t.Fatalf("%d tasks: the loop did not stop where it should", total)
	}
	rejected := 0
	for _, dep := range []string{"ping", "pong"} {
		for k, n := range events(t, c, dep) {
			if strings.HasSuffix(k, ":rejected") {
				rejected += n
			}
		}
	}
	if rejected == 0 {
		t.Fatal("no trigger stopped the loop")
	}
}

// While its provider account is held, a deployment's event triggers wait
// and fire once the hold ends.
// features: triggers
func TestEventTriggersWaitWhileHeld(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	sched := e.scheduler()
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	files := withTriggers(t, `[{"type":"event","name":"on-queued","on":{"types":["task.queued"],"deployment":"hello"}}]`)
	files["x-agen/harness.json"] = []byte(`{"provider":"openrouter","model":"deepseek/deepseek-v4-flash"}`)
	files["x-agen/secrets.json"] = []byte(`{"OPENROUTER_API_KEY":{"source":"env"}}`)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "watcher", BundleFiles: files})); err != nil {
		t.Fatal(err)
	}
	sched.Step(ctx)
	d, _ := e.store.GetDeployment(ctx, "default", "watcher")
	acct := e.hub.accountOf(ctx, d)
	if acct == "" {
		t.Fatal("no provider account for the watcher")
	}
	if _, _, err := e.store.HoldProvider(ctx, acct, "HTTP 402: no credit"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubmitTask(ctx, connect.NewRequest(&agenv1.SubmitTaskRequest{Ref: ref("", "hello"), Input: "x"})); err != nil {
		t.Fatal(err)
	}
	sched.Step(ctx)
	if n := len(tasksOf(t, e, "watcher")); n != 0 {
		t.Fatalf("fired while held: %d", n)
	}
	if _, err := e.store.ClearProviderHold(ctx, acct); err != nil {
		t.Fatal(err)
	}
	sched.Step(ctx)
	if n := len(tasksOf(t, e, "watcher")); n != 1 {
		t.Fatalf("after the hold: %d, want 1", n)
	}
}
