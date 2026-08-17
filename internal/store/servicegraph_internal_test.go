package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

type graphFixture struct {
	orgID     string
	projectID string
	// services by short name → id
	svc map[string]string
}

// seedGraph creates a project with n bare services named s0..s(n-1) (slug = name).
// Services need no declaration to participate in the graph — edges are outside
// the declaration axes by design.
func seedGraph(t *testing.T, st *Store, ctx context.Context, n int) graphFixture {
	t.Helper()
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "payments", "Payments")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	f := graphFixture{orgID: org.ID, projectID: proj.ID, svc: map[string]string{}}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("s%d", i)
		svc, err := st.CreateService(ctx, domain.Service{ProjectID: proj.ID, Slug: name, Name: name})
		if err != nil {
			t.Fatalf("service %s: %v", name, err)
		}
		f.svc[name] = svc.ID
	}
	return f
}

// The replace-set round-trip: read returns both directions plus the generation
// token; a non-no-op bumps the generation and audits the delta; an identical
// set bumps nothing and audits nothing (invariant 50).
func TestServiceGraphReplaceReadAndNoOp(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedGraph(t, st, ctx, 3)

	v, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s0"], []string{f.svc["s1"], f.svc["s2"]}, 0, GraphActor{Label: "seymur@teamlead.com"})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if v.GraphGeneration != 1 {
		t.Errorf("generation = %d, want 1", v.GraphGeneration)
	}
	if len(v.DependsOn) != 2 {
		t.Fatalf("depends_on = %d edges, want 2", len(v.DependsOn))
	}
	// The reverse direction, from the parent's point of view.
	pv, err := st.GetServiceDependencies(ctx, f.projectID, f.svc["s1"])
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}
	if len(pv.DependedOnBy) != 1 || pv.DependedOnBy[0].Slug != "s0" {
		t.Errorf("s1 depended_on_by = %+v, want [s0]", pv.DependedOnBy)
	}
	if pv.GraphGeneration != 0 {
		t.Errorf("the parent's OWN generation must be untouched by the child's write, got %d", pv.GraphGeneration)
	}

	var audits int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs WHERE action = 'service.dependencies.replaced'`).Scan(&audits); err != nil {
		t.Fatalf("count audits: %v", err)
	}
	if audits != 1 {
		t.Fatalf("audit rows = %d, want exactly 1 for one non-no-op write", audits)
	}
	var target string
	if err := st.pool.QueryRow(ctx,
		`SELECT target FROM audit_logs WHERE action = 'service.dependencies.replaced'`).Scan(&target); err != nil {
		t.Fatalf("audit target: %v", err)
	}
	for _, want := range []string{"actor=seymur@teamlead.com", "added=[s1,s2]", "removed=[]"} {
		if !strings.Contains(target, want) {
			t.Errorf("audit target %q misses %q", target, want)
		}
	}

	// No-op: identical set → no bump, no audit.
	v2, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s0"], []string{f.svc["s2"], f.svc["s1"]}, 1, GraphActor{Label: "seymur@teamlead.com"})
	if err != nil {
		t.Fatalf("no-op replace: %v", err)
	}
	if v2.GraphGeneration != 1 {
		t.Errorf("no-op bumped the generation to %d", v2.GraphGeneration)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs WHERE action = 'service.dependencies.replaced'`).Scan(&audits); err != nil {
		t.Fatalf("count audits: %v", err)
	}
	if audits != 1 {
		t.Errorf("no-op wrote an audit row (%d total)", audits)
	}

	// A removal audits too — created_by on surviving rows cannot testify about it.
	if _, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s0"], []string{f.svc["s1"]}, 1, GraphActor{Label: "seymur@teamlead.com"}); err != nil {
		t.Fatalf("removal replace: %v", err)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT target FROM audit_logs WHERE action = 'service.dependencies.replaced' ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&target); err != nil {
		t.Fatalf("removal audit: %v", err)
	}
	if !strings.Contains(target, "removed=[s2]") {
		t.Errorf("removal audit target %q misses removed=[s2]", target)
	}
}

