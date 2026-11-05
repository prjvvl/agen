package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

func withTriggers(t *testing.T, triggers string) map[string][]byte {
	f := helloFiles(t)
	f["x-agen/config.json"] = []byte(`{"kind":"pool","scale":{"min":0,"max":2},"triggers":` + triggers + `}`)
	return f
}

func events(t *testing.T, c interface {
	ListTriggerEvents(context.Context, *connect.Request[agenv1.ListTriggerEventsRequest]) (*connect.Response[agenv1.ListTriggerEventsResponse], error)
}, name string) map[string]int {
	r, err := c.ListTriggerEvents(context.Background(), connect.NewRequest(&agenv1.ListTriggerEventsRequest{Ref: ref("", name), Limit: 1000}))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, e := range r.Msg.Events {
		out[e.Trigger+":"+strings.ToLower(strings.TrimPrefix(e.State.String(), "TRIGGER_EVENT_STATE_"))]++
	}
	return out
}

// features: triggers
func TestCronTriggersFireOnceAndRecordMissedWindows(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	defer func(old func() int64) { store.NowMs = old }(store.NowMs)

	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: withTriggers(t, `[{"type":"cron","name":"bad","schedule":"not a cron"}]`)})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid schedule accepted: %v", err)
	}
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: withTriggers(t,
		`[{"type":"cron","name":"bad","schedule":"* * * * *","overlap":"whenever"}]`)})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid overlap accepted: %v", err)
	}
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: withTriggers(t,
		`[{"type":"cron","name":"bad","schedule":"* * * * *","jitter":"not a duration"}]`)})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid jitter accepted: %v", err)
	}
	d, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: withTriggers(t,
		`[{"type":"cron","name":"tick","schedule":"* * * * *","input":"tick!"}]`)}))
	if err != nil {
		t.Fatal(err)
	}
	created := time.UnixMilli(d.Msg.Deployment.CreatedAt.AsTime().UnixMilli())
	first := created.Truncate(time.Minute).Add(time.Minute) // first window after creation
	at := func(tm time.Time) { store.NowMs = func() int64 { return tm.UnixMilli() } }
	sched := e.scheduler()
	defer sched.Wait()

	// The first window fires once, however often the leader looks.
	at(first.Add(5 * time.Second))
	for i := 0; i < 3; i++ {
		if _, err := sched.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	sched.Wait()
	tasks, _ := c.ListTasks(ctx, connect.NewRequest(&agenv1.ListTasksRequest{Deployment: "hello"}))
	if len(tasks.Msg.Tasks) != 1 || tasks.Msg.Tasks[0].Source != "cron:tick" || tasks.Msg.Tasks[0].Input != "tick!" {
		t.Fatalf("tasks after first window: %v", tasks.Msg.Tasks)
	}
	if ev := events(t, c, "hello"); ev["tick:fired"] != 1 || len(ev) != 1 {
		t.Fatalf("events: %v", ev)
	}

	// The Hub was down for 10 minutes: 9 windows are missed (recorded, not
	// run); the current window (5s late) fires on time.
	at(first.Add(10*time.Minute + 5*time.Second))
	sched.Step(ctx)
	sched.Wait()
	if ev := events(t, c, "hello"); ev["tick:missed"] != 9 || ev["tick:fired"] != 2 {
		t.Fatalf("after outage: %v", ev)
	}
	tasks, _ = c.ListTasks(ctx, connect.NewRequest(&agenv1.ListTasksRequest{Deployment: "hello", Limit: 100}))
	if len(tasks.Msg.Tasks) != 2 {
		t.Fatalf("tasks: %d (only fired windows run)", len(tasks.Msg.Tasks))
	}

	// Created now (on the test clock), so no hourly window has passed yet.
	hourly, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "hourly", BundleFiles: withTriggers(t,
		`[{"type":"cron","name":"late","schedule":"0 * * * *"},{"type":"cron","name":"catch","schedule":"0 * * * *","catchUp":true}]`)}))
	if err != nil {
		t.Fatal(err)
	}
	// Hourly triggers, Hub back 10 minutes after the 4th missed window:
	// without catch_up all 4 are missed; with catch_up the latest is
	// redelivered once and the older 3 are missed.
	h1 := time.UnixMilli(hourly.Msg.Deployment.CreatedAt.AsTime().UnixMilli()).Truncate(time.Hour).Add(time.Hour)
	at(h1.Add(3*time.Hour + 10*time.Minute))
	sched.Step(ctx)
	sched.Wait()
	sched.Step(ctx)
	sched.Wait()
	ev := events(t, c, "hourly")
	if ev["late:missed"] != 4 || ev["late:fired"] != 0 || ev["catch:missed"] != 3 || ev["catch:fired"] != 1 {
		t.Fatalf("catch_up: %v", ev)
	}
	r, _ := c.ListTriggerEvents(ctx, connect.NewRequest(&agenv1.ListTriggerEventsRequest{Ref: ref("", "hourly"), State: agenv1.TriggerEventState_TRIGGER_EVENT_STATE_FIRED}))
	if len(r.Msg.Events) != 1 || !strings.Contains(r.Msg.Events[0].Message, "redelivered") || r.Msg.Events[0].TaskId == "" {
		t.Fatalf("redelivered: %v", r.Msg.Events)
	}
}

