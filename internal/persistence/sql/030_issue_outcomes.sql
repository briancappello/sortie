-- Migration 30: how each issue's latest work ended
--
-- One row per issue holding current state: the row is replaced or kept
-- when the orchestrator observes an ending and deleted when it picks the
-- issue up again, so the table is not a history. kind is handed_off,
-- merged, or terminal. observed_at is when the orchestrator observed the
-- ending, RFC 3339 in UTC. state is the tracker state a handoff wrote or
-- a terminal read found. merge_reaction_kind, pr_number, and
-- pr_repository name the pull request a merge observation found merged.
-- Issues that ended before this migration have no row.

CREATE TABLE issue_outcomes (
    issue_id            TEXT    PRIMARY KEY,
    kind                TEXT    NOT NULL,
    observed_at         TEXT    NOT NULL,
    state               TEXT    NOT NULL DEFAULT '',
    merge_reaction_kind TEXT    NOT NULL DEFAULT '',
    pr_number           INTEGER NOT NULL DEFAULT 0,
    pr_repository       TEXT    NOT NULL DEFAULT ''
);
