package store

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

func eventTypes(events []Event) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}

// Task, approval and trigger changes write their events in the same
// transaction, in order; filters and the cursor work.
func TestEventLogRecordsChangesInOrder(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("ev")
		only := EventFilter{Namespaces: []string{ns}}
		task, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "x", IdempotencyKey: "k", Labels: map[string]string{"project": "p1"}})
		must(t, err)
		// A resubmission with the same key is the same task: no new event.
		_, err = s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "x", IdempotencyKey: "k"})
		must(t, err)
		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 1, 60_000)
		must(t, err)
		must(t, s.StartTask(ctx, task.ID, leased[0].LeaseID, "i1"))
		must(t, s.StartTask(ctx, task.ID, leased[0].LeaseID, "i1")) // repeated: one event
		outcome := `{"status":"needs_input","summary":"Which repo?","inputFrom":"human","data":{"big":"x"}}`
		must(t, s.CompleteTaskWithOutcome(ctx, task.ID, leased[0].LeaseID, true, "Which repo?", "", "r1", "i1", outcome))
		other, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "e", Input: "y"})
		must(t, err)
		_, err = s.CancelTask(ctx, other.ID)
		must(t, err)
		ok, err := s.RecordTriggerEvent(ctx, TriggerEvent{Namespace: ns, Deployment: "d", Trigger: "nightly", State: "missed", DueMs: 1000, Message: "hub was down"})
		must(t, err)
		if !ok {
			t.Fatal("trigger event not recorded")
		}
		_, err = s.RecordTriggerEvent(ctx, TriggerEvent{Namespace: ns, Deployment: "d", Trigger: "nightly", State: "fired", DueMs: 2000})
		must(t, err)

		events, next, err := s.ListEvents(ctx, only)
		must(t, err)
		want := []string{"task.queued", "task.started", "task.finished", "task.needs_input", "task.queued", "task.finished", "trigger.missed"}
		if got := eventTypes(events); !equalStrings(got, want) {
			t.Fatalf("events %v, want %v", got, want)
		}
		for i := 1; i < len(events); i++ {
			if events[i].Seq <= events[i-1].Seq {
				t.Fatalf("seq not increasing: %v", events)
			}
		}
		if next != events[len(events)-1].Seq {
			t.Fatalf("next %d, last %d", next, events[len(events)-1].Seq)
		}
		fin := events[2]
		o, _ := fin.Data["outcome"].(map[string]any)
		if fin.TaskID != task.ID || fin.Labels["project"] != "p1" || o["status"] != "needs_input" || o["inputFrom"] != "human" || o["data"] != nil {
			t.Fatalf("task.finished: %+v", fin)
		}
		if events[3].Severity != "warning" || events[3].Data["summary"] != "Which repo?" {
			t.Fatalf("task.needs_input: %+v", events[3])
		}
		if c := events[5]; c.Data["state"] != "cancelled" || c.Deployment != "e" {
			t.Fatalf("cancel: %+v", c)
		}

		// Filters.
		f := only
		f.Types = []string{"task."}
		f.Labels = map[string]string{"project": "p1"}
		got, _, err := s.ListEvents(ctx, f)
		must(t, err)
		if !equalStrings(eventTypes(got), []string{"task.queued", "task.started", "task.finished", "task.needs_input"}) {
			t.Fatalf("label filter: %v", eventTypes(got))
		}
		f = only
		f.Severity = "warning"
		got, _, err = s.ListEvents(ctx, f)
		must(t, err)
		if !equalStrings(eventTypes(got), []string{"task.needs_input", "trigger.missed"}) {
			t.Fatalf("severity filter: %v", eventTypes(got))
		}
		f = only
		f.Deployment = "e"
		got, _, err = s.ListEvents(ctx, f)
		must(t, err)
		if len(got) != 2 {
			t.Fatalf("deployment filter: %v", eventTypes(got))
		}
		// The cursor: nothing after the last one; a non-matching tail still
		// moves the cursor on.
		f = only
		f.AfterSeq = next
		got, n2, err := s.ListEvents(ctx, f)
		must(t, err)
		if len(got) != 0 || n2 != next {
			t.Fatalf("after the end: %v %d", got, n2)
		}
		f = only
		f.Types = []string{"approval.pending"}
		got, n3, err := s.ListEvents(ctx, f)
		must(t, err)
		if len(got) != 0 || n3 != next {
			t.Fatalf("no match: %v, next %d want %d", got, n3, next)
		}
		// Other namespaces see none of it.
		got, _, err = s.ListEvents(ctx, EventFilter{Namespaces: []string{uniq("none")}})
		must(t, err)
		if len(got) != 0 {
			t.Fatalf("other namespace: %v", got)
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Concurrent writers get distinct seqs, and a reader following the cursor
// sees every event exactly once.
func TestEventSeqHasNoGapsUnderConcurrency(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("evc")
		var wg sync.WaitGroup
		errs := make(chan error, 40)
		seen := map[string]int{}
		var mu sync.Mutex
		done := make(chan struct{})
		// A reader follows the log while writers run.
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			f := EventFilter{Namespaces: []string{ns}}
			for {
				events, next, err := s.ListEvents(ctx, f)
				if err == nil {
					f.AfterSeq = next
					mu.Lock()
					for _, e := range events {
						seen[e.TaskID]++
					}
					mu.Unlock()
				}
				select {
				case <-done:
					if len(events) == 0 {
						return
					}
				default:
				}
			}
		}()
		for i := 0; i < 40; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "x"})
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			must(t, err)
		}
		close(done)
		<-readerDone
		if len(seen) != 40 {
			t.Fatalf("reader saw %d tasks, want 40", len(seen))
		}
		for id, n := range seen {
			if n != 1 {
				t.Fatalf("task %s seen %d times", id, n)
			}
		}
	})
}

// Events past retention are pruned.
func TestPruneEvents(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("evp")
		must(t, s.AppendEvent(ctx, Event{Namespace: ns, Type: "deployment.created", Deployment: "d", AtMs: 1000, Data: map[string]any{"x": 1}}))
		must(t, s.AppendEvent(ctx, Event{Namespace: ns, Type: "deployment.deleted", Deployment: "d"}))
		_, err := s.PruneEvents(ctx, 2000)
		must(t, err)
		events, _, err := s.ListEvents(ctx, EventFilter{Namespaces: []string{ns}})
		must(t, err)
		if !equalStrings(eventTypes(events), []string{"deployment.deleted"}) {
			t.Fatalf("after prune: %v", eventTypes(events))
		}
		b, _ := json.Marshal(events[0].Data)
		if string(b) != "{}" {
			t.Fatalf("empty data: %s", b)
		}
	})
}
