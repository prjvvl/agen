-- Hold (3.4, part): a queued task with held set is skipped by leasing, a
-- running one suspends at its next model call with wait_for "resume".
-- held_until_ms (0: no deadline) is when the scheduler clears the hold
-- itself. hold_reason is free text for PauseTask's caller.
ALTER TABLE tasks ADD COLUMN held INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN held_until_ms BIGINT NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN hold_reason TEXT NOT NULL DEFAULT '';
