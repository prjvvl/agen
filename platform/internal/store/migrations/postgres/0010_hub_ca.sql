-- The Hub's certificate authority (one row), shared by all Hub replicas. It
-- signs Nest client certificates at enrolment and the Hub's TLS certificate.
CREATE TABLE hub_ca (
  id INTEGER PRIMARY KEY,
  cert_pem TEXT NOT NULL,
  key_pem TEXT NOT NULL,
  created_ms BIGINT NOT NULL
);
CREATE INDEX nests_cert ON nests(cert_fingerprint);
