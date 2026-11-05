package hub

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// maxEventChain stops loops of event triggers: a task that event triggers
// led to this many times in a row does not fire another.
const maxEventChain = 5

var conversationVar = regexp.MustCompile(`\{\{\s*event\.([A-Za-z0-9_.]+)\s*\}\}`)

func validateEventTrigger(t *agenv1.Trigger) error {
	m := t.On
	if m == nil || len(m.Types) == 0 {
		return fmt.Errorf("on.types needs at least one event type")
	}
	for _, typ := range m.Types {
		if typ == "task.failed" || typ == "budget.exhausted" || !ValidTargetEvent(typ) {
			return fmt.Errorf("on.types: unknown event type %q (known: %s, or a prefix such as task.)", typ, strings.Join(EventTypes, ", "))
		}
	}
	if m.Severity != "" && m.Severity != "info" && m.Severity != "warning" && m.Severity != "critical" {
		return fmt.Errorf("on.severity must be info, warning or critical")
	}
	if m.Count > 1 {
		if d, err := time.ParseDuration(m.Within); err != nil || d <= 0 {
			return fmt.Errorf("on.count needs on.within, a duration such as 10m")
		}
	}
	if t.Coalesce != "" {
		if d, err := time.ParseDuration(t.Coalesce); err != nil || d <= 0 {
			return fmt.Errorf("coalesce %q: a duration such as 30s", t.Coalesce)
		}
	}
	for _, v := range conversationVar.FindAllStringSubmatch(t.Conversation, -1) {
		switch k := v[1]; {
		case k == "taskId", k == "parentTaskId", k == "deployment", k == "type", strings.HasPrefix(k, "labels."):
		default:
			return fmt.Errorf("conversation: unknown {{event.%s}} (use taskId, parentTaskId, deployment, type or labels.<key>)", k)
		}
	}
	return nil
}

// FireEventTriggers runs every event trigger over the events since its
// cursor (leader only). A new trigger starts at the newest event. A
// deployment whose provider account is held keeps its events until the hold
// ends.
func (s *Scheduler) FireEventTriggers(ctx context.Context) error {
	deps, err := s.Hub.Store.ListDeployments(ctx, "")
	if err != nil {
		return err
	}
	for _, d := range deps {
		for _, t := range PolicyOf(d).Triggers {
			if t.Type != "event" {
				continue
			}
			if err := s.fireEvents(ctx, d, t); err != nil && ctx.Err() == nil {
				s.Log.Warn("event trigger failed", "deployment", d.Namespace+"/"+d.Name, "trigger", t.Name, "err", err)
			}
		}
	}
	return nil
}

func (s *Scheduler) fireEvents(ctx context.Context, d store.Deployment, t *agenv1.Trigger) error {
	st := s.Hub.Store
	cur, found, err := st.GetTriggerCursor(ctx, d.Namespace, d.Name, t.Name)
	if err != nil {
		return err
	}
	if !found {
		last, err := st.LastEventSeq(ctx)
		if err != nil {
			return err
		}
		return st.SetTriggerCursor(ctx, d.Namespace, d.Name, t.Name, store.TriggerCursor{Seq: last})
	}
	if _, held := s.Hub.providerHold(ctx, d); held {
		return nil
	}
	m := t.On
	events, next, err := st.ListEvents(ctx, store.EventFilter{Namespaces: []string{d.Namespace}, AfterSeq: cur.Seq, Types: m.Types,
		Deployment: m.Deployment, Labels: m.Labels, Severity: m.Severity, Limit: 100})
	if err != nil || next == cur.Seq {
		return err
	}
	within, _ := time.ParseDuration(m.Within)
	for _, e := range events {
		// Never react to the tasks this trigger submitted itself.
		if e.Deployment == d.Name && e.Data["source"] == "event:"+t.Name {
			continue
		}
		if m.Count <= 1 {
			if err := s.fireEventTask(ctx, d, t, []store.Event{e}); err != nil {
				return err
			}
			continue
		}
		held, err := st.EventsBySeq(ctx, cur.Pending)
		if err != nil {
			return err
		}
		group := []store.Event{}
		for _, p := range held {
			if e.AtMs-p.AtMs <= within.Milliseconds() {
				group = append(group, p)
			}
		}
		group = append(group, e)
		if len(group) >= int(m.Count) {
			if err := s.fireEventTask(ctx, d, t, group); err != nil {
				return err
			}
			group = nil
		}
		cur.Pending = cur.Pending[:0]
		for _, g := range group {
			cur.Pending = append(cur.Pending, g.Seq)
		}
	}
	cur.Seq = next
	return st.SetTriggerCursor(ctx, d.Namespace, d.Name, t.Name, cur)
}

