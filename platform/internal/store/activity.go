package store

import (
	"context"
	"encoding/json"
)

// ActiveRun is an unfinished run, with its task's source and conversation.
type ActiveRun struct {
	RunRow
	Source          string
	ConversationKey string
	// The deployment of the parent run (empty without one).
	CallerNamespace, CallerDeployment string
}

// ActiveRuns returns the unfinished runs of deployments that exist (runs of
// deleted deployments are ended with them), oldest first.
func (s *Store) ActiveRuns(ctx context.Context, namespaces []string) ([]ActiveRun, error) {
	var q query
	q.in("r.namespace", namespaces)
	q.where = append(q.where, "r.ended_ms IS NULL", "r.status IN ('running', 'waiting_approval')")
	cols := "r.id, r.session_id, r.conversation_id, r.namespace, r.deployment, r.definition_digest, r.parent_run_id, r.root_run_id, r.status, r.input, r.output, r.error, " +
		"r.input_tokens, r.output_tokens, r.cost_usd, r.trace_id, r.started_ms, COALESCE(r.ended_ms, 0), r.task_id, r.requested_by, r.labels, r.step, " +
		"COALESCE(t.source, ''), COALESCE(t.conversation_key, ''), COALESCE(p.namespace, ''), COALESCE(p.deployment, '')"
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols+" FROM runs r JOIN deployments d ON d.namespace = r.namespace AND d.name = r.deployment "+
		"LEFT JOIN tasks t ON t.id = r.task_id AND r.task_id <> '' LEFT JOIN runs p ON p.id = r.parent_run_id AND r.parent_run_id <> ''"+q.clause()+" ORDER BY r.started_ms LIMIT 500", q.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActiveRun
	for rows.Next() {
		var a ActiveRun
		var labels string
		r := &a.RunRow
		if err := rows.Scan(&r.ID, &r.SessionID, &r.ConversationID, &r.Namespace, &r.Deployment, &r.DefinitionDigest, &r.ParentRunID, &r.RootRunID,
			&r.Status, &r.Input, &r.Output, &r.Error, &r.InputTokens, &r.OutputTokens, &r.CostUSD, &r.TraceID, &r.StartedMs, &r.EndedMs,
			&r.TaskID, &r.RequestedBy, &labels, &r.Step, &a.Source, &a.ConversationKey, &a.CallerNamespace, &a.CallerDeployment); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(labels), &r.Labels)
		out = append(out, a)
	}
	return out, rows.Err()
}

// OpenStep is what a run is doing: its latest span still open, and when it
// last finished a span.
type OpenStep struct {
	Name       string
	StartMs    int64
	Attributes map[string]any
	LastEndMs  int64
}

// OpenSteps returns, per run, the innermost open span (latest started) and
// the end of its last finished span.
func (s *Store) OpenSteps(ctx context.Context, runIDs []string) (map[string]OpenStep, error) {
	out := map[string]OpenStep{}
	if len(runIDs) == 0 {
		return out, nil
	}
	var q query
	q.in("run_id", runIDs)
	rows, err := s.db.QueryContext(ctx, "SELECT run_id, name, start_ms, attributes FROM spans"+q.clause()+" AND status = 'unfinished' AND name <> 'agen.run'", q.args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var run, name, attrs string
		var start int64
		if err := rows.Scan(&run, &name, &start, &attrs); err != nil {
			rows.Close()
			return nil, err
		}
		if cur := out[run]; start >= cur.StartMs {
			cur.Name, cur.StartMs, cur.Attributes = name, start, map[string]any{}
			_ = json.Unmarshal([]byte(attrs), &cur.Attributes)
			out[run] = cur
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, "SELECT run_id, MAX(end_ms) FROM spans"+q.clause()+" AND status <> 'unfinished' GROUP BY run_id", q.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var run string
		var end int64
		if err := rows.Scan(&run, &end); err != nil {
			return nil, err
		}
		cur := out[run]
		cur.LastEndMs = end
		out[run] = cur
	}
	return out, rows.Err()
}

// PendingApprovalsByRun returns the pending approval per run.
func (s *Store) PendingApprovalsByRun(ctx context.Context, runIDs []string) (map[string]Approval, error) {
	out := map[string]Approval{}
	if len(runIDs) == 0 {
		return out, nil
	}
	var q query
	q.in("run_id", runIDs)
	q.where = append(q.where, "state = 'pending'")
	rows, err := s.db.QueryContext(ctx, "SELECT id, run_id, created_ms FROM approvals"+q.clause()+" ORDER BY created_ms", q.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.ID, &a.RunID, &a.CreatedMs); err != nil {
			return nil, err
		}
		if _, ok := out[a.RunID]; !ok {
			out[a.RunID] = a
		}
	}
	return out, rows.Err()
}

