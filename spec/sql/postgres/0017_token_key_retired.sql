-- Rotated call-token keys are retired, not deleted: no longer published or
-- used for signing, but still used to verify stored requester records.
ALTER TABLE hub_token_key ADD COLUMN retired_ms BIGINT NOT NULL DEFAULT 0;
