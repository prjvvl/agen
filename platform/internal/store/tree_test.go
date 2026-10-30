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

// SubmitTask defaults RootTaskID to the task's own id when the caller
// leaves it unset (a plain root task, or an older caller): root_task_id is
// always populated, unlike root_run_id which stays empty until a run
// starts.
func TestSubmitTaskDefaultsRootTaskIDToItself(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("tr")
		got, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "x"})
		must(t, err)
		if got.RootTaskID != got.ID {
			t.Fatalf("root_task_id=%q, want self (%q)", got.RootTaskID, got.ID)
		}
	})
}

// ListTaskTree returns every task sharing a root_task_id, oldest first,
// and nothing from an unrelated tree.
func TestListTaskTreeReturnsOnlyTasksOfThatTree(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("tr")
		root, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "root"})
		must(t, err)
		child, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "child", ParentTaskID: root.ID, RootTaskID: root.ID})
		must(t, err)
		grandchild, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "grandchild", ParentTaskID: child.ID, RootTaskID: root.ID})
		must(t, err)
		other, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "unrelated"})
		must(t, err)

		tree, err := s.ListTaskTree(ctx, ns, root.ID)
		must(t, err)
		if len(tree) != 3 {
			t.Fatalf("tree has %d tasks, want 3: %v", len(tree), tree)
		}
		got := []string{tree[0].ID, tree[1].ID, tree[2].ID}
		want := []string{root.ID, child.ID, grandchild.ID}
		for i, w := range want {
			if got[i] != w {
				t.Fatalf("tree[%d]=%q, want %q (oldest first): %v", i, got[i], w, got)
			}
		}
		for _, tk := range tree {
			if tk.ID == other.ID {
				t.Fatalf("unrelated tree's task %q leaked into this tree", other.ID)
			}
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

// CancelTree cancels every unfinished task of a tree, including an async
// (reply_deployment set) child, unlike the automatic parent-close policy
// which leaves those running; it leaves an unrelated tree and already
// finished tasks alone.
func TestCancelTreeCancelsEveryUnfinishedTaskIncludingAsyncChildren(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("tr")
		root, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "root"})
		must(t, err)
		asyncChild, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "async", ParentTaskID: root.ID, RootTaskID: root.ID,
			ReplyDeployment: "d", ReplyConversationKey: "async:s1"})
		must(t, err)
		joinedChild, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "joined", ParentTaskID: root.ID, RootTaskID: root.ID})
		must(t, err)
		grandchild, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "grandchild", ParentTaskID: joinedChild.ID, RootTaskID: root.ID})
		must(t, err)
		alreadyDone, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "done", ParentTaskID: root.ID, RootTaskID: root.ID})
		must(t, err)
		other, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "unrelated"})
		must(t, err)

		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		leaseOf := map[string]string{}
		for _, lt := range leased {
			leaseOf[lt.ID] = lt.LeaseID
		}
		must(t, s.CompleteTask(ctx, alreadyDone.ID, leaseOf[alreadyDone.ID], true, "already done", "", "r1", "i1"))

		cancelled, err := s.CancelTree(ctx, root.ID)
		must(t, err)
		if len(cancelled) != 4 {
			t.Fatalf("cancelled %d tasks, want 4 (root, async child, joined child, grandchild): %v", len(cancelled), cancelled)
		}

		for name, id := range map[string]string{"root": root.ID, "async child": asyncChild.ID, "joined child": joinedChild.ID, "grandchild": grandchild.ID} {
			got, err := s.GetTask(ctx, id)
			must(t, err)
			if got.State != "cancelled" {
				t.Fatalf("%s (%s) state=%q, want cancelled", name, id, got.State)
			}
		}
		gotDone, err := s.GetTask(ctx, alreadyDone.ID)
		must(t, err)
		if gotDone.State != "succeeded" {
			t.Fatalf("already-finished task state=%q, want left as succeeded", gotDone.State)
		}
		gotOther, err := s.GetTask(ctx, other.ID)
		must(t, err)
		if gotOther.State == "cancelled" {
			t.Fatal("unrelated tree's task was cancelled")
		}
	})
}
