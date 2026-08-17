package store

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamlead-com/cerbix/internal/domain"
)

type impactFixture struct {
	projectID string
	svc       map[string]string // short name → service id
	mon       map[string]string // short name → monitor id
}

// seedImpact creates a project and, for each name, a service (slug = name) with
// one monitor declared in its monitors[] (empty SLI — valid, and correlation
// must key off membership, not SLI).
func seedImpact(t *testing.T, st *Store, ctx context.Context, names ...string) impactFixture {
	t.Helper()
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "payments", "Payments")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	f := impactFixture{projectID: proj.ID, svc: map[string]string{}, mon: map[string]string{}}
	for _, name := range names {
		m, err := st.CreateMonitor(ctx, domain.Monitor{
			ProjectID: proj.ID, Name: name + "-mon", Type: domain.MonitorHTTP,
			Target: "https://" + name + ".example.com/", IntervalSeconds: 30, Region: "core", Enabled: true,
		})
		if err != nil {
			t.Fatalf("monitor %s: %v", name, err)
		}
		f.mon[name] = m.ID
		svc, err := st.CreateService(ctx, domain.Service{ProjectID: proj.ID, Slug: name, Name: name})
		if err != nil {
			t.Fatalf("service %s: %v", name, err)
		}
		f.svc[name] = svc.ID
		if _, _, err := st.PutServiceDeclaration(ctx, proj.ID, svc.ID, domain.ServiceDeclaration{
			Monitors: []string{m.ID},
		}, 0, DeclarationOptions{CreatedBy: "t"}); err != nil {
			t.Fatalf("declaration %s: %v", name, err)
		}
	}
	return f
}

func (f impactFixture) edge(t *testing.T, st *Store, ctx context.Context, child, parent string, gen int64) {
	t.Helper()
	if _, err := st.ReplaceServiceDependencies(ctx,
		f.projectID, f.svc[child], []string{f.svc[parent]}, gen, GraphActor{Label: "t"}); err != nil {
		t.Fatalf("edge %s→%s: %v", child, parent, err)
	}
}

func (f impactFixture) open(t *testing.T, st *Store, ctx context.Context, name string) domain.Incident {
	t.Helper()
	inc, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: f.projectID, MonitorID: f.mon[name], Title: name + " is down",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor, Source: domain.SourceAuto,
	}, name+" failed", "auto")
	if err != nil {
		t.Fatalf("open incident %s: %v", name, err)
	}
	return inc
}

