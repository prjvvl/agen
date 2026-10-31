-- Replay cassettes (3.6, part: agen replay). Written by the engine
-- directly, only for a run of a deployment with recordInteractions on --
-- one row per run, the recorded interactions as JSON (agen-engine's own
-- provider::replay::Cassette shape). Pruned by the same retention sweep
-- as spans and logs.
CREATE TABLE cassettes (
  run_id TEXT PRIMARY KEY REFERENCES runs(id),
  data TEXT NOT NULL,
  created_ms BIGINT NOT NULL
);
