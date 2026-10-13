-- Migration 28: run origin on run history and retry entries
--
-- run_history gains trigger, reaction_kind, session_id, and ssh_host as
-- nullable text with no default. Every row written before this migration
-- holds NULL in each, which means "not recorded". A writer that records a
-- run binds text, the empty string for none. A NOT NULL DEFAULT '' column
-- would claim "no retry trigger" and "local run" for a run whose origin was
-- never captured.
--
-- retry_entries gains origin_trigger, what started the retry, kept through
-- every put-back. A row whose trigger is timer or empty never stored what
-- started it and keeps an empty origin_trigger.

ALTER TABLE run_history ADD COLUMN trigger TEXT;
ALTER TABLE run_history ADD COLUMN reaction_kind TEXT;
ALTER TABLE run_history ADD COLUMN session_id TEXT;
ALTER TABLE run_history ADD COLUMN ssh_host TEXT;
CREATE INDEX idx_run_history_identifier ON run_history(identifier);
ALTER TABLE retry_entries ADD COLUMN origin_trigger TEXT NOT NULL DEFAULT '';

UPDATE retry_entries SET origin_trigger = trigger WHERE trigger NOT IN ('timer', '');
