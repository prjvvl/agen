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
	// Kind: task, run, approval, instance or deployment.
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	Namespace  string `json:"namespace"`
	Deployment string `json:"deployment"`
	State      string `json:"state,omitempty"`
	TraceID    string `json:"traceId,omitempty"`
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
	seen := map[string]entry{} // kind/namespace/id -> last version seen
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
				if seen[key].change.Version == c.Version {
					continue
				}
				seen[key] = entry{c, now}
				if !first {
					e.publish(Event{Kind: c.Kind, ID: c.ID, Namespace: c.Namespace, Deployment: c.Deployment, State: c.State, TraceID: c.TraceID})
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
				case now.Sub(s.at) > 2*eventOverlap:
					delete(seen, key) // out of the window: it cannot come back as unseen
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

// EventsHandler serves GET /api/v1/events: a text/event-stream of change
// events for the namespaces the caller may see (approvals only with the
// approver scope). ?namespace= narrows it to one namespace.
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
		rc := http.NewResponseController(w)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		ch, done := h.feed().subscribe()
		defer done()
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
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
					return
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