// The lost-update contract (invariant 50): two operators read generation 1,
// both submit — the first commits, the second is told 409-stale, and the first
// writer's edit survives.
func TestServiceGraphStaleGenerationLosesCleanly(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedGraph(t, st, ctx, 3)

	if _, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s0"], []string{f.svc["s1"]}, 0, GraphActor{Label: "op-a"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	// op-b read generation 0 before op-a committed:
	_, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s0"], []string{f.svc["s2"]}, 0, GraphActor{Label: "op-b"})
	if !errors.Is(err, ErrServiceGraphStale) {
		t.Fatalf("stale write error = %v, want ErrServiceGraphStale", err)
	}
	v, err := st.GetServiceDependencies(ctx, f.projectID, f.svc["s0"])
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(v.DependsOn) != 1 || v.DependsOn[0].Slug != "s1" {
		t.Errorf("first-committer's set must survive, got %+v", v.DependsOn)
	}
}

// Validation: self and foreign-project references, cycles direct and
// transitive, and the edge-count bound (invariants 48, 58).
func TestServiceGraphValidation(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedGraph(t, st, ctx, 3)

	// self
	if _, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s0"], []string{f.svc["s0"]}, 0, GraphActor{Label: "t"}); !errors.Is(err, ErrServiceGraphForeign) {
		t.Errorf("self dependency error = %v, want ErrServiceGraphForeign", err)
	}
	// foreign project
	org2, _ := st.CreateOrganization(ctx, "other", "Other")
	proj2, err := st.CreateProject(ctx, org2.ID, "other", "Other")
	if err != nil {
		t.Fatalf("project2: %v", err)
	}
	alien, err := st.CreateService(ctx, domain.Service{ProjectID: proj2.ID, Slug: "alien", Name: "alien"})
	if err != nil {
		t.Fatalf("alien: %v", err)
	}
	if _, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s0"], []string{alien.ID}, 0, GraphActor{Label: "t"}); !errors.Is(err, ErrServiceGraphForeign) {
		t.Errorf("foreign dependency error = %v, want ErrServiceGraphForeign", err)
	}
	// direct cycle
	if _, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s0"], []string{f.svc["s1"]}, 0, GraphActor{Label: "t"}); err != nil {
		t.Fatalf("s0→s1: %v", err)
	}
	if _, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s1"], []string{f.svc["s0"]}, 0, GraphActor{Label: "t"}); !errors.Is(err, ErrServiceGraphCycle) {
		t.Errorf("direct cycle error = %v, want ErrServiceGraphCycle", err)
	}
	// transitive cycle: s1→s2 then s2→s0 while s0→s1 exists
	if _, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s1"], []string{f.svc["s2"]}, 0, GraphActor{Label: "t"}); err != nil {
		t.Fatalf("s1→s2: %v", err)
	}
	if _, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s2"], []string{f.svc["s0"]}, 0, GraphActor{Label: "t"}); !errors.Is(err, ErrServiceGraphCycle) {
		t.Errorf("transitive cycle error = %v, want ErrServiceGraphCycle", err)
	}
	// the direct-edge bound
	big := seedGraphMany(t, st, ctx, f.projectID, domain.MaxServiceDependencies+1)
	if _, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s2"], big, 0, GraphActor{Label: "t"}); !errors.Is(err, ErrServiceGraphLimit) {
		t.Errorf("limit error = %v, want ErrServiceGraphLimit", err)
	}
}

func seedGraphMany(t *testing.T, st *Store, ctx context.Context, projectID string, n int) []string {
	t.Helper()
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("bulk%d", i)
		svc, err := st.CreateService(ctx, domain.Service{ProjectID: projectID, Slug: name, Name: name})
		if err != nil {
			t.Fatalf("bulk service: %v", err)
		}
		out = append(out, svc.ID)
	}
	return out
}

