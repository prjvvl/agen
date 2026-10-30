-- report_progress (3.4, part): the task's latest self-reported progress,
-- a JSON object (empty string: never reported). The history is the
-- task.progress events, not this column.
ALTER TABLE tasks ADD COLUMN progress TEXT NOT NULL DEFAULT '';
