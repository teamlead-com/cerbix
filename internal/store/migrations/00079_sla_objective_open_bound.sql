-- +goose Up
-- D-0165 (iter-0142): objectives live in the OPEN interval (0,100). An objective of 100 is a
-- zero error budget, and the shared budget/burn math's allowed<=0 sentinel would answer a
-- total outage with 0x and fire no alert — 00078 made that value STORABLE for one review
-- round, so any row carrying it is clamped to the maximum admissible objective before the
-- CHECK tightens (no released deployment ever accepted 100: numeric(6,4) rejected it before
-- 00078, and 00078 never shipped outside this branch).
UPDATE sla_targets SET objective = 99.9999 WHERE objective >= 100;

ALTER TABLE sla_targets DROP CONSTRAINT sla_targets_objective_chk;
ALTER TABLE sla_targets ADD CONSTRAINT sla_targets_objective_chk
    CHECK (objective > 0 AND objective < 100);

-- +goose Down
ALTER TABLE sla_targets DROP CONSTRAINT sla_targets_objective_chk;
ALTER TABLE sla_targets ADD CONSTRAINT sla_targets_objective_chk
    CHECK (objective > 0 AND objective <= 100);
