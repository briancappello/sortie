-- Per-model token usage of a run
--
-- run_model_usage holds one row per model a run spent tokens on, written in
-- the transaction that inserts the run's run_history row. There is no foreign
-- key, as with the other run tables. Older rows have no parts and nothing is
-- backfilled.
--
-- resume_model_usage holds the per-model split of the resume_* figure as a
-- JSON object keyed by model name. It is NULL when the resume_* columns are
-- NULL, on rows written before the column existed, and on a run that handed
-- over no per-model figure.

CREATE TABLE run_model_usage (
    run_id             INTEGER NOT NULL,
    model              TEXT    NOT NULL,
    input_tokens       INTEGER NOT NULL,
    output_tokens      INTEGER NOT NULL,
    cache_read_tokens  INTEGER NOT NULL,
    cache_write_tokens INTEGER NOT NULL,
    PRIMARY KEY (run_id, model)
);

ALTER TABLE run_history ADD COLUMN resume_model_usage TEXT;
