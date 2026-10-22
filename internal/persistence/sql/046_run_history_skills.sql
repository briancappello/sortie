-- NULL marks a run that recorded no skill call, and a row written before the column existed.

ALTER TABLE run_history ADD COLUMN skills TEXT;
