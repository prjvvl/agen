-- How a run (and its task) ended: status, reason, summary, data (JSON).
ALTER TABLE runs ADD COLUMN outcome TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN outcome TEXT NOT NULL DEFAULT '';
