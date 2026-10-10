-- A caller-chosen conversation key: tasks of a deployment with the same key
-- share one session, so each continues the conversation of the last.
ALTER TABLE sessions ADD COLUMN conversation_key TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX sessions_conversation_key ON sessions(namespace, deployment, conversation_key) WHERE conversation_key <> '';
ALTER TABLE tasks ADD COLUMN conversation_key TEXT NOT NULL DEFAULT '';
CREATE INDEX tasks_conversation ON tasks(namespace, deployment, conversation_key, state) WHERE conversation_key <> '';

-- Free-form labels (JSON object) copied from a task to its runs and to the
-- tasks it delegates.
ALTER TABLE tasks ADD COLUMN labels TEXT NOT NULL DEFAULT '{}';
ALTER TABLE runs ADD COLUMN labels TEXT NOT NULL DEFAULT '{}';

ALTER TABLE approvals ADD COLUMN decided_ms BIGINT NOT NULL DEFAULT 0;

-- What an instance loaded: its tools and each tool server's status (JSON).
ALTER TABLE instances ADD COLUMN tools TEXT NOT NULL DEFAULT '{}';
