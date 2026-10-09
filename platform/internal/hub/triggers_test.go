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

func TestCronTriggersFireOnceAndRecordMissedWindows(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)
	defer func(old func() int64) { store.NowMs = old }(store.NowMs)

	if _, err := c.CreateDeployment(ctx, connect.NewRequest(&agenv1.CreateDeploymentRequest{BundleFiles: withTriggers(t, `[{"type":"cron","name":"bad","schedule":"not a cron"}]`)})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid schedule accepted: %v", err)
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

	// The first window fires once, however often the leader looks.
	at(first.Add(5 * time.Second))
	for i := 0; i < 3; i++ {
		if _, err := sched.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
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
	sched.Step(ctx)
	ev := events(t, c, "hourly")
	if ev["late:missed"] != 4 || ev["late:fired"] != 0 || ev["catch:missed"] != 3 || ev["catch:fired"] != 1 {
		t.Fatalf("catch_up: %v", ev)
	}
	r, _ := c.ListTriggerEvents(ctx, connect.NewRequest(&agenv1.ListTriggerEventsRequest{Ref: ref("", "hourly"), State: agenv1.TriggerEventState_TRIGGER_EVENT_STATE_FIRED}))
	if len(r.Msg.Events) != 1 || !strings.Contains(r.Msg.Events[0].Message, "redelivered") || r.Msg.Events[0].TaskId == "" {
		t.Fatalf("redelivered: %v", r.Msg.Events)
	}
}

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
	at(base.Add(30 * time.Minute))
	if _, err := c.UpdateDeployment(ctx, connect.NewRequest(&agenv1.UpdateDeploymentRequest{Ref: ref("", "hello"),
		Triggers: []*agenv1.Trigger{{Type: "cron", Name: "tick", Schedule: "* * * * *"}, {Type: "webhook", Name: "hook"}}})); err != nil {
		t.Fatal(err)
	}
	sched.Step(ctx)
	if ev := events(t, c, "hello"); len(ev) != 0 {
		t.Fatalf("back-filled windows from before the trigger existed: %v", ev)
	}
	at(base.Add(31*time.Minute - 5*time.Second))
	sched.Step(ctx)
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
