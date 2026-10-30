-- Queue modes for busy conversations (3.9, part): a deployment's
-- conversations policy (queueMode, debounce), JSON, same pattern as
-- scale/budget/limits.
ALTER TABLE deployments ADD COLUMN conversations TEXT NOT NULL DEFAULT '{}';
