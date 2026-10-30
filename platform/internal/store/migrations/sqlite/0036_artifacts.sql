-- Artifacts (3.8, part): named, content-addressed text an agent saves
-- during a run and reads back later (save_artifact / read_artifact),
-- so a long result does not have to stay in the model's context.
CREATE TABLE artifacts (
  namespace TEXT NOT NULL,
  id TEXT NOT NULL,
  name TEXT NOT NULL,
  media_type TEXT NOT NULL DEFAULT 'text/plain',
  size BIGINT NOT NULL,
  content TEXT NOT NULL,
  task_id TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '',
  created_ms BIGINT NOT NULL,
  PRIMARY KEY (namespace, id)
);
CREATE INDEX artifacts_by_task ON artifacts(namespace, task_id);
