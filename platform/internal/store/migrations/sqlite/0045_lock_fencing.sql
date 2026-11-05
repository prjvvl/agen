-- A durable fencing-token counter for locks (1.5), kept separate from
-- `locks` (who currently holds one, until when): a lock's grant history
-- must survive a release (which deletes the `locks` row), or the counter
-- would reset and a delayed write from a stale holder could collide with
-- a legitimate later one. Only advances on a genuine handover to a
-- different task (never a re-grant or lease extension to the same one).
CREATE TABLE lock_fences (
  namespace TEXT NOT NULL,
  name TEXT NOT NULL,
  fence INTEGER NOT NULL,
  PRIMARY KEY (namespace, name)
);
