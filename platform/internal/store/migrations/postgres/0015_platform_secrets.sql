-- Platform secrets (x-agen/secrets.json source "platform"), per namespace.
-- Values are sealed with AGEN_HUB_KEK when the Hubs have one. Only the Hub
-- reads them, and only for Nests running a deployment that declares them.
CREATE TABLE platform_secrets (
  namespace TEXT NOT NULL,
  name TEXT NOT NULL,
  value TEXT NOT NULL,
  updated_ms BIGINT NOT NULL,
  PRIMARY KEY (namespace, name)
);