// A cron trigger's jitter delays its firing by a deterministic amount
// within [0, jitter) after the window is due; re-evaluating before that
// offset elapses neither fires nor misses it.
// features: triggers
func TestCronJitterDelaysFiring(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	defer func(old func() int64) { store.NowMs = old }(store.NowMs)

	d, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "jit", BundleFiles: withTriggers(t,
		`[{"type":"cron","name":"tick","schedule":"* * * * *","jitter":"10s"}]`)}))
	if err != nil {
		t.Fatal(err)
	}
	created := time.UnixMilli(d.Msg.Deployment.CreatedAt.AsTime().UnixMilli())
	first := created.Truncate(time.Minute).Add(time.Minute)
	offset := jitterOffset("default", "jit", "tick", first, 10*time.Second)
	at := func(tm time.Time) { store.NowMs = func() int64 { return tm.UnixMilli() } }
	sched := e.scheduler()
	defer sched.Wait()

	// Before the jittered instant: not fired, not missed either.
	at(first.Add(offset - time.Second))
	sched.Step(ctx)
	sched.Wait()
	if ev := events(t, c, "jit"); len(ev) != 0 {
		t.Fatalf("before the jittered instant: %v", ev)
	}
	// At and after it: fires.
	at(first.Add(offset + time.Second))
	sched.Step(ctx)
	sched.Wait()
	if ev := events(t, c, "jit"); ev["tick:fired"] != 1 || len(ev) != 1 {
		t.Fatalf("after the jittered instant: %v", ev)
	}
}

// overlap "skip" refuses to fire while an earlier firing's task is still
// unfinished; "buffer_one" allows at most one more on top of one.
// features: triggers
func TestCronOverlapPolicies(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	defer func(old func() int64) { store.NowMs = old }(store.NowMs)

	d, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "ov", BundleFiles: withTriggers(t,
		`[{"type":"cron","name":"solo","schedule":"* * * * *","overlap":"skip"},`+
			`{"type":"cron","name":"pair","schedule":"* * * * *","overlap":"buffer_one"}]`)}))
	if err != nil {
		t.Fatal(err)
	}
	first := time.UnixMilli(d.Msg.Deployment.CreatedAt.AsTime().UnixMilli()).Truncate(time.Minute).Add(time.Minute)
	at := func(tm time.Time) { store.NowMs = func() int64 { return tm.UnixMilli() } }
	sched := e.scheduler()
	defer sched.Wait()

	at(first.Add(5 * time.Second))
	sched.Step(ctx)
	sched.Wait()
	at(first.Add(65 * time.Second))
	sched.Step(ctx)
	sched.Wait()
	at(first.Add(125 * time.Second))
	sched.Step(ctx)
	sched.Wait()
	ev := events(t, c, "ov")
	if ev["solo:fired"] != 1 || ev["solo:skipped"] != 2 {
		t.Fatalf("skip: %v", ev)
	}
	if ev["pair:fired"] != 2 || ev["pair:skipped"] != 1 {
		t.Fatalf("buffer_one: %v", ev)
	}
}

