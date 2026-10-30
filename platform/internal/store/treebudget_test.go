package store

import (
	"context"
	"testing"
)

// TreeUsage sums model usage across every run of every task of the tree
// rooted at rootTaskID (3.5, part: tree budgets), not just the root's own
// runs, and never a task of an unrelated tree.
func TestTreeUsageSumsAcrossTheWholeTree(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("tb")
		root, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "root"})
		must(t, err)
		child, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "child", ParentTaskID: root.ID, RootTaskID: root.ID})
		must(t, err)
		grandchild, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "grandchild", ParentTaskID: child.ID, RootTaskID: root.ID})
		must(t, err)
		unrelated, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "unrelated"})
		must(t, err)

		insertRun(t, s, ctx, root.ID, 100, 50, 0.01)
		insertRun(t, s, ctx, child.ID, 200, 100, 0.02)
		insertRun(t, s, ctx, grandchild.ID, 10, 5, 0.001)
		insertRun(t, s, ctx, unrelated.ID, 999, 999, 9.99)

		in, out, cost, err := s.TreeUsage(ctx, root.ID)
		must(t, err)
		if in != 310 || out != 155 {
			t.Fatalf("tokens in=%d out=%d, want 310/155", in, out)
		}
		if cost < 0.0309 || cost > 0.0311 {
			t.Fatalf("cost=%v, want ~0.031", cost)
		}
	})
}

// LabelSpentToday sums only the runs whose task carries the exact
// labelKey=labelVal pair, scoped to the given namespace/deployment and
// since sinceMs, filtering in Go the same way ListEvents's own label
// filter does (3.5, part: label budgets).
func TestLabelSpentTodaySumsOnlyMatchingLabelledTasks(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("lb")
		apollo, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "x", Labels: map[string]string{"team": "apollo"}})
		must(t, err)
		other, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "x", Labels: map[string]string{"team": "other"}})
		must(t, err)
		unlabelled, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "x"})
		must(t, err)

		now := NowMs()
		insertRun(t, s, ctx, apollo.ID, 0, 0, 1.0)
		insertRun(t, s, ctx, other.ID, 0, 0, 2.0)
		insertRun(t, s, ctx, unlabelled.ID, 0, 0, 3.0)

		spent, err := s.LabelSpentToday(ctx, "d", "d", "team", "apollo", now-1)
		must(t, err)
		if spent < 0.999 || spent > 1.001 {
			t.Fatalf("spent=%v, want ~1.0 (apollo's run only)", spent)
		}

		// Nothing started since a future time.
		spent, err = s.LabelSpentToday(ctx, "d", "d", "team", "apollo", now+1_000_000)
		must(t, err)
		if spent != 0 {
			t.Fatalf("spent=%v, want 0 (nothing started that recently)", spent)
		}
	})
}

// insertRun records one run with the given usage for taskID, with its
// own fresh session and conversation.
func insertRun(t *testing.T, s *Store, ctx context.Context, taskID string, inputTokens, outputTokens int64, costUSD float64) {
	t.Helper()
	sid, cid, rid, now := uniq("s"), uniq("c"), uniq("r"), NowMs()
	must(t, errOf(s.db.ExecContext(ctx, "INSERT INTO sessions (id, agent, namespace, deployment, memory, created_ms, updated_ms) VALUES ($1,'a','d','d','{}',$2,$2)", sid, now)))
	must(t, errOf(s.db.ExecContext(ctx, "INSERT INTO conversations (id, session_id, created_ms) VALUES ($1,$2,$3)", cid, sid, now)))
	must(t, errOf(s.db.ExecContext(ctx,
		"INSERT INTO runs (id, session_id, conversation_id, namespace, deployment, status, input, trace_id, started_ms, ended_ms, task_id, input_tokens, output_tokens, cost_usd) "+
			"VALUES ($1,$2,$3,'d','d','succeeded','hi','t',$4,$4,$5,$6,$7,$8)",
		rid, sid, cid, now, taskID, inputTokens, outputTokens, costUSD)))
}
