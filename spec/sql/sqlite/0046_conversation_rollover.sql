-- A running summary of a conversation's own history past a certain
-- length (1.8, part: rollover). summarized_through is how many of the
-- conversation's own messages (oldest first) are already folded into
-- summary -- once a message is summarized it is never sent to the
-- model again, but it stays in the messages table, fully readable, for
-- anyone reading the conversation back directly.
ALTER TABLE conversations ADD COLUMN summary TEXT NOT NULL DEFAULT '';
ALTER TABLE conversations ADD COLUMN summarized_through INTEGER NOT NULL DEFAULT 0;