// relationRows snapshots the whole impact relation as comparable strings.
func relationRows(t *testing.T, st *Store, ctx context.Context) []string {
	t.Helper()
	rows, err := st.pool.Query(ctx, `
		SELECT i.incident_id, s.slug, i.role, array_to_string(i.path, '>')
		  FROM incident_service_impacts i JOIN services s ON s.id = i.service_id
		 ORDER BY 1, 2, 3`)
	if err != nil {
		t.Fatalf("relation: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var inc, slug, role, path string
		if err := rows.Scan(&inc, &slug, &role, &path); err != nil {
			t.Fatalf("scan relation: %v", err)
		}
		out = append(out, inc+"|"+slug+"|"+role+"|"+path)
	}
	return out
}

func impactNotes(t *testing.T, st *Store, ctx context.Context, incidentID string) []string {
	t.Helper()
	rows, err := st.pool.Query(ctx,
		`SELECT body FROM incident_updates WHERE incident_id = $1 AND author = 'system' AND body LIKE $2 ORDER BY created_at`,
		incidentID, domain.ImpactMarker+"%")
	if err != nil {
		t.Fatalf("notes: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			t.Fatalf("scan note: %v", err)
		}
		out = append(out, b)
	}
	return out
}

// Both opening interleavings converge to the SAME relation content (invariants
// 52–55): child-first and parent-first end with the child holding
// probable_root(parent) and the parent holding affected(child), each with the
// identical root-first path, plus one 🕸 note per newly-annotated incident.
func TestCorrelateBothInterleavings(t *testing.T) {
	run := func(t *testing.T, parentFirst bool) []string {
		st, ctx := serviceSchemaStore(t)
		f := seedImpact(t, st, ctx, "payments", "checkout")
		f.edge(t, st, ctx, "checkout", "payments", 0)

		var childInc, parentInc domain.Incident
		if parentFirst {
			parentInc = f.open(t, st, ctx, "payments")
			if _, _, err := st.CorrelateIncident(ctx, parentInc.ID); err != nil {
				t.Fatalf("correlate parent: %v", err)
			}
			childInc = f.open(t, st, ctx, "checkout")
			if _, _, err := st.CorrelateIncident(ctx, childInc.ID); err != nil {
				t.Fatalf("correlate child: %v", err)
			}
		} else {
			childInc = f.open(t, st, ctx, "checkout")
			if _, _, err := st.CorrelateIncident(ctx, childInc.ID); err != nil {
				t.Fatalf("correlate child: %v", err)
			}
			parentInc = f.open(t, st, ctx, "payments")
			if _, _, err := st.CorrelateIncident(ctx, parentInc.ID); err != nil {
				t.Fatalf("correlate parent: %v", err)
			}
		}

		rows := relationRows(t, st, ctx)
		// Incident ids differ per run, so compare the (slug, role, path) shapes as
		// a set — plus the id-bearing note assertions below.
		got := map[string]bool{}
		for _, r := range rows {
			got[strings.SplitN(r, "|", 2)[1]] = true
		}
		if len(rows) != 2 || !got["payments|probable_root|payments>checkout"] || !got["checkout|affected|payments>checkout"] {
			t.Fatalf("relation = %v, want the probable_root/affected pair with the shared root-first path", rows)
		}
		if n := impactNotes(t, st, ctx, childInc.ID); len(n) != 1 || !strings.Contains(n[0], "probable root — payments (via payments → checkout)") {
			t.Errorf("child notes = %v", n)
		}
		if n := impactNotes(t, st, ctx, parentInc.ID); len(n) != 1 || !strings.Contains(n[0], "affected — checkout") {
			t.Errorf("parent notes = %v", n)
		}
		return rows
	}
	t.Run("child-first", func(t *testing.T) { run(t, false) })
	t.Run("parent-first", func(t *testing.T) { run(t, true) })
}

// A redelivery inserts zero rows, writes zero notes, and leaves the relation
// byte-identical (invariant 54).
func TestCorrelateRedeliveryIsByteIdentical(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedImpact(t, st, ctx, "payments", "checkout")
	f.edge(t, st, ctx, "checkout", "payments", 0)
	f.open(t, st, ctx, "payments")
	child := f.open(t, st, ctx, "checkout")

	first, _, err := st.CorrelateIncident(ctx, child.ID)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("first attempt inserted nothing")
	}
	before := relationRows(t, st, ctx)
	notesBefore := impactNotes(t, st, ctx, child.ID)

	again, _, err := st.CorrelateIncident(ctx, child.ID)
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("redelivery inserted %d links, want 0", len(again))
	}
	if after := relationRows(t, st, ctx); !reflect.DeepEqual(before, after) {
		t.Fatalf("relation changed across redelivery:\nbefore %v\nafter  %v", before, after)
	}
	if notesAfter := impactNotes(t, st, ctx, child.ID); len(notesAfter) != len(notesBefore) {
		t.Fatalf("redelivery wrote a note: %d → %d", len(notesBefore), len(notesAfter))
	}
}

