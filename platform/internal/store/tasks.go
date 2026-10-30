package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
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
	// NotBeforeMs: not leased before this time (0: at once). A task that is
	// not due yet does not hold up its conversation or wake the deployment.
	NotBeforeMs int64
	// ModelTier: cost tier for a routing model ("" = the bundle default).
	ModelTier string
	// ReplyDeployment and ReplyConversationKey: async delegation; when the
	// task ends its result is submitted to that deployment's conversation.
	ReplyDeployment      string
	ReplyConversationKey string
	// Outcome: how the task's run ended (JSON: status, reason, summary,
	// data, ...); "" until the Manager reports it.
	Outcome string
	// EventChain: how many event triggers in a row led to this task.
	EventChain int
	// Overrides: what this task may do, narrower than its bundle (JSON
	// TaskOverrides; "" = the bundle as is).
	Overrides string
	// NotAfterMs: not leased after this time (0: no limit). A still-queued
	// task past it ends failed, reason "expired".
	NotAfterMs int64
	// ScheduleKey: at most one queued task per (namespace, deployment, key);
	// submitting a task with the same key cancels the queued task that
	// already has it.
	ScheduleKey string
	// Priority: lease order within a deployment's queue ("urgent", "high",
	// "normal", "background"); "" means normal. Does not cross deployments.
	Priority string
	// Trust (3.7): "operator", "trusted" or "untrusted", resolved by the
	// Hub at submission time (never ""). A child task's is its caller's
	// current trust; a top-level one is its principal's, optionally
	// narrowed.
	Trust string
	// Locks (3.11, part): names this task must hold all of at once before
	// it is leased (the deployment's own `locks` config, plus any named on
	// submission), released when it ends.
	Locks []string
	// WaitFor (3.3, part): "<kind>:<ref>" the task is waiting for while
	// State is "waiting" (suspended: no instance, no lease, not
	// autoscaler load). "" otherwise.
	WaitFor string
	// OnParentEnd (3.4, part): Temporal's parent-close policy for this
	// task's own children when it ends. "cancel" (default, also ""): they
	// are cancelled too, recursively. "abandon": left running on their own.
	OnParentEnd string
	// RootTaskID (3.4, part): the tree this task belongs to. A task with no
	// parent is its own root; set by SubmitTask when empty (so it is always
	// populated, unlike RootRunID).
	RootTaskID string
	// Progress (3.4, part): the latest report_progress call, a JSON object
	// ({step, done, total, note, artifacts}); "" if never called.
	Progress string
	// MaxQueued, when positive, refuses the task (ErrQueueFull) while the
	// deployment has that many queued tasks. Not stored.
	MaxQueued int
}

// TaskOverrides is the stored (and wire, host-bound) form of what a task
// may do, narrower than its bundle. Every field can only narrow: a task
// never widens what its bundle or its parent task already allows.
type TaskOverrides struct {
	// Tools: only these of the bundle's tools (exact names or globs).
	// finish and load_skill are always available. Empty: every bundle tool.
	Tools []string `json:"tools,omitempty"`
	// Skills (of the bundle) to preload into the system prompt, in addition
	// to the bundle's own preloadSkills.
	Skills []string `json:"skills,omitempty"`
	// SystemAddendum: text appended after the system prompt.
	SystemAddendum string `json:"systemAddendum,omitempty"`
	// Permissions: extra rules, deny or ask only, checked before the
	// bundle's own rules.
	Permissions []OverrideRule `json:"permissions,omitempty"`
	// Budget: run limits, at most the bundle's own.
	Budget *RunBudget `json:"budget,omitempty"`
}

type OverrideRule struct {
	Tool string `json:"tool"`
	// "deny" or "ask".
	Action string `json:"action"`
}

type RunBudget struct {
	MaxTokens int64   `json:"maxTokens,omitempty"`
	MaxUsd    float64 `json:"maxUsd,omitempty"`
	MaxTurns  int32   `json:"maxTurns,omitempty"`
	// e.g. "20m".
	MaxDuration string `json:"maxDuration,omitempty"`
}

