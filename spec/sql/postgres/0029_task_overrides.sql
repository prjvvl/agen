-- What a task may do, narrower than its bundle (JSON TaskOverrides).
ALTER TABLE tasks ADD COLUMN overrides TEXT NOT NULL DEFAULT '';