// min_start_spacing keeps a fired window at least that long after the
// previous firing's, skipping (not missing) a window that comes sooner.
// features: triggers
func TestCronMinStartSpacing(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	defer func(old func() int64) { store.NowMs = old }(store.NowMs)

	d, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "sp", BundleFiles: withTriggers(t,
		`[{"type":"cron","name":"tick","schedule":"* * * * *","minStartSpacing":"2m"}]`)}))
	if err != nil {
		t.Fatal(err)
	}
	first := time.UnixMilli(d.Msg.Deployment.CreatedAt.AsTime().UnixMilli()).Truncate(time.Minute).Add(time.Minute)
	at := func(tm time.Time) { store.NowMs = func() int64 { return tm.UnixMilli() } }
	sched := e.scheduler()
	defer sched.Wait()

	at(first.Add(5 * time.Second)) // window 1: fires, nothing fired before it
	sched.Step(ctx)
	sched.Wait()
	at(first.Add(65 * time.Second)) // window 2, 1m after window 1: too soon
	sched.Step(ctx)
	sched.Wait()
	at(first.Add(125 * time.Second)) // window 3, 2m after window 1: spaced enough
	sched.Step(ctx)
	sched.Wait()
	if ev := events(t, c, "sp"); ev["tick:fired"] != 2 || ev["tick:skipped"] != 1 {
		t.Fatalf("min_start_spacing: %v", ev)
	}
}

// catch_up_window bounds how late the latest missed window may be and
// still be caught up; beyond it, every window of the backlog is missed.
// features: triggers
func TestCronCatchUpWindow(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	defer func(old func() int64) { store.NowMs = old }(store.NowMs)

	d, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{Name: "cw", BundleFiles: withTriggers(t,
		`[{"type":"cron","name":"tight","schedule":"0 * * * *","catchUp":true,"catchUpWindow":"5m"},`+
			`{"type":"cron","name":"loose","schedule":"0 * * * *","catchUp":true,"catchUpWindow":"15m"}]`)}))
	if err != nil {
		t.Fatal(err)
	}
	h1 := time.UnixMilli(d.Msg.Deployment.CreatedAt.AsTime().UnixMilli()).Truncate(time.Hour).Add(time.Hour)
	at := func(tm time.Time) { store.NowMs = func() int64 { return tm.UnixMilli() } }
	sched := e.scheduler()
	defer sched.Wait()

	// The leader comes back 10 minutes after the 4th missed window: beyond
	// "tight"'s 5m catch-up window (all 4 missed), within "loose"'s 15m
	// (the latest redelivered, the older 3 missed).
	at(h1.Add(3*time.Hour + 10*time.Minute))
	sched.Step(ctx)
	sched.Wait()
	ev := events(t, c, "cw")
	if ev["tight:missed"] != 4 || ev["tight:fired"] != 0 {
		t.Fatalf("tight catch_up_window: %v", ev)
	}
	if ev["loose:missed"] != 3 || ev["loose:fired"] != 1 {
		t.Fatalf("loose catch_up_window: %v", ev)
	}
}

