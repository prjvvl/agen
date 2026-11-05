-- Lease order within a deployment's queue: urgent, high, normal or
-- background. Empty means normal.
ALTER TABLE tasks ADD COLUMN priority TEXT NOT NULL DEFAULT '';
