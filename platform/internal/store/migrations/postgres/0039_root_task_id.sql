-- Tree queries (3.4, part): root_task_id names the tree a task belongs to.
-- Unlike root_run_id (empty until a run starts), it is always set: a task
-- with no parent is its own root, and every descendant inherits its
-- ancestor's.
ALTER TABLE tasks ADD COLUMN root_task_id TEXT NOT NULL DEFAULT '';
CREATE INDEX tasks_by_root_task ON tasks(root_task_id);
