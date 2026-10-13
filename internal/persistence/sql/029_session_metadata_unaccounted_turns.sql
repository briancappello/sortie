-- Migration 29: unaccounted-turn count on session_metadata
--
-- Counts the running session's turns that issued a model request and
-- produced no token figure from any source, as of the last in-flight
-- write. It lets the read-only tool server tell a lower-bound token sum
-- from a complete one while the session is still running. Existing rows
-- default to zero, and no such row matches a live dispatch: a dispatch
-- ID is never reused, so a row written before this migration is never
-- read as the running session's own.

ALTER TABLE session_metadata ADD COLUMN unaccounted_turns INTEGER NOT NULL DEFAULT 0;
