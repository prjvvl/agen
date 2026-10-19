-- Scheduled tasks and the cost tier for routing models.
ALTER TABLE tasks ADD COLUMN not_before_ms BIGINT NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN model_tier TEXT NOT NULL DEFAULT '';
