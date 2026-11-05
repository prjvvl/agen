package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prjvvl/agen/platform/internal/store"
)

// Event tells a client that something changed; it refetches what it shows.
type Event struct {
	// Kind: task, run, step, approval, instance or deployment.
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	Namespace  string `json:"namespace"`
	Deployment string `json:"deployment"`
	State      string `json:"state,omitempty"`
	TraceID    string `json:"traceId,omitempty"`
	// Steps: a model call (gen_ai.chat) or tool call (agen.tool) starting
	// (state "unfinished") or ending (its status).
	RunID   string `json:"runId,omitempty"`
	Name    string `json:"name,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Target  string `json:"target,omitempty"`
	StartMs int64  `json:"startMs,omitempty"`
	EndMs   int64  `json:"endMs,omitempty"`
}

// changeFeed fans changes out to subscribers. One watcher polls the Store while
// anyone listens, however many clients there are: hosts write run data
// straight to the Store, so the Store is where changes show up.
type changeFeed struct {
	hub      *Hub
	interval time.Duration

	mu   sync.Mutex
	subs map[chan Event]struct{}
	stop context.CancelFunc
}

// Overlap re-reads a few seconds back each poll, so rows written with a
// slightly older timestamp (another machine's clock, a slow transaction)
// are still seen; seen versions are not sent twice.
const eventOverlap = 10 * time.Second

func (e *changeFeed) subscribe() (chan Event, func()) {
	ch := make(chan Event, 256)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.subs == nil {
		e.subs = map[chan Event]struct{}{}
	}
	e.subs[ch] = struct{}{}
	if e.stop == nil {
		ctx, cancel := context.WithCancel(context.Background())
		e.stop = cancel
		go e.watch(ctx)
	}
	return ch, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		delete(e.subs, ch)
		if len(e.subs) == 0 && e.stop != nil {
			e.stop()
			e.stop = nil
		}
	}
}

func (e *changeFeed) publish(ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for ch := range e.subs {
		select {
		case ch <- ev:
		default: // a slow client misses events; it refetches on the next one
		}
	}
}

func (e *changeFeed) watch(ctx context.Context) {
	type entry struct {
		change store.Change
		at     time.Time
	}
	seen := map[string]entry{} // kind/namespace/id -> last version seen, while in the window
	first := true
	t := time.NewTicker(e.interval)
	defer t.Stop()
	for {
		changes, err := e.hub.Store.RecentChanges(ctx, store.NowMs()-eventOverlap.Milliseconds())
		if err == nil {
			now := time.Now()
			live := map[string]bool{}
			for _, c := range changes {
				key := c.Kind + "/" + c.Namespace + "/" + c.ID
				live[key] = true
				prev, known := seen[key]
				seen[key] = entry{c, now}
				if known && prev.change.Version == c.Version {
					continue
				}
				if !first {
					e.publish(Event{Kind: c.Kind, ID: c.ID, Namespace: c.Namespace, Deployment: c.Deployment, State: c.State, TraceID: c.TraceID,
						RunID: c.RunID, Name: c.Name, Tool: c.Tool, Target: c.Target, StartMs: c.StartMs, EndMs: c.EndMs})
				}
			}
			for key, s := range seen {
				switch c := s.change; {
				case c.Kind == "instance" || c.Kind == "deployment":
					// Listed in full: missing means removed.
					if !live[key] {
						delete(seen, key)
						e.publish(Event{Kind: c.Kind, ID: c.ID, Namespace: c.Namespace, Deployment: c.Deployment, State: "removed"})
					}
				case !live[key] && now.Sub(s.at) > time.Minute:
					// Long out of the window. (Rows drift in and out of it as
					// lease renewals touch them; that is not a change.)
					delete(seen, key)
				}
			}
			first = false
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// EventsHandler serves GET /api/v1/events: a text/event-stream for the
// namespaces the caller may see (approvals only with the approver scope).
// ?namespace= narrows it to one namespace. It carries two kinds of message:
//   - "change": something changed, refetch it (not kept; the console uses
//     these);
//   - the event log (event: <type>, id: <seq>), resumable with
//     Last-Event-ID or ?after=, filtered by ?type=, ?deployment=, ?label=
//     and ?severity=. Without a cursor it starts at the newest event.
//
// ?kinds=log sends only the event log, ?kinds=changes only changes.
func (h *Hub) EventsHandler() (string, http.Handler) {
	return "GET /api/v1/events", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := h.Auth.Authenticate(r.Context(), bearer(r.Header))
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		if p.OnBehalf || !p.Can(ScopeViewer) {
			http.Error(w, "the event stream requires the viewer scope", http.StatusForbidden)
			return
		}
		only := r.URL.Query().Get("namespace")
		if only != "" && !p.AllowsNamespace(only) {
			http.Error(w, "namespace "+only+" is not allowed for this token", http.StatusForbidden)
			return
		}
		kinds := r.URL.Query().Get("kinds")
		if kinds != "" && kinds != "log" && kinds != "changes" {
			http.Error(w, "kinds must be log or changes", http.StatusBadRequest)
			return
		}
		lf, err := logStreamFilter(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if lf, err = eventFilter(p, only, lf); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("after") == "" && r.Header.Get("Last-Event-ID") == "" {
			if lf.AfterSeq, err = h.Store.LastEventSeq(r.Context()); err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		rc := http.NewResponseController(w)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		var ch chan Event
		if kinds != "log" {
			var done func()
			ch, done = h.feed().subscribe()
			defer done()
		}
		var logTick <-chan time.Time
		if kinds != "changes" {
			t := time.NewTicker(min(time.Second, max(2*h.Poll, 50*time.Millisecond)))
			defer t.Stop()
			logTick = t.C
		}
		if _, err := fmt.Fprint(w, "event: ready\ndata: {}\n\n"); err != nil {
			return
		}
		_ = rc.Flush()
		ping := time.NewTicker(20 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ping.C:
				// A revoked or expired token ends the stream.
				if _, err := h.Auth.Authenticate(r.Context(), bearer(r.Header)); err != nil {
					return
				}
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
					return
				}
			case <-logTick:
				events, next, err := h.logEvents(r.Context(), p, lf)
				if err != nil {
					continue
				}
				lf.AfterSeq = next
				for _, e := range events {
					if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Type, eventJSON(e)); err != nil {
						return
					}
				}
			case ev := <-ch:
				if !p.AllowsNamespace(ev.Namespace) || (only != "" && ev.Namespace != only) || (ev.Kind == "approval" && !p.Can(ScopeApprover)) {
					continue
				}
				b, _ := json.Marshal(ev)
				if _, err := fmt.Fprintf(w, "event: change\ndata: %s\n\n", b); err != nil {
					return
				}
			}
			_ = rc.Flush()
		}
	})
}

func (h *Hub) feed() *changeFeed {
	h.evOnce.Do(func() { h.ev = &changeFeed{hub: h, interval: time.Second} })
	return h.ev
}
