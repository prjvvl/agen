-- Eval reports recorded against a deployment (2.43, part), oldest and
-- newest kept so a later deploy can compare against the last one.
CREATE TABLE eval_reports (
  id TEXT PRIMARY KEY,
  namespace TEXT NOT NULL,
  deployment TEXT NOT NULL,
  definition_digest TEXT NOT NULL DEFAULT '',
  report_json TEXT NOT NULL,
  created_ms BIGINT NOT NULL
);
CREATE INDEX eval_reports_lookup ON eval_reports(namespace, deployment, created_ms);