// ParseOverrides decodes a task's stored overrides ("" = none).
func ParseOverrides(raw string) (*TaskOverrides, error) {
	if raw == "" {
		return nil, nil
	}
	var o TaskOverrides
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// JSON encodes overrides for storage (nil/empty -> "").
func (o *TaskOverrides) JSON() string {
	if o == nil {
		return ""
	}
	b, err := json.Marshal(o)
	if err != nil {
		return ""
	}
	return string(b)
}

// CombineOverrides narrows parent's overrides by child's: tool and skill
// allowlists intersect (empty on one side keeps the other's), permission
// rules concatenate (parent's first), the system addendum appends, and
// budgets take the smaller of each set field. Either side may be nil.
func CombineOverrides(parent, child *TaskOverrides) *TaskOverrides {
	if parent == nil {
		return child
	}
	if child == nil {
		return parent
	}
	out := &TaskOverrides{
		Tools:          intersectOrEither(parent.Tools, child.Tools),
		Skills:         intersectOrEither(parent.Skills, child.Skills),
		SystemAddendum: strings.TrimSpace(parent.SystemAddendum + "\n" + child.SystemAddendum),
		Permissions:    append(append([]OverrideRule{}, parent.Permissions...), child.Permissions...),
	}
	out.Budget = combineBudgets(parent.Budget, child.Budget)
	return out
}

func intersectOrEither(a, b []string) []string {
	switch {
	case len(a) == 0:
		return b
	case len(b) == 0:
		return a
	}
	bs := map[string]bool{}
	for _, v := range b {
		bs[v] = true
	}
	var out []string
	for _, v := range a {
		if bs[v] {
			out = append(out, v)
		}
	}
	return out
}

func combineBudgets(a, b *RunBudget) *RunBudget {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := &RunBudget{}
	out.MaxTokens = minPositive(a.MaxTokens, b.MaxTokens)
	out.MaxUsd = minPositiveF(a.MaxUsd, b.MaxUsd)
	out.MaxTurns = int32(minPositive(int64(a.MaxTurns), int64(b.MaxTurns)))
	switch {
	case a.MaxDuration == "":
		out.MaxDuration = b.MaxDuration
	case b.MaxDuration == "":
		out.MaxDuration = a.MaxDuration
	default:
		da, erra := time.ParseDuration(a.MaxDuration)
		db, errb := time.ParseDuration(b.MaxDuration)
		if erra == nil && errb == nil && da < db {
			out.MaxDuration = a.MaxDuration
		} else {
			out.MaxDuration = b.MaxDuration
		}
	}
	return out
}

// minPositive is the smaller of a, b, treating 0 (unset) as "no limit".
func minPositive(a, b int64) int64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

func minPositiveF(a, b float64) float64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
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

const taskCols = "id, namespace, deployment, input, state, output, error, instance_id, run_id, source, attempts, idempotency_key, lease_id, lease_nest_id, lease_expires_ms, parent_task_id, parent_run_id, root_run_id, depth, traceparent, created_ms, updated_ms, submitted_by, conversation_key, labels, not_before_ms, model_tier, reply_deployment, reply_conversation_key, outcome, event_chain, overrides, not_after_ms, schedule_key, priority, trust, locks, wait_for, on_parent_end, root_task_id, progress"

func scanTask(r interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var labels, locks string
	err := r.Scan(&t.ID, &t.Namespace, &t.Deployment, &t.Input, &t.State, &t.Output, &t.Error, &t.InstanceID, &t.RunID, &t.Source,
		&t.Attempts, &t.IdempotencyKey, &t.LeaseID, &t.LeaseNestID, &t.LeaseExpiresMs, &t.ParentTaskID, &t.ParentRunID, &t.RootRunID,
		&t.Depth, &t.Traceparent, &t.CreatedMs, &t.UpdatedMs, &t.SubmittedBy, &t.ConversationKey, &labels, &t.NotBeforeMs, &t.ModelTier, &t.ReplyDeployment, &t.ReplyConversationKey, &t.Outcome, &t.EventChain, &t.Overrides, &t.NotAfterMs, &t.ScheduleKey, &t.Priority, &t.Trust, &locks, &t.WaitFor, &t.OnParentEnd, &t.RootTaskID, &t.Progress)
	if err == nil {
		err = json.Unmarshal([]byte(labels), &t.Labels)
	}
	if err == nil {
		err = json.Unmarshal([]byte(locks), &t.Locks)
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
	if t.RootTaskID == "" {
		// A task with no parent (or an older caller that never set it) is
		// the root of its own tree.
		t.RootTaskID = t.ID
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
	locksJSON, err := json.Marshal(t.Locks)
	if err != nil {
		return t, err
	}
	if t.Locks == nil {
		locksJSON = []byte("[]")
	}
	now := NowMs()
	maxQueued := t.MaxQueued
	if maxQueued <= 0 {
		maxQueued = math.MaxInt32
	}
	existing := func() (Task, error) {
		return scanTask(s.db.QueryRowContext(ctx, "SELECT "+taskCols+" FROM tasks WHERE namespace = $1 AND deployment = $2 AND idempotency_key = $3",
			t.Namespace, t.Deployment, t.IdempotencyKey))
	}
	inserted := int64(0)
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		// At most one queued task per (namespace, deployment, scheduleKey):
		// cancel whichever one already has it before inserting the new one.
		if t.ScheduleKey != "" {
			rows, err := tx.QueryContext(ctx, "SELECT "+taskCols+" FROM tasks WHERE namespace = $1 AND deployment = $2 AND schedule_key = $3 AND state = 'queued'",
				t.Namespace, t.Deployment, t.ScheduleKey)
			if err != nil {
				return err
			}
			replaced, err := scanTasks(rows)
			if err != nil {
				return err
			}
			for _, old := range replaced {
				if res, err := tx.ExecContext(ctx, "UPDATE tasks SET state = 'cancelled', error = 'replaced by a new task with the same schedule key', updated_ms = $1 WHERE id = $2 AND state = 'queued'",
					now, old.ID); err != nil {
					return err
				} else if n, _ := res.RowsAffected(); n == 1 {
					old.State, old.Error = "cancelled", "replaced by a new task with the same schedule key"
					if err := taskFinished(ctx, tx, old); err != nil {
						return err
					}
				}
			}
		}
		res, err := tx.ExecContext(ctx,
			"INSERT INTO tasks (id, namespace, deployment, input, state, source, idempotency_key, parent_task_id, parent_run_id, root_run_id, depth, traceparent, created_ms, updated_ms, submitted_by, conversation_key, labels, not_before_ms, model_tier, reply_deployment, reply_conversation_key, event_chain, overrides, not_after_ms, schedule_key, priority, trust, locks, on_parent_end, root_task_id) "+
				"SELECT $1, $2, $3, $4, 'queued', $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $22, $23, $24, $25, $26, $27, $28, $29, $30 "+
				"WHERE (SELECT COUNT(*) FROM tasks WHERE namespace = $2 AND deployment = $3 AND state = 'queued') < $21",
			t.ID, t.Namespace, t.Deployment, t.Input, t.Source, t.IdempotencyKey, t.ParentTaskID, t.ParentRunID, t.RootRunID, t.Depth, t.Traceparent, now, now, t.SubmittedBy,
			t.ConversationKey, string(labels), t.NotBeforeMs, t.ModelTier, t.ReplyDeployment, t.ReplyConversationKey, maxQueued, t.EventChain, t.Overrides, t.NotAfterMs, t.ScheduleKey, t.Priority, t.Trust, string(locksJSON), t.OnParentEnd, t.RootTaskID)
		if err != nil {
			return err
		}
		if inserted, _ = res.RowsAffected(); inserted == 0 {
			return nil
		}
		for _, name := range t.Locks {
			if _, err := tx.ExecContext(ctx, "INSERT INTO task_locks (namespace, task_id, name) VALUES ($1, $2, $3) ON CONFLICT (namespace, task_id, name) DO NOTHING",
				t.Namespace, t.ID, name); err != nil {
				return err
			}
		}
		data := map[string]any{"source": t.Source}
		if t.NotBeforeMs > 0 {
			data["notBeforeMs"] = t.NotBeforeMs
		}
		return appendEvent(ctx, tx, taskEvent(t, "task.queued", "info", data))
	})
	if isUnique(err) && t.IdempotencyKey != "" {
		return existing()
	}
	if err != nil {
		return t, err
	}
	if inserted == 0 {
		if t.IdempotencyKey != "" {
			if prev, err := existing(); err == nil {
				return prev, nil
			}
		}
		return t, fmt.Errorf("%w: %s/%s has %d queued tasks (limits.maxQueuedTasks)", ErrQueueFull, t.Namespace, t.Deployment, maxQueued)
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

// MergeIntoQueuedConversationTask merges t's input into the most recently
// created still-queued task of its conversation (3.9, part: "collect" queue
// mode), extending its not_before by debounceMs from now, instead of
// queuing t as a separate task. The caller queues t normally when this
// returns false (nothing to merge into, or it lost the race to a lease
// that took the row it would have merged into).
func (s *Store) MergeIntoQueuedConversationTask(ctx context.Context, t Task, debounceMs int64) (Task, bool, error) {
	if t.ConversationKey == "" {
		return Task{}, false, nil
	}
	now := NowMs()
	var merged Task
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx,
			"SELECT id FROM tasks WHERE namespace = $1 AND deployment = $2 AND conversation_key = $3 AND state = 'queued' ORDER BY created_ms DESC LIMIT 1",
			t.Namespace, t.Deployment, t.ConversationKey).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		m, err := scanTask(tx.QueryRowContext(ctx,
			"UPDATE tasks SET input = input || $1, not_before_ms = $2, updated_ms = $2 WHERE id = $3 AND state = 'queued' RETURNING "+taskCols,
			"\n\n"+t.Input, now+debounceMs, id))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		merged = m
		return nil
	})
	if err != nil {
		return Task{}, false, err
	}
	return merged, merged.ID != "", nil
}

