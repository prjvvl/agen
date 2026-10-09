-- One row per delegated call (A2A message id), so fan-out and total
-- delegation limits count a call once however often it is retried or resumed.
CREATE TABLE delegation_calls (
  message_id TEXT PRIMARY KEY,
  root_run_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  created_ms BIGINT NOT NULL
);
CREATE INDEX delegation_calls_root ON delegation_calls(root_run_id);
CREATE INDEX delegation_calls_run ON delegation_calls(run_id);
