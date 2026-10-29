-- Judgements on how a task went: a person, a check, or a judge model.
-- Approval decisions are recorded here too (by = "approval").
CREATE TABLE feedback (
  id TEXT PRIMARY KEY,
  namespace TEXT NOT NULL,
  deployment TEXT NOT NULL,
  task_id TEXT NOT NULL,
  score DOUBLE PRECISION NOT NULL,
  labels TEXT NOT NULL DEFAULT '{}',
  note TEXT NOT NULL DEFAULT '',
  by TEXT NOT NULL,
  created_ms BIGINT NOT NULL
);
CREATE INDEX feedback_lookup ON feedback(namespace, deployment, created_ms);
CREATE INDEX feedback_task ON feedback(task_id);
