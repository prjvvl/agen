CREATE TABLE definitions (
  digest TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  files TEXT NOT NULL,
  created_ms INTEGER NOT NULL
);

CREATE TABLE deployments (
  namespace TEXT NOT NULL,
  name TEXT NOT NULL,
  definition_digest TEXT NOT NULL,
  kind TEXT NOT NULL,
  scale TEXT NOT NULL DEFAULT '{}',
  budget TEXT NOT NULL DEFAULT '{}',
  limits TEXT NOT NULL DEFAULT '{}',
  triggers TEXT NOT NULL DEFAULT '[]',
  placement TEXT NOT NULL DEFAULT '{}',
  desired INTEGER NOT NULL DEFAULT 0,
  generation INTEGER NOT NULL DEFAULT 1,
  last_activity_ms INTEGER NOT NULL DEFAULT 0,
  created_ms INTEGER NOT NULL,
  updated_ms INTEGER NOT NULL,
  PRIMARY KEY (namespace, name)
);

CREATE TABLE nests (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  backend TEXT NOT NULL,
  labels TEXT NOT NULL DEFAULT '{}',
  capacity INTEGER NOT NULL DEFAULT 0,
  gateway_url TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  cert_fingerprint TEXT NOT NULL DEFAULT '',
  last_heartbeat_ms INTEGER NOT NULL DEFAULT 0,
  created_ms INTEGER NOT NULL
);

CREATE TABLE assignments (
  namespace TEXT NOT NULL,
  deployment TEXT NOT NULL,
  nest_id TEXT NOT NULL,
  count INTEGER NOT NULL,
  definition_digest TEXT NOT NULL,
  generation INTEGER NOT NULL,
  updated_ms INTEGER NOT NULL,
  PRIMARY KEY (namespace, deployment, nest_id)
);
CREATE INDEX assignments_nest ON assignments(nest_id);

CREATE TABLE instances (
  id TEXT PRIMARY KEY,
  namespace TEXT NOT NULL,
  deployment TEXT NOT NULL,
  nest_id TEXT NOT NULL,
  definition_digest TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  endpoint TEXT NOT NULL DEFAULT '',
  running_tasks INTEGER NOT NULL DEFAULT 0,
  message TEXT NOT NULL DEFAULT '',
  started_ms INTEGER NOT NULL,
  last_seen_ms INTEGER NOT NULL
);
CREATE INDEX instances_deployment ON instances(namespace, deployment);
CREATE INDEX instances_nest ON instances(nest_id);

CREATE TABLE tasks (
  id TEXT PRIMARY KEY,
  namespace TEXT NOT NULL,
  deployment TEXT NOT NULL,
  input TEXT NOT NULL,
  state TEXT NOT NULL,
  output TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  instance_id TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT 'api',
  attempts INTEGER NOT NULL DEFAULT 0,
  idempotency_key TEXT NOT NULL DEFAULT '',
  lease_id TEXT NOT NULL DEFAULT '',
  lease_nest_id TEXT NOT NULL DEFAULT '',
  lease_expires_ms INTEGER NOT NULL DEFAULT 0,
  parent_task_id TEXT NOT NULL DEFAULT '',
  parent_run_id TEXT NOT NULL DEFAULT '',
  root_run_id TEXT NOT NULL DEFAULT '',
  depth INTEGER NOT NULL DEFAULT 0,
  traceparent TEXT NOT NULL DEFAULT '',
  created_ms INTEGER NOT NULL,
  updated_ms INTEGER NOT NULL
);
CREATE INDEX tasks_queue ON tasks(namespace, deployment, state, created_ms);
CREATE INDEX tasks_lease ON tasks(state, lease_expires_ms);
CREATE UNIQUE INDEX tasks_idempotency ON tasks(namespace, deployment, idempotency_key) WHERE idempotency_key <> '';

CREATE TABLE trigger_events (
  id TEXT PRIMARY KEY,
  namespace TEXT NOT NULL,
  deployment TEXT NOT NULL,
  trigger_name TEXT NOT NULL,
  state TEXT NOT NULL,
  task_id TEXT NOT NULL DEFAULT '',
  due_ms INTEGER NOT NULL,
  recorded_ms INTEGER NOT NULL,
  message TEXT NOT NULL DEFAULT ''
);
CREATE INDEX trigger_events_deployment ON trigger_events(namespace, deployment, due_ms);
CREATE UNIQUE INDEX trigger_events_due ON trigger_events(namespace, deployment, trigger_name, due_ms);

CREATE TABLE approvals (
  id TEXT PRIMARY KEY,
  namespace TEXT NOT NULL,
  deployment TEXT NOT NULL,
  run_id TEXT NOT NULL DEFAULT '',
  tool TEXT NOT NULL,
  arguments TEXT NOT NULL DEFAULT '{}',
  state TEXT NOT NULL,
  requested_by TEXT NOT NULL DEFAULT '',
  decided_by TEXT NOT NULL DEFAULT '',
  created_ms INTEGER NOT NULL,
  expires_ms INTEGER NOT NULL
);
CREATE INDEX approvals_state ON approvals(state, expires_ms);

CREATE TABLE api_tokens (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  secret_hash TEXT NOT NULL UNIQUE,
  scopes TEXT NOT NULL DEFAULT '[]',
  namespaces TEXT NOT NULL DEFAULT '[]',
  created_ms INTEGER NOT NULL,
  expires_ms INTEGER NOT NULL DEFAULT 0,
  revoked INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE join_tokens (
  token_hash TEXT PRIMARY KEY,
  expires_ms INTEGER NOT NULL,
  used_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE leases (
  name TEXT PRIMARY KEY,
  holder TEXT NOT NULL,
  epoch INTEGER NOT NULL,
  expires_ms INTEGER NOT NULL
)