// ListTaskTree returns every task of the tree rooted at rootTaskID, oldest
// first (the root itself included, since it is its own root_task_id).
func (s *Store) ListTaskTree(ctx context.Context, namespace, rootTaskID string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+taskCols+" FROM tasks WHERE namespace = $1 AND root_task_id = $2 ORDER BY created_ms", namespace, rootTaskID)
	if err != nil {
		return nil, err
	}
	return scanTasks(rows)
}

// SetTaskProgress records a task's latest report_progress call (3.4,
// part). A terminal or already-gone task is left alone: progress reported
// after the fact is stale, not an error worth surfacing to the instance
// that reported it (ReportProgress has no lease fencing).
func (s *Store) SetTaskProgress(ctx context.Context, taskID, progressJSON string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE tasks SET progress = $1, updated_ms = $2 WHERE id = $3 AND state IN ('queued', 'leased', 'running', 'waiting')",
		progressJSON, NowMs(), taskID)
	return err
}

// ListTasks filters by namespace, deployment and state ("" = any), newest first.
// UnfinishedReplyTasks returns the unfinished tasks of a deployment that
// report back to the agent that started them (start_agent).
func (s *Store) UnfinishedReplyTasks(ctx context.Context, ns, deployment string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM tasks WHERE namespace = $1 AND deployment = $2 AND reply_deployment <> '' AND state IN ('queued', 'leased', 'running', 'waiting')", ns, deployment)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CountUnfinishedBySource counts a deployment's queued, leased or running
// tasks with this exact source (e.g. "cron:<trigger>"), for a trigger's
// overlap policy.
func (s *Store) CountUnfinishedBySource(ctx context.Context, ns, deployment, source string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE namespace = $1 AND deployment = $2 AND source = $3 AND state IN ('queued', 'leased', 'running', 'waiting')",
		ns, deployment, source).Scan(&n)
	return n, err
}

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
// in flight, so a conversation's tasks run one at a time, in order. Tasks not
// due yet (not_before_ms) are skipped and hold up nothing. The result of a
// task started with start_agent waits until the run that started it ends.
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
		now := NowMs()
		rows, err := tx.QueryContext(ctx, "SELECT id FROM tasks t WHERE namespace = $1 AND deployment = $2 AND state = 'queued' AND not_before_ms <= $4 AND (conversation_key = '' OR NOT EXISTS ("+
			"SELECT 1 FROM tasks o WHERE o.namespace = t.namespace AND o.deployment = t.deployment AND o.conversation_key = t.conversation_key AND o.id <> t.id AND "+
			"(o.state IN ('leased', 'running', 'waiting') OR (o.state = 'queued' AND o.not_before_ms <= $4 AND (o.created_ms < t.created_ms OR (o.created_ms = t.created_ms AND o.id < t.id)))))) "+
			"AND NOT (source LIKE 'reply:%' AND EXISTS (SELECT 1 FROM tasks c JOIN runs r ON r.id = c.parent_run_id WHERE c.id = t.parent_task_id AND (r.ended_ms IS NULL OR r.ended_ms = 0))) "+
			"AND NOT EXISTS (SELECT 1 FROM task_locks tl WHERE tl.namespace = t.namespace AND tl.task_id = t.id AND "+
			"EXISTS (SELECT 1 FROM locks l WHERE l.namespace = tl.namespace AND l.name = tl.name AND l.task_id <> t.id AND l.expires_ms > $4)) "+
			"ORDER BY CASE priority WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'background' THEN 3 ELSE 2 END, created_ms, id LIMIT $3"+lock, ns, deployment, max, now)
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
		for _, id := range ids {
			// Re-check lock availability right now: an earlier task in this
			// very batch may have just taken one of this task's locks (the
			// bulk SELECT above only ruled out locks already held before
			// this call started).
			var blocked int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_locks tl WHERE tl.namespace = $1 AND tl.task_id = $2 AND "+
				"EXISTS (SELECT 1 FROM locks l WHERE l.namespace = tl.namespace AND l.name = tl.name AND l.task_id <> $2 AND l.expires_ms > $3)",
				ns, id, now).Scan(&blocked); err != nil {
				return err
			}
			if blocked > 0 {
				continue
			}
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
			if len(t.Locks) > 0 {
				if _, err := tx.ExecContext(ctx,
					"INSERT INTO locks (namespace, name, task_id, expires_ms) SELECT namespace, name, $1, $2 FROM task_locks WHERE namespace = $3 AND task_id = $1 "+
						"ON CONFLICT (namespace, name) DO UPDATE SET task_id = excluded.task_id, expires_ms = excluded.expires_ms "+
						"WHERE locks.task_id = excluded.task_id OR locks.expires_ms <= $4",
					id, now+leaseMs, ns, now); err != nil {
					return err
				}
			}
			out = append(out, t)
		}
		return nil
	})
	return out, err
}

