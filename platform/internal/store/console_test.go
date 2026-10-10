package store

import (
	"context"
	"errors"
	"testing"
)

// The console's queries on both SQLite and Postgres.
func TestConsoleQueries(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("ns")
		r1, r2, tr := NewID(), NewID(), uniq("trace")
		now := NowMs()
		exec := func(q string, args ...any) {
			t.Helper()
			if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
				t.Fatal(err)
			}
		}
		s1, s2, c1, c2 := uniq("s"), uniq("s"), uniq("c"), uniq("c")
		exec("INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ($1,'a',$3,'lead','{}',$4,$4), ($2,'b',$3,'helper','{}',$4,$4)", s1, s2, ns, now)
		exec("INSERT INTO conversations (id, session_id, created_ms) VALUES ($1,$2,$5), ($3,$4,$5)", c1, s1, c2, s2, now)
		exec("INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, trace_id, started_ms, ended_ms, input_tokens, cost_usd, root_run_id, labels) "+
			"VALUES ($1,$2,$3,$4,'lead','succeeded','hi',$5,$6,$7,10,0.5,$1,'{\"team\":\"o_s\"}')", r1, s1, c1, ns, tr, now, now+100)
		exec("INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, trace_id, started_ms, ended_ms, input_tokens, cost_usd, root_run_id, parent_run_id) "+
			"VALUES ($1,$2,$3,$4,'helper','failed','sub',$5,$6,$7,5,0.25,$8,$8)", r2, s2, c2, ns, tr, now+10, now+50, r1)
		exec("INSERT INTO messages (conversation_id, seq, run_id, body, created_ms) VALUES ($1,1,'old','{}',$3), ($1,2,$2,'{}',$3)", c1, r1, now)
		exec("INSERT INTO spans (span_id, trace_id, run_id, name, start_ms, end_ms, seq) VALUES ($1,$2,$3,'agen.run',$4,$5,0)", uniq("sp"), tr, r1, now, now+100)

		all, err := s.ListRunsFiltered(ctx, RunFilter{Namespaces: []string{ns}})
		if err != nil || len(all) != 2 || all[0].ID != r2 {
			t.Fatalf("all runs: %v %v", all, err)
		}
		roots, _ := s.ListRunsFiltered(ctx, RunFilter{Namespaces: []string{ns}, RootsOnly: true})
		failed, _ := s.ListRunsFiltered(ctx, RunFilter{Namespaces: []string{ns}, Status: "failed"})
		labelled, _ := s.ListRunsFiltered(ctx, RunFilter{Namespaces: []string{ns}, Labels: map[string]string{"team": "o_s"}})
		wildcard, _ := s.ListRunsFiltered(ctx, RunFilter{Namespaces: []string{ns}, Labels: map[string]string{"team": "o%"}})
		page, _ := s.ListRunsFiltered(ctx, RunFilter{Namespaces: []string{ns}, BeforeMs: all[0].StartedMs, BeforeID: all[0].ID})
		if len(roots) != 1 || len(failed) != 1 || len(labelled) != 1 || len(wildcard) != 0 || len(page) != 1 || page[0].ID != r1 {
			t.Fatalf("filters: roots %d failed %d labelled %d wildcard %d page %v", len(roots), len(failed), len(labelled), len(wildcard), page)
		}
		trees, err := s.TreeUsages(ctx, []string{r1})
		if err != nil || trees[r1].Runs != 2 || trees[r1].InputTokens != 15 {
			t.Fatalf("tree usage: %v %v", trees, err)
		}
		msgs, err := s.Transcript(ctx, all[1], true)
		if err != nil || len(msgs) != 2 {
			t.Fatalf("transcript: %v %v", msgs, err)
		}
		stats, err := s.RunStats(ctx, []string{ns}, now-1)
		calls, err2 := s.CallStats(ctx, []string{ns}, now-1)
		if err != nil || err2 != nil || len(stats) != 2 || len(calls) != 1 || calls[0].FromDeployment != "lead" || calls[0].Status != "failed" {
			t.Fatalf("stats: %v %v %v %v", stats, calls, err, err2)
		}
		traces, err := s.TraceIDs(ctx, []string{r1})
		if err != nil || traces[r1] != tr {
			t.Fatalf("trace ids: %v %v", traces, err)
		}
		changes, err := s.RecentChanges(ctx, now-1)
		if err != nil {
			t.Fatal(err)
		}
		var sawRun bool
		for _, c := range changes {
			sawRun = sawRun || (c.Kind == "run" && c.ID == r1 && c.TraceID == tr)
		}
		if !sawRun {
			t.Fatal("the run's span did not show up as a change")
		}

		task, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "lead", Input: "x", MaxQueued: 1})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "lead", Input: "y", MaxQueued: 1}); !errors.Is(err, ErrQueueFull) {
			t.Fatalf("second queued task: %v", err)
		}
		exec("UPDATE runs SET task_id = $1 WHERE id = $2", task.ID, r1)
		a, err := s.CreateApproval(ctx, Approval{ID: NewID(), Namespace: ns, Deployment: "lead", RunID: r1, Tool: "x", Arguments: []byte("{}")}, 60_000)
		if err != nil {
			t.Fatal(err)
		}
		pending, err := s.PendingApprovals(ctx, []string{task.ID})
		if err != nil || pending[task.ID] != a.ID {
			t.Fatalf("pending: %v %v", pending, err)
		}

		target, secret, err := s.SetNotificationTarget(ctx, NotificationTarget{Namespace: ns, Name: "ops", URL: "https://example.invalid", Events: []string{"task.failed"}})
		if err != nil || secret == "" || target.CreatedMs == 0 {
			t.Fatalf("set target: %v %v", target, err)
		}
		got, err := s.NotificationSecret(ctx, ns, "ops")
		if err != nil || got != secret {
			t.Fatalf("secret: %v", err)
		}
		list, err := s.ListNotificationTargets(ctx, ns)
		if err != nil || len(list) != 1 || list[0].Events[0] != "task.failed" {
			t.Fatalf("list targets: %v %v", list, err)
		}
		if err := s.DeleteNotificationTarget(ctx, ns, "ops"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteNotificationTarget(ctx, ns, "ops"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("delete twice: %v", err)
		}
		if _, err := s.PruneObservability(ctx, 1); err != nil {
			t.Fatal(err)
		}
	})
}