// Resolved incidents are never annotated (invariant 56): an anchor resolved
// before its attempt is skipped entirely, and a witness that resolves while
// the attempt waits on its row lock is dropped by the under-lock recheck —
// check-then-act loses to the barrier.
func TestCorrelateResolveBarrier(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedImpact(t, st, ctx, "payments", "checkout")
	f.edge(t, st, ctx, "checkout", "payments", 0)
	parent := f.open(t, st, ctx, "payments")
	child := f.open(t, st, ctx, "checkout")

	// (a) anchor resolved before any attempt → the whole attempt is void.
	if _, err := st.AddIncidentUpdate(ctx, domain.IncidentUpdate{
		IncidentID: child.ID, Status: domain.IncidentResolved, Body: "fixed", Author: "t",
	}); err != nil {
		t.Fatalf("resolve child: %v", err)
	}
	links, _, err := st.CorrelateIncident(ctx, child.ID)
	if err != nil {
		t.Fatalf("correlate resolved anchor: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("resolved anchor gained %d links", len(links))
	}

	// (b) the witness resolves while the attempt is blocked on its lock: the
	// recheck under the lock must drop the link.
	child2 := f.open(t, st, ctx, "checkout")
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM incidents WHERE id = $1 FOR UPDATE`, parent.ID); err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	done := make(chan error, 1)
	var raced []domain.ServiceImpactLink
	go func() {
		var err error
		raced, _, err = st.CorrelateIncident(ctx, child2.ID)
		done <- err
	}()
	// Deterministic barrier ([276] P1-5b): wait until the attempt is DEMONSTRABLY
	// blocked on the incident row lock — a sleep could pass even with the
	// under-lock recheck deleted, because a late goroutine would simply read the
	// already-resolved witness.
	waitForLockWaiter(t, st, `FROM incidents WHERE id = ANY%FOR UPDATE`)
	if _, err := tx.Exec(ctx,
		`UPDATE incidents SET status = 'resolved', resolved_at = now() WHERE id = $1`, parent.ID); err != nil {
		t.Fatalf("resolve under lock: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit resolve: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("raced correlate: %v", err)
	}
	if len(raced) != 0 {
		t.Fatalf("attempt linked a witness that resolved under its lock: %+v", raced)
	}
	if rows := relationRows(t, st, ctx); len(rows) != 0 {
		t.Fatalf("relation = %v, want empty", rows)
	}
}

// The canonical path (invariant 55): a diamond resolves by lexicographic
// tie-break at equal length; a direct edge stores 2 slugs; a maximal depth-10
// chain stores 11 — the schema bound.
func TestCanonicalPathDiamondAndBounds(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	// diamond: d depends on b and c; b and c depend on a.
	f := seedImpact(t, st, ctx, "a", "b", "c", "d")
	f.edge(t, st, ctx, "b", "a", 0)
	f.edge(t, st, ctx, "c", "a", 0)
	if _, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["d"],
		[]string{f.svc["b"], f.svc["c"]}, 0, GraphActor{Label: "t"}); err != nil {
		t.Fatalf("diamond edges: %v", err)
	}
	f.open(t, st, ctx, "a")
	dInc := f.open(t, st, ctx, "d")
	if _, _, err := st.CorrelateIncident(ctx, dInc.ID); err != nil {
		t.Fatalf("correlate: %v", err)
	}
	var path string
	if err := st.pool.QueryRow(ctx, `
		SELECT array_to_string(path, '>') FROM incident_service_impacts
		 WHERE incident_id = $1 AND role = 'probable_root' AND service_id = $2`,
		dInc.ID, f.svc["a"]).Scan(&path); err != nil {
		t.Fatalf("path: %v", err)
	}
	if path != "a>b>d" {
		t.Fatalf("diamond path = %q, want the lexicographic winner a>b>d", path)
	}
	// direct edge = 2 slugs
	var bPath string
	bInc := f.open(t, st, ctx, "b")
	if _, _, err := st.CorrelateIncident(ctx, bInc.ID); err != nil {
		t.Fatalf("correlate b: %v", err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT array_to_string(path, '>') FROM incident_service_impacts
		 WHERE incident_id = $1 AND role = 'probable_root' AND service_id = $2`,
		bInc.ID, f.svc["a"]).Scan(&bPath); err != nil {
		t.Fatalf("b path: %v", err)
	}
	if bPath != "a>b" {
		t.Fatalf("direct-edge path = %q, want a>b", bPath)
	}
}

// The maximal valid chain: 10 edges, 11 endpoint-inclusive slugs — exactly the
// schema bound, proven end to end through a real correlation.
func TestCanonicalPathDepthTenChain(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	names := make([]string, 11)
	for i := range names {
		names[i] = fmt.Sprintf("c%02d", i)
	}
	f := seedImpact(t, st, ctx, names...)
	for i := 1; i < 11; i++ {
		f.edge(t, st, ctx, names[i], names[i-1], 0)
	}
	f.open(t, st, ctx, names[0])
	leaf := f.open(t, st, ctx, names[10])
	if _, _, err := st.CorrelateIncident(ctx, leaf.ID); err != nil {
		t.Fatalf("correlate: %v", err)
	}
	var length int
	if err := st.pool.QueryRow(ctx, `
		SELECT array_length(path, 1) FROM incident_service_impacts
		 WHERE incident_id = $1 AND role = 'probable_root' AND service_id = $2`,
		leaf.ID, f.svc[names[0]]).Scan(&length); err != nil {
		t.Fatalf("length: %v", err)
	}
	if length != 11 {
		t.Fatalf("depth-10 chain path length = %d, want 11", length)
	}
}

