package store

import (
	"context"
	"testing"
)

func TestFeedbackRecordsAndLists(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("fb")
		f1, err := s.RecordFeedback(ctx, Feedback{Namespace: ns, Deployment: "d", TaskID: "t1", Score: 1, By: "user:a", Labels: map[string]string{"kind": "manual"}})
		must(t, err)
		if f1.ID == "" || f1.CreatedMs == 0 {
			t.Fatalf("recorded: %+v", f1)
		}
		_, err = s.RecordFeedback(ctx, Feedback{Namespace: ns, Deployment: "d", TaskID: "t2", Score: 0, By: "approval", Labels: map[string]string{"kind": "approval"}})
		must(t, err)
		_, err = s.RecordFeedback(ctx, Feedback{Namespace: ns, Deployment: "other", TaskID: "t3", Score: 0.5, By: "user:a"})
		must(t, err)

		all, err := s.ListFeedback(ctx, ns, "", nil, 0, 0)
		must(t, err)
		if len(all) != 3 {
			t.Fatalf("all: %d", len(all))
		}
		if all[0].TaskID != "t3" {
			t.Fatalf("newest first: %+v", all[0])
		}
		byDep, err := s.ListFeedback(ctx, ns, "d", nil, 0, 0)
		must(t, err)
		if len(byDep) != 2 {
			t.Fatalf("by deployment: %d", len(byDep))
		}
		byLabel, err := s.ListFeedback(ctx, ns, "", map[string]string{"kind": "approval"}, 0, 0)
		must(t, err)
		if len(byLabel) != 1 || byLabel[0].TaskID != "t2" {
			t.Fatalf("by label: %+v", byLabel)
		}
		future, err := s.ListFeedback(ctx, ns, "", nil, NowMs()+3600_000, 0)
		must(t, err)
		if len(future) != 0 {
			t.Fatalf("since the future: %d", len(future))
		}
		other, err := s.ListFeedback(ctx, uniq("none"), "", nil, 0, 0)
		must(t, err)
		if len(other) != 0 {
			t.Fatalf("a different namespace sees nothing: %d", len(other))
		}
	})
}