// DeploymentQueue counts a deployment's waiting tasks.
type DeploymentQueue struct {
	Namespace, Deployment string
	Queued, Scheduled     int
	OldestQueuedMs        int64
}

// Queues returns waiting tasks per deployment: due ones (and the oldest of
// them) and scheduled ones.
func (s *Store) Queues(ctx context.Context, namespaces []string) ([]DeploymentQueue, error) {
	var q query
	q.in("namespace", namespaces)
	q.where = append(q.where, "state = 'queued'")
	now := q.arg(NowMs())
	rows, err := s.db.QueryContext(ctx, "SELECT namespace, deployment, "+
		"COALESCE(SUM(CASE WHEN not_before_ms <= "+now+" THEN 1 ELSE 0 END), 0), "+
		"COALESCE(SUM(CASE WHEN not_before_ms > "+now+" THEN 1 ELSE 0 END), 0), "+
		"COALESCE(MIN(CASE WHEN not_before_ms <= "+now+" THEN CASE WHEN not_before_ms > created_ms THEN not_before_ms ELSE created_ms END END), 0) "+
		"FROM tasks"+q.clause()+" GROUP BY namespace, deployment", q.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeploymentQueue
	for rows.Next() {
		var d DeploymentQueue
		if err := rows.Scan(&d.Namespace, &d.Deployment, &d.Queued, &d.Scheduled, &d.OldestQueuedMs); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Burst is a conversation that received many tasks recently.
type Burst struct {
	Namespace, Deployment, ConversationKey string
	Tasks                                  int
}

// Bursts returns conversations with at least min tasks created since sinceMs.
func (s *Store) Bursts(ctx context.Context, namespaces []string, sinceMs int64, min int) ([]Burst, error) {
	var q query
	q.in("namespace", namespaces)
	q.where = append(q.where, "conversation_key <> ''", "created_ms >= "+q.arg(sinceMs))
	rows, err := s.db.QueryContext(ctx, "SELECT namespace, deployment, conversation_key, COUNT(*) FROM tasks"+q.clause()+
		" GROUP BY namespace, deployment, conversation_key HAVING COUNT(*) >= "+q.arg(min), q.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Burst
	for rows.Next() {
		var b Burst
		if err := rows.Scan(&b.Namespace, &b.Deployment, &b.ConversationKey, &b.Tasks); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ModelCall is one finished model call (a gen_ai.chat span).
type ModelCall struct {
	Namespace, Deployment string
	Status                string
	DurationMs            int64
	EndMs                 int64
	Attributes            map[string]any
}

// ModelCalls returns finished model calls since sinceMs (at most limit,
// newest first).
func (s *Store) ModelCalls(ctx context.Context, namespaces []string, sinceMs int64, limit int) ([]ModelCall, error) {
	var q query
	q.in("r.namespace", namespaces)
	q.where = append(q.where, "sp.name = 'gen_ai.chat'", "sp.status <> 'unfinished'", "sp.end_ms >= "+q.arg(sinceMs))
	rows, err := s.db.QueryContext(ctx, "SELECT r.namespace, r.deployment, sp.status, sp.end_ms - sp.start_ms, sp.end_ms, sp.attributes FROM spans sp JOIN runs r ON r.id = sp.run_id"+
		q.clause()+" ORDER BY sp.end_ms DESC LIMIT "+q.arg(limit), q.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelCall
	for rows.Next() {
		var c ModelCall
		var attrs string
		if err := rows.Scan(&c.Namespace, &c.Deployment, &c.Status, &c.DurationMs, &c.EndMs, &attrs); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(attrs), &c.Attributes)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Entry is one run that no agent of the fleet asked for (its task's source:
// api, cron:x, webhook:x, reply:x; or an A2A call from outside), or the run
// of an async result going back to its caller.
type Entry struct {
	Namespace, Deployment string
	Source                string
	Status                string
}

// Entries returns runs started since sinceMs that have no parent run.
func (s *Store) Entries(ctx context.Context, namespaces []string, sinceMs int64) ([]Entry, error) {
	var q query
	q.in("r.namespace", namespaces)
	q.where = append(q.where, "r.parent_run_id = ''", "r.started_ms >= "+q.arg(sinceMs))
	rows, err := s.db.QueryContext(ctx, "SELECT r.namespace, r.deployment, COALESCE(t.source, ''), r.status FROM runs r "+
		"LEFT JOIN tasks t ON t.id = r.task_id AND r.task_id <> ''"+q.clause(), q.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Namespace, &e.Deployment, &e.Source, &e.Status); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
