-- Migration 1002 (fork range): image digest of the run's execution environment
--
-- environment_image_sha256 is the hex SHA-256 of the image the run's
-- execution environment instance was created from, such as a VM's pack,
-- as the instance records it. Empty for a run on the host, and for rows
-- written before this migration: nothing is backfilled.

ALTER TABLE run_history ADD COLUMN environment_image_sha256 TEXT NOT NULL DEFAULT '';
