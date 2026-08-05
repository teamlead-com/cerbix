-- +goose Up
-- Multi-window multi-burn-rate alerting (Google SRE canon, D-0098): the single
-- window/threshold/latch columns become an array of rules
-- {long_window_seconds, short_window_seconds, threshold, severity, firing}.
-- A rule fires when the burn rate is >= threshold in BOTH windows. Legacy
-- targets port as one page-severity rule with short = long (degenerate pair —
-- exactly the old behaviour).
ALTER TABLE sla_targets ADD COLUMN burn_rules jsonb NOT NULL DEFAULT '[]';

UPDATE sla_targets
   SET burn_rules = jsonb_build_array(jsonb_build_object(
           'long_window_seconds',  burn_window_seconds,
           'short_window_seconds', burn_window_seconds,
           'threshold',            burn_threshold,
           'severity',             'page',
           'firing',               burn_firing))
 WHERE burn_alert_enabled;

ALTER TABLE sla_targets
    DROP COLUMN burn_window_seconds,
    DROP COLUMN burn_threshold,
    DROP COLUMN burn_firing;

-- +goose Down
ALTER TABLE sla_targets
    ADD COLUMN burn_window_seconds integer NOT NULL DEFAULT 3600,
    ADD COLUMN burn_threshold      double precision NOT NULL DEFAULT 14.4,
    ADD COLUMN burn_firing         boolean NOT NULL DEFAULT false;

UPDATE sla_targets
   SET burn_window_seconds = COALESCE((burn_rules->0->>'long_window_seconds')::integer, 3600),
       burn_threshold      = COALESCE((burn_rules->0->>'threshold')::double precision, 14.4),
       burn_firing         = COALESCE((burn_rules->0->>'firing')::boolean, false)
 WHERE jsonb_array_length(burn_rules) > 0;

ALTER TABLE sla_targets DROP COLUMN burn_rules;
