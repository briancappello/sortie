-- Migration 27: commit a run exported from its execution environment
--
-- exported_commit is the commit a run pushed to the upstream's issue
-- branch when it ended. Empty for a run that pushed nothing, for a run
-- on the host, and for rows written before this migration: nothing is
-- backfilled.

ALTER TABLE run_history ADD COLUMN exported_commit TEXT NOT NULL DEFAULT '';
