# Spec: Multi-window multi-burn-rate alerts (Google SRE canon)

**Iteration:** iter-0038 · **Complexity:** M · **SPA:** yes — burn-rule editor in the SLA target
→ **create an artifact mockup before implementation** and get it approved.

## Pain point
Burn alerts (D-0079) work with a single window+threshold: they are either noisy (short window)
or slow (long window). The Google SRE canon is a pair of windows + severity.

## Mechanism
- **Schema:** `sla_targets.burn_rules jsonb` — an array of rules:
  `{long_window_s, short_window_s, threshold, severity: "page"|"ticket", firing: bool}`.
  The migration converts the old `burn_window_seconds/burn_threshold/burn_firing` into a single
  rule with severity=page; the old columns either remain (read as a legacy fallback) or
  are dropped — decide in the migration, preferably drop after the conversion.
- **Default when burn alerts are enabled** (canon):
  page = 14.4× over (1h AND 5m), ticket = 6× over (6h AND 30m).
- **Semantics:** a rule fires when burn-rate ≥ threshold **in both windows** — the short one
  confirms "still ongoing", the long one cuts off noise. Edge-triggered, `firing` latch
  per-rule (like the current `burn_firing`).
- **EvaluateBurnAlerts:** loop over the target's rules; two bad-fraction computations per
  rule (long+short; maintenance windows are excluded — as today). Message with severity:
  "🔥 [page] …" / "⚠️ [ticket] …", recovery — "✅ …" per-rule.
- **Delivery:** the monitor's existing channels. Routing page→escalation is a follow-up, not here.
- **domain:** `BurnRule` + validation (short < long; threshold > 0; ≤4 rules; windows within
  a reasonable range), `SLATarget.BurnRules []BurnRule`.

## SPA (artifact before code)
Rule editor in the SLA target form: rule table (long, short, ×threshold, severity),
add/remove buttons, a "Reset to SRE defaults" button, badges of firing rules on the
monitor/SLA card. Mockup — an artifact, to be approved before implementation.

## Tests
- domain: rule validation (short≥long → error, count limit).
- store: migration of a legacy target → one page rule; both-windows-AND (long fires/short
  does not → no alert; both fire → alert; latch does not duplicate); recovery per-rule; multiple
  rules are independent.
- E2E: inject bad heartbeats → page alert in the channel; fix the short window → recovery.

## Out of scope
Routing severity into escalation policies; burn alerts on project-level SLA
(remains monitor-level, as today).

## Acceptance
The default pair of rules is created by enabling burn alerts; page/ticket arrive with
severity in the text; legacy targets work without manual editing; `-race` green;
the UI editor matches the approved mockup; vue-tsc+build clean.
