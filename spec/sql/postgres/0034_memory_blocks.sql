-- Durable, named text an agent reads and updates across runs and
-- conversations (x-agen/config.json "memory"), namespace-scoped. version
-- is bumped on every update for compare-and-set (memory_update).
CREATE TABLE memory_blocks (
  namespace TEXT NOT NULL,
  id TEXT NOT NULL,
  label TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  value TEXT NOT NULL DEFAULT '',
  limit_chars INTEGER NOT NULL DEFAULT 0,
  version INTEGER NOT NULL DEFAULT 0,
  updated_ms BIGINT NOT NULL,
  PRIMARY KEY (namespace, id)
);
