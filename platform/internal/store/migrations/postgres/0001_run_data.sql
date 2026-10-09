CREATE TABLE sessions (
  id TEXT PRIMARY KEY,
  agent TEXT NOT NULL,
  namespace TEXT NOT NULL DEFAULT '',
  deployment TEXT NOT NULL DEFAULT '',
  memory TEXT NOT NULL DEFAULT '{}',
  created_ms BIGINT NOT NULL,
  updated_ms BIGINT NOT NULL
);

CREATE TABLE conversations (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES sessions(id),
  created_ms BIGINT NOT NULL,
  closed_ms BIGINT
);
CREATE INDEX conversations_session ON conversations(session_id, created_ms);

CREATE TABLE messages (
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  seq BIGINT NOT NULL,
  run_id TEXT NOT NULL,
  body TEXT NOT NULL,
  created_ms BIGINT NOT NULL,
  PRIMARY KEY (conversation_id, seq)
);

CREATE TABLE runs (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES sessions(id),
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  namespace TEXT NOT NULL DEFAULT '',
  deployment TEXT NOT NULL DEFAULT '',
  definition_digest TEXT NOT NULL DEFAULT '',
  task_id TEXT NOT NULL DEFAULT '',
  parent_run_id TEXT NOT NULL DEFAULT '',
  root_run_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  input TEXT NOT NULL,
  output TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  step BIGINT NOT NULL DEFAULT 0,
  input_tokens BIGINT NOT NULL DEFAULT 0,
  output_tokens BIGINT NOT NULL DEFAULT 0,
  cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
  trace_id TEXT NOT NULL DEFAULT '',
  started_ms BIGINT NOT NULL,
  ended_ms BIGINT
);
CREATE INDEX runs_deployment ON runs(namespace, deployment, started_ms);
CREATE INDEX runs_root ON runs(root_run_id);

CREATE TABLE spans (
  span_id TEXT PRIMARY KEY,
  trace_id TEXT NOT NULL,
  parent_span_id TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL,
  start_ms BIGINT NOT NULL,
  end_ms BIGINT NOT NULL,
  status TEXT NOT NULL DEFAULT 'ok',
  attributes TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX spans_trace ON spans(trace_id, start_ms);
CREATE INDEX spans_run ON spans(run_id);

CREATE TABLE effects (
  run_id TEXT NOT NULL,
  call_id TEXT NOT NULL,
  tool TEXT NOT NULL,
  arguments_hash TEXT NOT NULL,
  idempotency_key TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  result TEXT NOT NULL DEFAULT '',
  created_ms BIGINT NOT NULL,
  updated_ms BIGINT NOT NULL,
  PRIMARY KEY (run_id, call_id)
);

CREATE TABLE delegations (
  root_run_id TEXT PRIMARY KEY,
  count BIGINT NOT NULL
);

CREATE TABLE logs (
  id BIGSERIAL PRIMARY KEY,
  instance_id TEXT NOT NULL DEFAULT '',
  namespace TEXT NOT NULL DEFAULT '',
  deployment TEXT NOT NULL DEFAULT '',
  time_ms BIGINT NOT NULL,
  level TEXT NOT NULL,
  message TEXT NOT NULL
);
CREATE INDEX logs_deployment ON logs(namespace, deployment, time_ms);
