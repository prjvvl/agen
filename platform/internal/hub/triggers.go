package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/robfig/cron/v3"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// Triggers (docs/architecture.md §5) turn cron schedules and webhook calls
// into durable tasks, so a trigger that fires while nothing runs still waits
// in the queue. Every firing is a trigger event; a cron window the Hub was
// not running for is recorded as "missed" (or, with catch_up, the latest
// missed window fires once) — never dropped silently. Firing is exactly-once
// by construction: the task's idempotency key is the trigger and due time,
// and events are unique per trigger and due time.

// CronGrace is how late a cron window may be evaluated and still fire as
// scheduled; older windows count as missed.
var CronGrace = time.Minute

// maxCronBacklog bounds how many missed windows one tick walks through.
const maxCronBacklog = 1000

func validateTriggers(ts []*agenv1.Trigger) error {
	seen := map[string]bool{}
	for _, t := range ts {
		switch t.Type {
		case "cron":
			if _, err := cron.ParseStandard(t.Schedule); err != nil {
				return invalid(fmt.Sprintf("trigger %q: invalid cron schedule %q: %v", t.Name, t.Schedule, err))
			}
		case "webhook":
		default:
			return invalid(fmt.Sprintf("trigger %q: type must be cron or webhook", t.Name))
		}
		if t.Name == "" {
			return invalid("every trigger needs a name")
		}
		if seen[t.Name] {
			return invalid(fmt.Sprintf("trigger name %q is used twice", t.Name))
		}
		seen[t.Name] = true
	}
	return nil
}

// stampTriggers sets active_since_ms on triggers that are new (by name, type
// and schedule) and keeps it for unchanged ones.
func stampTriggers(ts, previous []*agenv1.Trigger) {
	now := store.NowMs()
	for _, t := range ts {
		t.ActiveSinceMs = now
		for _, p := range previous {
			if p.Name == t.Name && p.Type == t.Type && p.Schedule == t.Schedule && p.ActiveSinceMs > 0 {
				t.ActiveSinceMs = p.ActiveSinceMs
			}
		}
	}
}

// FireTriggers evaluates every cron trigger (leader only).
func (s *Scheduler) FireTriggers(ctx context.Context, _ int64) error {
	deps, err := s.Hub.Store.ListDeployments(ctx, "")
	if err != nil {
		return err
	}
	// Schedules are evaluated in UTC unless they start with CRON_TZ=<zone>.
	now := time.UnixMilli(store.NowMs()).UTC()
	for _, d := range deps {
		for _, t := range PolicyOf(d).Triggers {
			if t.Type != "cron" {
				continue
			}
			if err := s.fireCron(ctx, d, t, now); err != nil && ctx.Err() == nil {
				s.Log.Warn("cron trigger failed", "deployment", d.Namespace+"/"+d.Name, "trigger", t.Name, "err", err)
			}
		}
	}
	return nil
}

func (s *Scheduler) fireCron(ctx context.Context, d store.Deployment, t *agenv1.Trigger, now time.Time) error {
	sched, err := cron.ParseStandard(t.Schedule)
	if err != nil {
		return err
	}
	st := s.Hub.Store
	last, err := st.LastTriggerDue(ctx, d.Namespace, d.Name, t.Name)
	if err != nil {
		return err
	}
	// Never before the trigger existed (a trigger added by a later update,
	// or a deployment re-created under the same name).
	since := t.ActiveSinceMs
	if since == 0 {
		since = d.CreatedMs
	}
	last = max(last, since)
	var due []time.Time
	for next := sched.Next(time.UnixMilli(last).UTC()); !next.After(now) && len(due) < maxCronBacklog; next = sched.Next(next) {
		due = append(due, next)
	}
	for i, at := range due {
		latest := i == len(due)-1
		late := now.Sub(at) > CronGrace
		switch {
		case !late:
			if err := s.fire(ctx, d, t, at, ""); err != nil {
				return err
			}
		case latest && t.CatchUp:
			if err := s.fire(ctx, d, t, at, fmt.Sprintf("redelivered %s late (catch_up)", now.Sub(at).Round(time.Second))); err != nil {
				return err
			}
		default:
			if _, err := st.RecordTriggerEvent(ctx, store.TriggerEvent{Namespace: d.Namespace, Deployment: d.Name, Trigger: t.Name,
				State: "missed", DueMs: at.UnixMilli(), Message: "the Hub was not running at the due time"}); err != nil {
				return err
			}
			s.Log.Warn("cron window missed", "deployment", d.Namespace+"/"+d.Name, "trigger", t.Name, "due", at)
		}
	}
	return nil
}

