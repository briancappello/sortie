-- Migration 25: time a run spent held
--
-- held_ms is the total time, in milliseconds, a caller held the run
-- through the control API, which pauses its stall and turn clocks. Rows
-- written earlier keep 0: nothing is backfilled.

ALTER TABLE run_history ADD COLUMN held_ms INTEGER NOT NULL DEFAULT 0;