// ExtendLease extends a lease the caller still holds, and the expiry of any
// locks it holds (3.11, part) along with it.
func (s *Store) ExtendLease(ctx context.Context, taskID, leaseID string, leaseMs int64) error {
	now := NowMs()
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE tasks SET lease_expires_ms = $1, updated_ms = $2 WHERE id = $3 AND lease_id = $4 AND state IN ('leased', 'running')",
			now+leaseMs, now, taskID, leaseID)
		if err := fenced(res, err); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE locks SET expires_ms = $1 WHERE task_id = $2", now+leaseMs, taskID)
		return err
	})
}

// StartTask marks a leased task as running on an instance.
func (s *Store) StartTask(ctx context.Context, taskID, leaseID, instanceID string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var prev string
		err := tx.QueryRowContext(ctx, "SELECT state FROM tasks WHERE id = $1 AND lease_id = $2 AND state IN ('leased', 'running')", taskID, leaseID).Scan(&prev)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrFenced
		}
		if err != nil {
			return err
		}
		t, err := scanTask(tx.QueryRowContext(ctx, "UPDATE tasks SET state = 'running', instance_id = $1, updated_ms = $2 WHERE id = $3 AND lease_id = $4 AND state IN ('leased', 'running') RETURNING "+taskCols,
			instanceID, NowMs(), taskID, leaseID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrFenced
		}
		if err != nil || prev == "running" {
			return err
		}
		return appendEvent(ctx, tx, taskEvent(t, "task.started", "info", map[string]any{"instanceId": instanceID, "attempt": t.Attempts}))
	})
}