func (s *Scheduler) fire(ctx context.Context, d store.Deployment, t *agenv1.Trigger, at time.Time, note string) error {
	st := s.Hub.Store
	task, err := st.SubmitTask(ctx, store.Task{Namespace: d.Namespace, Deployment: d.Name, Input: t.Input, Source: "cron:" + t.Name,
		IdempotencyKey: fmt.Sprintf("cron:%s:%d", t.Name, at.UnixMilli()), SubmittedBy: "trigger:cron:" + t.Name})
	if err != nil {
		return err
	}
	if d.Paused {
		note = strings.TrimSpace(note + " (deployment paused: the task waits)")
	}
	if _, err := st.RecordTriggerEvent(ctx, store.TriggerEvent{Namespace: d.Namespace, Deployment: d.Name, Trigger: t.Name,
		State: "fired", TaskID: task.ID, DueMs: at.UnixMilli(), Message: note}); err != nil {
		return err
	}
	_ = st.TouchActivity(ctx, d.Namespace, d.Name)
	s.Log.Info("cron trigger fired", "deployment", d.Namespace+"/"+d.Name, "trigger", t.Name, "task", task.ID)
	return nil
}

// CreateWebhookSecret creates or rotates a webhook trigger's secret.
func (h *Hub) CreateWebhookSecret(ctx context.Context, req *connect.Request[agenv1.CreateWebhookSecretRequest]) (*connect.Response[agenv1.CreateWebhookSecretResponse], error) {
	ns, name := nsOr(req.Msg.GetRef().GetNamespace()), req.Msg.GetRef().GetName()
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	d, err := h.Store.GetDeployment(ctx, ns, name)
	if err != nil {
		return nil, connectErr(err)
	}
	if webhookTrigger(d, req.Msg.Trigger) == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s/%s has no webhook trigger %q", ns, name, req.Msg.Trigger))
	}
	secret, err := h.Store.SetWebhookSecret(ctx, ns, name, req.Msg.Trigger)
	if err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.CreateWebhookSecretResponse{Secret: secret, Path: "/hooks/" + ns + "/" + name + "/" + req.Msg.Trigger}), nil
}

func webhookTrigger(d store.Deployment, name string) *agenv1.Trigger {
	for _, t := range PolicyOf(d).Triggers {
		if t.Type == "webhook" && t.Name == name {
			return t
		}
	}
	return nil
}

// WebhookHandler serves POST /hooks/<ns>/<deployment>/<trigger>: the caller
// presents the trigger's secret (Authorization: Bearer); the body (up to
// 1 MiB) becomes the task input, after the trigger's input if it has one.
// An Idempotency-Key header makes retries return the same task.
func (h *Hub) WebhookHandler() (string, http.Handler) {
	return "POST /hooks/{ns}/{dep}/{trigger}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ns, name, trig := r.PathValue("ns"), r.PathValue("dep"), r.PathValue("trigger")
		ctx := r.Context()
		reply := func(status int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(v)
		}
		d, err := h.Store.GetDeployment(ctx, ns, name)
		if err != nil || webhookTrigger(d, trig) == nil {
			reply(http.StatusNotFound, map[string]string{"error": "no such webhook"})
			return
		}
		t := webhookTrigger(d, trig)
		secret := bearer(r.Header)
		ok, err := h.Store.CheckWebhookSecret(ctx, ns, name, trig, secret)
		if err != nil {
			reply(http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if !ok {
			if h.rejectionDue(ns + "/" + name + "/" + trig) {
				h.recordWebhook(ctx, store.TriggerEvent{Namespace: ns, Deployment: name, Trigger: trig, State: "rejected",
					Message: "invalid or missing secret from " + clientIP(r) + " (further rejections within a minute are not recorded)"})
			}
			reply(http.StatusUnauthorized, map[string]string{"error": "invalid or missing webhook secret"})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			reply(http.StatusRequestEntityTooLarge, map[string]string{"error": "body too large (max 1 MiB)"})
			return
		}
		input := string(body)
		if t.Input != "" {
			input = t.Input + "\n\n" + input
		}
		key := ""
		if k := r.Header.Get("Idempotency-Key"); k != "" {
			key = "webhook:" + trig + ":" + k
		}
		task, err := h.Store.SubmitTask(ctx, store.Task{Namespace: ns, Deployment: name, Input: input, Source: "webhook:" + trig,
			IdempotencyKey: key, SubmittedBy: "trigger:webhook:" + trig})
		if err != nil {
			reply(http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		_ = h.Store.TouchActivity(ctx, ns, name)
		h.recordWebhook(ctx, store.TriggerEvent{Namespace: ns, Deployment: name, Trigger: trig, State: "fired", TaskID: task.ID})
		reply(http.StatusAccepted, map[string]string{"task_id": task.ID})
	})
}

// recordWebhook records a webhook event; events are unique per due time, so
// calls in the same millisecond get the next free one.
func (h *Hub) recordWebhook(ctx context.Context, e store.TriggerEvent) {
	e.DueMs = store.NowMs()
	for i := 0; i < 100; i++ {
		ok, err := h.Store.RecordTriggerEvent(ctx, e)
		if err != nil || ok {
			return
		}
		e.DueMs++
		e.ID = ""
	}
}

// rejectionDue limits recorded webhook rejections to one per trigger per
// minute, so unauthenticated callers cannot fill the store.
func (h *Hub) rejectionDue(key string) bool {
	h.rejMu.Lock()
	defer h.rejMu.Unlock()
	if h.rejected == nil {
		h.rejected = map[string]time.Time{}
	}
	if time.Since(h.rejected[key]) < time.Minute {
		return false
	}
	h.rejected[key] = time.Now()
	return true
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}