// Cron tasks are always trusted; a webhook trigger's tasks default to
// untrusted, or whatever trust it names.
// features: triggers
func TestCronAndWebhookTrust(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: withTriggers(t,
		`[{"type":"cron","name":"tick","schedule":"* * * * *"},`+
			`{"type":"webhook","name":"plain"},`+
			`{"type":"webhook","name":"trusted-hook","trust":"trusted"}]`)})); err != nil {
		t.Fatal(err)
	}
	post := func(path, secret, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, e.url+path, bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	secretFor := func(trigger string) string {
		r, err := c.CreateWebhookSecret(ctx, connect.NewRequest(&agenv1.CreateWebhookSecretRequest{Ref: ref("", "hello"), Trigger: trigger}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg.Secret
	}
	var out struct {
		TaskID string `json:"task_id"`
	}
	resp := post("/hooks/default/hello/plain", secretFor("plain"), "x")
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	plain, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: out.TaskID}))
	if err != nil || plain.Msg.Task.Trust != "untrusted" {
		t.Fatalf("plain webhook: %v %v", plain, err)
	}
	resp2 := post("/hooks/default/hello/trusted-hook", secretFor("trusted-hook"), "x")
	json.NewDecoder(resp2.Body).Decode(&out)
	resp2.Body.Close()
	trustedHook, err := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: out.TaskID}))
	if err != nil || trustedHook.Msg.Task.Trust != "trusted" {
		t.Fatalf("trusted webhook: %v %v", trustedHook, err)
	}

	created := time.UnixMilli(0)
	d, _ := c.GetDeployment(ctx, connect.NewRequest(&agenv1.GetDeploymentRequest{Ref: ref("", "hello")}))
	created = time.UnixMilli(d.Msg.Deployment.CreatedAt.AsTime().UnixMilli())
	first := created.Truncate(time.Minute).Add(time.Minute)
	defer func(old func() int64) { store.NowMs = old }(store.NowMs)
	store.NowMs = func() int64 { return first.Add(5 * time.Second).UnixMilli() }
	hookSched := e.scheduler()
	hookSched.Step(ctx)
	hookSched.Wait()
	tasks, err := c.ListTasks(ctx, connect.NewRequest(&agenv1.ListTasksRequest{Deployment: "hello", Limit: 100}))
	if err != nil {
		t.Fatal(err)
	}
	var cronTask *agenv1.Task
	for _, tk := range tasks.Msg.Tasks {
		if tk.Source == "cron:tick" {
			cronTask = tk
		}
	}
	if cronTask == nil || cronTask.Trust != "trusted" {
		t.Fatalf("cron task trust: %v", cronTask)
	}
}

