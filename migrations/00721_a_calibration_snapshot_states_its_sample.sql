-- +goose Up
-- A calibration snapshot says how many predictions it was computed from and how
-- many it could not use (F-60).
--
-- `n_predictions` is the count that SURVIVED. `calibrationSourceSQL` joins
-- predictions to prediction_outcomes with an INNER join, so every prediction in
-- the window with no outcome -- unresolved, or repeatedly unresolvable through
-- F-59's dead feed -- is dropped before Go sees a row, and the calibrator holds
-- no logger, no counter and no metric. Nothing anywhere compared the window's
-- prediction count against the outcomes found.
--
-- So a reader of a snapshot could not tell 6 of 6 from 6 of 600. That matters
-- because agent_lifecycle_transitions.calibration_snapshot_id is a foreign key
-- onto this table: a promotion decision cites a snapshot whose sample loss was
-- unrecorded, and a strategy that resolves only its winners would look
-- beautifully calibrated.
--
-- The columns are NULLable on purpose. A row written before this migration was
-- computed without the figure, and NULL is the honest way to say "not
-- measured" -- the alternative, a default of zero, would assert that no
-- prediction was dropped, which is precisely the false statement this exists to
-- prevent. New rows always carry both, which the CHECK below requires.

ALTER TABLE calibration_snapshots
    ADD COLUMN window_predictions integer,
    ADD COLUMN window_scored      integer;

-- Both or neither, and a snapshot can never claim to have scored more
-- predictions than the window held. Written so the expressions cannot evaluate
-- to NULL when the columns are present (F-50): each side of the equality is an
-- IS NULL test, and the ordering test is guarded by an OR that is true whenever
-- either column is absent.
ALTER TABLE calibration_snapshots
    ADD CONSTRAINT calibration_snapshots_sample_stated_together
    CHECK ((window_predictions IS NULL) = (window_scored IS NULL));

ALTER TABLE calibration_snapshots
    ADD CONSTRAINT calibration_snapshots_scored_within_window
    CHECK (window_predictions IS NULL
           OR (window_scored >= 0 AND window_predictions >= window_scored));

COMMENT ON COLUMN calibration_snapshots.window_predictions IS
    'Predictions committed in the window and in scope, whether or not they resolved. NULL on rows written before migration 00721, where the figure was not computed.';
COMMENT ON COLUMN calibration_snapshots.window_scored IS
    'Of those, how many carried an outcome and entered the statistics. window_predictions - window_scored is the sample this snapshot could not see.';

-- +goose Down
SELECT 1; -- protected: dropping these would return the snapshots to stating a denominator they do not have
