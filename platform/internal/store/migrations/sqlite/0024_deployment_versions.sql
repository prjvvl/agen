-- The bundle versions each deployment has run (definitions are shared by
-- name across deployments, and roll back offers only a deployment's own).
CREATE TABLE deployment_versions (
  namespace TEXT NOT NULL,
  deployment TEXT NOT NULL,
  digest TEXT NOT NULL,
  used_ms INTEGER NOT NULL,
  PRIMARY KEY (namespace, deployment, digest)
);
INSERT INTO deployment_versions (namespace, deployment, digest, used_ms)
  SELECT namespace, name, definition_digest, updated_ms FROM deployments;
