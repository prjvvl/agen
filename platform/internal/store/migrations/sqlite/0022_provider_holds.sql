-- Provider accounts held after an account error (invalid key, no credit):
-- their deployments take no new work until a probe task succeeds.
CREATE TABLE provider_holds (
  account TEXT PRIMARY KEY,
  reason TEXT NOT NULL,
  failures INTEGER NOT NULL DEFAULT 1,
  held_ms BIGINT NOT NULL,
  retry_ms BIGINT NOT NULL,
  probe_task TEXT NOT NULL DEFAULT ''
);
