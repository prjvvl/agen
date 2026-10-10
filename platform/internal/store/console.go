package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// RunFilter selects runs for ListRunsFiltered.
type RunFilter struct {
	// Namespaces to include; empty means all.
	Namespaces []string
	Deployment string
	Status     string
	TaskID     string
	// RootsOnly keeps runs that started a trace (no parent run).
	RootsOnly bool
	// Labels the run must carry, all of them.
	Labels  map[string]string
	SinceMs int64
	// Before is a page cursor: only runs older than (BeforeMs, BeforeID).
	BeforeMs int64
	BeforeID string
	Limit    int
}

// query builds SQL with numbered placeholders in order.
type query struct {
	where []string
	args  []any
}

func (q *query) arg(v any) string {
	q.args = append(q.args, v)
	return fmt.Sprintf("$%d", len(q.args))
}

func (q *query) in(col string, vals []string) {
	if len(vals) == 0 {
		return
	}
	ph := make([]string, len(vals))
	for i, v := range vals {
		ph[i] = q.arg(v)
	}
	q.where = append(q.where, col+" IN ("+strings.Join(ph, ", ")+")")
}

func (q *query) clause() string {
	if len(q.where) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(q.where, " AND ")
}

// likeEscape escapes LIKE wildcards; queries use ESCAPE '!'.
func likeEscape(s string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(s)
}

