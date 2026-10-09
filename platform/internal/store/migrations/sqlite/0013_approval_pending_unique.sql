-- At most one pending approval per run, tool and arguments, so concurrent
-- asks (a run resumed twice) share one approval. Older duplicates expire.
UPDATE approvals SET state = 'expired'
  WHERE state = 'pending'
    AND id NOT IN (SELECT MIN(id) FROM approvals WHERE state = 'pending' GROUP BY run_id, tool, arguments);
CREATE UNIQUE INDEX approvals_one_pending ON approvals (run_id, tool, arguments) WHERE state = 'pending';