// features: triggers
func TestWebhookTriggers(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: withTriggers(t,
		`[{"type":"webhook","name":"deploy","input":"A deploy happened:"}]`)})); err != nil {
		t.Fatal(err)
	}
	post := func(path, secret, key, body string) (int, map[string]string) {
		req, _ := http.NewRequest(http.MethodPost, e.url+path, bytes.NewBufferString(body))
		if secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]string
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if st, _ := post("/hooks/default/hello/deploy", "", "", "x"); st != http.StatusUnauthorized {
		t.Fatalf("no secret: %d", st)
	}
	viewer, _ := c.CreateApiToken(ctx, connect.NewRequest(&agenv1.CreateApiTokenRequest{Name: "v", Scopes: []string{ScopeViewer}}))
	if _, err := e.client(viewer.Msg.Secret).CreateWebhookSecret(ctx, connect.NewRequest(&agenv1.CreateWebhookSecretRequest{Ref: ref("", "hello"), Trigger: "deploy"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer created secret: %v", err)
	}
	if _, err := c.CreateWebhookSecret(ctx, connect.NewRequest(&agenv1.CreateWebhookSecretRequest{Ref: ref("", "hello"), Trigger: "nope"})); code(err) != connect.CodeNotFound {
		t.Fatalf("unknown trigger: %v", err)
	}
	sec, err := c.CreateWebhookSecret(ctx, connect.NewRequest(&agenv1.CreateWebhookSecretRequest{Ref: ref("", "hello"), Trigger: "deploy"}))
	if err != nil || sec.Msg.Path != "/hooks/default/hello/deploy" {
		t.Fatalf("secret: %v %v", sec, err)
	}
	if st, _ := post(sec.Msg.Path, "agen_hook_wrong", "", "x"); st != http.StatusUnauthorized {
		t.Fatalf("wrong secret: %d", st)
	}
	st, out := post(sec.Msg.Path, sec.Msg.Secret, "evt-1", `{"sha":"abc"}`)
	if st != http.StatusAccepted || out["task_id"] == "" {
		t.Fatalf("accepted: %d %v", st, out)
	}
	if _, again := post(sec.Msg.Path, sec.Msg.Secret, "evt-1", `{"sha":"abc"}`); again["task_id"] != out["task_id"] {
		t.Fatalf("idempotent retry: %v vs %v", again, out)
	}
	tk, _ := c.GetTask(ctx, connect.NewRequest(&agenv1.GetTaskRequest{Id: out["task_id"]}))
	if tk.Msg.Task.Input != "A deploy happened:\n\n{\"sha\":\"abc\"}" || tk.Msg.Task.Source != "webhook:deploy" {
		t.Fatalf("task: %v", tk.Msg.Task)
	}
	if st, _ := post("/hooks/default/hello/other", sec.Msg.Secret, "", "x"); st != http.StatusNotFound {
		t.Fatalf("unknown hook: %d", st)
	}
	// A rotated secret replaces the old one.
	sec2, _ := c.CreateWebhookSecret(ctx, connect.NewRequest(&agenv1.CreateWebhookSecretRequest{Ref: ref("", "hello"), Trigger: "deploy"}))
	if st, _ := post(sec.Msg.Path, sec.Msg.Secret, "", "x"); st != http.StatusUnauthorized {
		t.Fatalf("old secret after rotation: %d", st)
	}
	if st, _ := post(sec.Msg.Path, sec2.Msg.Secret, "", "x"); st != http.StatusAccepted {
		t.Fatalf("new secret: %d", st)
	}
	// Rejections are recorded at most once a minute per trigger.
	if ev := events(t, c, "hello"); ev["deploy:rejected"] != 1 || ev["deploy:fired"] != 3 {
		t.Fatalf("events: %v", ev)
	}
}

// A cron trigger added by a later update, or a deployment re-created under
// the same name, does not back-fill "missed" windows; rm drops webhook
// secrets.
// features: triggers
func TestTriggersStartWhenAddedAndRmDropsSecrets(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	defer func(old func() int64) { store.NowMs = old }(store.NowMs)
	base := time.Now().Truncate(time.Minute).Add(10 * time.Second)
	at := func(tm time.Time) { store.NowMs = func() int64 { return tm.UnixMilli() } }
	at(base)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: withTriggers(t, `[]`)})); err != nil {
		t.Fatal(err)
	}
	sched := e.scheduler()
	defer sched.Wait()
	at(base.Add(30 * time.Minute))
	if _, err := c.UpdateDeployment(ctx, connect.NewRequest(&agenv1.UpdateDeploymentRequest{Ref: ref("", "hello"),
		Triggers: []*agenv1.Trigger{{Type: "cron", Name: "tick", Schedule: "* * * * *"}, {Type: "webhook", Name: "hook"}}})); err != nil {
		t.Fatal(err)
	}
	sched.Step(ctx)
	sched.Wait()
	if ev := events(t, c, "hello"); len(ev) != 0 {
		t.Fatalf("back-filled windows from before the trigger existed: %v", ev)
	}
	at(base.Add(31*time.Minute - 5*time.Second))
	sched.Step(ctx)
	sched.Wait()
	if ev := events(t, c, "hello"); ev["tick:fired"] != 1 || ev["tick:missed"] != 0 {
		t.Fatalf("first window after adding: %v", ev)
	}
	// Unchanged triggers keep their start on later updates.
	c.UpdateDeployment(ctx, connect.NewRequest(&agenv1.UpdateDeploymentRequest{Ref: ref("", "hello"), Scale: &agenv1.ScalePolicy{Min: 0, Max: 3}}))
	d, _ := c.GetDeployment(ctx, connect.NewRequest(&agenv1.GetDeploymentRequest{Ref: ref("", "hello")}))
	for _, tr := range d.Msg.Deployment.Triggers {
		if tr.ActiveSinceMs != base.Add(30*time.Minute).UnixMilli() {
			t.Fatalf("active_since changed: %v", tr)
		}
	}

	sec, err := c.CreateWebhookSecret(ctx, connect.NewRequest(&agenv1.CreateWebhookSecretRequest{Ref: ref("", "hello"), Trigger: "hook"}))
	if err != nil {
		t.Fatal(err)
	}
	c.DeleteDeployment(ctx, connect.NewRequest(&agenv1.DeleteDeploymentRequest{Ref: ref("", "hello")}))
	c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: withTriggers(t, `[{"type":"webhook","name":"hook"}]`)}))
	req, _ := http.NewRequest(http.MethodPost, e.url+sec.Msg.Path, bytes.NewBufferString("x"))
	req.Header.Set("Authorization", "Bearer "+sec.Msg.Secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old secret after rm and re-create: %d", resp.StatusCode)
	}
}