// The depth cap, pinned at the boundary (invariant 58): a 9-edge chain is
// valid, the 10th edge is valid, and the write that would make an 11-edge
// chain is the rejected one.
func TestServiceGraphDepthCapPinned(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedGraph(t, st, ctx, 12)

	link := func(child, parent string) error {
		_, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc[child], []string{f.svc[parent]}, 0, GraphActor{Label: "t"})
		return err
	}
	// chain s1→s0, s2→s1, … s9→s8: 9 edges, depth 9 — valid.
	for i := 1; i <= 9; i++ {
		if err := link(fmt.Sprintf("s%d", i), fmt.Sprintf("s%d", i-1)); err != nil {
			t.Fatalf("edge %d: %v", i, err)
		}
	}
	// the 10th edge (s10→s9): depth 10 — still valid.
	if err := link("s10", "s9"); err != nil {
		t.Fatalf("depth-10 edge rejected: %v", err)
	}
	// the 11th (s11→s10) would make an 11-edge chain — the rejected write.
	if err := link("s11", "s10"); !errors.Is(err, ErrServiceGraphDepth) {
		t.Fatalf("depth-11 write error = %v, want ErrServiceGraphDepth", err)
	}
	// and the middle matters too: hanging a parent chain above a service with
	// descendants counts the WHOLE chain through it. s0 currently roots a
	// 10-edge chain; giving s0 a parent exceeds the cap.
	if err := link("s0", "s11"); !errors.Is(err, ErrServiceGraphDepth) {
		t.Fatalf("through-chain depth error = %v, want ErrServiceGraphDepth", err)
	}
}

// Two concurrent writers whose edges are individually acyclic but jointly a
// cycle: the service_graph advisory lock serializes them and exactly one
// commits (invariant 58).
func TestServiceGraphConcurrentCycleWriters(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedGraph(t, st, ctx, 2)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s0"], []string{f.svc["s1"]}, 0, GraphActor{Label: "a"})
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s1"], []string{f.svc["s0"]}, 0, GraphActor{Label: "b"})
	}()
	wg.Wait()
	okCount, cycleCount := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			okCount++
		case errors.Is(err, ErrServiceGraphCycle):
			cycleCount++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if okCount != 1 || cycleCount != 1 {
		t.Fatalf("want exactly one commit and one cycle rejection, got ok=%d cycle=%d", okCount, cycleCount)
	}
	var edges int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_dependencies`).Scan(&edges); err != nil {
		t.Fatalf("count: %v", err)
	}
	if edges != 1 {
		t.Fatalf("edges = %d, want 1 — a jointly-cyclic pair must never both land", edges)
	}
}

// Invariant 49: an edge-only mutation creates no definition revision and no
// epoch and moves no canonical hash byte.
func TestEdgeWriteCreatesNoRevisionNoEpoch(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedDeclaration(t, st, ctx)
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "t"}); err != nil {
		t.Fatalf("declaration: %v", err)
	}
	other, err := st.CreateService(ctx, domain.Service{ProjectID: f.projectID, Slug: "upstream", Name: "upstream"})
	if err != nil {
		t.Fatalf("upstream: %v", err)
	}

	count := func() (revs, epochs int, hash string) {
		t.Helper()
		if err := st.pool.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM service_definition_revisions WHERE service_id=$1),
			       (SELECT count(*) FROM service_evaluation_epochs WHERE service_id=$1),
			       (SELECT COALESCE(max(snapshot_hash), '') FROM service_evaluation_epochs WHERE service_id=$1)`,
			f.serviceID).Scan(&revs, &epochs, &hash); err != nil {
			t.Fatalf("count: %v", err)
		}
		return revs, epochs, hash
	}
	r0, e0, h0 := count()
	if _, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.serviceID, []string{other.ID}, 0, GraphActor{Label: "t"}); err != nil {
		t.Fatalf("edge write: %v", err)
	}
	r1, e1, h1 := count()
	if r0 != r1 || e0 != e1 || h0 != h1 {
		t.Fatalf("edge-only write moved the declaration axes: revisions %d→%d epochs %d→%d hash %q→%q",
			r0, r1, e0, e1, h0, h1)
	}
}

