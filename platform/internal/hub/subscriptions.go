package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// DefaultTargetEvents are what a notification target without events gets
// (what targets got before the event log).
var DefaultTargetEvents = []string{"approval.pending", "task.failed", "budget.exhausted", "provider.held", "provider.resumed"}

// warningEvery is how often the leader records activity warnings.
const warningEvery = 30 * time.Second

// connectRequest wraps a request message for an internal call.
func connectRequest[T any](m *T) *connect.Request[T] { return connect.NewRequest(m) }

// Notification delivery: how long one event is retried before it is
// dropped, and the longest wait between tries.
const (
	targetGiveUpAfter = time.Hour
	targetMaxBackoff  = 5 * time.Minute
)

// targetEventName is the name under which target t receives e ("" = not
// at all). Besides log event types (or prefixes ending in "."), targets
// take two names of their own: task.failed (a task.finished that failed)
// and budget.exhausted (budget.threshold at 100%).
func targetEventName(events []string, e store.Event) string {
	if len(events) == 0 {
		events = DefaultTargetEvents
	}
	for _, want := range events {
		switch {
		case want == "task.failed":
			if e.Type == "task.finished" && e.Data["state"] == "failed" {
				return want
			}
		case want == "budget.exhausted":
			if e.Type == "budget.threshold" && fmt.Sprint(e.Data["percent"]) == "100" {
				return want
			}
		case want == e.Type, strings.HasSuffix(want, ".") && strings.HasPrefix(e.Type, want):
			return e.Type
		}
	}
	return ""
}

// ValidTargetEvent reports whether name can be asked for by a target.
func ValidTargetEvent(name string) bool {
	if name == "task.failed" || name == "budget.exhausted" {
		return true
	}
	if strings.HasSuffix(name, ".") {
		return slices.ContainsFunc(EventTypes, func(t string) bool { return strings.HasPrefix(t, name) })
	}
	return slices.Contains(EventTypes, name)
}

// EventTypes are the types the event log holds.
var EventTypes = []string{"task.queued", "task.started", "task.finished", "task.needs_input", "task.stuck",
	"approval.pending", "approval.decided", "budget.threshold", "provider.held", "provider.resumed", "activity.warning",
	"trigger.missed", "webhook.rejected", "deployment.created", "deployment.updated", "deployment.deleted"}

// targetBody is what a target receives for e: the type, the event, and the
// object it is about in its API shape (task, approval, deployment or hold).
func (h *Hub) targetBody(ctx context.Context, name string, e store.Event) []byte {
	body := map[string]any{"type": name, "event": eventJSON(e)}
	var field string
	var msg proto.Message
	switch {
	case strings.HasPrefix(e.Type, "task.") && e.TaskID != "":
		if t, err := h.Store.GetTask(ctx, e.TaskID); err == nil {
			field, msg = "task", TaskProto(t)
		}
	case strings.HasPrefix(e.Type, "approval."):
		if a, err := h.Store.GetApproval(ctx, fmt.Sprint(e.Data["approvalId"])); err == nil {
			field, msg = "approval", h.approvalProto(a, h.principalNames(ctx))
		}
	case strings.HasPrefix(e.Type, "provider."):
		field, msg = "hold", &agenv1.ProviderHold{Account: fmt.Sprint(e.Data["account"]), Reason: fmt.Sprint(e.Data["reason"])}
	case e.Deployment != "" && (strings.HasPrefix(e.Type, "budget.") || strings.HasPrefix(e.Type, "deployment.")):
		if d, err := h.Store.GetDeployment(ctx, e.Namespace, e.Deployment); err == nil {
			field, msg = "deployment", h.withBudget(ctx, d, deploymentProto(d, h.ready(ctx, d.Namespace, d.Name)))
		}
	}
	if msg != nil {
		if raw, err := protojson.Marshal(msg); err == nil {
			body[field] = json.RawMessage(raw)
		}
	}
	b, _ := json.Marshal(body)
	return b
}

// deliverTargets sends each notification target the events after its
// cursor, in order (leader, every tick). A failed delivery is tried again
// with a growing pause; one that keeps failing for targetGiveUpAfter is
// dropped (counted on the target) so the target moves on.
func (h *Hub) deliverTargets(ctx context.Context) {
	if !h.delivering.CompareAndSwap(false, true) {
		return // the previous round is still going
	}
	defer h.delivering.Store(false)
	last, err := h.Store.LastEventSeq(ctx)
	if err != nil {
		return
	}
	_ = h.Store.StartTargetsAt(ctx, last)
	targets, err := h.Store.ListNotificationTargets(ctx, "")
	if err != nil {
		return
	}
	now := store.NowMs()
	var wg sync.WaitGroup
	for _, t := range targets {
		if t.CursorSeq < 0 || t.RetryMs > now || t.CursorSeq >= last {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.deliverTarget(ctx, t)
		}()
	}
	wg.Wait()
}

