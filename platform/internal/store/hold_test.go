package store

import (
	"context"
	"testing"
)

// PauseTask holds a queued task; LeaseTasks skips it until held_until
// passes (lazy expiry) or ResumeTask clears the hold.
func TestPauseTaskHoldsAQueuedTaskAndLeaseTasksSkipsItUntilExpiry(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("hold")
		a, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "a"})
		must(t, err)

		held, err := s.PauseTask(ctx, a.ID, 0, "testing")
		must(t, err)
		if !held.Held || held.HoldReason != "testing" || held.HeldUntilMs != 0 {
			t.Fatalf("got held=%v reason=%q until=%d", held.Held, held.HoldReason, held.HeldUntilMs)
		}

		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		if len(leased) != 0 {
			t.Fatalf("leased a held task: %v", leased)
		}

		// A past deadline is skipped lazily: still held in storage, but no
		// longer excluded from leasing.
		must(t, errOf(s.db.ExecContext(ctx, "UPDATE tasks SET held_until_ms = 1 WHERE id = $1", a.ID)))
		leased, err = s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		if len(leased) != 1 || leased[0].ID != a.ID {
			t.Fatalf("an expired hold still blocked leasing: %v", leased)
		}
	})
}

// PauseTask leaves an already-terminal or unknown task alone.
func TestPauseTaskIgnoresATaskThatIsNotHoldable(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		got, err := s.PauseTask(ctx, "no-such-task", 0, "x")
		if err == nil {
			t.Fatalf("want an error for an unknown task, got %+v", got)
		}
	})
}

// ResumeTask clears the hold and, when the task is waiting specifically
// for it (wait_for "resume", set by the engine's own suspend once it
// noticed IsHeld), wakes it directly, since nothing else will.
func TestResumeTaskClearsTheHoldAndWakesAResumeWait(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("hold")
		a, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "a"})
		must(t, err)
		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		must(t, s.StartTask(ctx, a.ID, leased[0].LeaseID, "i1"))
		_, err = s.PauseTask(ctx, a.ID, 0, "stand by")
		must(t, err)
		must(t, s.SuspendTask(ctx, a.ID, leased[0].LeaseID, "resume"))

		got, err := s.GetTask(ctx, a.ID)
		must(t, err)
		if got.State != "waiting" || got.WaitFor != "resume" || !got.Held {
			t.Fatalf("got state=%q waitFor=%q held=%v", got.State, got.WaitFor, got.Held)
		}

		resumed, err := s.ResumeTask(ctx, a.ID)
		must(t, err)
		if resumed.Held || resumed.State != "queued" || resumed.WaitFor != "" {
			t.Fatalf("got held=%v state=%q waitFor=%q", resumed.Held, resumed.State, resumed.WaitFor)
		}
	})
}

// A held task waiting on something else (a child, here) only has its hold
// lifted by ResumeTask: it still waits for that other thing.
func TestResumeTaskDoesNotWakeAWaitForOtherThanResume(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("hold")
		a, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "a"})
		must(t, err)
		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		must(t, s.SuspendTask(ctx, a.ID, leased[0].LeaseID, "children"))
		_, err = s.PauseTask(ctx, a.ID, 0, "also on hold")
		must(t, err)

		resumed, err := s.ResumeTask(ctx, a.ID)
		must(t, err)
		if resumed.Held {
			t.Fatalf("still held: %+v", resumed)
		}
		if resumed.State != "waiting" || resumed.WaitFor != "children" {
			t.Fatalf("resuming a hold woke an unrelated wait: state=%q waitFor=%q", resumed.State, resumed.WaitFor)
		}
	})
}

// IsHeld is effective held status: held with no deadline, or one still in
// the future, is true; a passed deadline (even though still stored as
// held=1) is false; never held is false.
func TestIsHeldReflectsTheDeadline(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("hold")
		a, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "a"})
		must(t, err)

		held, err := s.IsHeld(ctx, a.ID)
		must(t, err)
		if held {
			t.Fatal("not yet held")
		}

		_, err = s.PauseTask(ctx, a.ID, 0, "x")
		must(t, err)
		held, err = s.IsHeld(ctx, a.ID)
		must(t, err)
		if !held {
			t.Fatal("held with no deadline should report held")
		}

		must(t, errOf(s.db.ExecContext(ctx, "UPDATE tasks SET held_until_ms = $1 WHERE id = $2", NowMs()+1_000_000, a.ID)))
		held, err = s.IsHeld(ctx, a.ID)
		must(t, err)
		if !held {
			t.Fatal("deadline in the future should still report held")
		}

		must(t, errOf(s.db.ExecContext(ctx, "UPDATE tasks SET held_until_ms = 1 WHERE id = $1", a.ID)))
		held, err = s.IsHeld(ctx, a.ID)
		must(t, err)
		if held {
			t.Fatal("a passed deadline should report not held")
		}
	})
}