// CompleteTask records a result. Fails with ErrFenced unless leaseID is the
// task's current lease (a partitioned nest cannot complete a re-leased task).
func (s *Store) CompleteTask(ctx context.Context, taskID, leaseID string, success bool, output, errMsg, runID, instanceID string) error {
	return s.CompleteTaskWithOutcome(ctx, taskID, leaseID, success, output, errMsg, runID, instanceID, "")
}

// CompleteTaskWithOutcome is CompleteTask with the run's outcome (JSON).
func (s *Store) CompleteTaskWithOutcome(ctx context.Context, taskID, leaseID string, success bool, output, errMsg, runID, instanceID, outcome string) error {
	state := "failed"
	if success {
		state = "succeeded"
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		t, err := scanTask(tx.QueryRowContext(ctx,
			"UPDATE tasks SET state = $1, output = $2, error = $3, run_id = $4, instance_id = $5, lease_expires_ms = 0, updated_ms = $6, outcome = $7 "+
				"WHERE id = $8 AND lease_id = $9 AND state IN ('leased', 'running') RETURNING "+taskCols,
			state, output, errMsg, runID, instanceID, NowMs(), outcome, taskID, leaseID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrFenced
		}
		if err != nil {
			return err
		}
		if !success {
			if err := endTaskRun(ctx, tx, t, "failed", errMsg); err != nil {
				return err
			}
		}
		return taskFinished(ctx, tx, t)
	})
}

// SuspendTask moves a leased or running task to "waiting" (3.3, part): it
// gives up its lease (another nest may now pick up other work, and it is
// no longer autoscaler load) and records waitFor ("<kind>:<ref>"), the
// run_inbox row that must arrive before WriteInbox requeues it. Fails
// with ErrFenced unless leaseID is the task's current lease.
func (s *Store) SuspendTask(ctx context.Context, taskID, leaseID, waitFor string) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE tasks SET state = 'waiting', lease_id = '', lease_nest_id = '', lease_expires_ms = 0, wait_for = $1, updated_ms = $2 WHERE id = $3 AND lease_id = $4 AND state IN ('leased', 'running')",
		waitFor, NowMs(), taskID, leaseID)
	return fenced(res, err)
}

