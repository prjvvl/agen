package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

// Event is one entry of the event log.
type Event struct {
	Seq          int64
	Namespace    string
	Type         string
	Deployment   string
	TaskID       string
	ParentTaskID string
	Labels       map[string]string
	// Severity: info, warning or critical.
	Severity string
	// Data: details per type, a JSON object.
	Data map[string]any
	AtMs int64
	// DedupeKey: an event with a key already in the log is not written
	// again (for things announced once: thresholds, warnings).
	DedupeKey string
}

// Severities in order.
var severityRank = map[string]int{"info": 0, "warning": 1, "critical": 2}

// SeverityAtLeast reports whether s is at least min ("" = any).
func SeverityAtLeast(s, min string) bool {
	return min == "" || severityRank[s] >= severityRank[min]
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// appendEvent writes e in the caller's transaction. It takes the next seq
// from the event_seq row, so it should be the transaction's last write: the
// row stays locked until commit, which keeps seq order equal to commit order.
func appendEvent(ctx context.Context, ex execer, e Event) error {
	_, err := insertEvent(ctx, ex, e)
	return err
}

// insertEvent is appendEvent; it reports false when e's DedupeKey was
// already in the log.
func insertEvent(ctx context.Context, ex execer, e Event) (bool, error) {
	if e.Severity == "" {
		e.Severity = "info"
	}
	labels, _ := json.Marshal(e.Labels)
	if e.Labels == nil {
		labels = []byte("{}")
	}
	data, _ := json.Marshal(e.Data)
	if e.Data == nil {
		data = []byte("{}")
	}
	var seq int64
	if err := ex.QueryRowContext(ctx, "UPDATE event_seq SET seq = seq + 1 WHERE id = 1 RETURNING seq").Scan(&seq); err != nil {
		return false, err
	}
	if e.AtMs == 0 {
		e.AtMs = NowMs()
	}
	// A duplicate key leaves a gap in seq; readers do not mind gaps.
	res, err := ex.ExecContext(ctx, "INSERT INTO events (seq, namespace, type, deployment, task_id, parent_task_id, labels, severity, data, at_ms, dedupe_key) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) "+
		"ON CONFLICT (dedupe_key) WHERE dedupe_key <> '' DO NOTHING",
		seq, e.Namespace, e.Type, e.Deployment, e.TaskID, e.ParentTaskID, string(labels), e.Severity, string(data), e.AtMs, e.DedupeKey)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// AppendEvent writes one event on its own.
func (s *Store) AppendEvent(ctx context.Context, e Event) error {
	return s.inTx(ctx, func(tx *sql.Tx) error { return appendEvent(ctx, tx, e) })
}

// AppendEventOnce writes e unless an event with its DedupeKey is already in
// the log; it reports whether it was written.
func (s *Store) AppendEventOnce(ctx context.Context, e Event) (bool, error) {
	var ok bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		ok, err = insertEvent(ctx, tx, e)
		return err
	})
	return ok, err
}

// taskEvent is an event about task t.
func taskEvent(t Task, typ, severity string, data map[string]any) Event {
	return Event{Namespace: t.Namespace, Type: typ, Deployment: t.Deployment, TaskID: t.ID, ParentTaskID: t.ParentTaskID,
		Labels: t.Labels, Severity: severity, Data: data}
}

