-- A paused deployment runs no instances: the autoscaler and wake-ups leave it
-- at 0 until it is resumed. Queued tasks wait.
ALTER TABLE deployments ADD COLUMN paused INTEGER NOT NULL DEFAULT 0;
