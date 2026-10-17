-- Migration 1004 (fork range): what satisfies a plan blocker
--
-- One row per issue and kind. kind is 'merged', with ref the merge commit
-- and pr the pull request as owner/repo#number, or 'no_change', with ref
-- 'commit:<sha>' or 'plan:<key>'. Rows are never deleted: a story that
-- merged keeps satisfying its dependents after its workspace is gone and
-- after a restart. The backfill copies every merge commit that merge
-- completion already latched.

CREATE TABLE plan_satisfaction (
    issue_id    TEXT NOT NULL,
    kind        TEXT NOT NULL,
    ref         TEXT NOT NULL,
    pr          TEXT NOT NULL DEFAULT '',
    recorded_at TEXT NOT NULL,
    PRIMARY KEY (issue_id, kind)
);

INSERT INTO plan_satisfaction (issue_id, kind, ref, pr, recorded_at)
SELECT issue_id, 'merged', fingerprint, '', updated_at
FROM reaction_fingerprints
WHERE kind = 'merge-completion' AND fingerprint != '';