func (h *Hub) deliverTarget(ctx context.Context, t store.NotificationTarget) {
	events, next, err := h.Store.ListEvents(ctx, store.EventFilter{Namespaces: []string{t.Namespace}, AfterSeq: t.CursorSeq,
		Deployment: t.Filter.Deployment, Labels: t.Filter.Labels, Severity: t.Filter.Severity, Limit: 50})
	if err != nil {
		return
	}
	key, err := h.Store.NotificationSecret(ctx, t.Namespace, t.Name)
	if err != nil {
		return
	}
	cursor := t.CursorSeq
	for _, e := range events {
		name := targetEventName(t.Events, e)
		if name == "" {
			continue
		}
		err := postSigned(ctx, t.URL, name, h.targetBody(ctx, name, e), key, 1)
		if err == nil {
			if h.Store.TargetDelivered(ctx, t.Namespace, t.Name, cursor, e.Seq, false) != nil {
				return // replaced or moved meanwhile
			}
			cursor = e.Seq
			t.FailingSinceMs = 0
			continue
		}
		now := store.NowMs()
		if t.FailingSinceMs > 0 && now-t.FailingSinceMs > targetGiveUpAfter.Milliseconds() {
			_ = h.Store.AppendLog(ctx, "", t.Namespace, e.Deployment, "error",
				fmt.Sprintf("%s (event %d): notification to %s (%s) dropped after failing for %s: %s", name, e.Seq, t.Name, t.URL, human(now-t.FailingSinceMs), err))
			_ = h.Store.TargetDelivered(ctx, t.Namespace, t.Name, cursor, e.Seq, true)
			return
		}
		failing := int64(0)
		if t.FailingSinceMs > 0 {
			failing = now - t.FailingSinceMs
		}
		wait := min(max(failing, 2000), targetMaxBackoff.Milliseconds())
		_ = h.Store.TargetFailed(ctx, t.Namespace, t.Name, err.Error(), now+wait)
		if t.FailingSinceMs == 0 {
			_ = h.Store.AppendLog(ctx, "", t.Namespace, e.Deployment, "warn", fmt.Sprintf("%s: notification to %s (%s) failed: %s; retrying", name, t.Name, t.URL, err))
		}
		return
	}
	// Past what did not match: the cursor moves on.
	if next > cursor {
		_ = h.Store.TargetDelivered(ctx, t.Namespace, t.Name, cursor, next, false)
	}
}

// budgetThresholds records budget.threshold at 50, 80 and 100% of a
// deployment's daily budget, each once per UTC day.
func (h *Hub) budgetThresholds(ctx context.Context, d store.Deployment) {
	limit := PolicyOf(d).Budget.MaxUsdPerDay
	if limit <= 0 {
		return
	}
	spent, _, err := h.spentToday(ctx, d)
	if err != nil {
		return
	}
	day := time.UnixMilli(dayStartMs()).UTC().Format(time.DateOnly)
	for _, pct := range []int{50, 80, 100} {
		if spent < limit*float64(pct)/100 {
			break
		}
		severity := map[int]string{50: "info", 80: "warning", 100: "critical"}[pct]
		_, _ = h.Store.AppendEventOnce(ctx, store.Event{Namespace: d.Namespace, Type: "budget.threshold", Deployment: d.Name, Severity: severity,
			Data:      map[string]any{"percent": pct, "scope": "deployment", "spentUsd": spent, "limitUsd": limit, "day": day},
			DedupeKey: fmt.Sprintf("budget:%s/%s:%s:%d", d.Namespace, d.Name, day, pct)})
	}
}

// recordWarnings turns GetActivity's warnings into activity.warning events
// (each kind once per subject and UTC day) and runs that went quiet into
// task.stuck (leader, every warningEvery).
func (h *Hub) recordWarnings(ctx context.Context) {
	r, err := h.GetActivity(WithPrincipal(ctx, Principal{ID: "hub", Scopes: map[string]bool{ScopeAdmin: true}}), connectRequest(&agenv1.GetActivityRequest{}))
	if err != nil {
		return
	}
	day := time.Now().UTC().Format(time.DateOnly)
	tasks := map[string]string{}
	for _, run := range r.Msg.Runs {
		tasks[run.RunId] = run.TaskId
	}
	for _, w := range r.Msg.Warnings {
		ns, dep := w.GetRef().GetNamespace(), w.GetRef().GetName()
		_, _ = h.Store.AppendEventOnce(ctx, store.Event{Namespace: ns, Type: "activity.warning", Deployment: dep, TaskID: tasks[w.RunId], Severity: w.Severity,
			Data:      map[string]any{"kind": w.Kind, "message": w.Message, "runId": w.RunId},
			DedupeKey: fmt.Sprintf("warn:%s:%s/%s:%s:%s", w.Kind, ns, dep, w.RunId, day)})
		if w.Kind == "quiet_run" && tasks[w.RunId] != "" {
			_, _ = h.Store.AppendEventOnce(ctx, store.Event{Namespace: ns, Type: "task.stuck", Deployment: dep, TaskID: tasks[w.RunId], Severity: "warning",
				Data:      map[string]any{"runId": w.RunId, "message": w.Message},
				DedupeKey: fmt.Sprintf("stuck:%s:%s", w.RunId, day)})
		}
	}
}
