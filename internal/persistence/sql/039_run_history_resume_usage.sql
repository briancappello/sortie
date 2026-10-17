-- Migration 39: resume usage on run history
--
-- The four resume_* columns hold the usage figure the agent runtime itself
-- kept for the run's session at the run's last reporting turn, which the next
-- run that resumes the session starts from. They are NULL together: on rows
-- written before the columns existed and on a run that reported no figure.
-- Nothing is backfilled.

ALTER TABLE run_history ADD COLUMN resume_input_tokens INTEGER;
ALTER TABLE run_history ADD COLUMN resume_output_tokens INTEGER;
ALTER TABLE run_history ADD COLUMN resume_cache_read_tokens INTEGER;
ALTER TABLE run_history ADD COLUMN resume_cache_write_tokens INTEGER;
