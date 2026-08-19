package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Regressions for review round 1/2 [314]. Each test names the defect it pins, because every one
// of these passed review-less before: the code looked right and the tests asked the wrong question.

// [314] P0-1A — a DORMANT binding used to escape every tenant constraint. A composite FK is MATCH
// SIMPLE, so `(monitor_id, source_project)` with a NULL project is not enforced at all, and the
// old CHECK required a project only for a non-manual SOURCE. The direct-SQL insert below is the
// negative the spec asks for at §15.0 and the one my first submission never wrote.
func TestDormantBindingCannotEscapeTenancy(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	org2, _ := st.CreateOrganization(ctx, "other", "Other")
	proj2, _ := st.CreateProject(ctx, org2.ID, "p2", "P2")
	alien, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj2.ID, Name: "alien-http", Type: domain.MonitorHTTP,
		Target: "https://alien.example.com/", IntervalSeconds: 60, Region: "core", Enabled: true,
	})
	if err != nil {
		t.Fatalf("alien monitor: %v", err)
	}

	// The exact row that used to be accepted: manual source, NO project, a FOREIGN monitor id
	// sitting dormant.
	_, err = st.pool.Exec(ctx, `
		INSERT INTO components (status_page_id, org_id, source, name, monitor_id)
		VALUES ($1, $2, 'manual', 'smuggled', $3)`, f.pageProj, f.orgID, alien.ID)
	if err == nil {
		t.Fatal("a foreign monitor was planted as a DORMANT binding with no project")
	}
	// And the honest version of the same row — a binding WITH its project — is refused by the
	// composite FK, since that project belongs to another org.
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO components (status_page_id, org_id, source, name, monitor_id, source_project)
		VALUES ($1, $2, 'manual', 'smuggled2', $3, $4)`,
		f.pageProj, f.orgID, alien.ID, proj2.ID); err == nil {
		t.Fatal("a foreign monitor was planted as a dormant binding WITH its own project")
	}
	// A project with NO binding at all is equally refused: the project is the project OF the
	// bindings, so carrying one without them is meaningless state.
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO components (status_page_id, org_id, source, name, source_project)
		VALUES ($1, $2, 'manual', 'projectless', $3)`, f.pageProj, f.orgID, f.projectID); err == nil {
		t.Fatal("a component carries a source project with no binding to justify it")
	}
	// The legitimate shape still works: a purely manual component with neither.
	if _, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageProj, Name: "Third-party CDN",
	}); err != nil {
		t.Fatalf("a plain manual component was refused: %v", err)
	}
}

