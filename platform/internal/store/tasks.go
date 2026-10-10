package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// Task is a durable unit of work for a deployment.
type Task struct {
	ID             string
	Namespace      string
	Deployment     string
	Input          string
	State          string // queued | leased | running | succeeded | failed | cancelled
	Output         string
	Error          string
	InstanceID     string
	RunID          string
	Source         string
	Attempts       int
	IdempotencyKey string
	LeaseID        string
	LeaseNestID    string
	LeaseExpiresMs int64
	ParentTaskID   string
	ParentRunID    string
	RootRunID      string
	Depth          int
	Traceparent    string
	// SubmittedBy is the principal that caused the task (see migration 7).
	SubmittedBy string
	CreatedMs   int64
	UpdatedMs   int64
	// ConversationKey: tasks of a deployment with the same key continue one
	// conversation; they are leased one at a time, oldest first. Keys are
	// prefixed with their source ("api:", "webhook:<trigger>:").
	ConversationKey string
	Labels          map[string]string
}

// CheckLabels enforces the limits on task labels: at most 32, keys of 1-63
// characters from [A-Za-z0-9._/-], values of at most 256 bytes.
func CheckLabels(labels map[string]string) error {
	if len(labels) > 32 {
		return fmt.Errorf("at most 32 labels, got %d", len(labels))
	}
	for k, v := range labels {
		if !labelKey.MatchString(k) {
			return fmt.Errorf("label key %q must be 1-63 characters of letters, digits, '.', '_', '/' or '-'", k)
		}
		if len(v) > 256 {
			return fmt.Errorf("label %q: value longer than 256 bytes", k)
		}
	}
	return nil
}