// plannedRandomTimes is deterministic (the same trigger plans the same
// times for the same day, so showing them before they're due and firing
// them later always agree), spaced at least minGap apart, and stays
// within its window. A different trigger plans different times (2.6).
func TestPlannedRandomTimesAreDeterministicSpacedAndWithinWindow(t *testing.T) {
	day := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	from, to, err := parseWindow("02:00-06:00")
	if err != nil {
		t.Fatal(err)
	}
	const minGap = 30 * time.Minute
	a := plannedRandomTimes("default", "hello", "nightly", day, 3, from, to, minGap)
	b := plannedRandomTimes("default", "hello", "nightly", day, 3, from, to, minGap)
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("got %d and %d times, want 3 each: %v %v", len(a), len(b), a, b)
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			t.Fatalf("not deterministic: %v vs %v", a, b)
		}
		if at := a[i]; at.Before(day.Add(from)) || !at.Before(day.Add(to)) {
			t.Fatalf("time %v outside window %s-%s", at, day.Add(from), day.Add(to))
		}
		if i > 0 && a[i].Sub(a[i-1]) < minGap {
			t.Fatalf("times %v and %v are less than minGap apart", a[i-1], a[i])
		}
	}
	if other := plannedRandomTimes("default", "hello", "other", day, 3, from, to, minGap); other[0].Equal(a[0]) {
		t.Fatal("two different triggers planned the exact same first time")
	}
}

// A random trigger's count/window/minGap/zone/days are validated at
// CreateDeployment (2.6).
// features: triggers
func TestCreateDeploymentValidatesRandomTrigger(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	bad := []string{
		`[{"type":"random","name":"r","count":0,"window":"02:00-06:00"}]`,
		`[{"type":"random","name":"r","count":1,"window":"not-a-window"}]`,
		`[{"type":"random","name":"r","count":1,"window":"06:00-02:00"}]`,
		`[{"type":"random","name":"r","count":1,"window":"02:00-06:00","minGap":"not-a-duration"}]`,
		`[{"type":"random","name":"r","count":1,"window":"02:00-06:00","zone":"Nowhere/Nothing"}]`,
		`[{"type":"random","name":"r","count":1,"window":"02:00-06:00","days":["someday"]}]`,
	}
	for _, triggers := range bad {
		if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: withTriggers(t, triggers)})); code(err) != connect.CodeInvalidArgument {
			t.Fatalf("accepted: %s: %v", triggers, err)
		}
	}
	dep, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: withTriggers(t,
		`[{"type":"random","name":"nightly","count":2,"window":"02:00-06:00","minGap":"30m","zone":"America/New_York","days":["mon","wed","fri"]}]`)}))
	if err != nil || dep.Msg.Deployment.Triggers[0].Count != 2 {
		t.Fatalf("create with a valid random trigger: %v %v", dep, err)
	}
}