// Invariant 48 at the schema, for the relation and the anchor: a cross-project
// impact row and a cross-project incident anchor are both unrepresentable to a
// direct SQL writer.
func TestImpactTenancyBySchema(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedImpact(t, st, ctx, "payments")
	inc := f.open(t, st, ctx, "payments")

	org2, _ := st.CreateOrganization(ctx, "other", "Other")
	proj2, err := st.CreateProject(ctx, org2.ID, "other", "Other")
	if err != nil {
		t.Fatalf("project2: %v", err)
	}
	alienSvc, err := st.CreateService(ctx, domain.Service{ProjectID: proj2.ID, Slug: "alien", Name: "alien"})
	if err != nil {
		t.Fatalf("alien service: %v", err)
	}
	alienMon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj2.ID, Name: "alien-mon", Type: domain.MonitorHTTP,
		Target: "https://alien.example.com/", IntervalSeconds: 30, Region: "core", Enabled: true,
	})
	if err != nil {
		t.Fatalf("alien monitor: %v", err)
	}

	for _, proj := range []string{f.projectID, proj2.ID} {
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO incident_service_impacts (incident_id, service_id, project_id, role, path)
			VALUES ($1, $2, $3, 'affected', ARRAY['x','y'])`,
			inc.ID, alienSvc.ID, proj); err == nil {
			t.Fatalf("cross-project impact accepted with project_id=%s", proj)
		}
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE incidents SET monitor_id = $1 WHERE id = $2`, alienMon.ID, inc.ID); err == nil {
		t.Fatal("cross-project monitor anchor accepted")
	}
}

// waitForLockWaiter blocks until some backend is demonstrably waiting on a lock
// while executing a query matching pattern (SQL LIKE, wildcards allowed inside)
// — the deterministic barrier of [276] P1-5b. Bounded: fails after ~5s.
func waitForLockWaiter(t *testing.T, st *Store, pattern string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := st.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			 WHERE wait_event_type = 'Lock' AND query LIKE '%' || $1 || '%'`, pattern).Scan(&n); err != nil {
			t.Fatalf("poll lock waiter: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no backend ever waited on the expected lock — the barrier was never reached")
}

// waitForAdvisoryWaiter blocks until some backend is waiting on an advisory
// lock (the snapshot-lock barriers of [276] P1-1). Bounded: fails after ~5s.
func waitForAdvisoryWaiter(t *testing.T, st *Store) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := st.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&n); err != nil {
			t.Fatalf("poll advisory waiter: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no backend ever waited on an advisory lock — the barrier was never reached")
}

// openManual opens a MANUAL monitor-anchored incident — unlike auto incidents
// these have no one-open-per-monitor index, which is exactly the unbounded
// witness source the [278] bound exists for.
func (f impactFixture) openManual(t *testing.T, st *Store, ctx context.Context, name, title string) domain.Incident {
	t.Helper()
	inc, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: f.projectID, MonitorID: f.mon[name], Title: title,
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMinor, Source: domain.SourceManual,
	}, title, "t")
	if err != nil {
		t.Fatalf("open manual incident %s: %v", title, err)
	}
	return inc
}

// The links+notes transaction is all-or-nothing ([276] P1-5a): a failed 🕸-note
// insert rolls the LINK batch back with it, and the retry then produces exactly
// one batch — never links without the promised note, never a duplicate.
func TestCorrelateNoteFailureRollsBackLinks(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedImpact(t, st, ctx, "payments", "checkout")
	f.edge(t, st, ctx, "checkout", "payments", 0)
	f.open(t, st, ctx, "payments")
	child := f.open(t, st, ctx, "checkout")

	st.correlateNoteFault = func() error { return fmt.Errorf("injected note failure") }
	if _, _, err := st.CorrelateIncident(ctx, child.ID); err == nil {
		t.Fatal("faulted attempt reported success")
	}
	if rows := relationRows(t, st, ctx); len(rows) != 0 {
		t.Fatalf("links survived a failed note insert: %v", rows)
	}
	if n := impactNotes(t, st, ctx, child.ID); len(n) != 0 {
		t.Fatalf("notes survived the rollback: %v", n)
	}

	st.correlateNoteFault = nil
	links, _, err := st.CorrelateIncident(ctx, child.ID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(links) == 0 {
		t.Fatal("retry inserted nothing")
	}
	if n := impactNotes(t, st, ctx, child.ID); len(n) != 1 {
		t.Fatalf("retry notes = %d, want exactly one batch note", len(n))
	}
}

// The attempt snapshot is ONE committed state ([276] P1-1): a graph replace
// committing while the attempt waits on the service_graph lock is fully
// visible to it — the attempt can never encode an edge that was gone before
// its witness existed.
func TestCorrelateSnapshotBarrierGraphReplace(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedImpact(t, st, ctx, "payments", "checkout")
	f.edge(t, st, ctx, "checkout", "payments", 0)
	f.open(t, st, ctx, "payments")
	child := f.open(t, st, ctx, "checkout")

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := lockServiceGraph(ctx, tx, f.projectID); err != nil {
		t.Fatalf("hold graph lock: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM service_dependencies WHERE service_id = $1`, f.svc["checkout"]); err != nil {
		t.Fatalf("delete edge under lock: %v", err)
	}

	done := make(chan error, 1)
	var raced []domain.ServiceImpactLink
	go func() {
		var err error
		raced, _, err = st.CorrelateIncident(ctx, child.ID)
		done <- err
	}()
	waitForAdvisoryWaiter(t, st) // the attempt is blocked BEFORE any graph read
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit edge removal: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("raced correlate: %v", err)
	}
	if len(raced) != 0 {
		t.Fatalf("attempt used an edge that was removed before it read the graph: %+v", raced)
	}
	if rows := relationRows(t, st, ctx); len(rows) != 0 {
		t.Fatalf("relation = %v, want empty", rows)
	}
}

