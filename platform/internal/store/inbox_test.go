package store

import (
	"context"
	"testing"
)

// DrainInbox claims unconsumed non-"child" rows (and only them), oldest
// first, and never returns the same row twice.
func TestDrainInboxClaimsSignalRowsOnceAndLeavesChildRowsAlone(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("ib")
		a, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "a"})
		must(t, err)

		_, err = s.WriteInbox(ctx, InboxRow{Namespace: ns, TaskID: a.ID, RunID: "r1", Kind: "signal", Ref: "caller-1", Payload: "hurry up"})
		must(t, err)
		_, err = s.WriteInbox(ctx, InboxRow{Namespace: ns, TaskID: a.ID, RunID: "r1", Kind: "child", Ref: "child-1", Payload: `{"status":"done"}`})
		must(t, err)
		_, err = s.WriteInbox(ctx, InboxRow{Namespace: ns, TaskID: a.ID, RunID: "r1", Kind: "signal", Ref: "caller-2", Payload: "second"})
		must(t, err)

		rows, err := s.DrainInbox(ctx, a.ID)
		must(t, err)
		if len(rows) != 2 {
			t.Fatalf("drained %d rows, want 2 (the two signals, not the child row): %v", len(rows), rows)
		}
		if rows[0].Ref != "caller-1" || rows[1].Ref != "caller-2" {
			t.Fatalf("drained out of order: %v", rows)
		}

		// A second drain finds nothing left to claim.
		again, err := s.DrainInbox(ctx, a.ID)
		must(t, err)
		if len(again) != 0 {
			t.Fatalf("second drain returned %d rows, want 0 (already claimed): %v", len(again), again)
		}
	})
}

// DrainInbox on a task with nothing in its inbox returns an empty slice,
// not an error.
func TestDrainInboxOnAnEmptyInboxReturnsNothing(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("ib")
		a, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "a"})
		must(t, err)
		rows, err := s.DrainInbox(ctx, a.ID)
		must(t, err)
		if len(rows) != 0 {
			t.Fatalf("got %v, want none", rows)
		}
	})
}
