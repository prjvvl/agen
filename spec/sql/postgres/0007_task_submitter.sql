-- The principal that submitted a task (API token id, trigger or parent
-- task's submitter). Approvals raised by the task's run name it as the
-- requester, so nobody can approve work they asked for themselves.
ALTER TABLE tasks ADD COLUMN submitted_by TEXT NOT NULL DEFAULT '';
