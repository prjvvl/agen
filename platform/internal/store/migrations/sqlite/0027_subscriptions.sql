-- Events announced once (budget thresholds, warnings) carry a dedupe key.
ALTER TABLE events ADD COLUMN dedupe_key TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX events_dedupe ON events(dedupe_key) WHERE dedupe_key <> '';

-- Notification targets are delivered from the event log: filters, a cursor
-- (-1: start at the newest event), and delivery state.
ALTER TABLE notification_targets ADD COLUMN filter TEXT NOT NULL DEFAULT '{}';
ALTER TABLE notification_targets ADD COLUMN cursor_seq BIGINT NOT NULL DEFAULT -1;
ALTER TABLE notification_targets ADD COLUMN retry_ms BIGINT NOT NULL DEFAULT 0;
ALTER TABLE notification_targets ADD COLUMN failing_since_ms BIGINT NOT NULL DEFAULT 0;
ALTER TABLE notification_targets ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE notification_targets ADD COLUMN last_delivered_ms BIGINT NOT NULL DEFAULT 0;
ALTER TABLE notification_targets ADD COLUMN dropped BIGINT NOT NULL DEFAULT 0;
