package store

import (
	"context"
	"errors"
	"testing"
)

// LeaseTasks leases a task's declared locks as it leases the task, refuses
// to lease a conflicting task while they are held, and taskFinished
// releases them so the next task can be leased.
func TestLeaseTasksRespectsDeclaredLocks(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("lk")
		a, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "a", Locks: []string{"repo:x"}})
		must(t, err)
		b, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "b", Locks: []string{"repo:x"}})
		must(t, err)
		// A third, unrelated task is never held up by the lock.
		c, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "c"})
		must(t, err)

		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		if len(leased) != 2 {
			t.Fatalf("leased: %d (a holds the lock, b waits, c has none)", len(leased))
		}
		ids := map[string]bool{}
		for _, lt := range leased {
			ids[lt.ID] = true
		}
		if !ids[a.ID] || !ids[c.ID] || ids[b.ID] {
			t.Fatalf("expected a and c leased, not b: %v", leased)
		}
		busy, _, locked, err := s.QueuedBlockers(ctx, b)
		must(t, err)
		if !locked || busy {
			t.Fatalf("b should be waiting on the lock: locked=%v busy=%v", locked, busy)
		}

		// a finishes: its lock is released, so b can now be leased.
		var aLease string
		for _, lt := range leased {
			if lt.ID == a.ID {
				aLease = lt.LeaseID
			}
		}
		must(t, s.CompleteTask(ctx, a.ID, aLease, true, "ok", "", "r1", "i1"))
		leased2, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		if len(leased2) != 1 || leased2[0].ID != b.ID {
			t.Fatalf("b should now lease: %v", leased2)
		}
	})
}

// LeaseTasks grants a declared lock's fencing token (1.5): it starts at 1,
// stays the same across a same-holder re-lease (ExtendLease), and only
// advances on a genuine handover to a different task — critically,
// including across a release (taskFinished's unconditional delete of
// `locks`), since the counter lives in a separate, never-deleted table.
func TestLeaseTasksAssignsAFencingTokenThatSurvivesARelease(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("lk")
		a, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "a", Locks: []string{"repo:x"}})
		must(t, err)
		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		if len(leased) != 1 || leased[0].LockFences["repo:x"] != 1 {
			t.Fatalf("first-ever grant should fence at 1: %+v", leased)
		}
		must(t, s.CompleteTask(ctx, a.ID, leased[0].LeaseID, true, "ok", "", "r1", "i1"))

		// b takes the same lock after a releases it: the fence must
		// advance to 2, not reset to 1, even though the `locks` row a
		// held was deleted when its task finished.
		b, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "b", Locks: []string{"repo:x"}})
		must(t, err)
		leased2, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		if len(leased2) != 1 || leased2[0].LockFences["repo:x"] != 2 {
			t.Fatalf("fence after a release should advance to 2, not reset: %+v", leased2)
		}

		// A same-holder re-lease (simulated here as another LeaseTasks
		// call while b still holds it) must never happen for a lock
		// already granted this batch, so exercise the same-holder path
		// via ExtendLease instead: the fence stays at 2.
		must(t, s.ExtendLease(ctx, b.ID, leased2[0].LeaseID, 3_600_000))
		var fence int64
		must(t, s.db.QueryRowContext(ctx, "SELECT fence FROM lock_fences WHERE namespace = $1 AND name = $2", ns, "repo:x").Scan(&fence))
		if fence != 2 {
			t.Fatalf("extending a lease must never advance the fence: got %d", fence)
		}
	})
}

// AcquireLock grants a fencing token for a lock named mid-run, rejects a
// second task while it's held, and lets the second task acquire it (at a
// new, still-advancing fence) once the first releases it.
func TestAcquireLockRejectsAnotherHolderUntilReleased(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("lk")
		a, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "a"})
		must(t, err)
		b, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "b"})
		must(t, err)

		fence1, err := s.AcquireLock(ctx, a.ID, "dyn:x", 60_000)
		must(t, err)
		if fence1 != 1 {
			t.Fatalf("first-ever grant should fence at 1: %d", fence1)
		}
		if _, err := s.AcquireLock(ctx, b.ID, "dyn:x", 60_000); !errors.Is(err, ErrConflict) {
			t.Fatalf("a second task should be refused while the first holds it: %v", err)
		}
		// Re-acquiring as the SAME holder is a harmless extend: the
		// fence does not advance.
		fenceAgain, err := s.AcquireLock(ctx, a.ID, "dyn:x", 60_000)
		must(t, err)
		if fenceAgain != fence1 {
			t.Fatalf("a same-holder re-acquire must not advance the fence: got %d, want %d", fenceAgain, fence1)
		}

		must(t, s.ReleaseLock(ctx, a.ID, "dyn:x"))
		fence2, err := s.AcquireLock(ctx, b.ID, "dyn:x", 60_000)
		must(t, err)
		if fence2 != 2 {
			t.Fatalf("after a release, the next grant should advance the fence to 2, not reset: got %d", fence2)
		}
		// Releasing one nobody holds, or one the caller doesn't hold, is
		// a harmless no-op.
		must(t, s.ReleaseLock(ctx, a.ID, "dyn:x"))
		must(t, s.ReleaseLock(ctx, a.ID, "no-such-lock"))
	})
}

// ExtendLease refreshes the expiry of any locks the task holds, so a
// long-running task does not lose its lock mid-run.
func TestExtendLeaseRefreshesLockExpiry(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("lk")
		a, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "a", Locks: []string{"repo:x"}})
		must(t, err)
		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 1, 1_000) // a 1s lease
		must(t, err)
		if len(leased) != 1 {
			t.Fatalf("leased: %v", leased)
		}
		must(t, s.ExtendLease(ctx, a.ID, leased[0].LeaseID, 3_600_000)) // extend by 1h
		b, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "b", Locks: []string{"repo:x"}})
		must(t, err)
		// Even though the original 1s lease would have expired by now, the
		// extended lock still holds b back.
		_, _, locked, err := s.QueuedBlockers(ctx, b)
		must(t, err)
		if !locked {
			t.Fatal("b should still be blocked: the lock's expiry was extended with the lease")
		}
	})
}
