package store

// iter-0204 — one owner for HOW heartbeat readers exclude maintenance windows.
//
// The rule is unchanged (func-sla-sli.md "Maintenance windows"): a heartbeat of monitor m at ts is
// excluded when a maintenance window w has starts_at <= ts < min(ends_at, cancel_effective_at) and
// either w is m's own window (w.monitor_id = m) or w is a project window (w.monitor_id IS NULL) of
// m's project. archived_at is deliberately not consulted, as before.
//
// What changed is the SHAPE. Every reader used to test that rule with a correlated NOT EXISTS
// against maintenance_windows itself, which the planner may run — and on the pilot did run — as an
// index probe PER HEARTBEAT: 1.4–1.6 million shared buffer accesses for one statement, nearly all of
// it, with one window in the whole table. Now each statement first selects the windows of its own
// scope into a MATERIALIZED set — a handful of rows, materialized once per statement — and the
// per-heartbeat test runs against that set. The reads of maintenance_windows no longer grow with the
// heartbeats, whatever the planner prefers
// (TestMaintenanceWindowsAreReadOncePerStatementNotPerHeartbeat), and every reader keeps
// excluding exactly what it excluded (TestMaintenanceExclusionIsTheSameRuleForEveryReader).

// maintenanceWindowsCTE returns the CTE `mwin(monitor_id, starts_at, ends_at)`: the maintenance
// windows that apply to each monitor of `scope` (an earlier CTE with an `id` column) and end after
// `from` (an SQL expression; a window ending at or before it cannot cover a heartbeat the statement
// reads). Heartbeats reference monitors with ON DELETE CASCADE, so every heartbeat's monitor is in
// `monitors` and the join loses nothing.
func maintenanceWindowsCTE(scope, from string) string {
	return `mwin AS MATERIALIZED (
		    SELECT sm.id AS monitor_id, mw.starts_at, ` + maintEffectiveEnd + ` AS ends_at
		      FROM ` + scope + ` sc
		      JOIN monitors sm ON sm.id = sc.id
		      JOIN maintenance_windows mw
		        ON mw.monitor_id = sm.id OR (mw.monitor_id IS NULL AND mw.project_id = sm.project_id)
		     WHERE ` + maintEffectiveEnd + ` > ` + from + `)`
}

// maintenanceExcluded is the per-heartbeat test against mwin, for a heartbeat aliased `h`.
const maintenanceExcluded = `NOT EXISTS (
		       SELECT 1 FROM mwin w
		        WHERE w.monitor_id = h.monitor_id AND w.starts_at <= h.ts AND w.ends_at > h.ts)`

// todayStartUTC is the start of the current UTC day as a timestamptz expression — the day boundary
// of heartbeats_daily, whatever the session's TimeZone. Comparing h.ts with the bare DATE
// `(now() AT TIME ZONE 'UTC')::date` instead reads that date as midnight in the SESSION's zone,
// which the "today" readers did until iter-0204 review round 1
// (TestTodayReadersStartAtUTCMidnightWhateverTheSessionTimeZone).
const todayStartUTC = `(((now() AT TIME ZONE 'UTC')::date)::timestamp AT TIME ZONE 'UTC')`
