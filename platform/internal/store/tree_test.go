package store

import (
	"context"
	"testing"
)

// taskFinished's default parent-close policy (3.4, part) cancels a task's
// own unfinished children when it ends, recursively down the tree.
func TestParentFinishingCancelsUnfinishedChildrenByDefault(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("tr")
		parent, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "parent"})
		must(t, err)
		child, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "child", ParentTaskID: parent.ID, ParentRunID: "pr1"})
		must(t, err)
		grandchild, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "grandchild", ParentTaskID: child.ID, ParentRunID: "cr1"})
		must(t, err)

		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		leaseOf := map[string]string{}
		for _, lt := range leased {
			leaseOf[lt.ID] = lt.LeaseID
		}
		must(t, s.CompleteTask(ctx, parent.ID, leaseOf[parent.ID], true, "done", "", "pr1", "pi1"))

		gotChild, err := s.GetTask(ctx, child.ID)
		must(t, err)
		if gotChild.State != "cancelled" {
			t.Fatalf("child state=%q, want cancelled", gotChild.State)
		}
		gotGrandchild, err := s.GetTask(ctx, grandchild.ID)
		must(t, err)
		if gotGrandchild.State != "cancelled" {
			t.Fatalf("grandchild state=%q, want cancelled (recursive)", gotGrandchild.State)
		}
	})
}

// on_parent_end: "abandon" leaves a task's children running on their own
// when it ends.
func TestParentFinishingAbandonsChildrenWhenAsked(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("tr")
		parent, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "parent", OnParentEnd: "abandon"})
		must(t, err)
		child, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "child", ParentTaskID: parent.ID, ParentRunID: "pr1"})
		must(t, err)

		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		leaseOf := map[string]string{}
		for _, lt := range leased {
			leaseOf[lt.ID] = lt.LeaseID
		}
		must(t, s.CompleteTask(ctx, parent.ID, leaseOf[parent.ID], true, "done", "", "pr1", "pi1"))

		gotChild, err := s.GetTask(ctx, child.ID)
		must(t, err)
		if gotChild.State != "leased" {
			t.Fatalf("abandoned child state=%q, want left as leased", gotChild.State)
		}
	})
}

// An async (reply_deployment set) child, like start_agent's, is never
// cascaded on either policy: its whole point is to outlive the task that
// started it, so a parent finishing right after starting one (today's
// normal start_agent flow) must not cancel it underneath it.
func TestParentFinishingNeverCancelsAnAsyncReplyChild(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("tr")
		parent, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "parent"})
		must(t, err)
		child, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "child", ParentTaskID: parent.ID, ParentRunID: "pr1",
			ReplyDeployment: "d", ReplyConversationKey: "async:s1"})
		must(t, err)

		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		leaseOf := map[string]string{}
		for _, lt := range leased {
			leaseOf[lt.ID] = lt.LeaseID
		}
		must(t, s.CompleteTask(ctx, parent.ID, leaseOf[parent.ID], true, "Started; I will report back.", "", "pr1", "pi1"))

		gotChild, err := s.GetTask(ctx, child.ID)
		must(t, err)
		if gotChild.State != "leased" {
			t.Fatalf("async reply child state=%q, want left running as leased", gotChild.State)
		}
	})
}

// A child that already finished before its parent does is left alone
// (cancelling only touches non-terminal children).
func TestParentFinishingLeavesAlreadyFinishedChildrenAlone(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("tr")
		parent, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "parent"})
		must(t, err)
		child, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "child", ParentTaskID: parent.ID, ParentRunID: "pr1"})
		must(t, err)

		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		leaseOf := map[string]string{}
		for _, lt := range leased {
			leaseOf[lt.ID] = lt.LeaseID
		}
		must(t, s.CompleteTask(ctx, child.ID, leaseOf[child.ID], true, "child done", "", "cr1", "ci1"))
		must(t, s.CompleteTask(ctx, parent.ID, leaseOf[parent.ID], true, "parent done", "", "pr1", "pi1"))

		gotChild, err := s.GetTask(ctx, child.ID)
		must(t, err)
		if gotChild.State != "succeeded" {
			t.Fatalf("already-finished child state=%q, want left as succeeded", gotChild.State)
		}
	})
}