// InboxRow is one entry of a suspended task's run_inbox: something that
// happened while it waited. Draining it into the resumed run (turning it
// back into a tool result or a message) is engine work, not implemented
// yet; today only WriteInbox's own requeue-on-match runs.
type InboxRow struct {
	ID        string
	Namespace string
	TaskID    string
	RunID     string
	// Kind: "child" (Ref: the child task's id), or "signal" (Ref: the
	// sending task's id).
	Kind      string
	Ref       string
	Payload   string
	CreatedMs int64
}

// writeInboxRow is WriteInbox's core, run in the caller's transaction (so
// taskFinished can write a child's result into its parent's inbox as part
// of the same transaction that ends the child).
func writeInboxRow(ctx context.Context, ex execer, r InboxRow) (woken bool, err error) {
	if r.ID == "" {
		r.ID = NewID()
	}
	now := NowMs()
	if _, err := ex.ExecContext(ctx, "INSERT INTO run_inbox (id, namespace, task_id, run_id, kind, ref, payload, created_ms, consumed_ms) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 0)",
		r.ID, r.Namespace, r.TaskID, r.RunID, r.Kind, r.Ref, r.Payload, now); err != nil {
		return false, err
	}
	// A "children" wait (3.4, part: "join") is woken by any "child" row,
	// not one matching a specific ref like the exact "child:<id>" wait
	// call_agent's own suspend uses.
	res, err := ex.ExecContext(ctx,
		"UPDATE tasks SET state = 'queued', wait_for = '', priority = 'urgent', updated_ms = $1 WHERE id = $2 AND state = 'waiting' AND (wait_for = $3 OR (wait_for = 'children' AND $4 = 'child'))",
		now, r.TaskID, r.Kind+":"+r.Ref, r.Kind)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// WriteInbox records one run_inbox row for a task; if the task is
// "waiting" on exactly this kind and ref, it is requeued in the same
// transaction (priority boosted to urgent; its attempt count is
// untouched).
func (s *Store) WriteInbox(ctx context.Context, r InboxRow) (woken bool, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		woken, err = writeInboxRow(ctx, tx, r)
		return err
	})
	return woken, err
}

// DrainInbox claims a task's unconsumed run_inbox entries other than
// "child" rows, oldest first (3.4/3.9, part: the engine turns them into
// run messages before its next model call; "child" rows are drained by
// call_agent's own re-check of the child's status on resume instead, never
// by this). Claiming (setting consumed_ms) happens in the same
// transaction as the read, so a retried call after a transport error
// never redelivers one already claimed.
func (s *Store) DrainInbox(ctx context.Context, taskID string) ([]InboxRow, error) {
	var out []InboxRow
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			"SELECT id, namespace, task_id, run_id, kind, ref, payload, created_ms FROM run_inbox WHERE task_id = $1 AND kind <> 'child' AND consumed_ms = 0 ORDER BY created_ms",
			taskID)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var r InboxRow
			if err := rows.Scan(&r.ID, &r.Namespace, &r.TaskID, &r.RunID, &r.Kind, &r.Ref, &r.Payload, &r.CreatedMs); err != nil {
				rows.Close()
				return err
			}
			out = append(out, r)
			ids = append(ids, r.ID)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		if len(ids) == 0 {
			return nil
		}
		var q query
		now := q.arg(NowMs())
		q.in("id", ids)
		_, err = tx.ExecContext(ctx, "UPDATE run_inbox SET consumed_ms = "+now+q.clause(), q.args...)
		return err
	})
	return out, err
}

// CountUnfinishedChildren counts taskID's own direct, unfinished *joined*
// children (3.4, part: "join" — reply_deployment unset, same scope
// cancelChildrenOnParentEnd uses; an async, start_agent-with-detach:true
// child is never counted, since outliving its starter is its whole
// point). Used by finish (refuses to end done while this is positive) and
// by the engine's own turn loop (suspends instead of calling the model
// again while this is positive).
func (s *Store) CountUnfinishedChildren(ctx context.Context, taskID string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM tasks WHERE parent_task_id = $1 AND reply_deployment = '' AND state IN ('queued', 'leased', 'running', 'waiting')",
		taskID).Scan(&n)
	return n, err
}