// The same barrier for MEMBERSHIP: a declaration-side membership change
// committing while the attempt waits on service_membership is fully visible —
// the attempt never mixes old membership with newer witnesses.
func TestCorrelateSnapshotBarrierMembershipChange(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedImpact(t, st, ctx, "payments", "checkout")
	f.edge(t, st, ctx, "checkout", "payments", 0)
	f.open(t, st, ctx, "payments")
	child := f.open(t, st, ctx, "checkout")

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := lockServiceMembership(ctx, tx, f.projectID); err != nil {
		t.Fatalf("hold membership lock: %v", err)
	}
	// The payments monitor leaves the payments service's membership: its open
	// incident then witnesses nothing.
	if _, err := tx.Exec(ctx, `DELETE FROM service_member_refs WHERE monitor_id = $1`, f.mon["payments"]); err != nil {
		t.Fatalf("remove membership under lock: %v", err)
	}

	done := make(chan error, 1)
	var raced []domain.ServiceImpactLink
	go func() {
		var err error
		raced, _, err = st.CorrelateIncident(ctx, child.ID)
		done <- err
	}()
	waitForAdvisoryWaiter(t, st)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit membership removal: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("raced correlate: %v", err)
	}
	if len(raced) != 0 {
		t.Fatalf("attempt used membership that was removed before it read: %+v", raced)
	}
}

