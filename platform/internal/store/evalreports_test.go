package store

import (
	"context"
	"testing"
)

// RecordEvalReport stores a report; ListEvalReports returns a
// deployment's own reports, newest first (2.43, part: eval history).
func TestEvalReportsRecordsAndListsNewestFirst(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("ev")
		r1, err := s.RecordEvalReport(ctx, EvalReport{Namespace: ns, Deployment: "d", DefinitionDigest: "sha256:1", ReportJSON: `[{"scenario":"a","passK":true}]`})
		must(t, err)
		if r1.ID == "" || r1.CreatedMs == 0 {
			t.Fatalf("recorded: %+v", r1)
		}
		_, err = s.RecordEvalReport(ctx, EvalReport{Namespace: ns, Deployment: "d", DefinitionDigest: "sha256:2", ReportJSON: `[{"scenario":"a","passK":false}]`})
		must(t, err)
		_, err = s.RecordEvalReport(ctx, EvalReport{Namespace: ns, Deployment: "other", DefinitionDigest: "sha256:3", ReportJSON: `[]`})
		must(t, err)

		all, err := s.ListEvalReports(ctx, ns, "d", 0)
		must(t, err)
		if len(all) != 2 {
			t.Fatalf("d's own reports: %d", len(all))
		}
		if all[0].DefinitionDigest != "sha256:2" {
			t.Fatalf("newest first: %+v", all[0])
		}
		other, err := s.ListEvalReports(ctx, ns, "other", 0)
		must(t, err)
		if len(other) != 1 {
			t.Fatalf("other's own reports only: %d", len(other))
		}
		limited, err := s.ListEvalReports(ctx, ns, "d", 1)
		must(t, err)
		if len(limited) != 1 || limited[0].DefinitionDigest != "sha256:2" {
			t.Fatalf("limit=1: %+v", limited)
		}
	})
}
