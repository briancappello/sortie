-- Migration 1004 (fork range): what satisfies a plan blocker
--
-- One row per issue and kind (comments must hold no semicolon, because
-- the runner splits statements on it):
--   'merged': ref is the merge commit, pr the pull request as owner/repo#number,
--   'no_change': a confirmed claim, ref is 'commit:<sha>' or 'plan:<key>', and
--     cites is the cited story's issue id for a plan key,
--   'unverified_no_change': a claim Sortie could not confirm, ref is the
--     reason. It satisfies nothing and only explains why dependents wait.
--
-- Rows are never deleted: a story that merged keeps satisfying its dependents after its workspace is gone and
-- after a restart. The backfill copies every merge commit that merge
-- completion already latched.

CREATE TABLE plan_satisfaction (
    issue_id    TEXT NOT NULL,
    kind        TEXT NOT NULL,
    ref         TEXT NOT NULL,
    pr          TEXT NOT NULL DEFAULT '',
    cites       TEXT NOT NULL DEFAULT '',
    recorded_at TEXT NOT NULL,
    PRIMARY KEY (issue_id, kind)
);

INSERT INTO plan_satisfaction (issue_id, kind, ref, pr, recorded_at)
SELECT issue_id, 'merged', fingerprint, '', updated_at
FROM reaction_fingerprints
WHERE kind = 'merge-completion' AND fingerprint != '';