// Invariant 48, at the schema: a cross-project edge is unrepresentable even for
// a direct SQL writer that bypasses every application check.
func TestServiceGraphTenancyBySchema(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedGraph(t, st, ctx, 1)
	org2, _ := st.CreateOrganization(ctx, "other", "Other")
	proj2, err := st.CreateProject(ctx, org2.ID, "other", "Other")
	if err != nil {
		t.Fatalf("project2: %v", err)
	}
	alien, err := st.CreateService(ctx, domain.Service{ProjectID: proj2.ID, Slug: "alien", Name: "alien"})
	if err != nil {
		t.Fatalf("alien: %v", err)
	}
	// One shared project_id under both composite FKs: whichever project we claim,
	// one of the two references cannot hold.
	for _, proj := range []string{f.projectID, proj2.ID} {
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO service_dependencies (service_id, depends_on_id, project_id) VALUES ($1,$2,$3)`,
			f.svc["s0"], alien.ID, proj); err == nil {
			t.Fatalf("direct cross-project edge accepted with project_id=%s", proj)
		}
	}
}

// The desired-edge delete guard (invariant 51): a target named by an APPLIED
// file-owned service's depends_on refuses deletion, naming the provider; the
// same edge UI-owned never pins.
func TestDeleteServicePinnedByFileEdge(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedGraph(t, st, ctx, 2)
	if _, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s0"], []string{f.svc["s1"]}, 0, GraphActor{Label: "file:shop.yaml"}); err != nil {
		t.Fatalf("edge: %v", err)
	}

	// UI-owned dependent: the target deletes fine (edges cascade).
	if err := st.DeleteService(ctx, f.projectID, f.svc["s1"]); err != nil {
		t.Fatalf("unpinned delete: %v", err)
	}
	var edges int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_dependencies`).Scan(&edges); err != nil {
		t.Fatalf("count: %v", err)
	}
	if edges != 0 {
		t.Fatalf("edges after cascade = %d, want 0", edges)
	}

	// Recreate the target and the edge; now mark the DEPENDENT file-owned.
	tgt, err := st.CreateService(ctx, domain.Service{ProjectID: f.projectID, Slug: "s1b", Name: "s1b"})
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if _, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s0"], []string{tgt.ID}, 1, GraphActor{Label: "file:shop.yaml"}); err != nil {
		t.Fatalf("edge2: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO managed_services (service_id, provider_id, org_id, project_id, source_uid)
		SELECT $1, 'file:shop.yaml', p.org_id, p.id, 's0' FROM projects p WHERE p.id = $2`,
		f.svc["s0"], f.projectID); err != nil {
		t.Fatalf("manage: %v", err)
	}
	err = st.DeleteService(ctx, f.projectID, tgt.ID)
	var pinned ErrServicePinnedByFile
	if !errors.As(err, &pinned) {
		t.Fatalf("pinned delete error = %v, want ErrServicePinnedByFile", err)
	}
	if pinned.Provider != "file:shop.yaml" || pinned.Service != "s0" {
		t.Errorf("pin names %+v, want provider file:shop.yaml / service s0", pinned)
	}

	// An ORPHANED file-owned dependent still pins ([276] P1-2): MaC keeps an
	// absent-from-bundle service as file-owned last-known-good, and its desired
	// edges are exactly what the pin keeps restorable.
	if _, err := st.pool.Exec(ctx,
		`UPDATE managed_services SET orphaned_at = now() WHERE service_id = $1`, f.svc["s0"]); err != nil {
		t.Fatalf("orphan: %v", err)
	}
	err = st.DeleteService(ctx, f.projectID, tgt.ID)
	if !errors.As(err, &pinned) {
		t.Fatalf("orphaned-dependent delete error = %v, want ErrServicePinnedByFile — an orphan is still file-owned desired state", err)
	}
	if pinned.Provider != "file:shop.yaml" || pinned.Service != "s0" {
		t.Errorf("orphan pin names %+v, want provider file:shop.yaml / service s0", pinned)
	}
}

// The response token always names the returned set ([276] P2-1): reads race a
// writer alternating between two edge sets whose parity is tied to the
// generation; a split-snapshot read would eventually pair a new set with an
// old token. Failure here is deterministic evidence of a torn read; the
// single-statement read can never produce one.
func TestGetServiceDependenciesTokenNamesTheSet(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedGraph(t, st, ctx, 3)

	// generation g odd → {s1}, even (≥2) → {s2}.
	sets := map[int64]string{}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		defer close(done)
		gen := int64(0)
		for i := 0; i < 40; i++ {
			parent := "s1"
			if (gen+1)%2 == 0 {
				parent = "s2"
			}
			if _, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s0"],
				[]string{f.svc[parent]}, gen, GraphActor{Label: "racer"}); err != nil {
				done <- err
				return
			}
			gen++
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	sets[1], sets[0] = "s1", "" // gen 0 = empty set
	for i := 0; i < 200; i++ {
		v, err := st.GetServiceDependencies(ctx, f.projectID, f.svc["s0"])
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		var got string
		if len(v.DependsOn) == 1 {
			got = v.DependsOn[0].Slug
		} else if len(v.DependsOn) > 1 {
			t.Fatalf("set of %d edges never written", len(v.DependsOn))
		}
		var want string
		switch {
		case v.GraphGeneration == 0:
			want = ""
		case v.GraphGeneration%2 == 1:
			want = "s1"
		default:
			want = "s2"
		}
		if got != want {
			t.Fatalf("token %d paired with set %q, want %q — a torn read across snapshots", v.GraphGeneration, got, want)
		}
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatalf("racer: %v", err)
	}
}

// The audit row carries TYPED actor attribution ([276] P1-4): a human write
// lands actor_user_id, a token write lands via_token, a provider write is a
// machine actor (NULL user) with its label in the target — never an actorless
// human edit.
func TestServiceGraphAuditActorAttribution(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedGraph(t, st, ctx, 4)

	var userID string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO users (email, display_name) VALUES ('seymur@teamlead.com', 'S M') RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}

	cases := []struct {
		actor     GraphActor
		wantUser  *string
		wantToken bool
		parent    string
	}{
		{GraphActor{UserID: userID, ViaToken: false, Label: "seymur@teamlead.com"}, &userID, false, "s1"},
		{GraphActor{UserID: userID, ViaToken: true, Label: "token:deploy-bot"}, &userID, true, "s2"},
		{GraphActor{Label: "file:shop.yaml"}, nil, false, "s3"},
	}
	for i, c := range cases {
		if _, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s0"],
			[]string{f.svc[c.parent]}, int64(i), c.actor); err != nil {
			t.Fatalf("case %d replace: %v", i, err)
		}
		var gotUser *string
		var gotToken bool
		var target string
		if err := st.pool.QueryRow(ctx, `
			SELECT actor_user_id, via_token, target FROM audit_logs
			 WHERE action = 'service.dependencies.replaced'
			 ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&gotUser, &gotToken, &target); err != nil {
			t.Fatalf("case %d audit: %v", i, err)
		}
		if (c.wantUser == nil) != (gotUser == nil) || (c.wantUser != nil && *gotUser != *c.wantUser) {
			t.Errorf("case %d actor_user_id = %v, want %v", i, gotUser, c.wantUser)
		}
		if gotToken != c.wantToken {
			t.Errorf("case %d via_token = %v, want %v", i, gotToken, c.wantToken)
		}
		if !strings.Contains(target, "actor="+c.actor.Label) {
			t.Errorf("case %d target %q misses the label", i, target)
		}
	}
}