// WakeExpiredHolds wakes only a waiting task held specifically on "resume"
// (not some other wait) whose deadline has passed; a held task without a
// deadline, or one whose deadline has not passed yet, is left waiting.
func TestWakeExpiredHoldsWakesOnlyExpiredResumeWaits(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("hold")

		expired, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "expired"})
		must(t, err)
		notYet, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "not-yet"})
		must(t, err)
		otherWait, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "other-wait"})
		must(t, err)
		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		leaseOf := map[string]string{}
		for _, lt := range leased {
			leaseOf[lt.ID] = lt.LeaseID
		}

		must(t, s.SuspendTask(ctx, expired.ID, leaseOf[expired.ID], "resume"))
		_, err = s.PauseTask(ctx, expired.ID, 0, "x")
		must(t, err)
		must(t, errOf(s.db.ExecContext(ctx, "UPDATE tasks SET held_until_ms = 1 WHERE id = $1", expired.ID)))

		must(t, s.SuspendTask(ctx, notYet.ID, leaseOf[notYet.ID], "resume"))
		_, err = s.PauseTask(ctx, notYet.ID, NowMs()+1_000_000, "x")
		must(t, err)

		must(t, s.SuspendTask(ctx, otherWait.ID, leaseOf[otherWait.ID], "children"))
		_, err = s.PauseTask(ctx, otherWait.ID, 0, "x")
		must(t, err)
		must(t, errOf(s.db.ExecContext(ctx, "UPDATE tasks SET held_until_ms = 1 WHERE id = $1", otherWait.ID)))

		woken, err := s.WakeExpiredHolds(ctx)
		must(t, err)
		if len(woken) != 1 || woken[0] != expired.ID {
			t.Fatalf("woken=%v, want just %q", woken, expired.ID)
		}
		got, err := s.GetTask(ctx, expired.ID)
		must(t, err)
		if got.State != "queued" || got.Held {
			t.Fatalf("expired task got state=%q held=%v", got.State, got.Held)
		}
		got, err = s.GetTask(ctx, notYet.ID)
		must(t, err)
		if got.State != "waiting" {
			t.Fatalf("not-yet-expired task was woken early: state=%q", got.State)
		}
		got, err = s.GetTask(ctx, otherWait.ID)
		must(t, err)
		if got.State != "waiting" || got.WaitFor != "children" {
			t.Fatalf("a hold expiring should not wake an unrelated wait: state=%q waitFor=%q", got.State, got.WaitFor)
		}
	})
}

// PauseTree and ResumeTree touch every unfinished task of the tree,
// including an async (reply_deployment set) one: like CancelTree, an
// explicit hold is not the automatic on_parent_end cascade.
func TestPauseTreeAndResumeTreeCoverTheWholeTreeIncludingAsync(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("hold")
		parent, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "parent"})
		must(t, err)
		joined, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "joined", ParentTaskID: parent.ID, RootTaskID: parent.ID})
		must(t, err)
		async, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "async", ParentTaskID: parent.ID, RootTaskID: parent.ID,
			ReplyDeployment: "d", ReplyConversationKey: "async:s1"})
		must(t, err)
		unrelated, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "unrelated"})
		must(t, err)

		held, err := s.PauseTree(ctx, parent.ID, 0, "tree hold")
		must(t, err)
		if len(held) != 3 {
			t.Fatalf("held %d tasks, want 3 (parent, joined, async): %v", len(held), held)
		}
		for _, id := range []string{parent.ID, joined.ID, async.ID} {
			got, err := s.GetTask(ctx, id)
			must(t, err)
			if !got.Held {
				t.Fatalf("%s: not held", id)
			}
		}
		gotUnrelated, err := s.GetTask(ctx, unrelated.ID)
		must(t, err)
		if gotUnrelated.Held {
			t.Fatal("an unrelated task was held by PauseTree")
		}

		resumed, err := s.ResumeTree(ctx, parent.ID)
		must(t, err)
		if len(resumed) != 3 {
			t.Fatalf("resumed %d tasks, want 3: %v", len(resumed), resumed)
		}
		for _, id := range []string{parent.ID, joined.ID, async.ID} {
			got, err := s.GetTask(ctx, id)
			must(t, err)
			if got.Held {
				t.Fatalf("%s: still held after ResumeTree", id)
			}
		}
	})
}

func errOf(_ any, err error) error { return err }
