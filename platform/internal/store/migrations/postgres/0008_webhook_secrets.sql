-- Per-trigger webhook secrets (only a hash is stored). A webhook call must
-- present the secret. The Hub turns accepted calls into tasks.
CREATE TABLE webhook_secrets (
  namespace TEXT NOT NULL,
  deployment TEXT NOT NULL,
  trigger_name TEXT NOT NULL,
  secret_hash TEXT NOT NULL,
  created_ms BIGINT NOT NULL,
  PRIMARY KEY (namespace, deployment, trigger_name)
);
