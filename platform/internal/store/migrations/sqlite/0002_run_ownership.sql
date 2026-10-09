ALTER TABLE sessions ADD COLUMN singleton INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX sessions_singleton ON sessions(namespace, deployment) WHERE singleton = 1;
CREATE UNIQUE INDEX conversations_one_open ON conversations(session_id) WHERE closed_ms IS NULL;
ALTER TABLE runs ADD COLUMN owner TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN epoch INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX runs_one_active_per_conversation ON runs(conversation_id) WHERE ended_ms IS NULL