// A random trigger fires exactly its planned times for a day once each
// (idempotency-key dedup, the same mechanism cron relies on), never more
// no matter how many ticks re-evaluate the same day (2.6). Uses
// Scheduler.fireRandom directly with a controlled `now`/trigger so the
// test does not depend on the real wall-clock time of day.
// features: triggers
func TestRandomTriggerFiresEachPlannedTimeOnceAndRecordsThem(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	d, err := e.store.GetDeployment(ctx, "default", "hello")
	if err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC()
	// Midnight today: excludes every one of yesterday's planned times
	// (which fireRandom also considers) while still including every
	// possible planned time of today, regardless of when today the
	// deployment (and so, by default, the trigger) was actually created
	// a moment ago.
	since := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()
	trig := &agenv1.Trigger{Type: "random", Name: "nightly", Count: 3, Window: "00:00-23:59", MinGap: "1h", ActiveSinceMs: since}
	from, to, err := parseWindow(trig.Window)
	if err != nil {
		t.Fatal(err)
	}
	planned := plannedRandomTimes(d.Namespace, d.Name, trig.Name, day, trig.Count, from, to, time.Hour)
	if len(planned) != 3 {
		t.Fatalf("planned %d times, want 3: %v", len(planned), planned)
	}
	sched := e.scheduler()
	defer sched.Wait()
	// Late in today's own calendar date (not tomorrow's: fireRandom reads
	// "yesterday" and "today" off now's own date), after every planned
	// time the "00:00-23:59" window could produce.
	now := time.Date(day.Year(), day.Month(), day.Day(), 23, 59, 59, 0, time.UTC)
	if err := sched.fireRandom(ctx, d, trig, now); err != nil {
		t.Fatal(err)
	}
	got := tasksOf(t, e, "hello")
	if len(got) != len(planned) {
		t.Fatalf("got %d tasks, want %d (one per planned time): %v", len(got), len(planned), got)
	}
	if ev := events(t, c, "hello"); ev["nightly:fired"] != len(planned) {
		t.Fatalf("trigger events: %v, want %d fired", ev, len(planned))
	}
	// A later tick re-evaluating the same day does not duplicate.
	if err := sched.fireRandom(ctx, d, trig, now); err != nil {
		t.Fatal(err)
	}
	if got2 := tasksOf(t, e, "hello"); len(got2) != len(planned) {
		t.Fatalf("re-firing duplicated: got %d tasks, want %d", len(got2), len(planned))
	}
}

// A random trigger never fires on a day its `days` excludes (2.6).
func TestRandomTriggerSkipsExcludedDays(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.client(admin).CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: helloFiles(t)})); err != nil {
		t.Fatal(err)
	}
	d, err := e.store.GetDeployment(ctx, "default", "hello")
	if err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC()
	since := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()
	today := [...]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}[day.Weekday()]
	trig := &agenv1.Trigger{Type: "random", Name: "weekday-only", Count: 1, Window: "00:00-23:59", Days: []string{today}, ActiveSinceMs: since}
	sched := e.scheduler()
	defer sched.Wait()
	now := time.Date(day.Year(), day.Month(), day.Day(), 23, 59, 59, 0, time.UTC)
	if err := sched.fireRandom(ctx, d, trig, now); err != nil {
		t.Fatal(err)
	}
	if got := tasksOf(t, e, "hello"); len(got) != 1 {
		t.Fatalf("today (an allowed day) should have fired once: %v", got)
	}
	trig2 := &agenv1.Trigger{Type: "random", Name: "never-today", Count: 1, Window: "00:00-23:59", ActiveSinceMs: since,
		Days: func() []string {
			var out []string
			for _, dd := range [...]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"} {
				if dd != today {
					out = append(out, dd)
				}
			}
			return out
		}()}
	if err := sched.fireRandom(ctx, d, trig2, now); err != nil {
		t.Fatal(err)
	}
	if got := tasksOf(t, e, "hello"); len(got) != 1 {
		t.Fatalf("today (an excluded day) should not have fired: %v", got)
	}
}