// RequeueExpiredLeases returns tasks whose lease expired to the queue. A task
// that has already been leased maxAttempts times fails instead, so a task
// that keeps killing its host does not loop forever (maxAttempts <= 0: no cap);
// the ids of tasks that failed are returned.
func (s *Store) RequeueExpiredLeases(ctx context.Context, maxAttempts int) (int64, []string, error) {
	now := NowMs()
	var n int64
	var failed []string
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
				if err == nil {
					ft := t
					ft.State, ft.Error = "failed", "task lease expired "+itoa(maxAttempts)+" times (the instance running it crashed or became unreachable)"
					err = taskFinished(ctx, tx, ft)
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
			if level == "error" {
				failed = append(failed, t.ID)
			}
			if err := appendLog(ctx, tx, t.InstanceID, t.Namespace, t.Deployment, level, msg); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return n, failed, nil
}

// ExpireOverdueTasks fails still-queued tasks whose not_after has passed
// (reason "expired"), instead of leaving them queued indefinitely. Returns
// the ids that were expired.
func (s *Store) ExpireOverdueTasks(ctx context.Context) ([]string, error) {
	now := NowMs()
	var expired []string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT "+taskCols+" FROM tasks WHERE state = 'queued' AND not_after_ms > 0 AND not_after_ms < $1", now)
		if err != nil {
			return err
		}
		due, err := scanTasks(rows)
		if err != nil {
			return err
		}
		for _, t := range due {
			res, err := tx.ExecContext(ctx, "UPDATE tasks SET state = 'failed', error = 'expired: not_after passed before the task was leased', updated_ms = $1 WHERE id = $2 AND state = 'queued'",
				now, t.ID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue
			}
			t.State, t.Error = "failed", "expired: not_after passed before the task was leased"
			if err := taskFinished(ctx, tx, t); err != nil {
				return err
			}
			expired = append(expired, t.ID)
		}
		return nil
	})
	return expired, err
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
		t, err := scanTask(tx.QueryRowContext(ctx, "UPDATE tasks SET state = 'cancelled', wait_for = '', updated_ms = $1 WHERE id = $2 AND state IN ('queued', 'leased', 'running', 'waiting') RETURNING "+taskCols, NowMs(), id))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := endTaskRun(ctx, tx, t, "cancelled", "cancelled"); err != nil {
			return err
		}
		return taskFinished(ctx, tx, t)
	})
	if err != nil {
		return Task{}, err
	}
	return s.GetTask(ctx, id)
}

// CancelTree hard-cancels every unfinished task of the tree rooted at
// rootTaskID (found through root_task_id, so the whole tree in one query,
// not a parent-by-parent cascade), and returns the ones it actually
// cancelled, oldest first. Unlike the parent-close policy (on_parent_end),
// this touches every task of the tree, including an async
// (reply_deployment set) one: cancelling a whole tree is an explicit ask,
// not an automatic side effect of something else ending.
func (s *Store) CancelTree(ctx context.Context, rootTaskID string) ([]Task, error) {
	var cancelled []Task
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			"SELECT id FROM tasks WHERE root_task_id = $1 AND state IN ('queued', 'leased', 'running', 'waiting') ORDER BY created_ms", rootTaskID)
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
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		for _, id := range ids {
			t, err := scanTask(tx.QueryRowContext(ctx,
				"UPDATE tasks SET state = 'cancelled', wait_for = '', updated_ms = $1 WHERE id = $2 AND state IN ('queued', 'leased', 'running', 'waiting') RETURNING "+taskCols,
				NowMs(), id))
			if errors.Is(err, sql.ErrNoRows) {
				// id was confirmed unfinished when this call started (the
				// query above), so if it is cancelled now, it was cascaded
				// by taskFinished's own parent-close policy onto a sibling
				// cancelled earlier in this very loop: still ours to report.
				if ct, err2 := scanTask(tx.QueryRowContext(ctx, "SELECT "+taskCols+" FROM tasks WHERE id = $1", id)); err2 == nil && ct.State == "cancelled" {
					cancelled = append(cancelled, ct)
				}
				continue
			}
			if err != nil {
				return err
			}
			if err := endTaskRun(ctx, tx, t, "cancelled", "cancelled: its tree was cancelled"); err != nil {
				return err
			}
			if err := taskFinished(ctx, tx, t); err != nil {
				return err
			}
			cancelled = append(cancelled, t)
		}
		return nil
	})
	return cancelled, err
}

// endTaskRun ends the unfinished run of a task the Hub has finished, and
// fences off its owner (a host may still be running it). Otherwise the run
// would keep its conversation open, and the next task with the same
// conversation key could never start.
func endTaskRun(ctx context.Context, tx *sql.Tx, t Task, status, msg string) error {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM runs WHERE namespace = $1 AND deployment = $2 AND task_id = $3 AND ended_ms IS NULL",
		t.Namespace, t.Deployment, t.ID)
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
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "UPDATE runs SET status = $1, error = $2, ended_ms = $3, epoch = epoch + 1 WHERE id = $4 AND ended_ms IS NULL",
			status, msg, NowMs(), id); err != nil {
			return err
		}
		if err := closeRunLeftovers(ctx, tx, id); err != nil {
			return err
		}
	}
	return rows.Err()
}

