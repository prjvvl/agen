package store

import (
	"context"
	"testing"
)

// CountUnfinishedChildren counts only direct, unfinished *joined* children
// (reply_deployment unset): an async (detach: true) child, a grandchild,
// an already-finished child and an unrelated task are all excluded.
func TestCountUnfinishedChildrenScopesToDirectJoinedUnfinishedOnes(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("jn")
		parent, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "parent"})
		must(t, err)
		joined, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "joined", ParentTaskID: parent.ID})
		must(t, err)
		async, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "async", ParentTaskID: parent.ID,
			ReplyDeployment: "d", ReplyConversationKey: "async:s1"})
		must(t, err)
		grandchild, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "grandchild", ParentTaskID: joined.ID})
		must(t, err)
		alreadyDone, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "done", ParentTaskID: parent.ID})
		must(t, err)

		n, err := s.CountUnfinishedChildren(ctx, parent.ID)
		must(t, err)
		if n != 2 {
			t.Fatalf("count=%d, want 2 (joined + alreadyDone, before it finishes); async and the grandchild don't count", n)
		}

		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		leaseOf := map[string]string{}
		for _, lt := range leased {
			leaseOf[lt.ID] = lt.LeaseID
		}
		must(t, s.CompleteTask(ctx, alreadyDone.ID, leaseOf[alreadyDone.ID], true, "done", "", "r1", "i1"))

		n, err = s.CountUnfinishedChildren(ctx, parent.ID)
		must(t, err)
		if n != 1 {
			t.Fatalf("count=%d, want 1 (just joined, now that alreadyDone finished)", n)
		}

		// A task with no children at all.
		n, err = s.CountUnfinishedChildren(ctx, joined.ID)
		must(t, err)
		if n != 1 {
			t.Fatalf("joined's own count=%d, want 1 (the grandchild)", n)
		}
		_ = async
		_ = grandchild
	})
}

// A task waiting on "children" (3.4, part: "join") is woken by any "child"
// row, regardless of its ref; a "signal" row, or a "child" row for a task
// waiting on a specific "child:<id>", never wakes it this way.
func TestWriteInboxWakesAChildrenWaitOnAnyChildRow(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("jn")
		a, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "a"})
		must(t, err)
		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		must(t, s.SuspendTask(ctx, a.ID, leased[0].LeaseID, "children"))

		// A signal does not wake a "children" wait.
		woken, err := s.WriteInbox(ctx, InboxRow{Namespace: ns, TaskID: a.ID, RunID: "r1", Kind: "signal", Ref: "x", Payload: "hi"})
		must(t, err)
		if woken {
			t.Fatal("a signal row woke a children-wait")
		}
		got, err := s.GetTask(ctx, a.ID)
		must(t, err)
		if got.State != "waiting" {
			t.Fatalf("still waiting, got state=%q", got.State)
		}

		// Any "child" row, whatever its ref, wakes it.
		woken, err = s.WriteInbox(ctx, InboxRow{Namespace: ns, TaskID: a.ID, RunID: "r1", Kind: "child", Ref: "whichever-child", Payload: "{}"})
		must(t, err)
		if !woken {
			t.Fatal("a child row did not wake a children-wait")
		}
		got, err = s.GetTask(ctx, a.ID)
		must(t, err)
		if got.State != "queued" || got.WaitFor != "" {
			t.Fatalf("got state=%q waitFor=%q", got.State, got.WaitFor)
		}
	})
}

// A task waiting on a specific "child:<id>" (call_agent's own suspend, 3.3)
// is never woken by an unrelated child's row, "children" wording or not.
func TestWriteInboxDoesNotWakeASpecificChildWaitOnAnUnrelatedChild(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("jn")
		a, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "a"})
		must(t, err)
		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		must(t, s.SuspendTask(ctx, a.ID, leased[0].LeaseID, "child:task-1"))

		woken, err := s.WriteInbox(ctx, InboxRow{Namespace: ns, TaskID: a.ID, RunID: "r1", Kind: "child", Ref: "task-2", Payload: "{}"})
		must(t, err)
		if woken {
			t.Fatal("woken by an unrelated child's row")
		}
		woken, err = s.WriteInbox(ctx, InboxRow{Namespace: ns, TaskID: a.ID, RunID: "r1", Kind: "child", Ref: "task-1", Payload: "{}"})
		must(t, err)
		if !woken {
			t.Fatal("not woken by the exact child it is waiting for")
		}
	})
}