var labelKey = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,63}$`)

// Terminal reports whether the task has finished.
func (t Task) Terminal() bool {
	return t.State == "succeeded" || t.State == "failed" || t.State == "cancelled"
}

const taskCols = "id, namespace, deployment, input, state, output, error, instance_id, run_id, source, attempts, idempotency_key, lease_id, lease_nest_id, lease_expires_ms, parent_task_id, parent_run_id, root_run_id, depth, traceparent, created_ms, updated_ms, submitted_by, conversation_key, labels"

func scanTask(r interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var labels string
	err := r.Scan(&t.ID, &t.Namespace, &t.Deployment, &t.Input, &t.State, &t.Output, &t.Error, &t.InstanceID, &t.RunID, &t.Source,
		&t.Attempts, &t.IdempotencyKey, &t.LeaseID, &t.LeaseNestID, &t.LeaseExpiresMs, &t.ParentTaskID, &t.ParentRunID, &t.RootRunID,
		&t.Depth, &t.Traceparent, &t.CreatedMs, &t.UpdatedMs, &t.SubmittedBy, &t.ConversationKey, &labels)
	if err == nil {
		err = json.Unmarshal([]byte(labels), &t.Labels)
	}
	return t, err
}

func scanTasks(rows *sql.Rows) ([]Task, error) {
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SubmitTask queues a task. With an idempotency key, resubmitting returns the
// existing task instead of creating a new one.
func (s *Store) SubmitTask(ctx context.Context, t Task) (Task, error) {
	if t.ID == "" {
		t.ID = NewID()
	}
	if t.Source == "" {
		t.Source = "api"
	}
	labels, err := json.Marshal(t.Labels)
	if err != nil {
		return t, err
	}
	if t.Labels == nil {
		labels = []byte("{}")
	}
	now := NowMs()
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO tasks (id, namespace, deployment, input, state, source, idempotency_key, parent_task_id, parent_run_id, root_run_id, depth, traceparent, created_ms, updated_ms, submitted_by, conversation_key, labels) "+
			"VALUES ($1, $2, $3, $4, 'queued', $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)",
		t.ID, t.Namespace, t.Deployment, t.Input, t.Source, t.IdempotencyKey, t.ParentTaskID, t.ParentRunID, t.RootRunID, t.Depth, t.Traceparent, now, now, t.SubmittedBy,
		t.ConversationKey, string(labels))
	if isUnique(err) && t.IdempotencyKey != "" {
		return scanTask(s.db.QueryRowContext(ctx, "SELECT "+taskCols+" FROM tasks WHERE namespace = $1 AND deployment = $2 AND idempotency_key = $3",
			t.Namespace, t.Deployment, t.IdempotencyKey))
	}
	if err != nil {
		return t, err
	}
	return s.GetTask(ctx, t.ID)
}

// GetTask returns one task.
func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, "SELECT "+taskCols+" FROM tasks WHERE id = $1", id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// ListTasks filters by namespace, deployment and state ("" = any), newest first.
func (s *Store) ListTasks(ctx context.Context, ns, deployment, state string, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+taskCols+" FROM tasks WHERE ($1 = '' OR namespace = $1) AND ($2 = '' OR deployment = $2) AND ($3 = '' OR state = $3) ORDER BY created_ms DESC, id DESC LIMIT $4",
		ns, deployment, state, limit)
	if err != nil {
		return nil, err
	}
	return scanTasks(rows)
}

// LeaseTasks atomically moves up to max queued tasks of a deployment to
// "leased" for a nest, each with a fresh fencing lease id. A task with a
// conversation key waits while an older task with the same key is queued or
// in flight, so a conversation's tasks run one at a time, in order.
func (s *Store) LeaseTasks(ctx context.Context, nestID, ns, deployment string, max int, leaseMs int64) ([]Task, error) {
	if max <= 0 {
		return nil, nil
	}
	lock := ""
	if s.dialect == Postgres {
		lock = " FOR UPDATE SKIP LOCKED"
	}
	var out []Task
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT id FROM tasks t WHERE namespace = $1 AND deployment = $2 AND state = 'queued' AND (conversation_key = '' OR NOT EXISTS ("+
			"SELECT 1 FROM tasks o WHERE o.namespace = t.namespace AND o.deployment = t.deployment AND o.conversation_key = t.conversation_key AND o.id <> t.id AND "+
			"(o.state IN ('leased', 'running') OR (o.state = 'queued' AND (o.created_ms < t.created_ms OR (o.created_ms = t.created_ms AND o.id < t.id)))))) "+
			"ORDER BY created_ms, id LIMIT $3"+lock, ns, deployment, max)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		now := NowMs()
		for _, id := range ids {
			t, err := scanTask(tx.QueryRowContext(ctx,
				"UPDATE tasks SET state = 'leased', lease_id = $1, lease_nest_id = $2, lease_expires_ms = $3, attempts = attempts + 1, updated_ms = $4 "+
					"WHERE id = $5 AND state = 'queued' RETURNING "+taskCols,
				NewID(), nestID, now+leaseMs, now, id))
			if errors.Is(err, sql.ErrNoRows) {
				continue // taken concurrently
			}
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return nil
	})
	return out, err
}

// ExtendLease extends a lease the caller still holds.
func (s *Store) ExtendLease(ctx context.Context, taskID, leaseID string, leaseMs int64) error {
	now := NowMs()
	res, err := s.db.ExecContext(ctx, "UPDATE tasks SET lease_expires_ms = $1, updated_ms = $2 WHERE id = $3 AND lease_id = $4 AND state IN ('leased', 'running')",
		now+leaseMs, now, taskID, leaseID)
	return fenced(res, err)
}

// StartTask marks a leased task as running on an instance.
func (s *Store) StartTask(ctx context.Context, taskID, leaseID, instanceID string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE tasks SET state = 'running', instance_id = $1, updated_ms = $2 WHERE id = $3 AND lease_id = $4 AND state IN ('leased', 'running')",
		instanceID, NowMs(), taskID, leaseID)
	return fenced(res, err)
}

// CompleteTask records a result. Fails with ErrFenced unless leaseID is the
// task's current lease (a partitioned nest cannot complete a re-leased task).
func (s *Store) CompleteTask(ctx context.Context, taskID, leaseID string, success bool, output, errMsg, runID, instanceID string) error {
	state := "failed"
	if success {
		state = "succeeded"
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		t, err := scanTask(tx.QueryRowContext(ctx,
			"UPDATE tasks SET state = $1, output = $2, error = $3, run_id = $4, instance_id = $5, lease_expires_ms = 0, updated_ms = $6 "+
				"WHERE id = $7 AND lease_id = $8 AND state IN ('leased', 'running') RETURNING "+taskCols,
			state, output, errMsg, runID, instanceID, NowMs(), taskID, leaseID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrFenced
		}
		if err != nil || success {
			return err
		}
		return endTaskRun(ctx, tx, t, "failed", errMsg)
	})
}

// RequeueExpiredLeases returns tasks whose lease expired to the queue. A task
// that has already been leased maxAttempts times fails instead, so a task
// that keeps killing its host does not loop forever (maxAttempts <= 0: no cap).
func (s *Store) RequeueExpiredLeases(ctx context.Context, maxAttempts int) (int64, error) {
	now := NowMs()
	var n int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT "+taskCols+" FROM tasks WHERE state IN ('leased', 'running') AND lease_expires_ms < $1", now)
		if err != nil {
			return err
		}
		expired, err := scanTasks(rows)
		if err != nil {
			return err
		}
		for _, t := range expired {
			where := "nest " + t.LeaseNestID
			if t.InstanceID != "" {
				where = "instance " + t.InstanceID + " on " + where
			}
			var res sql.Result
			var msg, level string
			if maxAttempts > 0 && t.Attempts >= maxAttempts {
				res, err = tx.ExecContext(ctx, "UPDATE tasks SET state = 'failed', error = $1, lease_expires_ms = 0, updated_ms = $2 WHERE id = $3 AND lease_id = $4",
					"task lease expired "+itoa(maxAttempts)+" times (the instance running it crashed or became unreachable)", now, t.ID, t.LeaseID)
				msg, level = fmt.Sprintf("task %s failed: its lease on %s expired on attempt %d of %d", t.ID, where, t.Attempts, maxAttempts), "error"
				if err == nil {
					err = endTaskRun(ctx, tx, t, "failed", "the task failed: its lease expired too many times")
				}
			} else {
				res, err = tx.ExecContext(ctx, "UPDATE tasks SET state = 'queued', lease_id = '', lease_nest_id = '', lease_expires_ms = 0, updated_ms = $1 WHERE id = $2 AND lease_id = $3",
					now, t.ID, t.LeaseID)
				msg, level = fmt.Sprintf("task %s requeued: its lease on %s expired on attempt %d (the instance crashed or became unreachable); the next attempt resumes its run", t.ID, where, t.Attempts), "warn"
			}
			if err != nil {
				return err
			}
			if k, _ := res.RowsAffected(); k == 0 {
				continue
			}
			n++
			if err := appendLog(ctx, tx, t.InstanceID, t.Namespace, t.Deployment, level, msg); err != nil {
				return err
			}
		}
		return nil
	})
	return n, err
}

// ReleaseTask gives a leased task back to the queue before its lease expires
// (the host could not take it). notStarted also refunds the attempt.
func (s *Store) ReleaseTask(ctx context.Context, taskID, leaseID string, notStarted bool) error {
	refund := 0
	if notStarted {
		refund = 1
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		t, err := scanTask(tx.QueryRowContext(ctx, "SELECT "+taskCols+" FROM tasks WHERE id = $1", taskID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrFenced
		}
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			"UPDATE tasks SET state = 'queued', lease_id = '', lease_nest_id = '', lease_expires_ms = 0, instance_id = '', attempts = attempts - $1, updated_ms = $2 "+
				"WHERE id = $3 AND lease_id = $4 AND state IN ('leased', 'running')",
			refund, NowMs(), taskID, leaseID)
		if err := fenced(res, err); err != nil {
			return err
		}
		msg := fmt.Sprintf("task %s returned to the queue: the instance could not take it now", taskID)
		level := "info"
		if !notStarted {
			msg = fmt.Sprintf("task %s returned to the queue after attempt %d: the instance running it failed; the next attempt resumes its run", taskID, t.Attempts)
			level = "warn"
		}
		return appendLog(ctx, tx, t.InstanceID, t.Namespace, t.Deployment, level, msg)
	})
}

// CancelTask cancels a task that has not finished.
func (s *Store) CancelTask(ctx context.Context, id string) (Task, error) {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		t, err := scanTask(tx.QueryRowContext(ctx, "UPDATE tasks SET state = 'cancelled', updated_ms = $1 WHERE id = $2 AND state IN ('queued', 'leased', 'running') RETURNING "+taskCols, NowMs(), id))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return endTaskRun(ctx, tx, t, "cancelled", "cancelled")
	})
	if err != nil {
		return Task{}, err
	}
	return s.GetTask(ctx, id)
}

// endTaskRun ends the unfinished run of a task the Hub has finished, and
// fences off its owner (a host may still be running it). Otherwise the run
// would keep its conversation open, and the next task with the same
// conversation key could never start.
func endTaskRun(ctx context.Context, tx *sql.Tx, t Task, status, msg string) error {
	_, err := tx.ExecContext(ctx, "UPDATE runs SET status = $1, error = $2, ended_ms = $3, epoch = epoch + 1 "+
		"WHERE namespace = $4 AND deployment = $5 AND task_id = $6 AND ended_ms IS NULL",
		status, msg, NowMs(), t.Namespace, t.Deployment, t.ID)
	return err
}

// QueueStats counts unfinished tasks of a deployment.
type QueueStats struct{ Queued, InFlight int }

// Queue returns queued and in-flight (leased or running) counts.
func (s *Store) Queue(ctx context.Context, ns, deployment string) (QueueStats, error) {
	var q QueueStats
	err := s.db.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(CASE WHEN state = 'queued' THEN 1 ELSE 0 END), 0), COALESCE(SUM(CASE WHEN state IN ('leased', 'running') THEN 1 ELSE 0 END), 0) FROM tasks WHERE namespace = $1 AND deployment = $2 AND state IN ('queued', 'leased', 'running')",
		ns, deployment).Scan(&q.Queued, &q.InFlight)
	return q, err
}

func fenced(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrFenced
	}
	return nil
}
