-- Btree index rows are limited to about 2.7 KB, and approval arguments can
-- be larger (a tool writing a file). Index a hash of the arguments instead.
DROP INDEX IF EXISTS approvals_one_pending;
CREATE UNIQUE INDEX approvals_one_pending ON approvals (run_id, tool, md5(arguments)) WHERE state = 'pending';
