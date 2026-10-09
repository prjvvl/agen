-- The Hub's Ed25519 key for A2A call tokens (one row), shared by all Hub
-- replicas. Gateways verify tokens with its public half, cached by Nests.
CREATE TABLE hub_token_key (
  id INTEGER PRIMARY KEY,
  private_key TEXT NOT NULL,
  created_ms BIGINT NOT NULL
);
