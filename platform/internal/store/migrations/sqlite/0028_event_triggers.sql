-- Where each event trigger is in the event log (seq), and the matching
-- events it holds while it counts towards on.count (JSON list of seqs).
CREATE TABLE trigger_cursors (
  namespace TEXT NOT NULL,
  deployment TEXT NOT NULL,
  trigger_name TEXT NOT NULL,
  seq BIGINT NOT NULL,
  pending TEXT NOT NULL DEFAULT '[]',
  updated_ms BIGINT NOT NULL,
  PRIMARY KEY (namespace, deployment, trigger_name)
);

-- Tasks submitted by event triggers: how many event triggers led to them,
-- so a loop of triggers stops.
ALTER TABLE tasks ADD COLUMN event_chain INTEGER NOT NULL DEFAULT 0;
