package store

import (
	"context"
	"testing"
)

// SetTaskProgress records a task's latest progress while it is unfinished,
// and is left alone once it ends (reporting after the fact is stale, not
// an error).
func TestSetTaskProgressRecordsLatestWhileUnfinishedAndIsIgnoredAfterwards(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("pr")
		a, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "a"})
		must(t, err)

		must(t, s.SetTaskProgress(ctx, a.ID, `{"step":"reading page 1","done":false}`))
		got, err := s.GetTask(ctx, a.ID)
		must(t, err)
		if got.Progress != `{"step":"reading page 1","done":false}` {
			t.Fatalf("progress=%q", got.Progress)
		}

		leased, err := s.LeaseTasks(ctx, "n1", ns, "d", 10, 60_000)
		must(t, err)
		must(t, s.CompleteTask(ctx, a.ID, leased[0].LeaseID, true, "done", "", "r1", "i1"))

		// Reported after the task already ended: left as it was.
		must(t, s.SetTaskProgress(ctx, a.ID, `{"step":"too late","done":true}`))
		got, err = s.GetTask(ctx, a.ID)
		must(t, err)
		if got.Progress != `{"step":"reading page 1","done":false}` {
			t.Fatalf("progress after a stale report=%q, want unchanged", got.Progress)
		}
	})
}
