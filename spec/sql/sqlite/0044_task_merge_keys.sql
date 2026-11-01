-- Idempotency keys folded into another task by a merge (3.9, part:
-- "collect" queue mode and event-trigger coalescing). tasks.idempotency_key
-- only ever holds the first submission's own key, so a later submission
-- that merged into it instead of inserting its own row needs its key
-- recorded here, or retrying it would merge a second time rather than
-- returning the task it already merged into.
CREATE TABLE task_merge_keys (
  namespace TEXT NOT NULL,
  deployment TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  task_id TEXT NOT NULL,
  created_ms INTEGER NOT NULL,
  PRIMARY KEY (namespace, deployment, idempotency_key)
);