// [314] P0-1B — the two deferred triggers were not a lock. DEFERRED validates at COMMIT against
// each transaction's OWN snapshot, so an insert and a page-narrowing could both pass and commit a
// state neither would have accepted alone. Both commit orders are pinned.
func TestPageScopeRaceIsSerializedInBothOrders(t *testing.T) {
	for _, order := range []string{"insert-commits-last", "narrow-commits-last"} {
		t.Run(order, func(t *testing.T) {
			st, ctx := serviceSchemaStore(t)
			f := seedProjection(t, st, ctx)
			alien, err := st.CreateService(ctx, domain.Service{
				ProjectID: f.otherProj, Slug: "alien", Name: "Alien",
			})
			if err != nil {
				t.Fatalf("alien service: %v", err)
			}

			insert := func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					INSERT INTO components (status_page_id, org_id, source, name, service_id, source_project)
					VALUES ($1,$2,'service','Alien',$3,$4)`,
					f.pageOrg, f.orgID, alien.ID, f.otherProj)
				return err
			}
			narrow := func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx,
					`UPDATE status_pages SET project_id = $2 WHERE id = $1`, f.pageOrg, f.projectID)
				return err
			}

			// The two intents run in SEPARATE sessions, and the second one is expected to BLOCK:
			// the generation trigger takes the page row on insert and the narrowing UPDATE takes it
			// too, so the page row is the serialization point. A sequential test cannot express
			// this — it would deadlock against itself and prove nothing.
			first, second := insert, narrow
			if order == "narrow-commits-last" {
				first, second = narrow, insert
			}
			t1, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatalf("t1: %v", err)
			}
			defer t1.Rollback(ctx) //nolint:errcheck // may already be done
			if err := first(t1); err != nil {
				t.Fatalf("t1 first statement: %v", err)
			}

			secondErr := make(chan error, 1)
			go func() {
				t2, err := st.pool.Begin(ctx)
				if err != nil {
					secondErr <- err
					return
				}
				defer t2.Rollback(ctx) //nolint:errcheck // may already be done
				if err := second(t2); err != nil {
					secondErr <- err
					return
				}
				secondErr <- t2.Commit(ctx)
			}()
			select {
			case err := <-secondErr:
				t.Fatalf("the second session did not wait on the page row (err=%v): there is no serialization point", err)
			case <-time.After(300 * time.Millisecond):
			}

			t1Err := t1.Commit(ctx)
			var t2Err error
			select {
			case t2Err = <-secondErr:
			case <-time.After(10 * time.Second):
				t.Fatal("the second session never finished after the first released the page")
			}
			if t1Err == nil && t2Err == nil {
				t.Fatal("both transactions committed: the page-scope rule was bypassed by interleaving")
			}
			if t1Err != nil && t2Err != nil {
				t.Fatalf("neither transaction survived (t1=%v, t2=%v): the guard is refusing legal work", t1Err, t2Err)
			}

			// Whichever survived, the FINAL state is consistent: no project-scoped page holds a
			// foreign project's component.
			var bad int
			if err := st.pool.QueryRow(ctx, `
				SELECT count(*) FROM components c JOIN status_pages sp ON sp.id = c.status_page_id
				 WHERE sp.project_id IS NOT NULL AND c.source_project IS NOT NULL
				   AND c.source_project <> sp.project_id`).Scan(&bad); err != nil {
				t.Fatalf("consistency read: %v", err)
			}
			if bad != 0 {
				t.Fatalf("%d components sit outside their page's project scope", bad)
			}
		})
	}
}

// [314] P0-2 — deleting a monitor a page RENDERS used to break the delete outright: the FK action
// nulled the binding and left `source='monitor'`, which the CHECK then rejected. Shipped
// behaviour is that the component becomes manual; the transition lives in a trigger because the
// project cascade deletes monitors with no application on the path.
func TestDeletingARenderedMonitorTurnsItsComponentManual(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	active, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageProj, Name: "Payments API", MonitorID: f.monitorID,
	})
	if err != nil {
		t.Fatalf("active component: %v", err)
	}
	// A second component holds the SAME monitor as a dormant binding behind a service source.
	dormantHolder, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageProj, Name: "Checkout", ServiceID: f.serviceID,
	})
	if err != nil {
		t.Fatalf("service component: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE components SET monitor_id = $2 WHERE id = $1`, dormantHolder.ID, f.monitorID); err != nil {
		t.Fatalf("plant dormant monitor: %v", err)
	}
	genBefore := pageGeneration(t, st, ctx, f.pageProj)

	if err := st.DeleteMonitor(ctx, f.monitorID); err != nil {
		t.Fatalf("deleting a monitor that backs a component must still work: %v", err)
	}

	got, err := st.GetComponent(ctx, active.ID)
	if err != nil {
		t.Fatalf("the component was destroyed with its monitor: %v", err)
	}
	if got.Source != domain.ComponentSourceManual || got.MonitorID != "" {
		t.Fatalf("active component = %+v, want manual with the binding cleared", got)
	}
	if got.SourceProject != "" {
		t.Fatalf("source project %q survived with no binding to justify it", got.SourceProject)
	}
	held, err := st.GetComponent(ctx, dormantHolder.ID)
	if err != nil {
		t.Fatalf("get service component: %v", err)
	}
	// The service source is untouched; only the dormant monitor binding is gone, and the project
	// stays because the service binding still needs it.
	if held.Source != domain.ComponentSourceService || held.MonitorID != "" || held.SourceProject != f.projectID {
		t.Fatalf("service component = %+v, want service source kept and only the dormant monitor cleared", held)
	}
	if pageGeneration(t, st, ctx, f.pageProj) <= genBefore {
		t.Fatal("the page generation did not advance, so a preview taken before the delete stays confirmable")
	}
}

// [314] P1-1 — a project CASCADE deletes components through FK actions with no application on the
// path. A surviving org-level page's generation had to advance anyway, or an operator's preview
// for a DIFFERENT component could be confirmed after a neighbour vanished.
func TestProjectCascadeAdvancesTheSurvivingPageGeneration(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	// An org-level page carrying a component from the project we are about to delete.
	victimSvc, err := st.CreateService(ctx, domain.Service{
		ProjectID: f.otherProj, Slug: "victim", Name: "Victim",
	})
	if err != nil {
		t.Fatalf("victim service: %v", err)
	}
	if _, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageOrg, Name: "Victim", ServiceID: victimSvc.ID,
	}); err != nil {
		t.Fatalf("victim component: %v", err)
	}
	// A survivor on the same page, whose operator holds a preview.
	survivor, err := st.CreateComponent(ctx, domain.Component{StatusPageID: f.pageOrg, Name: "Survivor"})
	if err != nil {
		t.Fatalf("survivor: %v", err)
	}
	plan, err := st.PreviewComponentConversion(ctx, f.orgID, survivor.ID,
		ComponentConversionTarget{Source: domain.ComponentSourceService, ServiceID: f.serviceID})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}

	if err := st.DeleteProject(ctx, f.orgID, f.otherProj); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if gen := pageGeneration(t, st, ctx, f.pageOrg); gen == plan.PageGeneration {
		t.Fatalf("generation stayed at %d through a cascade that removed a component", gen)
	}
	// The consequence that matters: the pre-cascade consent is dead.
	_, err = st.ConfirmComponentConversion(ctx, f.orgID, survivor.ID,
		ComponentConversionTarget{Source: domain.ComponentSourceService, ServiceID: f.serviceID},
		plan.Revision, plan.PageGeneration, convActor)
	if !errors.Is(err, ErrComponentConversionStale) {
		t.Fatalf("confirm after cascade = %v, want ErrComponentConversionStale", err)
	}
}

