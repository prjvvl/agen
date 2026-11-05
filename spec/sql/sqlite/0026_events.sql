-- The event log: one row per notable change, in commit order. seq comes from
-- the single event_seq row, updated in the same transaction as the change,
-- so events become visible in seq order and a reader following a cursor
-- never skips one that commits late.
CREATE TABLE event_seq (id INTEGER PRIMARY KEY, seq BIGINT NOT NULL);
INSERT INTO event_seq (id, seq) VALUES (1, 0);
CREATE TABLE events (
  seq BIGINT PRIMARY KEY,
  namespace TEXT NOT NULL,
  type TEXT NOT NULL,
  deployment TEXT NOT NULL DEFAULT '',
  task_id TEXT NOT NULL DEFAULT '',
  parent_task_id TEXT NOT NULL DEFAULT '',
  labels TEXT NOT NULL DEFAULT '{}',
  severity TEXT NOT NULL DEFAULT 'info',
  data TEXT NOT NULL DEFAULT '{}',
  at_ms BIGINT NOT NULL
);
CREATE INDEX events_namespace ON events(namespace, seq);
CREATE INDEX events_at ON events(at_ms);
