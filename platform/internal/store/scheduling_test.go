package store

import (
	"context"
	"testing"
)

// At most one queued task per (namespace, deployment, scheduleKey);
// resubmitting the same key cancels the queued one that already has it.
// A task that is not queued any more (leased, running or finished) is left
// alone, and a different key never interferes.
func TestScheduleKeyReplacesTheQueuedTask(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("sk")
		first, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "a", ScheduleKey: "nightly"})
		must(t, err)
		other, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "b", ScheduleKey: "other-key"})
		must(t, err)
		second, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "c", ScheduleKey: "nightly"})
		must(t, err)
		if second.ID == first.ID {
			t.Fatal("a new task must be created, not the same row reused")
		}
		got, err := s.GetTask(ctx, first.ID)
		must(t, err)
		if got.State != "cancelled" {
			t.Fatalf("the earlier task with the same key must be cancelled: %q", got.State)
		}
		got2, err := s.GetTask(ctx, second.ID)
		must(t, err)
		if got2.State != "queued" {
			t.Fatalf("the new task must be queued: %q", got2.State)
		}
		gotOther, err := s.GetTask(ctx, other.ID)
		must(t, err)
		if gotOther.State != "queued" {
			t.Fatalf("a different schedule key must be left alone: %q", gotOther.State)
		}

		// Once a task has left the queue (leased), a new submission with its
		// key does not touch it.
		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 5, 60_000)
		must(t, err)
		if len(leased) != 2 { // second + other
			t.Fatalf("leased: %d", len(leased))
		}
		third, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "d", ScheduleKey: "nightly"})
		must(t, err)
		gotSecond, err := s.GetTask(ctx, second.ID)
		must(t, err)
		if gotSecond.State == "cancelled" {
			t.Fatal("a leased task must not be cancelled by a new submission")
		}
		gotThird, err := s.GetTask(ctx, third.ID)
		must(t, err)
		if gotThird.State != "queued" {
			t.Fatalf("third: %q", gotThird.State)
		}
	})
}

// A queued task past its not_after fails as expired instead of waiting
// indefinitely; a task without one, or not yet due, is untouched.
func TestExpireOverdueTasks(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("na")
		now := NowMs()
		overdue, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "a", NotAfterMs: now - 1000})
		must(t, err)
		notYet, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "b", NotAfterMs: now + 3600_000})
		must(t, err)
		none, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "c"})
		must(t, err)
		expired, err := s.ExpireOverdueTasks(ctx)
		must(t, err)
		if len(expired) != 1 || expired[0] != overdue.ID {
			t.Fatalf("expired: %v", expired)
		}
		got, err := s.GetTask(ctx, overdue.ID)
		must(t, err)
		if got.State != "failed" || got.Error == "" {
			t.Fatalf("overdue task: %+v", got)
		}
		gotNotYet, err := s.GetTask(ctx, notYet.ID)
		must(t, err)
		if gotNotYet.State != "queued" {
			t.Fatalf("not yet due: %q", gotNotYet.State)
		}
		gotNone, err := s.GetTask(ctx, none.ID)
		must(t, err)
		if gotNone.State != "queued" {
			t.Fatalf("no not_after: %q", gotNone.State)
		}
		// Running again finds nothing more to expire.
		again, err := s.ExpireOverdueTasks(ctx)
		must(t, err)
		if len(again) != 0 {
			t.Fatalf("second sweep: %v", again)
		}
	})
}

// LeaseTasks takes urgent before high before normal (and no priority) before
// background, regardless of submission order; ties within a priority still
// go oldest first.
func TestLeaseTasksOrdersByPriority(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("pr")
		normal, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "a"})
		must(t, err)
		background, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "b", Priority: "background"})
		must(t, err)
		urgent, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "c", Priority: "urgent"})
		must(t, err)
		high, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "d", Priority: "high"})
		must(t, err)
		var order []string
		for i := 0; i < 4; i++ {
			leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 1, 60_000)
			must(t, err)
			if len(leased) != 1 {
				t.Fatalf("lease %d: got %d", i, len(leased))
			}
			order = append(order, leased[0].ID)
		}
		want := []string{urgent.ID, high.ID, normal.ID, background.ID}
		for i := range want {
			if order[i] != want[i] {
				t.Fatalf("lease order: got %v, want %v", order, want)
			}
		}
	})
}
