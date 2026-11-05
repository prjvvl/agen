-- Trust (3.7): operator, trusted or untrusted. Empty means trusted for
-- tokens, and is resolved to a concrete value by the Hub when a task is
-- submitted.
ALTER TABLE api_tokens ADD COLUMN trust TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN trust TEXT NOT NULL DEFAULT '';