// [318] P0-2 — the REAL cycle test. The earlier version asserted only that an update blocks while
// another session holds the page, which a component→page path satisfies just as well: it blocks
// INSIDE the generation trigger, holding the component tuple. That is exactly the deadlock.
//
// This forces the cycle instead: T2 takes the component first (as the old code did implicitly),
// T1 takes the page first, then each reaches for what the other holds. With one order everywhere
// the pair serializes; with two orders PostgreSQL returns 40P01 to somebody.
func TestComponentMutationsCannotDeadlock(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	c, err := st.CreateComponent(ctx, domain.Component{StatusPageID: f.pageProj, Name: "A"})
	if err != nil {
		t.Fatalf("component: %v", err)
	}

	// T1: the confirm-shaped path — page, then the component.
	t1, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("t1: %v", err)
	}
	defer t1.Rollback(ctx) //nolint:errcheck // may already be done
	if _, err := t1.Exec(ctx,
		`SELECT 1 FROM status_pages WHERE id = $1 FOR UPDATE`, f.pageProj); err != nil {
		t.Fatalf("t1 page lock: %v", err)
	}

	// T2: the store's own update path. If it takes the component before the page, it will hold
	// that tuple while waiting for T1's page — and T1's next statement completes the cycle.
	t2done := make(chan error, 1)
	go func() {
		_, err := st.UpdateComponent(ctx, f.orgID, domain.Component{
			ID: c.ID, StatusPageID: c.StatusPageID, Name: "A renamed",
		})
		t2done <- err
	}()
	// Give T2 time to reach whatever it locks first.
	time.Sleep(300 * time.Millisecond)

	// T1 now wants the component. Under the OLD order this is the moment PostgreSQL detects the
	// cycle and kills one of the two with 40P01.
	_, t1Err := t1.Exec(ctx, `UPDATE components SET description = 'from t1' WHERE id = $1`, c.ID)
	commitErr := error(nil)
	if t1Err == nil {
		commitErr = t1.Commit(ctx)
	}
	var t2Err error
	select {
	case t2Err = <-t2done:
	case <-time.After(10 * time.Second):
		t.Fatal("the update never finished: the two paths are stuck on each other")
	}
	for _, err := range []error{t1Err, commitErr, t2Err} {
		if err == nil {
			continue
		}
		if strings.Contains(err.Error(), "40P01") || strings.Contains(strings.ToLower(err.Error()), "deadlock") {
			t.Fatalf("deadlock detected — the two paths do not share ONE lock order: %v", err)
		}
		t.Fatalf("unexpected error: %v", err)
	}
}

