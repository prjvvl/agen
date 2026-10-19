package store

import (
	"context"
	"encoding/json"
	"strconv"
)

// Change is a row that changed recently, as seen by the Hub's event stream.
type Change struct {
	// Kind: task, run, step, approval, instance or deployment.
	Kind       string
	ID         string
	Namespace  string
	Deployment string
	State      string
	TraceID    string
	// Version distinguishes successive changes of the same row.
	Version string
	// Steps (a model or tool call starting or ending): the run, the span
	// name (gen_ai.chat, agen.tool), the tool, and the agent a delegation
	// targets; State is "unfinished" while it runs, then its status.
	RunID, Name, Tool, Target string
	StartMs, EndMs            int64
}

// RecentChanges returns tasks, runs and approvals that changed since sinceMs
// (runs: those that recorded a span), and every instance and deployment, so
// the caller can compare them with what it saw before.
func (s *Store) RecentChanges(ctx context.Context, sinceMs int64) ([]Change, error) {
	var out []Change
	scan := func(kind, q string, args ...any) error {
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c := Change{Kind: kind}
			var version int64
			if err := rows.Scan(&c.ID, &c.Namespace, &c.Deployment, &c.State, &c.TraceID, &version); err != nil {
				return err
			}
			c.Version = c.State + "@" + strconv.FormatInt(version, 10)
			out = append(out, c)
		}
		return rows.Err()
	}
	// Lease renewals touch updated_ms every few seconds; a task changes when
	// its state or attempt does.
	if err := scan("task", "SELECT id, namespace, deployment, state, '', attempts FROM tasks WHERE updated_ms > $1", sinceMs); err != nil {
		return nil, err
	}
	if err := scan("run", "SELECT r.id, r.namespace, r.deployment, r.status, r.trace_id, s.last FROM runs r "+
		"JOIN (SELECT run_id, MAX(end_ms) AS last FROM spans WHERE end_ms > $1 GROUP BY run_id) s ON s.run_id = r.id", sinceMs); err != nil {
		return nil, err
	}
	// Runs the Hub ended (failed or cancelled tasks, orphaned runs) end no
	// span of their own.
	if err := scan("run", "SELECT id, namespace, deployment, status, trace_id, ended_ms FROM runs WHERE ended_ms > $1", sinceMs); err != nil {
		return nil, err
	}
	// Steps: spans opened or closed in the window (an open span is stored
	// with end_ms = start_ms and status "unfinished"). On a busy fleet the
	// newest are kept; they are sent oldest first.
	rows, err := s.db.QueryContext(ctx, "SELECT sp.span_id, r.namespace, r.deployment, sp.status, sp.trace_id, sp.run_id, sp.name, sp.start_ms, sp.end_ms, sp.attributes "+
		"FROM spans sp JOIN runs r ON r.id = sp.run_id WHERE sp.end_ms > $1 AND sp.name IN ('gen_ai.chat', 'agen.tool') ORDER BY sp.end_ms DESC LIMIT 2000", sinceMs)
	if err != nil {
		return nil, err
	}
	var steps []Change
	for rows.Next() {
		c := Change{Kind: "step"}
		var attrs string
		if err := rows.Scan(&c.ID, &c.Namespace, &c.Deployment, &c.State, &c.TraceID, &c.RunID, &c.Name, &c.StartMs, &c.EndMs, &attrs); err != nil {
			rows.Close()
			return nil, err
		}
		var a map[string]any
		_ = json.Unmarshal([]byte(attrs), &a)
		c.Tool, _ = a["gen_ai.tool.name"].(string)
		c.Target, _ = a["agen.delegate.target"].(string)
		c.Version = c.State + "@" + strconv.FormatInt(c.EndMs, 10)
		steps = append(steps, c)
	}
	rows.Close()
	for i := len(steps) - 1; i >= 0; i-- {
		out = append(out, steps[i])
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := scan("approval", "SELECT id, namespace, deployment, state, '', CASE WHEN decided_ms > created_ms THEN decided_ms ELSE created_ms END FROM approvals "+
		"WHERE created_ms > $1 OR decided_ms > $1 OR (state = 'expired' AND expires_ms > $1)", sinceMs); err != nil {
		return nil, err
	}
	if err := scan("instance", "SELECT id, namespace, deployment, state, '', running_tasks FROM instances"); err != nil {
		return nil, err
	}
	if err := scan("deployment", "SELECT name, namespace, name, CAST(desired AS TEXT), '', updated_ms FROM deployments"); err != nil {
		return nil, err
	}
	return out, nil
}
