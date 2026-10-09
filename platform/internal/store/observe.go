package store

import (
	"context"
	"encoding/json"
)

// Read models over the engine's run-data tables (written by agen-host through
// the Rust storage port; see docs/architecture.md §7).

// RunRow is a run as recorded by the engine.
type RunRow struct {
	ID               string
	SessionID        string
	ConversationID   string
	Namespace        string
	Deployment       string
	DefinitionDigest string
	ParentRunID      string
	RootRunID        string
	TaskID           string
	Status           string
	Input            string
	Output           string
	Error            string
	InputTokens      int64
	OutputTokens     int64
	CostUSD          float64
	TraceID          string
	StartedMs        int64
	EndedMs          int64 // 0 while running
	// RequestedBy: who asked for a run that is not a Hub task (from a
	// verified A2A call token).
	RequestedBy string
}

const runCols = "id, session_id, conversation_id, namespace, deployment, definition_digest, parent_run_id, root_run_id, status, input, output, error, input_tokens, output_tokens, cost_usd, trace_id, started_ms, COALESCE(ended_ms, 0), task_id, requested_by"

func (s *Store) queryRuns(ctx context.Context, q string, args ...any) ([]RunRow, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunRow
	for rows.Next() {
		var r RunRow
		if err := rows.Scan(&r.ID, &r.SessionID, &r.ConversationID, &r.Namespace, &r.Deployment, &r.DefinitionDigest, &r.ParentRunID,
			&r.RootRunID, &r.Status, &r.Input, &r.Output, &r.Error, &r.InputTokens, &r.OutputTokens, &r.CostUSD, &r.TraceID,
			&r.StartedMs, &r.EndedMs, &r.TaskID, &r.RequestedBy); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRuns lists runs, newest first ("" = any namespace/deployment).
func (s *Store) ListRuns(ctx context.Context, ns, deployment string, limit int) ([]RunRow, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.queryRuns(ctx, "SELECT "+runCols+" FROM runs WHERE ($1 = '' OR namespace = $1) AND ($2 = '' OR deployment = $2) ORDER BY started_ms DESC, id DESC LIMIT $3",
		ns, deployment, limit)
}

// GetRun returns one run.
func (s *Store) GetRun(ctx context.Context, id string) (RunRow, error) {
	list, err := s.queryRuns(ctx, "SELECT "+runCols+" FROM runs WHERE id = $1", id)
	if err != nil {
		return RunRow{}, err
	}
	if len(list) == 0 {
		return RunRow{}, ErrNotFound
	}
	return list[0], nil
}

// SpanRow is one span.
type SpanRow struct {
	SpanID       string
	TraceID      string
	ParentSpanID string
	RunID        string
	Name         string
	StartMs      int64
	EndMs        int64
	Status       string
	Attributes   json.RawMessage
}

// Trace returns every span and run of a trace (across agents).
func (s *Store) Trace(ctx context.Context, traceID string) ([]SpanRow, []RunRow, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT span_id, trace_id, parent_span_id, run_id, name, start_ms, end_ms, status, attributes FROM spans WHERE trace_id = $1 ORDER BY start_ms, seq, span_id", traceID)
	if err != nil {
		return nil, nil, err
	}
	var spans []SpanRow
	for rows.Next() {
		var sp SpanRow
		var attrs string
		if err := rows.Scan(&sp.SpanID, &sp.TraceID, &sp.ParentSpanID, &sp.RunID, &sp.Name, &sp.StartMs, &sp.EndMs, &sp.Status, &attrs); err != nil {
			rows.Close()
			return nil, nil, err
		}
		sp.Attributes = json.RawMessage(attrs)
		spans = append(spans, sp)
	}
	rows.Close()
	runs, err := s.queryRuns(ctx, "SELECT "+runCols+" FROM runs WHERE trace_id = $1 ORDER BY started_ms", traceID)
	return spans, runs, err
}

// LogRow is one operational log line.
type LogRow struct {
	InstanceID string
	TimeMs     int64
	Level      string
	Message    string
}

// Logs returns log lines, oldest first, after sinceMs.
func (s *Store) Logs(ctx context.Context, ns, deployment, instanceID string, sinceMs int64, limit int) ([]LogRow, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT instance_id, time_ms, level, message FROM (SELECT id, instance_id, time_ms, level, message FROM logs "+
			"WHERE ($1 = '' OR namespace = $1) AND ($2 = '' OR deployment = $2) AND ($3 = '' OR instance_id = $3) AND time_ms > $4 "+
			"ORDER BY time_ms DESC, id DESC LIMIT $5) recent ORDER BY time_ms, id",
		ns, deployment, instanceID, sinceMs, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogRow
	for rows.Next() {
		var l LogRow
		if err := rows.Scan(&l.InstanceID, &l.TimeMs, &l.Level, &l.Message); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Usage sums model usage of a deployment's runs started since sinceMs.
func (s *Store) Usage(ctx context.Context, ns, deployment string, sinceMs int64) (inputTokens, outputTokens int64, costUSD float64, err error) {
	err = s.db.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cost_usd), 0) FROM runs WHERE namespace = $1 AND deployment = $2 AND started_ms >= $3",
		ns, deployment, sinceMs).Scan(&inputTokens, &outputTokens, &costUSD)
	return
}
