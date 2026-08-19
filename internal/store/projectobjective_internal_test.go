package store

import (
	"errors"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// A project-scoped SLO objective (iter-0155). `sla_targets.project_id` was dormant schema for eleven
// migrations; making it writable is only safe because the database refuses the one thing the owner's
// decision excludes — a project target that pages.
//
// The schema half is asserted with DIRECT SQL, not through the store: the store not offering a burn
// argument proves what this code does today, and the CHECK proves what any writer can do tomorrow —
// a migration, a psql session, a future handler written by someone who read §16.4 and assumed every
// scope pages now.
func TestProjectObjectiveIsStoredAndCannotPage(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	t1, err := st.UpsertProjectSLATarget(ctx, proj.ID, "30d", 99.9)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if t1.ProjectID != proj.ID || t1.Objective != 99.9 || t1.Window != "30d" {
		t.Fatalf("stored target = %+v", t1)
	}
	if t1.BurnAlertEnabled || len(t1.BurnRules) != 0 {
		t.Errorf("a project target was stored with burn alerting: %+v", t1)
	}
	// The upsert REPLACES the objective rather than creating a second row for the same window.
	if _, err := st.UpsertProjectSLATarget(ctx, proj.ID, "30d", 99.95); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	var rows int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM sla_targets WHERE project_id = $1 AND window_name = '30d'`, proj.ID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d rows for one (project, window) — the partial unique index was not the arbiter", rows)
	}
	got, err := st.GetProjectSLATarget(ctx, proj.ID, "30d")
	if err != nil || got.Objective != 99.95 {
		t.Fatalf("read back = %+v, %v", got, err)
	}
	// A window the project never set is ErrNotFound, not a zero-valued target.
	if _, err := st.GetProjectSLATarget(ctx, proj.ID, "7d"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unset window error = %v, want ErrNotFound", err)
	}

	// The schema refuses paging at this scope, whoever writes it.
	if _, err := st.pool.Exec(ctx,
		`UPDATE sla_targets SET burn_alert_enabled = true WHERE project_id = $1`, proj.ID); err == nil {
		t.Error("a direct UPDATE enabled burn alerting on a PROJECT target — 'reporting only' must be a " +
			"database guarantee, not a convention in the store (sla_targets_project_no_burn_chk)")
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE sla_targets SET burn_rules = '[{"window":"1h","threshold":14.4}]'::jsonb WHERE project_id = $1`, proj.ID); err == nil {
		t.Error("a direct UPDATE gave a PROJECT target burn rules — the CHECK covers the rules array too, " +
			"because a rule set with alerting off is a page waiting for one boolean")
	}
	// ...and the monitor scope is untouched by that CHECK: it may page, exactly as before.
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "api", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	if _, err := st.UpsertMonitorSLATarget(ctx, mon.ID, "30d", 99.9, true, nil); err != nil {
		t.Fatalf("a monitor target can still page, and must: %v", err)
	}
}
