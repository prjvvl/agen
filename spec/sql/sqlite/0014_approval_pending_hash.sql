-- Postgres indexes a hash of the arguments here (btree row size limit).
-- SQLite has no such limit: the index is recreated unchanged.
DROP INDEX IF EXISTS approvals_one_pending;
CREATE UNIQUE INDEX approvals_one_pending ON approvals (run_id, tool, arguments) WHERE state = 'pending';