// taskFinished are the events of a task that just ended: task.finished, and
// task.needs_input when its outcome asks for an answer.
func taskFinished(ctx context.Context, ex execer, t Task) error {
	data := map[string]any{"state": t.State, "source": t.Source}
	severity := "info"
	if t.State == "failed" {
		severity = "warning"
		data["error"] = truncate(t.Error, 500)
	}
	var outcome map[string]any
	if t.Outcome != "" && json.Unmarshal([]byte(t.Outcome), &outcome) == nil {
		short := map[string]any{"status": outcome["status"]}
		for _, k := range []string{"reason", "inputFrom"} {
			if v, ok := outcome[k]; ok {
				short[k] = v
			}
		}
		if s, ok := outcome["summary"].(string); ok {
			short["summary"] = truncate(s, 500)
		}
		data["outcome"] = short
	}
	if t.RunID != "" {
		data["runId"] = t.RunID
	}
	if err := appendEvent(ctx, ex, taskEvent(t, "task.finished", severity, data)); err != nil {
		return err
	}
	if outcome["status"] == "needs_input" {
		return appendEvent(ctx, ex, taskEvent(t, "task.needs_input", "warning", map[string]any{"summary": data["outcome"].(map[string]any)["summary"], "inputFrom": outcome["inputFrom"]}))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// EventFilter selects events for ListEvents.
type EventFilter struct {
	// Namespaces: allowed namespaces (nil = all).
	Namespaces []string
	AfterSeq   int64
	// Types: exact types, or prefixes ending in ".".
	Types      []string
	Deployment string
	Labels     map[string]string
	Severity   string
	Limit      int
}

// Matches reports whether e passes the filter's type, deployment, label and
// severity conditions (not namespaces or seq).
func (f EventFilter) Matches(e Event) bool {
	if len(f.Types) > 0 {
		ok := false
		for _, t := range f.Types {
			if t == e.Type || (strings.HasSuffix(t, ".") && strings.HasPrefix(e.Type, t)) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if f.Deployment != "" && e.Deployment != f.Deployment {
		return false
	}
	for k, v := range f.Labels {
		if e.Labels[k] != v {
			return false
		}
	}
	return SeverityAtLeast(e.Severity, f.Severity)
}

// ListEvents returns matching events after f.AfterSeq, oldest first, and the
// last seq it looked at (so a caller skips past events that did not match).
func (s *Store) ListEvents(ctx context.Context, f EventFilter) ([]Event, int64, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	next := f.AfterSeq
	var out []Event
	// Scan in pages; type and label filters are applied here, so a page may
	// match nothing.
	for page := 0; page < 20 && len(out) < limit; page++ {
		q := "SELECT seq, namespace, type, deployment, task_id, parent_task_id, labels, severity, data, at_ms FROM events WHERE seq > $1"
		args := []any{next}
		if f.Namespaces != nil {
			if len(f.Namespaces) == 0 {
				return nil, next, nil
			}
			ph := make([]string, len(f.Namespaces))
			for i, ns := range f.Namespaces {
				args = append(args, ns)
				ph[i] = "$" + itoa(len(args))
			}
			q += " AND namespace IN (" + strings.Join(ph, ", ") + ")"
		}
		if f.Deployment != "" {
			args = append(args, f.Deployment)
			q += " AND deployment = $" + itoa(len(args))
		}
		q += " ORDER BY seq LIMIT 500"
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, next, err
		}
		n := 0
		for rows.Next() {
			var e Event
			var labels, data string
			if err := rows.Scan(&e.Seq, &e.Namespace, &e.Type, &e.Deployment, &e.TaskID, &e.ParentTaskID, &labels, &e.Severity, &data, &e.AtMs); err != nil {
				rows.Close()
				return nil, next, err
			}
			n++
			_ = json.Unmarshal([]byte(labels), &e.Labels)
			_ = json.Unmarshal([]byte(data), &e.Data)
			if f.Matches(e) {
				out = append(out, e)
			}
			next = e.Seq
			if len(out) == limit {
				break
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, next, err
		}
		if n < 500 {
			break
		}
	}
	return out, next, nil
}

// LastEventSeq is the seq of the newest event (0 when there is none).
func (s *Store) LastEventSeq(ctx context.Context) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, "SELECT seq FROM event_seq WHERE id = 1").Scan(&seq)
	return seq, err
}

// PruneEvents deletes events older than beforeMs.
func (s *Store) PruneEvents(ctx context.Context, beforeMs int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM events WHERE at_ms < $1", beforeMs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
