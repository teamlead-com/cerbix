package store

import "testing"

// FR-021 §16.6b — the two sampled gauges. They answer the questions a slice cannot: how many alerts
// are open right now, and how much work is waiting. `backlog` is the §16.5a freshness predicate read
// from the other side — a row is fresh while `now() < lease_until`, so it is DUE once that stops
// being true, and a row that has never been evaluated is due by definition.
func TestServiceAlertStatsCountsOpenAlertsAndDueWork(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := burnAlertService(t, st, ctx, oneBurnRule)

	// The arming fixture stands in for a SUCCESSFUL live evaluation, so it leaves a fresh lease on
	// `service_alert_state`. Remove it: this first assertion is about a service nothing has ever
	// evaluated, which is the most backlogged a service can be.
	if _, err := st.pool.Exec(ctx, `DELETE FROM service_alert_state`); err != nil {
		t.Fatalf("clear live state: %v", err)
	}

	// Nothing evaluated yet: the service owns paging and its target declares a rule, so BOTH arms
	// have exactly one unit of work waiting, and nothing is open.
	got, err := st.ServiceAlertStats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if got.ActiveHealth != 0 || got.ActiveBurn != 0 || got.BacklogHealth != 1 || got.BacklogBurn != 1 {
		t.Fatalf("before any evaluation: %+v, want no open alerts and one unit due per arm", got)
	}

	// A firing burn rule: one open burn episode, and its target is no longer due — the evaluator
	// wrote a lease.
	plantBurn(t, st, ctx, f, 5, minute/60)
	if ev := burnEvalOnce(t, st, ctx); ev.Onsets != 1 {
		t.Fatalf("onsets = %d, want the rule to fire", ev.Onsets)
	}
	if got, err = st.ServiceAlertStats(ctx); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if got.ActiveBurn != 1 || got.BacklogBurn != 0 {
		t.Fatalf("after an onset: %+v, want one open burn alert and no burn backlog", got)
	}
	if got.BacklogHealth != 1 {
		t.Fatalf("the live arm stopped being due because the BURN arm ran: %+v", got)
	}

	// An expired lease is due again — that is the whole point of the gauge: a stalled evaluator has
	// to read as a rising backlog rather than as an absence of alerts.
	if _, err := st.pool.Exec(ctx,
		`UPDATE service_burn_alert_state SET lease_until = now() - interval '1 minute'`); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if got, err = st.ServiceAlertStats(ctx); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if got.BacklogBurn != 1 {
		t.Fatalf("an expired lease did not read as backlog: %+v", got)
	}

	// A CLOSED episode is not an open alert.
	if _, err := st.pool.Exec(ctx,
		`UPDATE service_alert_episodes SET closed_at = now(), close_reason = 'recovered'`); err != nil {
		t.Fatalf("close episode: %v", err)
	}
	if got, err = st.ServiceAlertStats(ctx); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if got.ActiveBurn != 0 {
		t.Fatalf("a closed episode still counted as active: %+v", got)
	}

	// A service that does NOT own paging is not backlog: nothing evaluates it and nothing should
	// suggest somebody is waiting on it.
	if _, err := st.pool.Exec(ctx,
		`UPDATE services SET owns_paging = false WHERE id = $1`, f.serviceID); err != nil {
		t.Fatalf("disown: %v", err)
	}
	if got, err = st.ServiceAlertStats(ctx); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if got.BacklogHealth != 0 || got.BacklogBurn != 0 {
		t.Fatalf("a service that owns no paging still read as backlog: %+v", got)
	}
}
