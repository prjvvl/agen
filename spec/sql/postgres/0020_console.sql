-- Indexes for the trace list, metrics and the live event stream.
CREATE INDEX runs_started ON runs(started_ms);
CREATE INDEX runs_parent ON runs(parent_run_id) WHERE parent_run_id <> '';
CREATE INDEX spans_end ON spans(end_ms);
CREATE INDEX tasks_updated ON tasks(updated_ms);

-- Where the Hub POSTs a namespace's events. The signing secret is sealed
-- with AGEN_HUB_KEK when the Hubs have one.
CREATE TABLE notification_targets (
  namespace TEXT NOT NULL,
  name TEXT NOT NULL,
  url TEXT NOT NULL,
  events TEXT NOT NULL DEFAULT '[]',
  secret TEXT NOT NULL,
  created_ms BIGINT NOT NULL,
  PRIMARY KEY (namespace, name)
);

-- A token that acts for the submitter of the task it works on (the assistant).
ALTER TABLE api_tokens ADD COLUMN on_behalf BIGINT NOT NULL DEFAULT 0;