// ListRunsFiltered lists runs newest first.
func (s *Store) ListRunsFiltered(ctx context.Context, f RunFilter) ([]RunRow, error) {
	var q query
	q.in("namespace", f.Namespaces)
	if f.Deployment != "" {
		q.where = append(q.where, "deployment = "+q.arg(f.Deployment))
	}
	if f.Status != "" {
		q.where = append(q.where, "status = "+q.arg(f.Status))
	}
	if f.TaskID != "" {
		q.where = append(q.where, "task_id = "+q.arg(f.TaskID))
	}
	if f.RootsOnly {
		q.where = append(q.where, "parent_run_id = ''")
	}
	for k, v := range f.Labels {
		// Labels are stored as compact JSON objects ({"k":"v"}).
		kj, _ := json.Marshal(k)
		vj, _ := json.Marshal(v)
		q.where = append(q.where, "labels LIKE "+q.arg("%"+likeEscape(string(kj)+":"+string(vj))+"%")+" ESCAPE '!'")
	}
	if f.SinceMs > 0 {
		q.where = append(q.where, "started_ms >= "+q.arg(f.SinceMs))
	}
	if f.BeforeMs > 0 {
		ms, id := q.arg(f.BeforeMs), q.arg(f.BeforeID)
		q.where = append(q.where, "(started_ms < "+ms+" OR (started_ms = "+ms+" AND id < "+id+"))")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	return s.queryRuns(ctx, "SELECT "+runCols+" FROM runs"+q.clause()+" ORDER BY started_ms DESC, id DESC LIMIT "+q.arg(limit), q.args...)
}

// TreeUsage is the usage of a run tree.
type TreeUsage struct {
	Runs         int
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
}

// TreeUsages sums usage over each root run's tree (the root included).
func (s *Store) TreeUsages(ctx context.Context, roots []string) (map[string]TreeUsage, error) {
	out := map[string]TreeUsage{}
	if len(roots) == 0 {
		return out, nil
	}
	var q query
	q.in("root_run_id", roots)
	rows, err := s.db.QueryContext(ctx, "SELECT root_run_id, COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cost_usd), 0) FROM runs"+
		q.clause()+" GROUP BY root_run_id", q.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var u TreeUsage
		if err := rows.Scan(&id, &u.Runs, &u.InputTokens, &u.OutputTokens, &u.CostUSD); err != nil {
			return nil, err
		}
		out[id] = u
	}
	return out, rows.Err()
}

// MessageRow is one stored conversation message (Body is the engine's JSON).
type MessageRow struct {
	Seq       int64
	RunID     string
	Body      string
	CreatedMs int64
}

// Transcript returns a run's messages, or with history also the earlier
// messages of its conversation, in order.
func (s *Store) Transcript(ctx context.Context, run RunRow, history bool) ([]MessageRow, error) {
	q, arg := "SELECT seq, run_id, body, created_ms FROM messages WHERE run_id = $1 ORDER BY seq", run.ID
	if history {
		q = "SELECT seq, run_id, body, created_ms FROM messages WHERE conversation_id = $1 AND seq <= " +
			"COALESCE((SELECT MAX(seq) FROM messages WHERE run_id = $2), 0) ORDER BY seq"
	}
	args := []any{arg}
	if history {
		args = []any{run.ConversationID, run.ID}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageRow
	for rows.Next() {
		var m MessageRow
		if err := rows.Scan(&m.Seq, &m.RunID, &m.Body, &m.CreatedMs); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RunStat is the part of a run that metrics need.
type RunStat struct {
	Namespace    string
	Deployment   string
	Status       string
	StartedMs    int64
	EndedMs      int64
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
}

// RunStats returns runs started since sinceMs (in namespaces; empty = all).
func (s *Store) RunStats(ctx context.Context, namespaces []string, sinceMs int64) ([]RunStat, error) {
	var q query
	q.in("namespace", namespaces)
	q.where = append(q.where, "started_ms >= "+q.arg(sinceMs))
	rows, err := s.db.QueryContext(ctx, "SELECT namespace, deployment, status, started_ms, COALESCE(ended_ms, 0), input_tokens, output_tokens, cost_usd FROM runs"+q.clause(), q.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunStat
	for rows.Next() {
		var r RunStat
		if err := rows.Scan(&r.Namespace, &r.Deployment, &r.Status, &r.StartedMs, &r.EndedMs, &r.InputTokens, &r.OutputTokens, &r.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CallStat is one delegated call between deployments.
type CallStat struct {
	FromNamespace, FromDeployment string
	ToNamespace, ToDeployment     string
	Status                        string
}

// CallStats returns delegated runs started since sinceMs with the deployment
// of their parent run. A call is included when either end is in namespaces.
func (s *Store) CallStats(ctx context.Context, namespaces []string, sinceMs int64) ([]CallStat, error) {
	var q query
	q.where = append(q.where, "c.parent_run_id <> ''", "c.started_ms >= "+q.arg(sinceMs))
	if len(namespaces) > 0 {
		ph := make([]string, len(namespaces))
		for i, ns := range namespaces {
			ph[i] = q.arg(ns)
		}
		list := strings.Join(ph, ", ")
		q.where = append(q.where, "(c.namespace IN ("+list+") OR p.namespace IN ("+list+"))")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT p.namespace, p.deployment, c.namespace, c.deployment, c.status FROM runs c JOIN runs p ON p.id = c.parent_run_id"+q.clause(), q.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CallStat
	for rows.Next() {
		var c CallStat
		if err := rows.Scan(&c.FromNamespace, &c.FromDeployment, &c.ToNamespace, &c.ToDeployment, &c.Status); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PendingApprovals returns the pending approval id per task, for tasks that
// wait for one.
func (s *Store) PendingApprovals(ctx context.Context, taskIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	var q query
	q.in("r.task_id", taskIDs)
	q.where = append(q.where, "a.state = 'pending'")
	rows, err := s.db.QueryContext(ctx, "SELECT r.task_id, a.id FROM approvals a JOIN runs r ON r.id = a.run_id"+q.clause()+" ORDER BY a.created_ms", q.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var task, id string
		if err := rows.Scan(&task, &id); err != nil {
			return nil, err
		}
		if _, ok := out[task]; !ok {
			out[task] = id
		}
	}
	return out, rows.Err()
}

// PruneObservability deletes spans of runs that ended before beforeMs, and
// log lines and trigger events older than it. Runs and messages stay: they
// are conversation state.
func (s *Store) PruneObservability(ctx context.Context, beforeMs int64) (int64, error) {
	var total int64
	for _, q := range []string{
		"DELETE FROM spans WHERE end_ms < $1 AND run_id IN (SELECT id FROM runs WHERE ended_ms IS NOT NULL AND ended_ms < $1)",
		"DELETE FROM logs WHERE time_ms < $1",
		"DELETE FROM trigger_events WHERE recorded_ms < $1",
	} {
		res, err := s.db.ExecContext(ctx, q, beforeMs)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// ---- notification targets ----

// NotificationTarget is where the Hub POSTs a namespace's events.
type NotificationTarget struct {
	Namespace string
	Name      string
	URL       string
	Events    []string
	CreatedMs int64
}

// SetNotificationTarget stores (or replaces) a target with a new signing
// secret, which it returns.
func (s *Store) SetNotificationTarget(ctx context.Context, t NotificationTarget) (NotificationTarget, string, error) {
	secret := NewSecret("agen_ntf_")
	sealed, err := s.seal(secret)
	if err != nil {
		return t, "", err
	}
	if t.Events == nil {
		t.Events = []string{}
	}
	events, _ := json.Marshal(t.Events)
	t.CreatedMs = NowMs()
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM notification_targets WHERE namespace = $1 AND name = $2", t.Namespace, t.Name); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO notification_targets (namespace, name, url, events, secret, created_ms) VALUES ($1, $2, $3, $4, $5, $6)",
			t.Namespace, t.Name, t.URL, string(events), sealed, t.CreatedMs)
		return err
	})
	return t, secret, err
}

// ListNotificationTargets lists targets ("" = every namespace).
func (s *Store) ListNotificationTargets(ctx context.Context, ns string) ([]NotificationTarget, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT namespace, name, url, events, created_ms FROM notification_targets WHERE ($1 = '' OR namespace = $1) ORDER BY namespace, name", ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotificationTarget
	for rows.Next() {
		var t NotificationTarget
		var events string
		if err := rows.Scan(&t.Namespace, &t.Name, &t.URL, &events, &t.CreatedMs); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(events), &t.Events)
		out = append(out, t)
	}
	return out, rows.Err()
}

// NotificationSecret returns a target's signing secret.
func (s *Store) NotificationSecret(ctx context.Context, ns, name string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT secret FROM notification_targets WHERE namespace = $1 AND name = $2", ns, name).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return s.unseal(v)
}

// DeleteNotificationTarget removes a target.
func (s *Store) DeleteNotificationTarget(ctx context.Context, ns, name string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM notification_targets WHERE namespace = $1 AND name = $2", ns, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
