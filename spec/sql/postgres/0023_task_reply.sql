-- Async delegation: where a task's result goes when it ends.
ALTER TABLE tasks ADD COLUMN reply_deployment TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN reply_conversation_key TEXT NOT NULL DEFAULT '';
