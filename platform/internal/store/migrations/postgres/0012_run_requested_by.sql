-- Who asked for a run that is not a Hub task: the principal of the verified
-- A2A call token that started it (set by the Gateway). Approvals of the run
-- name it, so that principal cannot approve its own request.
ALTER TABLE runs ADD COLUMN requested_by TEXT NOT NULL DEFAULT '';
