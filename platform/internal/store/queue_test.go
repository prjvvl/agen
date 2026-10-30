package store

import (
	"context"
	"testing"
)

// MergeIntoQueuedConversationTask merges into the most recently created
// still-queued task of the conversation, extending its not_before, instead
// of creating a separate row; it has nothing to merge into for a fresh
// conversation, or once the existing task has been leased.
func TestMergeIntoQueuedConversationTaskMergesOrDeclines(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("q")

		// Nothing queued yet: declines, so the caller queues a fresh task.
		merged, ok, err := s.MergeIntoQueuedConversationTask(ctx, Task{Namespace: ns, Deployment: "d", ConversationKey: "c1", Input: "first"}, 60_000)
		must(t, err)
		if ok {
			t.Fatalf("merged into nothing: %v", merged)
		}
		first, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", ConversationKey: "c1", Input: "first"})
		must(t, err)

		// A second submission merges into the first's still-queued row.
		merged, ok, err = s.MergeIntoQueuedConversationTask(ctx, Task{Namespace: ns, Deployment: "d", ConversationKey: "c1", Input: "second"}, 60_000)
		must(t, err)
		if !ok || merged.ID != first.ID {
			t.Fatalf("merge: ok=%v id=%q, want %q", ok, merged.ID, first.ID)
		}
		if merged.Input != "first\n\nsecond" {
			t.Fatalf("merged input=%q", merged.Input)
		}
		if merged.NotBeforeMs <= NowMs() {
			t.Fatalf("not_before_ms=%d not extended into the future", merged.NotBeforeMs)
		}

		// A third submission merges into the SAME row again (not a new one).
		merged, ok, err = s.MergeIntoQueuedConversationTask(ctx, Task{Namespace: ns, Deployment: "d", ConversationKey: "c1", Input: "third"}, 60_000)
		must(t, err)
		if !ok || merged.ID != first.ID || merged.Input != "first\n\nsecond\n\nthird" {
			t.Fatalf("third merge: ok=%v id=%q input=%q", ok, merged.ID, merged.Input)
		}

		// Once leased, there is nothing left to merge into (not_before is
		// cleared by hand here since the merges above pushed it 60s out).
		_, err = s.db.ExecContext(ctx, "UPDATE tasks SET not_before_ms = 0 WHERE id = $1", first.ID)
		must(t, err)
		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		if len(leased) != 1 {
			t.Fatalf("leased: %v", leased)
		}
		_, ok, err = s.MergeIntoQueuedConversationTask(ctx, Task{Namespace: ns, Deployment: "d", ConversationKey: "c1", Input: "fourth"}, 60_000)
		must(t, err)
		if ok {
			t.Fatal("merged into an already-leased task")
		}

		// A different conversation's queued task is never touched.
		other, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", ConversationKey: "c2", Input: "unrelated"})
		must(t, err)
		_, ok, err = s.MergeIntoQueuedConversationTask(ctx, Task{Namespace: ns, Deployment: "d", ConversationKey: "c1", Input: "fifth"}, 60_000)
		must(t, err)
		if ok {
			t.Fatal("merged across conversations")
		}
		gotOther, err := s.GetTask(ctx, other.ID)
		must(t, err)
		if gotOther.Input != "unrelated" {
			t.Fatalf("unrelated conversation's task was touched: %q", gotOther.Input)
		}
	})
}