// A file-owned service's edges are not UI-mutable ([288] P0): the UI wrapper
// refuses with ErrServiceManagedByFile — active AND orphaned, since an orphan
// is still the provider's last-known-good — while the internal mutator the
// provider track uses stays available.
func TestReplaceServiceDependenciesRefusesFileOwned(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedGraph(t, st, ctx, 2)
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO managed_services (service_id, provider_id, org_id, project_id, source_uid)
		SELECT $1, 'file:shop.yaml', p.org_id, p.id, 's0' FROM projects p WHERE p.id = $2`,
		f.svc["s0"], f.projectID); err != nil {
		t.Fatalf("manage: %v", err)
	}
	for _, phase := range []string{"active", "orphaned"} {
		if phase == "orphaned" {
			if _, err := st.pool.Exec(ctx,
				`UPDATE managed_services SET orphaned_at = now() WHERE service_id = $1`, f.svc["s0"]); err != nil {
				t.Fatalf("orphan: %v", err)
			}
		}
		_, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s0"],
			[]string{f.svc["s1"]}, 0, GraphActor{Label: "ui-editor"})
		if !errors.Is(err, ErrServiceManagedByFile) {
			t.Fatalf("%s file-owned UI write error = %v, want ErrServiceManagedByFile", phase, err)
		}
		var edges int
		if err := st.pool.QueryRow(ctx,
			`SELECT count(*) FROM service_dependencies WHERE service_id = $1`, f.svc["s0"]).Scan(&edges); err != nil {
			t.Fatalf("count: %v", err)
		}
		if edges != 0 {
			t.Fatalf("%s: a refused UI write still mutated the graph (%d edges)", phase, edges)
		}
	}
	// A UI-owned sibling is unaffected — the guard is per service, not per project.
	if _, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s1"],
		[]string{f.svc["s0"]}, 0, GraphActor{Label: "ui-editor"}); err != nil {
		t.Fatalf("UI-owned sibling write: %v", err)
	}
}

// Create-with-edges is ATOMIC ([288] P1-5): an invalid parent leaves no
// service, no edge, no generation residue and no audit row — proven against
// real PG, where the fake's shape cannot stand in for a transaction.
func TestCreateServiceWithDependenciesIsAtomic(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedGraph(t, st, ctx, 1)
	org2, _ := st.CreateOrganization(ctx, "other", "Other")
	proj2, err := st.CreateProject(ctx, org2.ID, "other", "Other")
	if err != nil {
		t.Fatalf("project2: %v", err)
	}
	alien, err := st.CreateService(ctx, domain.Service{ProjectID: proj2.ID, Slug: "alien", Name: "alien"})
	if err != nil {
		t.Fatalf("alien: %v", err)
	}

	before := func() (services, edges, audits int) {
		t.Helper()
		if err := st.pool.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM services WHERE project_id = $1),
			       (SELECT count(*) FROM service_dependencies WHERE project_id = $1),
			       (SELECT count(*) FROM audit_logs WHERE action = 'service.dependencies.replaced')`,
			f.projectID).Scan(&services, &edges, &audits); err != nil {
			t.Fatalf("counts: %v", err)
		}
		return services, edges, audits
	}
	s0, e0, a0 := before()

	// (a) a foreign parent must abort the whole thing.
	if _, err := st.CreateServiceWithDependencies(ctx,
		domain.Service{ProjectID: f.projectID, Slug: "checkout", Name: "checkout"},
		[]string{alien.ID}, GraphActor{Label: "t"}); !errors.Is(err, ErrServiceGraphForeign) {
		t.Fatalf("foreign-parent create error = %v, want ErrServiceGraphForeign", err)
	}
	if s1, e1, a1 := before(); s1 != s0 || e1 != e0 || a1 != a0 {
		t.Fatalf("failed create left residue: services %d→%d edges %d→%d audits %d→%d", s0, s1, e0, e1, a0, a1)
	}
	var orphan int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM services WHERE project_id = $1 AND slug = 'checkout'`, f.projectID).Scan(&orphan); err != nil {
		t.Fatalf("orphan check: %v", err)
	}
	if orphan != 0 {
		t.Fatal("the service row survived a failed edge validation — create-with-edges is not atomic")
	}

	// (b) the success path lands both, with the audit, in one go.
	svc, err := st.CreateServiceWithDependencies(ctx,
		domain.Service{ProjectID: f.projectID, Slug: "checkout", Name: "checkout"},
		[]string{f.svc["s0"]}, GraphActor{Label: "t"})
	if err != nil {
		t.Fatalf("create-with-edges: %v", err)
	}
	v, err := st.GetServiceDependencies(ctx, f.projectID, svc.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(v.DependsOn) != 1 || v.GraphGeneration != 1 {
		t.Fatalf("created state = %+v", v)
	}
	if s2, e2, a2 := before(); s2 != s0+1 || e2 != e0+1 || a2 != a0+1 {
		t.Fatalf("success counts = services %d edges %d audits %d", s2, e2, a2)
	}
}

// The domain cap lives in the SHARED mutator ([288] P2): a caller that skips
// the wrapper's normalization still cannot exceed it, and duplicates in the
// request collapse instead of double-inserting.
func TestSharedMutatorNormalizesAndCaps(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedGraph(t, st, ctx, 2)
	// Duplicates collapse: one edge, generation 1.
	v, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s0"],
		[]string{f.svc["s1"], f.svc["s1"], f.svc["s1"]}, 0, GraphActor{Label: "t"})
	if err != nil {
		t.Fatalf("duplicate parents: %v", err)
	}
	if len(v.DependsOn) != 1 || v.GraphGeneration != 1 {
		t.Fatalf("duplicates did not collapse: %+v", v)
	}
	// The cap is enforced inside the mutator, so create-with-edges hits it too.
	big := seedGraphMany(t, st, ctx, f.projectID, domain.MaxServiceDependencies+1)
	if _, err := st.CreateServiceWithDependencies(ctx,
		domain.Service{ProjectID: f.projectID, Slug: "over-cap", Name: "over-cap"},
		big, GraphActor{Label: "t"}); !errors.Is(err, ErrServiceGraphLimit) {
		t.Fatalf("create-with-edges over cap = %v, want ErrServiceGraphLimit", err)
	}
}

// The edge view carries bounded provenance ([298] P1-4): a file-owned NEIGHBOUR is named
// by its provider on both directions, a UI-owned one carries nothing. The downstream case
// is the load-bearing one — a file-owned dependent PINS this service, so a reader who
// cannot see it cannot predict the 409 on delete.
func TestServiceGraphEdgeCarriesOwnership(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedGraph(t, st, ctx, 3)
	// s0 depends on s1 (UI-owned); s2 depends on s0 and is FILE-owned.
	if _, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s0"],
		[]string{f.svc["s1"]}, 0, GraphActor{Label: "t"}); err != nil {
		t.Fatalf("upstream edge: %v", err)
	}
	if _, err := st.ReplaceServiceDependencies(ctx, f.projectID, f.svc["s2"],
		[]string{f.svc["s0"]}, 0, GraphActor{Label: "t"}); err != nil {
		t.Fatalf("downstream edge: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO managed_services (service_id, provider_id, org_id, project_id, source_uid)
		SELECT $1, 'file:shop.yaml', p.org_id, p.id, 's2' FROM projects p WHERE p.id = $2`,
		f.svc["s2"], f.projectID); err != nil {
		t.Fatalf("manage: %v", err)
	}

	v, err := st.GetServiceDependencies(ctx, f.projectID, f.svc["s0"])
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(v.DependsOn) != 1 || v.DependsOn[0].ManagedBy != "" {
		t.Errorf("UI-owned upstream carries ownership: %+v", v.DependsOn)
	}
	if len(v.DependedOnBy) != 1 || v.DependedOnBy[0].ManagedBy != "file:shop.yaml" {
		t.Fatalf("file-owned dependent = %+v, want managed_by file:shop.yaml", v.DependedOnBy)
	}
	// The pin the chip predicts is real: deleting s0 is refused, naming that provider.
	err = st.DeleteService(ctx, f.projectID, f.svc["s0"])
	var pinned ErrServicePinnedByFile
	if !errors.As(err, &pinned) || pinned.Provider != "file:shop.yaml" {
		t.Fatalf("delete of the pinned target = %v, want the 409 the chip predicts", err)
	}
}
