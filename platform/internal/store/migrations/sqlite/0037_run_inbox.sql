-- Durable suspension (3.3, part): a task that must wait for something (a
-- child task's result, a signal, a timer) gives up its lease and moves to
-- "waiting" with a wait_for describing what it is waiting for
-- ("<kind>:<ref>"). run_inbox holds what has happened for it while it
-- waits. Writing a row that matches a waiting task's wait_for requeues it.
-- Draining the inbox into the resumed run (turning rows back into tool
-- results or messages) is engine work, not implemented yet.
ALTER TABLE tasks ADD COLUMN wait_for TEXT NOT NULL DEFAULT '';

CREATE TABLE run_inbox (
  id TEXT PRIMARY KEY,
  namespace TEXT NOT NULL,
  task_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  ref TEXT NOT NULL,
  payload TEXT NOT NULL DEFAULT '',
  created_ms BIGINT NOT NULL,
  consumed_ms BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX run_inbox_by_task ON run_inbox(task_id, consumed_ms);
