-- Declared locks (3.11, part): a task waits in the queue until it can
-- take all of its locks at once (avoiding deadlocks), then holds them
-- until it ends. task_locks is what a task has declared, and locks is
-- what is currently granted (one task at a time per namespace and name).
ALTER TABLE tasks ADD COLUMN locks TEXT NOT NULL DEFAULT '[]';

CREATE TABLE task_locks (
  namespace TEXT NOT NULL,
  task_id TEXT NOT NULL,
  name TEXT NOT NULL,
  PRIMARY KEY (namespace, task_id, name)
);
CREATE INDEX task_locks_lookup ON task_locks(namespace, task_id);

CREATE TABLE locks (
  namespace TEXT NOT NULL,
  name TEXT NOT NULL,
  task_id TEXT NOT NULL,
  expires_ms BIGINT NOT NULL,
  PRIMARY KEY (namespace, name)
);
