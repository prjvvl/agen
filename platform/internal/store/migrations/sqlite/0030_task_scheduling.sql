-- not_after_ms: a still-queued task past it ends failed/expired instead of
-- waiting indefinitely. schedule_key: at most one queued task per
-- deployment and key: resubmitting the same key cancels the queued one.
ALTER TABLE tasks ADD COLUMN not_after_ms BIGINT NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN schedule_key TEXT NOT NULL DEFAULT '';
CREATE INDEX tasks_schedule_key ON tasks(namespace, deployment, schedule_key) WHERE schedule_key <> '';
