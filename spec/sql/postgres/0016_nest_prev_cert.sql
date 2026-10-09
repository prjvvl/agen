-- Certificate renewal keeps the previous certificate valid until the new
-- one is first used, so a lost renewal reply does not lock a Nest out.
ALTER TABLE nests ADD COLUMN prev_cert_fingerprint TEXT NOT NULL DEFAULT '';
