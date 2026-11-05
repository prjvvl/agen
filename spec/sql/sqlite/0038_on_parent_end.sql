-- Temporal's parent-close policy (3.4, part): what happens to a task's own
-- unfinished children when it ends. "cancel" (default, also empty): they
-- are cancelled too, recursively. "abandon": left running on their own.
ALTER TABLE tasks ADD COLUMN on_parent_end TEXT NOT NULL DEFAULT '';
