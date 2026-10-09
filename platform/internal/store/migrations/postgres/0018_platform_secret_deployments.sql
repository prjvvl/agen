-- The deployments allowed to read a platform secret (JSON list). Declaring
-- the name in a bundle is not enough: an operator could otherwise deploy
-- a bundle that declares an admin's secret and read it.
ALTER TABLE platform_secrets ADD COLUMN deployments TEXT NOT NULL DEFAULT '[]';
