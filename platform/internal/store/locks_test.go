package store

import (
	"context"
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