// fireEventTask submits one task for events (exactly once: its key is the
// trigger and the last event's seq).
func (s *Scheduler) fireEventTask(ctx context.Context, d store.Deployment, t *agenv1.Trigger, events []store.Event) error {
	st := s.Hub.Store
	last := events[len(events)-1]
	chain := 0
	for _, e := range events {
		if e.TaskID == "" {
			continue
		}
		if src, err := st.GetTask(ctx, e.TaskID); err == nil && strings.HasPrefix(src.Source, "event:") {
			chain = max(chain, src.EventChain+1)
		}
	}
	if chain >= maxEventChain {
		_, err := st.RecordTriggerEvent(ctx, store.TriggerEvent{Namespace: d.Namespace, Deployment: d.Name, Trigger: t.Name, State: "rejected",
			DueMs: last.Seq, Message: fmt.Sprintf("event %d: %d event triggers in a row led here; not firing again (a loop?)", last.Seq, chain)})
		return err
	}
	var b strings.Builder
	if t.Input != "" {
		b.WriteString(t.Input)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "[Events from trigger %s]\n", t.Name)
	for _, e := range events {
		b.Write(eventJSON(e))
		b.WriteByte('\n')
	}
	labels := map[string]string{}
	for k, v := range last.Labels {
		labels[k] = v
	}
	for k, v := range t.Labels {
		labels[k] = v
	}
	conversation := ""
	if t.Conversation != "" {
		value := conversationVar.ReplaceAllStringFunc(t.Conversation, func(v string) string {
			k := conversationVar.FindStringSubmatch(v)[1]
			switch {
			case k == "taskId":
				return last.TaskID
			case k == "parentTaskId":
				return last.ParentTaskID
			case k == "deployment":
				return last.Deployment
			case k == "type":
				return last.Type
			default:
				return last.Labels[strings.TrimPrefix(k, "labels.")]
			}
		})
		if strings.TrimSpace(value) != "" {
			conversation = "event:" + t.Name + ":" + value
		}
	}
	candidate := store.Task{Namespace: d.Namespace, Deployment: d.Name, Input: b.String(), Source: "event:" + t.Name,
		IdempotencyKey: fmt.Sprintf("event:%s:%d", t.Name, last.Seq), SubmittedBy: "trigger:event:" + t.Name, Labels: labels,
		ConversationKey: conversation, EventChain: chain, MaxQueued: int(PolicyOf(d).Limits.MaxQueuedTasks), Trust: "trusted"}
	merged := false
	var task store.Task
	// Coalescing (3.6/M4, event coalescing): a matching event that arrives
	// while this trigger already has a task queued in the target
	// conversation merges into it instead of firing a separate one, the
	// same mechanism "collect" queue mode uses.
	if t.Coalesce != "" && candidate.ConversationKey != "" {
		if cd, err := time.ParseDuration(t.Coalesce); err == nil {
			m, ok, err := st.MergeIntoQueuedConversationTask(ctx, candidate, cd.Milliseconds())
			if err != nil {
				return err
			}
			if ok {
				task, merged = m, true
			}
		}
	}
	if !merged {
		var err error
		task, err = st.SubmitTask(ctx, candidate)
		if err != nil {
			return err
		}
	}
	msg := fmt.Sprintf("%d event(s), the last %s (seq %d)", len(events), last.Type, last.Seq)
	if merged {
		msg += "; merged into the conversation's still-queued task"
	}
	if _, err := st.RecordTriggerEvent(ctx, store.TriggerEvent{Namespace: d.Namespace, Deployment: d.Name, Trigger: t.Name, State: "fired",
		TaskID: task.ID, DueMs: last.Seq, Message: msg}); err != nil {
		return err
	}
	_ = st.TouchActivity(ctx, d.Namespace, d.Name)
	s.Log.Info("event trigger fired", "deployment", d.Namespace+"/"+d.Name, "trigger", t.Name, "task", task.ID, "events", len(events), "merged", merged)
	return nil
}
