package store

import (
	"context"
	"testing"
)

// SuspendTask moves a leased task to "waiting", giving up its lease and
// recording what it is waiting for.
func TestSuspendTaskMovesToWaitingAndClearsLease(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("sp")
		a, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "a"})
		must(t, err)
		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		if len(leased) != 1 {
			t.Fatalf("leased: %v", leased)
		}
		must(t, s.SuspendTask(ctx, a.ID, leased[0].LeaseID, "child:c1"))

		got, err := s.GetTask(ctx, a.ID)
		must(t, err)
		if got.State != "waiting" || got.WaitFor != "child:c1" || got.LeaseID != "" {
			t.Fatalf("got state=%q waitFor=%q leaseID=%q", got.State, got.WaitFor, got.LeaseID)
		}

		// A stale lease ID (already given up) cannot suspend again.
		if err := s.SuspendTask(ctx, a.ID, leased[0].LeaseID, "child:c2"); err == nil {
			t.Fatal("expected fenced error on stale lease")
		}
	})
}

// WriteInbox requeues a waiting task only when kind+ref matches its
// wait_for exactly; a non-matching row is recorded but does not wake it.
func TestWriteInboxWakesOnlyMatchingWait(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("sp")
		a, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "a"})
		must(t, err)
		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		must(t, s.SuspendTask(ctx, a.ID, leased[0].LeaseID, "child:c1"))

		woken, err := s.WriteInbox(ctx, InboxRow{Namespace: ns, TaskID: a.ID, RunID: "r1", Kind: "child", Ref: "c2", Payload: "{}"})
		must(t, err)
		if woken {
			t.Fatal("non-matching ref should not wake the task")
		}
		got, err := s.GetTask(ctx, a.ID)
		must(t, err)
		if got.State != "waiting" {
			t.Fatalf("still waiting on the matching ref, got state=%q", got.State)
		}

		woken, err = s.WriteInbox(ctx, InboxRow{Namespace: ns, TaskID: a.ID, RunID: "r1", Kind: "child", Ref: "c1", Payload: `{"status":"done"}`})
		must(t, err)
		if !woken {
			t.Fatal("matching ref should wake the task")
		}
		got, err = s.GetTask(ctx, a.ID)
		must(t, err)
		if got.State != "queued" || got.WaitFor != "" || got.Priority != "urgent" {
			t.Fatalf("got state=%q waitFor=%q priority=%q", got.State, got.WaitFor, got.Priority)
		}
	})
}

// taskFinished writes a child's outcome into its parent's inbox; if the
// parent is waiting on exactly that child, it is woken as part of the
// same transaction that ends the child.
func TestChildFinishingWakesWaitingParent(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("sp")
		parent, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "parent"})
		must(t, err)
		child, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "child", ParentTaskID: parent.ID, ParentRunID: "pr1"})
		must(t, err)

		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		var parentLease, childLease string
		for _, lt := range leased {
			switch lt.ID {
			case parent.ID:
				parentLease = lt.LeaseID
			case child.ID:
				childLease = lt.LeaseID
			}
		}
		must(t, s.SuspendTask(ctx, parent.ID, parentLease, "child:"+child.ID))
		must(t, s.CompleteTask(ctx, child.ID, childLease, true, "ok", "", "cr1", "ci1"))

		got, err := s.GetTask(ctx, parent.ID)
		must(t, err)
		if got.State != "queued" || got.WaitFor != "" {
			t.Fatalf("parent should be woken by its child finishing: state=%q waitFor=%q", got.State, got.WaitFor)
		}
	})
}