// The [278] witness bound: with six open anchored incidents on one upstream
// service, the attempt selects the OLDEST five by (started_at, id), reports
// overflow 1, marks the SERVICE exactly once (service-level completeness is
// unchanged), back-fills exactly the five selected witnesses, and leaves the
// newest incident without a row — deterministically, never silently.
func TestCorrelateWitnessBoundDeterministic(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedImpact(t, st, ctx, "payments", "checkout")
	f.edge(t, st, ctx, "checkout", "payments", 0)

	witnesses := make([]domain.Incident, 6)
	for i := range witnesses {
		witnesses[i] = f.openManual(t, st, ctx, "payments", fmt.Sprintf("manual outage %d", i))
	}
	child := f.open(t, st, ctx, "checkout")

	links, overflow, err := st.CorrelateIncident(ctx, child.ID)
	if err != nil {
		t.Fatalf("correlate: %v", err)
	}
	if overflow != 1 {
		t.Fatalf("overflow = %d, want 1 (six witnesses, bound five)", overflow)
	}
	// 1 probable_root on the anchor (the SERVICE marked once) + 5 affected back-fills.
	var roots, affected int
	for _, l := range links {
		switch l.Role {
		case domain.ImpactProbableRoot:
			roots++
		case domain.ImpactAffected:
			affected++
		}
	}
	if roots != 1 || affected != domain.MaxCorrelationWitnessesPerService {
		t.Fatalf("links = %d roots / %d affected, want 1 / %d", roots, affected, domain.MaxCorrelationWitnessesPerService)
	}
	// The NEWEST witness (index 5) is the deterministically unselected one.
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM incident_service_impacts WHERE incident_id = $1`, witnesses[5].ID).Scan(&n); err != nil {
		t.Fatalf("count newest: %v", err)
	}
	if n != 0 {
		t.Fatalf("the over-bound newest witness gained %d rows, want 0", n)
	}
	for i := 0; i < domain.MaxCorrelationWitnessesPerService; i++ {
		if err := st.pool.QueryRow(ctx,
			`SELECT count(*) FROM incident_service_impacts WHERE incident_id = $1 AND role = 'affected'`, witnesses[i].ID).Scan(&n); err != nil {
			t.Fatalf("count witness %d: %v", i, err)
		}
		if n != 1 {
			t.Fatalf("selected witness %d has %d affected rows, want 1", i, n)
		}
	}
}

// The impact read is tenant-scoped at the owning data boundary ([276] P0-1): a
// caller scoped to another project gets nothing for a foreign incident id even
// with the handler-level access check removed.
func TestListIncidentImpactsTenantScoped(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedImpact(t, st, ctx, "payments", "checkout")
	f.edge(t, st, ctx, "checkout", "payments", 0)
	f.open(t, st, ctx, "payments")
	child := f.open(t, st, ctx, "checkout")
	if _, _, err := st.CorrelateIncident(ctx, child.ID); err != nil {
		t.Fatalf("correlate: %v", err)
	}

	own, err := st.ListIncidentImpacts(ctx, f.projectID, child.ID)
	if err != nil {
		t.Fatalf("own-project list: %v", err)
	}
	if len(own) == 0 {
		t.Fatal("own-project list returned nothing")
	}

	org2, _ := st.CreateOrganization(ctx, "other", "Other")
	proj2, err := st.CreateProject(ctx, org2.ID, "other", "Other")
	if err != nil {
		t.Fatalf("project2: %v", err)
	}
	foreign, err := st.ListIncidentImpacts(ctx, proj2.ID, child.ID)
	if err != nil {
		t.Fatalf("foreign-project list: %v", err)
	}
	if len(foreign) != 0 {
		t.Fatalf("a foreign project scope read %d impact rows: %+v", len(foreign), foreign)
	}
}

// Overflow is scoped to the anchor's REACHABLE endpoints ([283]): a pile of
// open incidents on an UNRELATED service produces no links, no overflow and no
// witness reads beyond the reachable scope — never false omission telemetry.
func TestCorrelateUnrelatedServiceNeverCountsOverflow(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedImpact(t, st, ctx, "payments", "checkout", "foo")
	f.edge(t, st, ctx, "checkout", "payments", 0)
	// foo is connected to nothing; give it six open anchored incidents.
	for i := 0; i < 6; i++ {
		f.openManual(t, st, ctx, "foo", fmt.Sprintf("foo outage %d", i))
	}
	child := f.open(t, st, ctx, "checkout")

	links, overflow, err := st.CorrelateIncident(ctx, child.ID)
	if err != nil {
		t.Fatalf("correlate: %v", err)
	}
	if overflow != 0 {
		t.Fatalf("overflow = %d for an anchor whose reachable endpoints have no witnesses — false omission telemetry", overflow)
	}
	if len(links) != 0 {
		t.Fatalf("links = %+v, want none", links)
	}
}

// countingTracer counts pgx query executions on a connection.
type countingTracer struct{ n int }

func (t *countingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.n++
	return ctx
}
func (t *countingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// The neighbour-health read issues a CONSTANT number of SQL statements
// regardless of the neighbour count ([288] P1-2, invariant 60). The old
// per-neighbour loop issued up to five EACH, and downstream fan-in is bounded
// only by the project service cap — this test would have caught ~995
// statements for one detail read.
func TestNeighbourHealthFixedStatementCount(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")

	// A 30-neighbour set: one hub with many downstream dependents, each with its
	// own declared monitor, so every neighbour has real epoch/observation inputs.
	names := make([]string, 0, 31)
	names = append(names, "hub")
	for i := 0; i < 30; i++ {
		names = append(names, fmt.Sprintf("dep%02d", i))
	}
	f := seedImpact(t, st, ctx, names...)
	neighbours := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		n := fmt.Sprintf("dep%02d", i)
		f.edge(t, st, ctx, n, "hub", 0)
		neighbours = append(neighbours, f.svc[n])
	}

	measure := func(ids []string) int {
		t.Helper()
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("parse dsn: %v", err)
		}
		tracer := &countingTracer{}
		cfg.ConnConfig.Tracer = tracer
		cfg.MaxConns = 2
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		defer pool.Close()
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // read-only
		var asOf time.Time
		if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&asOf); err != nil {
			t.Fatalf("clock: %v", err)
		}
		start := tracer.n
		if _, err := serviceNeighbourHealthTx(ctx, tx, f.projectID, ids, asOf.UTC()); err != nil {
			t.Fatalf("neighbour health: %v", err)
		}
		return tracer.n - start
	}

	one := measure(neighbours[:1])
	thirty := measure(neighbours)
	if one != thirty {
		t.Fatalf("statement count grew with the neighbour set: 1 neighbour = %d, 30 neighbours = %d — this is the N+1 invariant 60 forbids", one, thirty)
	}
	if thirty > 6 {
		t.Fatalf("neighbour health issued %d statements; the batched contract is four set-wise reads", thirty)
	}
}

// The batched verdicts must be IDENTICAL to the single-service owner's
// (serviceHealthNowTx) — one semantics owner, only the loading is batched.
func TestNeighbourHealthMatchesSingleServiceOwner(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedImpact(t, st, ctx, "hub", "a", "b", "c")
	f.edge(t, st, ctx, "a", "hub", 0)
	f.edge(t, st, ctx, "b", "hub", 0)
	f.edge(t, st, ctx, "c", "hub", 0)
	// Give the set a mix: one down monitor, one up, one never-confirmed.
	if _, err := st.pool.Exec(ctx, `UPDATE monitors SET status = 'down' WHERE id = $1`, f.mon["a"]); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE monitors SET status = 'up' WHERE id = $1`, f.mon["b"]); err != nil {
		t.Fatalf("up: %v", err)
	}

	ids := []string{f.svc["a"], f.svc["b"], f.svc["c"]}
	batch, err := st.ServiceNeighbourHealth(ctx, f.projectID, ids)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	for _, id := range ids {
		single, err := st.ServiceHealthNow(ctx, f.projectID, id)
		if err != nil {
			t.Fatalf("single %s: %v", id, err)
		}
		got := batch[id]
		if got.SLI != single.SLI || got.Diagnostics != single.Diagnostics ||
			!reflect.DeepEqual(got.FailingMonitors, single.FailingMonitors) {
			t.Fatalf("service %s: batched %+v != single-owner %+v", id, got, single)
		}
	}
	// A service outside the project is absent from the map, never a guessed entry.
	org2, _ := st.CreateOrganization(ctx, "other", "Other")
	proj2, err := st.CreateProject(ctx, org2.ID, "other", "Other")
	if err != nil {
		t.Fatalf("project2: %v", err)
	}
	alien, err := st.CreateService(ctx, domain.Service{ProjectID: proj2.ID, Slug: "alien", Name: "alien"})
	if err != nil {
		t.Fatalf("alien: %v", err)
	}
	m, err := st.ServiceNeighbourHealth(ctx, f.projectID, []string{alien.ID})
	if err != nil {
		t.Fatalf("foreign: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("a foreign service produced a health entry: %+v", m)
	}
}