// [314] P1-2 — the ceiling has to ACTUALLY only shrink: lowered after every removal, and never
// raisable, including by a direct write.
func TestPageComponentCeilingOnlyShrinks(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	// The legacy oversized page, built the way 00081's backfill produced one: the components
	// existed FIRST (they predate the ceiling), and the ceiling then inherited their count. The
	// rows go in directly because CreateComponent is exactly the gate this state predates.
	ids := make([]string, 0, 60)
	for i := 0; i < 60; i++ {
		var id string
		if err := st.pool.QueryRow(ctx, `
			INSERT INTO components (status_page_id, org_id, source, name, position)
			VALUES ($1,$2,'manual',$3,$4) RETURNING id`,
			f.pageProj, f.orgID, fmt.Sprintf("legacy-%02d", i), i).Scan(&id); err != nil {
			t.Fatalf("component %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	// The inherited ceiling of 60 is what the MIGRATION's backfill produces for a page that
	// already held 60 components — and the backfill runs BEFORE the trigger exists, so it is the
	// only writer that can ever set a value above the floor. The trigger is therefore disabled for
	// exactly the seeding statement and re-enabled immediately: constructing the legacy state is
	// not the same as claiming an operator may reach it.
	if _, err := st.pool.Exec(ctx,
		`ALTER TABLE status_pages DISABLE TRIGGER status_page_ceiling_shrink_only_trg`); err != nil {
		t.Fatalf("disable trigger for seeding: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE status_pages SET component_ceiling = 60 WHERE id = $1`, f.pageProj); err != nil {
		t.Fatalf("seed ceiling: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`ALTER TABLE status_pages ENABLE TRIGGER status_page_ceiling_shrink_only_trg`); err != nil {
		t.Fatalf("re-enable trigger: %v", err)
	}
	// With the trigger live, ANY raise is refused — including one that would create no headroom.
	// §15.0 says only-shrink, and an admissible-value carve-out only weakened it.
	if _, err := st.pool.Exec(ctx,
		`UPDATE status_pages SET component_ceiling = 61 WHERE id = $1`, f.pageProj); err == nil {
		t.Fatal("the ceiling was raised above its current value")
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE status_pages SET component_ceiling = 60 WHERE id = $1`, f.pageProj); err != nil {
		t.Fatalf("setting the ceiling to its own value must be a no-op, not a refusal: %v", err)
	}
	if ceiling(t, st, ctx, f.pageProj) != 60 {
		t.Fatalf("ceiling = %d before any removal, want 60", ceiling(t, st, ctx, f.pageProj))
	}
	// One remediation: 60 → 59, and the page cannot grow back.
	if err := st.DeleteComponent(ctx, ids[0]); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := ceiling(t, st, ctx, f.pageProj); got != 59 {
		t.Fatalf("ceiling = %d after removing one of 60, want 59", got)
	}
	if _, err := st.CreateComponent(ctx, domain.Component{StatusPageID: f.pageProj, Name: "regrow"}); !errors.Is(err, ErrPageComponentCeiling) {
		t.Fatalf("the page regrew to its old size: %v", err)
	}
	// Repeated remediation keeps working, and stops at the floor of 50.
	for i := 1; i <= 15; i++ {
		if err := st.DeleteComponent(ctx, ids[i]); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	if got := ceiling(t, st, ctx, f.pageProj); got != 50 {
		t.Fatalf("ceiling = %d after shrinking below the floor, want the floor 50", got)
	}
	// A direct upward write is refused — the invariant is not merely an application habit.
	if _, err := st.pool.Exec(ctx,
		`UPDATE status_pages SET component_ceiling = 200 WHERE id = $1`, f.pageProj); err == nil {
		t.Fatal("the ceiling was raised by a direct write")
	} else if !strings.Contains(err.Error(), "status_page_ceiling_may_only_shrink") {
		t.Fatalf("upward write error = %v, want the named refusal", err)
	}
	// And even a raise back to the value this page ONCE had is refused: shrinking is permanent.
	if _, err := st.pool.Exec(ctx,
		`UPDATE status_pages SET component_ceiling = 59 WHERE id = $1`, f.pageProj); err == nil {
		t.Fatal("a shrunk ceiling was restored to an earlier value")
	}
}

// [314] P1-1 — the counters are DB-owned, so EVERY mutation moves them, not only the calls that
// remembered to.
func TestCountersMoveOnEveryMutation(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	c, err := st.CreateComponent(ctx, domain.Component{StatusPageID: f.pageProj, Name: "one"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	gen := pageGeneration(t, st, ctx, f.pageProj)
	rev := c.Revision

	// A DIRECT update — no application bump anywhere in sight.
	if _, err := st.pool.Exec(ctx, `UPDATE components SET name = 'two' WHERE id = $1`, c.ID); err != nil {
		t.Fatalf("direct update: %v", err)
	}
	after, err := st.GetComponent(ctx, c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Revision <= rev {
		t.Fatalf("revision %d → %d on a direct update", rev, after.Revision)
	}
	if g := pageGeneration(t, st, ctx, f.pageProj); g <= gen {
		t.Fatalf("page generation %d → %d on a direct update", gen, g)
	}
	gen, rev = pageGeneration(t, st, ctx, f.pageProj), after.Revision

	// And a direct DELETE.
	if _, err := st.pool.Exec(ctx, `DELETE FROM components WHERE id = $1`, c.ID); err != nil {
		t.Fatalf("direct delete: %v", err)
	}
	if g := pageGeneration(t, st, ctx, f.pageProj); g <= gen {
		t.Fatalf("page generation %d → %d on a direct delete", gen, g)
	}
	_ = rev
}

func pageGeneration(t *testing.T, st *Store, ctx context.Context, pageID string) int64 {
	t.Helper()
	var g int64
	if err := st.pool.QueryRow(ctx,
		`SELECT component_generation FROM status_pages WHERE id = $1`, pageID).Scan(&g); err != nil {
		t.Fatalf("read generation: %v", err)
	}
	return g
}

func ceiling(t *testing.T, st *Store, ctx context.Context, pageID string) int {
	t.Helper()
	var c int
	if err := st.pool.QueryRow(ctx,
		`SELECT component_ceiling FROM status_pages WHERE id = $1`, pageID).Scan(&c); err != nil {
		t.Fatalf("read ceiling: %v", err)
	}
	return c
}

// [318] P1-3 — moving a component between pages changes BOTH of them. `COALESCE(NEW, OLD)` bumped
// only the destination, so a preview taken against the SOURCE page stayed confirmable after the
// line it summarized had left.
func TestMovingAComponentBumpsBothPages(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	c, err := st.CreateComponent(ctx, domain.Component{StatusPageID: f.pageOrg, Name: "Mover"})
	if err != nil {
		t.Fatalf("component: %v", err)
	}
	// A neighbour on the SOURCE page whose operator holds a preview.
	neighbour, err := st.CreateComponent(ctx, domain.Component{StatusPageID: f.pageOrg, Name: "Neighbour"})
	if err != nil {
		t.Fatalf("neighbour: %v", err)
	}
	plan, err := st.PreviewComponentConversion(ctx, f.orgID, neighbour.ID,
		ComponentConversionTarget{Source: domain.ComponentSourceService, ServiceID: f.serviceID})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	srcBefore := pageGeneration(t, st, ctx, f.pageOrg)
	dstBefore := pageGeneration(t, st, ctx, f.pageProj)

	if _, err := st.pool.Exec(ctx,
		`UPDATE components SET status_page_id = $2 WHERE id = $1`, c.ID, f.pageProj); err != nil {
		t.Fatalf("move: %v", err)
	}
	if got := pageGeneration(t, st, ctx, f.pageProj); got <= dstBefore {
		t.Fatalf("destination generation %d → %d", dstBefore, got)
	}
	if got := pageGeneration(t, st, ctx, f.pageOrg); got <= srcBefore {
		t.Fatalf("SOURCE generation stayed at %d after losing a component — a preview against it "+
			"is still confirmable", got)
	}
	// The consequence the counter exists for.
	if _, err := st.ConfirmComponentConversion(ctx, f.orgID, neighbour.ID,
		ComponentConversionTarget{Source: domain.ComponentSourceService, ServiceID: f.serviceID},
		plan.Revision, plan.PageGeneration, convActor); !errors.Is(err, ErrComponentConversionStale) {
		t.Fatalf("confirm after a neighbour left the page = %v, want ErrComponentConversionStale", err)
	}
}

// [318] P1-4 — a database left at goose 81 from BEFORE the migration was folded looks current and
// silently runs the pre-fold schema, which is exactly how the reviewer's two DSNs reported a
// version they did not have. Every run of this package fails loudly with what to do instead.
func TestSchemaMatchesTheFrozenMigration(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	required := []struct{ kind, name string }{
		{"trigger", "monitor_delete_release_components_trg"},
		{"trigger", "component_revision_bump_trg"},
		{"trigger", "component_page_generation_bump_trg"},
		{"trigger", "status_page_ceiling_shrink_only_trg"},
		{"trigger", "components_page_scope_trg"},
	}
	for _, r := range required {
		var present bool
		if err := st.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = $1 AND NOT tgisinternal)`,
			r.name).Scan(&present); err != nil {
			t.Fatalf("check %s: %v", r.name, err)
		}
		if !present {
			t.Fatalf("%s %s is missing while goose reports the migration applied. This database "+
				"predates the folded 00081: DROP and RECREATE it, then rerun. A stale schema that "+
				"reports itself current is what made an earlier both-modes claim unreproducible.",
				r.kind, r.name)
		}
	}
	// The binding⇔project CHECK must be the FOLDED one (keyed on the presence of a binding), not
	// the first version keyed on `source`.
	var def string
	if err := st.pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conname = 'components_binding_project_chk'`).Scan(&def); err != nil {
		t.Fatalf("read binding check: %v", err)
	}
	if !strings.Contains(def, "monitor_id IS NULL") || !strings.Contains(def, "service_id IS NULL") {
		t.Fatalf("components_binding_project_chk is the PRE-FOLD definition (%s): recreate this database", def)
	}
}
