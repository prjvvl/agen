package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

// EventRetention is how long the event log keeps events.
const EventRetention = 7 * 24 * time.Hour

// MaxEventWait bounds ListEvents wait_seconds.
const MaxEventWait = 300 * time.Second

func eventProto(e store.Event) *agenv1.Event {
	data, _ := structpb.NewStruct(e.Data)
	return &agenv1.Event{Seq: e.Seq, Namespace: e.Namespace, Type: e.Type, Deployment: e.Deployment, TaskId: e.TaskID,
		ParentTaskId: e.ParentTaskID, Labels: e.Labels, Severity: e.Severity, Data: data, At: ms(e.AtMs)}
}

// eventFilter is the store filter for what p may see of a request.
func eventFilter(p Principal, ns string, f store.EventFilter) (store.EventFilter, error) {
	switch {
	case ns != "":
		if !p.AllowsNamespace(ns) {
			return f, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("namespace %s is not allowed for this token", ns))
		}
		f.Namespaces = []string{ns}
	case len(p.Namespaces) > 0:
		f.Namespaces = p.Namespaces
	}
	if f.Severity != "" && f.Severity != "info" && f.Severity != "warning" && f.Severity != "critical" {
		return f, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("severity must be info, warning or critical"))
	}
	return f, nil
}

// visibleEvents drops events p may not see (approvals need the approver
// scope, as in ListApprovals).
func visibleEvents(p Principal, events []store.Event) []store.Event {
	if p.Can(ScopeApprover) {
		return events
	}
	out := events[:0:0]
	for _, e := range events {
		if !strings.HasPrefix(e.Type, "approval.") {
			out = append(out, e)
		}
	}
	return out
}

func (h *Hub) ListEvents(ctx context.Context, req *connect.Request[agenv1.ListEventsRequest]) (*connect.Response[agenv1.ListEventsResponse], error) {
	m := req.Msg
	p := PrincipalFrom(ctx)
	limit := int(m.Limit)
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		return nil, invalid("limit must be at most 1000")
	}
	f, err := eventFilter(p, m.Namespace, store.EventFilter{AfterSeq: m.AfterSeq, Types: m.Types, Deployment: m.Deployment,
		Labels: m.Labels, Severity: m.Severity, Limit: limit})
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(min(time.Duration(m.WaitSeconds)*time.Second, MaxEventWait))
	for {
		events, next, err := h.Store.ListEvents(ctx, f)
		if err != nil {
			return nil, connectErr(err)
		}
		events = visibleEvents(p, events)
		if len(events) > 0 || !time.Now().Before(deadline) {
			out := &agenv1.ListEventsResponse{NextSeq: next}
			for _, e := range events {
				out.Events = append(out.Events, eventProto(e))
			}
			return connect.NewResponse(out), nil
		}
		f.AfterSeq = next
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(h.Poll):
		}
	}
}

// event records an event the Hub raises itself (deployment changes,
// provider holds, thresholds, warnings). A failure is logged, never fatal.
func (h *Hub) event(ctx context.Context, e store.Event) {
	if err := h.Store.AppendEvent(context.WithoutCancel(ctx), e); err != nil {
		_ = h.Store.AppendLog(ctx, "", e.Namespace, e.Deployment, "warn", "could not record event "+e.Type+": "+err.Error())
	}
}

// deploymentEvent records deployment.created, .updated or .deleted.
func (h *Hub) deploymentEvent(ctx context.Context, typ, ns, name string, data map[string]any) {
	h.event(ctx, store.Event{Namespace: ns, Type: typ, Deployment: name, Data: data})
}

// logEvents reads the next page of log events p may see.
func (h *Hub) logEvents(ctx context.Context, p Principal, f store.EventFilter) ([]store.Event, int64, error) {
	events, next, err := h.Store.ListEvents(ctx, f)
	if err != nil {
		return nil, f.AfterSeq, err
	}
	return visibleEvents(p, events), next, nil
}

// logStreamFilter reads the event-log part of an /api/v1/events request:
// ?after= or Last-Event-ID (the cursor), ?type= (repeatable), ?deployment=,
// ?label=k=v (repeatable) and ?severity=.
func logStreamFilter(r *http.Request) (store.EventFilter, error) {
	q := r.URL.Query()
	f := store.EventFilter{Types: q["type"], Deployment: q.Get("deployment"), Severity: q.Get("severity"), Limit: 500}
	cursor := q.Get("after")
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		cursor = id
	}
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n < 0 {
			return f, fmt.Errorf("the cursor (after / Last-Event-ID) must be an event seq")
		}
		f.AfterSeq = n
	}
	for _, l := range q["label"] {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			return f, fmt.Errorf("label must be key=value")
		}
		if f.Labels == nil {
			f.Labels = map[string]string{}
		}
		f.Labels[k] = v
	}
	return f, nil
}

// eventJSON is an event as the stream and notifications send it.
func eventJSON(e store.Event) json.RawMessage {
	b, _ := protojson.Marshal(eventProto(e))
	return b
}

// pruneEvents drops events past their retention (leader, now and then).
func (h *Hub) pruneEvents(ctx context.Context) {
	if _, err := h.Store.PruneEvents(ctx, store.NowMs()-EventRetention.Milliseconds()); err != nil {
		_ = h.Store.AppendLog(ctx, "", "default", "", "warn", "event retention: "+err.Error())
	}
}