// QueuedBlockers says what in the queue holds a queued task back: an older
// unfinished task of its conversation, (an async result) the run that
// started the work still running, or one of its declared locks (3.11,
// part) currently held by another task.
func (s *Store) QueuedBlockers(ctx context.Context, t Task) (conversationBusy, callerRunning, locksHeld bool, err error) {
	if t.ConversationKey != "" {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks o WHERE o.namespace = $1 AND o.deployment = $2 AND o.conversation_key = $3 AND o.id <> $4 AND "+
			"(o.state IN ('leased', 'running', 'waiting') OR (o.state = 'queued' AND o.not_before_ms <= $6 AND (o.created_ms < $5 OR (o.created_ms = $5 AND o.id < $4))))",
			t.Namespace, t.Deployment, t.ConversationKey, t.ID, t.CreatedMs, NowMs()).Scan(&n); err != nil {
			return false, false, false, err
		}
		conversationBusy = n > 0
	}
	if strings.HasPrefix(t.Source, "reply:") && t.ParentTaskID != "" {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks c JOIN runs r ON r.id = c.parent_run_id WHERE c.id = $1 AND (r.ended_ms IS NULL OR r.ended_ms = 0)",
			t.ParentTaskID).Scan(&n); err != nil {
			return false, false, false, err
		}
		callerRunning = n > 0
	}
	if len(t.Locks) > 0 {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM locks l WHERE l.namespace = $1 AND l.task_id <> $2 AND l.expires_ms > $3 AND l.name IN ("+
			"SELECT name FROM task_locks WHERE namespace = $1 AND task_id = $2)",
			t.Namespace, t.ID, NowMs()).Scan(&n); err != nil {
			return false, false, false, err
		}
		locksHeld = n > 0
	}
	return conversationBusy, callerRunning, locksHeld, nil
}

// AssignedCount is how many instances of a deployment the scheduler has
// placed on Nests.
func (s *Store) AssignedCount(ctx context.Context, ns, deployment string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(count), 0) FROM assignments WHERE namespace = $1 AND deployment = $2", ns, deployment).Scan(&n)
	return n, err
}

// QueueStats counts unfinished tasks of a deployment.
type QueueStats struct{ Queued, InFlight int }

// Queue returns queued (and due) and in-flight (leased or running) counts.
func (s *Store) Queue(ctx context.Context, ns, deployment string) (QueueStats, error) {
	var q QueueStats
	err := s.db.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(CASE WHEN state = 'queued' AND not_before_ms <= $3 THEN 1 ELSE 0 END), 0), COALESCE(SUM(CASE WHEN state IN ('leased', 'running') THEN 1 ELSE 0 END), 0) FROM tasks WHERE namespace = $1 AND deployment = $2 AND state IN ('queued', 'leased', 'running')",
		ns, deployment, NowMs()).Scan(&q.Queued, &q.InFlight)
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

// ScheduledTasks returns queued tasks that wait for their start time,
// soonest first.
func (s *Store) ScheduledTasks(ctx context.Context, ns, deployment string, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+taskCols+" FROM tasks WHERE ($1 = '' OR namespace = $1) AND ($2 = '' OR deployment = $2) AND state = 'queued' AND not_before_ms > $3 ORDER BY not_before_ms, id LIMIT $4",
		ns, deployment, NowMs(), limit)
	if err != nil {
		return nil, err
	}
	return scanTasks(rows)
}

// TriggerProblem counts missed or rejected trigger events of one trigger.
type TriggerProblem struct {
	Namespace, Deployment, Trigger, State string
	Count                                 int
	LastMs                                int64
	Message                               string
}

// TriggerProblems returns missed and rejected trigger events since sinceMs,
// grouped by trigger and state.
func (s *Store) TriggerProblems(ctx context.Context, namespaces []string, sinceMs int64) ([]TriggerProblem, error) {
	var q query
	q.in("namespace", namespaces)
	q.where = append(q.where, "state IN ('missed', 'rejected')", "recorded_ms >= "+q.arg(sinceMs))
	rows, err := s.db.QueryContext(ctx, "SELECT namespace, deployment, trigger_name, state, COUNT(*), MAX(recorded_ms), MAX(message) FROM trigger_events"+q.clause()+
		" GROUP BY namespace, deployment, trigger_name, state", q.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TriggerProblem
	for rows.Next() {
		var t TriggerProblem
		if err := rows.Scan(&t.Namespace, &t.Deployment, &t.Trigger, &t.State, &t.Count, &t.LastMs, &t.Message); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
