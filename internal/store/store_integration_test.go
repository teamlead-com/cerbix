package store_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/ingest"
	"github.com/teamlead-com/cerbix/internal/prober"
	"github.com/teamlead-com/cerbix/internal/scheduler"
	"github.com/teamlead-com/cerbix/internal/store"
	"github.com/teamlead-com/cerbix/internal/worker"
)

// These tests require a real Postgres. They are opt-in via CERBIX_TEST_DATABASE_DSN
// and skipped otherwise, so default `go test ./...` and CI stay hermetic.
func testStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run store integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.TruncateAll(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return st, ctx
}

func TestOrgProjectUserMembershipRoundTrip(t *testing.T) {
	st, ctx := testStore(t)

	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	got, err := st.GetOrganization(ctx, org.ID)
	if err != nil || got.Slug != "acme" {
		t.Fatalf("get org: %v %+v", err, got)
	}

	proj, err := st.CreateProject(ctx, org.ID, "api", "API")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.GetProject(ctx, proj.ID); err != nil {
		t.Fatalf("get project: %v", err)
	}

	// JIT upsert is idempotent by oidc_sub and refreshes mutable fields.
	u1, err := st.UpsertUserByOIDCSub(ctx, "sub-1", "a@x.com", "A")
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	u2, err := st.UpsertUserByOIDCSub(ctx, "sub-1", "a2@x.com", "A2")
	if err != nil {
		t.Fatalf("re-upsert user: %v", err)
	}
	if u1.ID != u2.ID {
		t.Fatalf("upsert changed id: %s != %s", u1.ID, u2.ID)
	}
	if u2.Email != "a2@x.com" || u2.DisplayName != "A2" {
		t.Fatalf("upsert did not refresh fields: %+v", u2)
	}

	m, err := st.CreateMembership(ctx, domain.Membership{
		UserID: u1.ID, OrgID: org.ID, Role: domain.RoleOrgAdmin,
	})
	if err != nil {
		t.Fatalf("create membership: %v", err)
	}
	if m.Scope() != domain.ScopeOrg {
		t.Fatalf("expected org scope, got %s", m.Scope())
	}
}

func TestGetNotFound(t *testing.T) {
	st, ctx := testStore(t)
	if _, err := st.GetOrganization(ctx, "00000000-0000-0000-0000-000000000000"); err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestTenantIsolation(t *testing.T) {
	st, ctx := testStore(t)

	acme, _ := st.CreateOrganization(ctx, "acme", "Acme")
	globex, _ := st.CreateOrganization(ctx, "globex", "Globex")
	lApi, _ := st.CreateProject(ctx, acme.ID, "api", "API")
	lAuth, _ := st.CreateProject(ctx, acme.ID, "orders", "Orders")
	_, _ = st.CreateProject(ctx, globex.ID, "file-service", "File Service")

	// alice: org-level member of Acme → sees Acme + all its projects only.
	alice, _ := st.UpsertUserByOIDCSub(ctx, "alice", "alice@x", "Alice")
	if _, err := st.CreateMembership(ctx, domain.Membership{
		UserID: alice.ID, OrgID: acme.ID, Role: domain.RoleViewer,
	}); err != nil {
		t.Fatalf("alice membership: %v", err)
	}

	// bob: project-level member of Acme/api only → sees just that project.
	bob, _ := st.UpsertUserByOIDCSub(ctx, "bob", "bob@x", "Bob")
	if _, err := st.CreateMembership(ctx, domain.Membership{
		UserID: bob.ID, OrgID: acme.ID, ProjectID: lApi.ID, Role: domain.RoleProjectAdmin,
	}); err != nil {
		t.Fatalf("bob membership: %v", err)
	}

	aliceOrgs, _ := st.ListOrganizationsForUser(ctx, alice.ID)
	if len(aliceOrgs) != 1 || aliceOrgs[0].ID != acme.ID {
		t.Fatalf("alice orgs = %+v, want only Acme", aliceOrgs)
	}
	aliceProjects, _ := st.ListProjectsForUser(ctx, alice.ID)
	if len(aliceProjects) != 2 {
		t.Fatalf("alice should see 2 Acme projects, got %d", len(aliceProjects))
	}

	bobProjects, _ := st.ListProjectsForUser(ctx, bob.ID)
	if len(bobProjects) != 1 || bobProjects[0].ID != lApi.ID {
		t.Fatalf("bob projects = %+v, want only Acme/api", bobProjects)
	}
	bobOrgs, _ := st.ListOrganizationsForUser(ctx, bob.ID)
	if len(bobOrgs) != 1 || bobOrgs[0].ID != acme.ID {
		t.Fatalf("bob orgs = %+v, want Acme", bobOrgs)
	}

	_ = lAuth // referenced for clarity above
}

func TestListAndAdminHelpers(t *testing.T) {
	st, ctx := testStore(t)

	acme, _ := st.CreateOrganization(ctx, "acme", "Acme")
	globex, _ := st.CreateOrganization(ctx, "globex", "Globex")
	_, _ = st.CreateProject(ctx, acme.ID, "api", "API")
	_, _ = st.CreateProject(ctx, acme.ID, "webhook", "Webhook")

	// ListOrganizations returns all (global-admin view).
	orgs, err := st.ListOrganizations(ctx)
	if err != nil || len(orgs) != 2 {
		t.Fatalf("list orgs = %+v (err %v), want 2", orgs, err)
	}

	// ListProjectsByOrg is org-scoped.
	limProjects, err := st.ListProjectsByOrg(ctx, acme.ID)
	if err != nil || len(limProjects) != 2 {
		t.Fatalf("acme projects = %+v (err %v), want 2", limProjects, err)
	}
	if p, _ := st.ListProjectsByOrg(ctx, globex.ID); len(p) != 0 {
		t.Fatalf("globex projects = %+v, want 0", p)
	}

	// User lookups + global-admin toggle.
	u, _ := st.UpsertUserByOIDCSub(ctx, "carol", "carol@x", "Carol")
	bySub, err := st.GetUserByOIDCSub(ctx, "carol")
	if err != nil || bySub.ID != u.ID {
		t.Fatalf("get by sub = %+v (err %v)", bySub, err)
	}
	if _, err := st.GetUserByOIDCSub(ctx, "nobody"); err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound for missing sub, got %v", err)
	}
	if err := st.SetGlobalAdmin(ctx, u.ID, true); err != nil {
		t.Fatalf("set global admin: %v", err)
	}
	if got, _ := st.GetUser(ctx, u.ID); !got.IsGlobalAdmin {
		t.Fatal("global admin flag not persisted")
	}
	if err := st.SetGlobalAdmin(ctx, "00000000-0000-0000-0000-000000000000", true); err != store.ErrNotFound {
		t.Fatalf("set global admin on missing user = %v, want store.ErrNotFound", err)
	}

	// Memberships listing (org- and project-level).
	proj := limProjects[0]
	if _, err := st.CreateMembership(ctx, domain.Membership{UserID: u.ID, OrgID: acme.ID, Role: domain.RoleEditor}); err != nil {
		t.Fatalf("org membership: %v", err)
	}
	if _, err := st.CreateMembership(ctx, domain.Membership{UserID: u.ID, OrgID: acme.ID, ProjectID: proj.ID, Role: domain.RoleViewer}); err != nil {
		t.Fatalf("project membership: %v", err)
	}
	ms, err := st.ListMembershipsForUser(ctx, u.ID)
	if err != nil || len(ms) != 2 {
		t.Fatalf("memberships = %+v (err %v), want 2", ms, err)
	}

	if _, err := st.GetProject(ctx, "00000000-0000-0000-0000-000000000000"); err != store.ErrNotFound {
		t.Fatalf("get missing project = %v, want store.ErrNotFound", err)
	}
}

func TestCreateMembershipRejectsInvalidDomain(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	u, _ := st.UpsertUserByOIDCSub(ctx, "dave", "dave@x", "Dave")
	// project_admin at org scope is rejected by domain.Validate before any SQL.
	if _, err := st.CreateMembership(ctx, domain.Membership{UserID: u.ID, OrgID: org.ID, Role: domain.RoleProjectAdmin}); err == nil {
		t.Fatal("expected domain validation error for project_admin at org scope")
	}
}

func TestSessionLifecycle(t *testing.T) {
	st, ctx := testStore(t)
	u, _ := st.UpsertUserByOIDCSub(ctx, "sess-user", "s@x", "S")

	sess, err := st.CreateSession(ctx, u.ID, "raw-token-1", time.Now().Add(time.Hour))
	if err != nil || sess.UserID != u.ID {
		t.Fatalf("create session: %v %+v", err, sess)
	}
	got, err := st.SessionByToken(ctx, "raw-token-1")
	if err != nil || got.ID != sess.ID {
		t.Fatalf("session by token: %v %+v", err, got)
	}
	// Wrong token → not found.
	if _, err := st.SessionByToken(ctx, "nope"); err != store.ErrNotFound {
		t.Fatalf("bad token = %v, want ErrNotFound", err)
	}
	// Expired session is not returned.
	if _, err := st.CreateSession(ctx, u.ID, "expired", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("create expired: %v", err)
	}
	if _, err := st.SessionByToken(ctx, "expired"); err != store.ErrNotFound {
		t.Fatalf("expired token = %v, want ErrNotFound", err)
	}
	n, err := st.DeleteExpiredSessions(ctx)
	if err != nil || n < 1 {
		t.Fatalf("delete expired = %d (err %v)", n, err)
	}
	// Delete live session.
	if err := st.DeleteSession(ctx, "raw-token-1"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := st.SessionByToken(ctx, "raw-token-1"); err != store.ErrNotFound {
		t.Fatal("session should be gone")
	}
}

func TestDeleteSessionsByUser(t *testing.T) {
	st, ctx := testStore(t)
	u, _ := st.UpsertUserByOIDCSub(ctx, "multi-sess", "m@x", "M")
	other, _ := st.UpsertUserByOIDCSub(ctx, "other-user", "o@x", "O")
	mk := func(uid, tok string) {
		if _, err := st.CreateSession(ctx, uid, tok, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("create session %s: %v", tok, err)
		}
	}
	mk(u.ID, "cur")   // the caller's current session
	mk(u.ID, "phone") // another device
	mk(u.ID, "old")   // a stale/stolen session
	mk(other.ID, "someone-else")

	// Password change: keep the current session, drop the user's others.
	n, err := st.DeleteSessionsByUser(ctx, u.ID, "cur")
	if err != nil || n != 2 {
		t.Fatalf("delete-except-current = %d (err %v), want 2", n, err)
	}
	if _, err := st.SessionByToken(ctx, "cur"); err != nil {
		t.Fatalf("current session must survive: %v", err)
	}
	for _, tok := range []string{"phone", "old"} {
		if _, err := st.SessionByToken(ctx, tok); err != store.ErrNotFound {
			t.Fatalf("session %q should be gone, got %v", tok, err)
		}
	}
	// Another user's session is untouched.
	if _, err := st.SessionByToken(ctx, "someone-else"); err != nil {
		t.Fatalf("other user's session must be untouched: %v", err)
	}
	// Blank exception drops all remaining sessions for the user.
	if n, err := st.DeleteSessionsByUser(ctx, u.ID, ""); err != nil || n != 1 {
		t.Fatalf("delete-all = %d (err %v), want 1 (the current one)", n, err)
	}
	if _, err := st.SessionByToken(ctx, "cur"); err != store.ErrNotFound {
		t.Fatal("delete-all should have removed the current session too")
	}
}

func TestAuthFlowConsumedOnce(t *testing.T) {
	st, ctx := testStore(t)
	flow := store.AuthFlow{State: "st-1", Nonce: "n", PKCEVerifier: "v", RedirectTo: "/x", ExpiresAt: time.Now().Add(time.Minute)}
	if err := st.CreateAuthFlow(ctx, flow); err != nil {
		t.Fatalf("create flow: %v", err)
	}
	got, err := st.TakeAuthFlow(ctx, "st-1")
	if err != nil || got.Nonce != "n" || got.RedirectTo != "/x" {
		t.Fatalf("take flow: %v %+v", err, got)
	}
	// Second take fails: single-use.
	if _, err := st.TakeAuthFlow(ctx, "st-1"); err != store.ErrNotFound {
		t.Fatalf("second take = %v, want ErrNotFound", err)
	}
	// Expired flow is not returned.
	_ = st.CreateAuthFlow(ctx, store.AuthFlow{State: "st-2", Nonce: "n", PKCEVerifier: "v", ExpiresAt: time.Now().Add(-time.Minute)})
	if _, err := st.TakeAuthFlow(ctx, "st-2"); err != store.ErrNotFound {
		t.Fatalf("expired take = %v, want ErrNotFound", err)
	}
	if _, err := st.DeleteExpiredAuthFlows(ctx); err != nil {
		t.Fatalf("delete expired flows: %v", err)
	}
}

func TestListMembershipsByOrg(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	ua, _ := st.UpsertUserByOIDCSub(ctx, "a", "a@x", "A")
	ub, _ := st.UpsertUserByOIDCSub(ctx, "b", "b@x", "B")
	_, _ = st.CreateMembership(ctx, domain.Membership{UserID: ua.ID, OrgID: org.ID, Role: domain.RoleOrgAdmin})
	_, _ = st.CreateMembership(ctx, domain.Membership{UserID: ub.ID, OrgID: org.ID, ProjectID: proj.ID, Role: domain.RoleViewer})

	members, err := st.ListMembershipsByOrg(ctx, org.ID)
	if err != nil || len(members) != 2 {
		t.Fatalf("members = %+v (err %v), want 2", members, err)
	}
}

func TestMonitorCRUDAndHeartbeats(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "health", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 30, TimeoutSeconds: 5, Conditions: []string{"[STATUS] == 200"}, Enabled: true, AutoIncident: true,
		Tags: []string{"env:prod", "team:api"}, Region: "geo1",
	})
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	if mon.Status != domain.StatusPending {
		t.Fatalf("new monitor status = %q, want pending", mon.Status)
	}
	if !mon.AutoIncident {
		t.Fatal("created monitor should keep AutoIncident=true")
	}
	if len(mon.Tags) != 2 || mon.Tags[0] != "env:prod" {
		t.Fatalf("tags should round-trip: %#v", mon.Tags)
	}
	if mon.Region != "geo1" {
		t.Fatalf("region should round-trip: %q", mon.Region)
	}
	// ListRegions includes core (always) plus any region in use.
	regions, err := st.ListRegions(ctx)
	if err != nil {
		t.Fatalf("list regions: %v", err)
	}
	if len(regions) != 2 || regions[0] != "core" || regions[1] != "geo1" {
		t.Fatalf("regions = %#v, want [core geo1]", regions)
	}
	got, err := st.GetMonitor(ctx, mon.ID)
	if err != nil || len(got.Conditions) != 1 || got.Conditions[0] != "[STATUS] == 200" {
		t.Fatalf("get monitor: %v %+v", err, got)
	}
	if !got.AutoIncident {
		t.Fatalf("AutoIncident should round-trip true: %+v", got)
	}
	// Toggle auto-incident off via UpdateMonitor.
	mon.AutoIncident = false
	if upd, err := st.UpdateMonitor(ctx, mon); err != nil || upd.AutoIncident {
		t.Fatalf("update AutoIncident=false: err=%v got=%+v", err, upd)
	}
	if reread, _ := st.GetMonitor(ctx, mon.ID); reread.AutoIncident {
		t.Fatalf("AutoIncident should persist false: %+v", reread)
	}
	if list, _ := st.ListMonitorsByProject(ctx, proj.ID); len(list) != 1 {
		t.Fatalf("list monitors = %d, want 1", len(list))
	}
	if enabled, _ := st.ListEnabledMonitors(ctx); len(enabled) != 1 {
		t.Fatalf("enabled monitors = %d, want 1", len(enabled))
	}

	// Heartbeats.
	if _, err := st.LatestHeartbeat(ctx, mon.ID); err != store.ErrNotFound {
		t.Fatalf("no heartbeat yet should be ErrNotFound, got %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := st.InsertHeartbeat(ctx, domain.Heartbeat{MonitorID: mon.ID, Up: i%2 == 0, LatencyMS: int64(i), Code: 200}); err != nil {
			t.Fatalf("insert heartbeat: %v", err)
		}
	}
	recent, err := st.ListRecentHeartbeats(ctx, mon.ID, 10)
	if err != nil || len(recent) != 3 {
		t.Fatalf("recent heartbeats = %d (err %v), want 3", len(recent), err)
	}
	if _, err := st.LatestHeartbeat(ctx, mon.ID); err != nil {
		t.Fatalf("latest heartbeat: %v", err)
	}

	if _, err := st.SetMonitorStatus(ctx, mon.ID, domain.StatusUp); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if got, _ := st.GetMonitor(ctx, mon.ID); got.Status != domain.StatusUp {
		t.Fatalf("status not persisted: %q", got.Status)
	}
	if err := st.DeleteMonitor(ctx, mon.ID); err != nil {
		t.Fatalf("delete monitor: %v", err)
	}
	if _, err := st.GetMonitor(ctx, mon.ID); err != store.ErrNotFound {
		t.Fatal("monitor should be gone")
	}
}

func TestLeadershipMutualExclusion(t *testing.T) {
	st, ctx := testStore(t)
	const key int64 = 0x123456

	release1, check1, ok1, err := st.TryBecomeLeader(ctx, key)
	if err != nil || !ok1 {
		t.Fatalf("first acquire: ok=%v err=%v", ok1, err)
	}
	// The leader's liveness check reports the lock is still held.
	if held, err := check1(ctx); err != nil || !held {
		t.Fatalf("check while held: held=%v err=%v, want true", held, err)
	}
	// A second attempt on the same key must fail while held.
	release2, _, ok2, err := st.TryBecomeLeader(ctx, key)
	if err != nil {
		t.Fatalf("second acquire err: %v", err)
	}
	if ok2 {
		release2()
		t.Fatal("second acquire should fail while lock held")
	}
	release1()
	// After release, it can be acquired again.
	release3, _, ok3, err := st.TryBecomeLeader(ctx, key)
	if err != nil || !ok3 {
		t.Fatalf("re-acquire after release: ok=%v err=%v", ok3, err)
	}
	release3()
}

func TestCheckingPipelineEndToEnd(t *testing.T) {
	st, _ := testStore(t)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("OK"))
	}))
	defer target.Close()

	setup, cancelSetup := context.WithTimeout(context.Background(), 10*time.Second)
	org, _ := st.CreateOrganization(setup, "acme", "Acme")
	proj, _ := st.CreateProject(setup, org.ID, "api", "API")
	mon, err := st.CreateMonitor(setup, domain.Monitor{
		ProjectID: proj.ID, Name: "health", Type: domain.MonitorHTTP, Target: target.URL,
		IntervalSeconds: 1, TimeoutSeconds: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	cancelSetup()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	disp := dispatch.NewInProc(16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.New(st, disp, logger).Run(ctx)
	go worker.New(disp, prober.NewRunner(), 2, logger).Run(ctx)
	go ingest.New(st, disp, nil, logger).Run(ctx)

	// Poll until a heartbeat is recorded and the monitor is marked up.
	deadline := time.Now().Add(6 * time.Second)
	for {
		poll, cancelPoll := context.WithTimeout(context.Background(), 2*time.Second)
		hb, err := st.LatestHeartbeat(poll, mon.ID)
		got, _ := st.GetMonitor(poll, mon.ID)
		cancelPoll()
		if err == nil && hb.Up && got.Status == domain.StatusUp {
			return // success
		}
		if time.Now().After(deadline) {
			t.Fatalf("pipeline did not produce an up heartbeat in time (hb=%+v err=%v status=%q)", hb, err, got.Status)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestLocalUsersCoexistWithOIDC(t *testing.T) {
	st, ctx := testStore(t)

	// OIDC user (oidc_sub set, no password).
	oidcUser, err := st.UpsertUserByOIDCSub(ctx, "kc-1", "oidc@x", "OIDC")
	if err != nil {
		t.Fatalf("oidc user: %v", err)
	}
	if _, err := st.PasswordHashByID(ctx, oidcUser.ID); err != store.ErrNotFound {
		t.Fatalf("oidc user should have no password: %v", err)
	}

	// Local user (password, no oidc_sub).
	local, err := st.CreateLocalUser(ctx, "local@x", "Local", "hash-abc", true)
	if err != nil {
		t.Fatalf("local user: %v", err)
	}
	if local.OIDCSub != "" {
		t.Fatalf("local user should have empty oidc_sub, got %q", local.OIDCSub)
	}
	got, err := st.GetUser(ctx, local.ID)
	if err != nil || !got.IsGlobalAdmin {
		t.Fatalf("get local user: %v %+v", err, got)
	}

	// Credential lookup + password change.
	cred, err := st.LocalCredentialByEmail(ctx, "local@x")
	if err != nil || cred.UserID != local.ID || cred.PasswordHash != "hash-abc" {
		t.Fatalf("local credential: %v %+v", err, cred)
	}
	if _, err := st.LocalCredentialByEmail(ctx, "oidc@x"); err != store.ErrNotFound {
		t.Fatalf("oidc email should not resolve as local credential: %v", err)
	}
	if err := st.SetPassword(ctx, local.ID, "hash-xyz"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	if h, _ := st.PasswordHashByID(ctx, local.ID); h != "hash-xyz" {
		t.Fatalf("password not updated: %q", h)
	}

	if n, _ := st.CountUsers(ctx); n != 2 {
		t.Fatalf("count users = %d, want 2", n)
	}
}

func TestSLAAndMaintenanceExclusion(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, _ := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "health", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true,
	})

	// 4 heartbeats (~now): 3 up, 1 down.
	for _, up := range []bool{true, true, false, true} {
		if err := st.InsertHeartbeat(ctx, domain.Heartbeat{MonitorID: mon.ID, Up: up, LatencyMS: 10}); err != nil {
			t.Fatalf("insert heartbeat: %v", err)
		}
	}

	since := time.Now().Add(-24 * time.Hour)
	c, err := st.MonitorSLI(ctx, mon.ID, since)
	if err != nil || c.Total != 4 || c.Up != 3 {
		t.Fatalf("monitor sli = %+v (err %v), want total=4 up=3", c, err)
	}

	// A maintenance window covering "now" excludes all four heartbeats.
	now := time.Now()
	if _, err := st.CreateMaintenanceWindow(ctx, domain.MaintenanceWindow{
		ProjectID: proj.ID, MonitorID: mon.ID,
		StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), Reason: "upgrade",
	}); err != nil {
		t.Fatalf("create maintenance: %v", err)
	}
	c, err = st.MonitorSLI(ctx, mon.ID, since)
	if err != nil || c.Total != 0 {
		t.Fatalf("with maintenance sli = %+v (err %v), want total=0 (all excluded)", c, err)
	}

	// Project-level SLI sees the same exclusion.
	pc, err := st.ProjectSLI(ctx, proj.ID, since)
	if err != nil || pc.Total != 0 {
		t.Fatalf("project sli = %+v (err %v), want total=0", pc, err)
	}
}

func TestSLATargetUpsert(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, _ := st.CreateMonitor(ctx, domain.Monitor{ProjectID: proj.ID, Name: "h", Type: domain.MonitorHTTP, Target: "https://x", IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true})

	if _, err := st.GetMonitorSLATarget(ctx, mon.ID, "30d"); err != store.ErrNotFound {
		t.Fatalf("no target yet → ErrNotFound, got %v", err)
	}
	t1, err := st.UpsertMonitorSLATarget(ctx, mon.ID, "30d", 99.9, false, nil)
	if err != nil || t1.Objective != 99.9 {
		t.Fatalf("upsert: %v %+v", err, t1)
	}
	// Upsert again updates objective + burn flag in place (unique monitor+window).
	t2, err := st.UpsertMonitorSLATarget(ctx, mon.ID, "30d", 99.95, true, nil)
	if err != nil || t2.ID != t1.ID || t2.Objective != 99.95 || !t2.BurnAlertEnabled {
		t.Fatalf("re-upsert: %v %+v", err, t2)
	}
	got, err := st.GetMonitorSLATarget(ctx, mon.ID, "30d")
	if err != nil || got.Objective != 99.95 {
		t.Fatalf("get target: %v %+v", err, got)
	}
}

func TestMaintenanceWindowCRUD(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	now := time.Now()
	mw, err := st.CreateMaintenanceWindow(ctx, domain.MaintenanceWindow{
		ProjectID: proj.ID, StartsAt: now, EndsAt: now.Add(time.Hour), Reason: "x",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	list, err := st.ListMaintenanceWindowsByProject(ctx, proj.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v (err %v), want 1", list, err)
	}
	if _, err := st.GetMaintenanceWindow(ctx, mw.ID); err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := st.DeleteMaintenanceWindow(ctx, mw.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetMaintenanceWindow(ctx, mw.ID); err != store.ErrNotFound {
		t.Fatal("should be gone")
	}
}

func TestIncidentLifecyclePersistence(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	inc, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: proj.ID, Title: "api down", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "we are investigating", "author1")
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}
	if inc.ResolvedAt != nil {
		t.Fatal("new incident should not be resolved")
	}

	// The opening update is created in the same transaction.
	ups, err := st.ListIncidentUpdates(ctx, inc.ID)
	if err != nil || len(ups) != 1 {
		t.Fatalf("opening update: %+v (err %v), want 1", ups, err)
	}

	// Advancing status is reflected on the incident; resolving stamps resolved_at.
	if _, err := st.AddIncidentUpdate(ctx, domain.IncidentUpdate{
		IncidentID: inc.ID, Status: domain.IncidentIdentified, Body: "found it", Author: "author1",
	}); err != nil {
		t.Fatalf("add update: %v", err)
	}
	if _, err := st.AddIncidentUpdate(ctx, domain.IncidentUpdate{
		IncidentID: inc.ID, Status: domain.IncidentResolved, Body: "fixed", Author: "author1",
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatalf("get incident: %v", err)
	}
	if got.Status != domain.IncidentResolved {
		t.Fatalf("status = %q, want resolved", got.Status)
	}
	if got.ResolvedAt == nil {
		t.Fatal("resolved_at should be stamped once resolved")
	}
	if ups, _ := st.ListIncidentUpdates(ctx, inc.ID); len(ups) != 3 {
		t.Fatalf("timeline = %d entries, want 3", len(ups))
	}

	// Postmortem upsert then read back.
	if _, err := st.UpsertPostmortem(ctx, inc.ID, "root cause", "author1"); err != nil {
		t.Fatalf("upsert postmortem: %v", err)
	}
	pm, err := st.UpsertPostmortem(ctx, inc.ID, "root cause v2", "author2")
	if err != nil {
		t.Fatalf("re-upsert postmortem: %v", err)
	}
	if pm.Body != "root cause v2" || pm.Author != "author2" {
		t.Fatalf("postmortem not replaced: %+v", pm)
	}
	got2, err := st.GetPostmortem(ctx, inc.ID)
	if err != nil || got2.Body != "root cause v2" {
		t.Fatalf("get postmortem = %+v (err %v)", got2, err)
	}

	// Incidents are project-isolated.
	if list, _ := st.ListIncidentsByProject(ctx, proj.ID); len(list) != 1 {
		t.Fatalf("list incidents = %d, want 1", len(list))
	}
	if _, err := st.GetPostmortem(ctx, "00000000-0000-0000-0000-000000000000"); err != store.ErrNotFound {
		t.Fatal("missing postmortem should be ErrNotFound")
	}
}

func TestStatusPageAndComponentPersistence(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, _ := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "api-health", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 10, Enabled: true,
	})

	sp, err := st.CreateStatusPage(ctx, domain.StatusPage{
		OrgID: org.ID, Slug: "acme-status", Title: "Acme", Visibility: domain.VisibilityPublic,
	})
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}
	// Slug is unique.
	if _, err := st.CreateStatusPage(ctx, domain.StatusPage{
		OrgID: org.ID, Slug: "acme-status", Title: "Dup", Visibility: domain.VisibilityPublic,
	}); err == nil {
		t.Fatal("duplicate slug should be rejected")
	}

	got, err := st.GetStatusPageBySlug(ctx, "acme-status")
	if err != nil || got.ID != sp.ID {
		t.Fatalf("get by slug = %+v (err %v)", got, err)
	}

	comp, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: sp.ID, Name: "API", MonitorID: mon.ID, Position: 1,
	})
	if err != nil {
		t.Fatalf("create component: %v", err)
	}
	manual, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: sp.ID, Name: "Docs", ManualStatus: domain.CompOperational, Position: 2,
	})
	if err != nil {
		t.Fatalf("create manual component: %v", err)
	}
	comps, err := st.ListComponentsByPage(ctx, sp.ID)
	if err != nil || len(comps) != 2 || comps[0].ID != comp.ID {
		t.Fatalf("list components = %+v (err %v), want 2 ordered", comps, err)
	}

	if err := st.DeleteComponent(ctx, manual.ID); err != nil {
		t.Fatalf("delete component: %v", err)
	}
	if _, err := st.GetComponent(ctx, manual.ID); err != store.ErrNotFound {
		t.Fatal("deleted component should be gone")
	}

	// Open-incident query excludes resolved ones.
	openInc, _ := st.CreateIncident(ctx, domain.Incident{
		ProjectID: proj.ID, Title: "down", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opening", "a")
	_, _ = st.CreateIncident(ctx, domain.Incident{
		ProjectID: proj.ID, Title: "old", Status: domain.IncidentResolved,
		Impact: domain.ImpactMinor, Source: domain.SourceManual,
	}, "opening", "a")
	open, err := st.ListOpenIncidentsByProject(ctx, proj.ID)
	if err != nil || len(open) != 1 || open[0].ID != openInc.ID {
		t.Fatalf("open incidents = %+v (err %v), want 1 unresolved", open, err)
	}
}

func TestStatusPageUpdateAndDeleteCascade(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")

	sp, err := st.CreateStatusPage(ctx, domain.StatusPage{
		OrgID: org.ID, Slug: "s", Title: "S", Visibility: domain.VisibilityInternal,
	})
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}
	comp, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: sp.ID, Name: "API", ManualStatus: domain.CompOperational,
	})
	if err != nil {
		t.Fatalf("create component: %v", err)
	}

	// Update title + visibility (unlisted token set by the handler/caller).
	sp.Title = "Renamed"
	sp.Visibility = domain.VisibilityUnlisted
	sp.UnlistedToken = "tok123"
	updated, err := st.UpdateStatusPage(ctx, sp)
	if err != nil {
		t.Fatalf("update status page: %v", err)
	}
	if updated.Title != "Renamed" || updated.Visibility != domain.VisibilityUnlisted || updated.UnlistedToken != "tok123" {
		t.Fatalf("update = %+v, want renamed unlisted with token", updated)
	}

	// Delete the page → its components cascade away (FK ON DELETE CASCADE).
	if err := st.DeleteStatusPage(ctx, sp.ID); err != nil {
		t.Fatalf("delete status page: %v", err)
	}
	if _, err := st.GetStatusPage(ctx, sp.ID); err != store.ErrNotFound {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
	if _, err := st.GetComponent(ctx, comp.ID); err != store.ErrNotFound {
		t.Fatalf("component after page delete = %v, want ErrNotFound (cascade)", err)
	}
	// Deleting a missing page is ErrNotFound.
	if err := st.DeleteStatusPage(ctx, sp.ID); err != store.ErrNotFound {
		t.Fatalf("delete missing = %v, want ErrNotFound", err)
	}
}

func TestMonitorStatusTransitionAndAutoIncident(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, _ := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "api-health", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 10, Enabled: true,
	})

	// SetMonitorStatus returns the previous status (transition detection).
	prev, err := st.SetMonitorStatus(ctx, mon.ID, domain.StatusDown)
	if err != nil || prev != domain.StatusPending {
		t.Fatalf("first transition prev = %q (err %v), want pending", prev, err)
	}
	prev, _ = st.SetMonitorStatus(ctx, mon.ID, domain.StatusUp)
	if prev != domain.StatusDown {
		t.Fatalf("second transition prev = %q, want down", prev)
	}
	if _, err := st.SetMonitorStatus(ctx, "00000000-0000-0000-0000-000000000000", domain.StatusUp); err != store.ErrNotFound {
		t.Fatal("status on missing monitor should be ErrNotFound")
	}

	// An auto-incident links to the monitor and is found while open.
	inc, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: proj.ID, MonitorID: mon.ID, Title: "api-health is down",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor, Source: domain.SourceAuto,
	}, "auto-opened", "auto")
	if err != nil || inc.MonitorID != mon.ID {
		t.Fatalf("create auto incident: %+v (err %v)", inc, err)
	}
	found, err := st.FindOpenAutoIncidentByMonitor(ctx, mon.ID)
	if err != nil || found.ID != inc.ID {
		t.Fatalf("find open auto = %+v (err %v)", found, err)
	}

	// Resolving it clears the open lookup.
	if _, err := st.AddIncidentUpdate(ctx, domain.IncidentUpdate{
		IncidentID: inc.ID, Status: domain.IncidentResolved, Author: "auto",
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := st.FindOpenAutoIncidentByMonitor(ctx, mon.ID); err != store.ErrNotFound {
		t.Fatal("no open auto-incident should remain after resolve")
	}
}

func TestWebhookScopeSelection(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	p1, _ := st.CreateProject(ctx, org.ID, "api", "API")
	p2, _ := st.CreateProject(ctx, org.ID, "web", "Web")

	// Org-wide (enabled), p1-scoped (enabled), p2-scoped (enabled), and a disabled org-wide.
	orgWide, _ := st.CreateWebhook(ctx, domain.Webhook{OrgID: org.ID, URL: "https://a", Enabled: true})
	p1Hook, _ := st.CreateWebhook(ctx, domain.Webhook{OrgID: org.ID, ProjectID: p1.ID, URL: "https://b", Enabled: true})
	_, _ = st.CreateWebhook(ctx, domain.Webhook{OrgID: org.ID, ProjectID: p2.ID, URL: "https://c", Enabled: true})
	_, _ = st.CreateWebhook(ctx, domain.Webhook{OrgID: org.ID, URL: "https://d", Enabled: false})

	hooks, err := st.ListEnabledWebhooksForProject(ctx, p1.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := map[string]bool{}
	for _, h := range hooks {
		ids[h.ID] = true
	}
	// p1 sees the org-wide + its own hook, but not p2's and not the disabled one.
	if len(hooks) != 2 || !ids[orgWide.ID] || !ids[p1Hook.ID] {
		t.Fatalf("p1 applicable webhooks = %+v, want org-wide + p1", hooks)
	}

	// Delete works.
	if err := st.DeleteWebhook(ctx, p1Hook.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetWebhook(ctx, p1Hook.ID); err != store.ErrNotFound {
		t.Fatal("deleted webhook should be gone")
	}
}

func TestNotificationChannelPersistence(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, _ := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "api-health", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 10, Enabled: true,
	})

	enabled, err := st.CreateNotificationChannel(ctx, domain.NotificationChannel{
		ProjectID: proj.ID, Type: domain.ChannelWebhook, Name: "ops",
		Config: map[string]string{"url": "https://hook/x"}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if enabled.Config["url"] != "https://hook/x" {
		t.Fatalf("config not round-tripped: %+v", enabled.Config)
	}
	disabled, _ := st.CreateNotificationChannel(ctx, domain.NotificationChannel{
		ProjectID: proj.ID, Type: domain.ChannelSlack, Name: "muted",
		Config: map[string]string{"url": "https://hook/y"}, Enabled: false,
	})

	if list, _ := st.ListChannelsByProject(ctx, proj.ID); len(list) != 2 {
		t.Fatalf("list channels = %d, want 2", len(list))
	}

	// Link both; only the enabled one is a delivery target.
	if err := st.LinkMonitorChannel(ctx, mon.ID, enabled.ID); err != nil {
		t.Fatalf("link: %v", err)
	}
	_ = st.LinkMonitorChannel(ctx, mon.ID, disabled.ID)
	if err := st.LinkMonitorChannel(ctx, mon.ID, enabled.ID); err != nil {
		t.Fatalf("re-link should be idempotent: %v", err)
	}
	if linked, _ := st.ListMonitorChannels(ctx, mon.ID); len(linked) != 2 {
		t.Fatalf("linked channels = %d, want 2", len(linked))
	}
	forDelivery, err := st.ListEnabledChannelsForMonitor(ctx, mon.ID)
	if err != nil || len(forDelivery) != 1 || forDelivery[0].ID != enabled.ID {
		t.Fatalf("enabled-for-monitor = %+v (err %v), want only the enabled one", forDelivery, err)
	}

	// Unlink + delete.
	if err := st.UnlinkMonitorChannel(ctx, mon.ID, enabled.ID); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if linked, _ := st.ListMonitorChannels(ctx, mon.ID); len(linked) != 1 {
		t.Fatalf("after unlink = %d, want 1", len(linked))
	}
	if err := st.DeleteNotificationChannel(ctx, disabled.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetNotificationChannel(ctx, disabled.ID); err != store.ErrNotFound {
		t.Fatal("deleted channel should be gone")
	}
}

func TestGetUserByEmail(t *testing.T) {
	st, ctx := testStore(t)
	u, _ := st.UpsertUserByOIDCSub(ctx, "kc-1", "alice@x", "Alice")
	got, err := st.GetUserByEmail(ctx, "alice@x")
	if err != nil || got.ID != u.ID {
		t.Fatalf("by email = %+v (err %v)", got, err)
	}
	if _, err := st.GetUserByEmail(ctx, "nobody@x"); err != store.ErrNotFound {
		t.Fatal("unknown email should be ErrNotFound")
	}
}

func TestUpdateMonitor(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, _ := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "old", Type: domain.MonitorHTTP, Target: "https://a",
		IntervalSeconds: 60, TimeoutSeconds: 10, Conditions: []string{"[STATUS] == 200"}, Enabled: true,
	})

	mon.Name = "new"
	mon.Target = "https://b"
	mon.IntervalSeconds = 30
	mon.Conditions = []string{"[STATUS] == 204", "[RESPONSE_TIME] < 300"}
	mon.Enabled = false
	updated, err := st.UpdateMonitor(ctx, mon)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "new" || updated.Target != "https://b" || updated.IntervalSeconds != 30 ||
		len(updated.Conditions) != 2 || updated.Enabled {
		t.Fatalf("update not applied: %+v", updated)
	}
	if updated.Type != domain.MonitorHTTP {
		t.Fatalf("type should be immutable, got %q", updated.Type)
	}
	// Persisted.
	got, _ := st.GetMonitor(ctx, mon.ID)
	if got.Name != "new" || got.IntervalSeconds != 30 {
		t.Fatalf("update not persisted: %+v", got)
	}
	// Unknown id.
	missing := mon
	missing.ID = "00000000-0000-0000-0000-000000000000"
	if _, err := st.UpdateMonitor(ctx, missing); err != store.ErrNotFound {
		t.Fatal("update of missing monitor should be ErrNotFound")
	}
}

func TestPushMonitorToken(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	created, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "nightly-backup", Type: domain.MonitorPush,
		IntervalSeconds: 3600, Enabled: true, PushToken: "cbxp_secret",
	})
	if err != nil {
		t.Fatalf("create push monitor: %v", err)
	}
	if created.PushToken != "cbxp_secret" {
		t.Fatalf("push_token not persisted: %q", created.PushToken)
	}
	got, err := st.GetMonitorByPushToken(ctx, "cbxp_secret")
	if err != nil || got.ID != created.ID {
		t.Fatalf("get by push token = %+v (err %v)", got, err)
	}
	if _, err := st.GetMonitorByPushToken(ctx, "nope"); err != store.ErrNotFound {
		t.Fatal("unknown push token should be ErrNotFound")
	}
}

func TestApiTokenPersistence(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")

	hash := store.HashToken("cbx_secret-value")
	tok, err := st.CreateApiToken(ctx, domain.ApiToken{
		OrgID: org.ID, Name: "ci", Role: domain.RoleEditor, CreatedBy: "u1",
	}, hash)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if tok.LastUsedAt != nil {
		t.Fatal("new token should not have last_used_at")
	}

	got, err := st.ApiTokenByHash(ctx, hash)
	if err != nil || got.ID != tok.ID {
		t.Fatalf("by hash = %+v (err %v)", got, err)
	}
	if _, err := st.ApiTokenByHash(ctx, store.HashToken("wrong")); err != store.ErrNotFound {
		t.Fatal("unknown hash should be ErrNotFound")
	}

	if err := st.TouchApiToken(ctx, tok.ID); err != nil {
		t.Fatalf("touch: %v", err)
	}
	after, _ := st.GetApiToken(ctx, tok.ID)
	if after.LastUsedAt == nil {
		t.Fatal("last_used_at should be set after touch")
	}

	list, err := st.ListApiTokensByOrg(ctx, org.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v (err %v), want 1", list, err)
	}

	// Hash uniqueness.
	if _, err := st.CreateApiToken(ctx, domain.ApiToken{OrgID: org.ID, Name: "dup", Role: domain.RoleViewer}, hash); err == nil {
		t.Fatal("duplicate hash should be rejected")
	}

	if err := st.DeleteApiToken(ctx, tok.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetApiToken(ctx, tok.ID); err != store.ErrNotFound {
		t.Fatal("deleted token should be gone")
	}
}

func TestProjectMembershipMustBelongToOrg(t *testing.T) {
	st, ctx := testStore(t)

	acme, _ := st.CreateOrganization(ctx, "acme", "Acme")
	globex, _ := st.CreateOrganization(ctx, "globex", "Globex")
	globexProj, _ := st.CreateProject(ctx, globex.ID, "file-service", "File Service")
	user, _ := st.UpsertUserByOIDCSub(ctx, "eve", "eve@x", "Eve")

	// Grant references a project from a different org than org_id → composite FK
	// must reject it.
	_, err := st.CreateMembership(ctx, domain.Membership{
		UserID: user.ID, OrgID: acme.ID, ProjectID: globexProj.ID, Role: domain.RoleEditor,
	})
	if err == nil {
		t.Fatal("expected FK violation for cross-org project membership")
	}
}

func TestOrgMembersEnrichmentAndMutation(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	admin, _ := st.UpsertUserByOIDCSub(ctx, "sub-admin", "admin@x.com", "Ada Admin")
	editor, _ := st.UpsertUserByOIDCSub(ctx, "sub-ed", "ed@x.com", "Ed Editor")
	am, _ := st.CreateMembership(ctx, domain.Membership{UserID: admin.ID, OrgID: org.ID, Role: domain.RoleOrgAdmin})
	em, _ := st.CreateMembership(ctx, domain.Membership{UserID: editor.ID, OrgID: org.ID, Role: domain.RoleEditor})
	// A session makes the editor "last active".
	if _, err := st.CreateSession(ctx, editor.ID, "tok-ed", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}

	members, err := st.ListOrgMembers(ctx, org.ID)
	if err != nil || len(members) != 2 {
		t.Fatalf("list members = %+v (err %v), want 2", members, err)
	}
	byUser := map[string]domain.Member{}
	for _, m := range members {
		byUser[m.UserID] = m
	}
	if byUser[admin.ID].Email != "admin@x.com" || byUser[admin.ID].DisplayName != "Ada Admin" {
		t.Fatalf("admin enrichment = %+v", byUser[admin.ID])
	}
	if byUser[editor.ID].LastActiveAt == nil {
		t.Fatal("editor should have a last_active_at from the session")
	}
	if byUser[admin.ID].LastActiveAt != nil {
		t.Fatal("admin has no session, last_active_at should be nil")
	}

	// Admin count reflects the single org admin.
	if n, _ := st.CountOrgAdmins(ctx, org.ID); n != 1 {
		t.Fatalf("org admins = %d, want 1", n)
	}
	// Promote the editor to org admin.
	if _, err := st.UpdateMembershipRole(ctx, em.ID, domain.RoleOrgAdmin); err != nil {
		t.Fatalf("update role: %v", err)
	}
	if n, _ := st.CountOrgAdmins(ctx, org.ID); n != 2 {
		t.Fatalf("org admins after promote = %d, want 2", n)
	}
	// Remove the original admin; one admin remains.
	if err := st.DeleteMembership(ctx, am.ID); err != nil {
		t.Fatalf("delete membership: %v", err)
	}
	if _, err := st.GetMembership(ctx, am.ID); err != store.ErrNotFound {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
	if n, _ := st.CountOrgAdmins(ctx, org.ID); n != 1 {
		t.Fatalf("org admins after delete = %d, want 1", n)
	}
}

func TestSubscriberLifecycleAndFanout(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, _ := st.CreateMonitor(ctx, domain.Monitor{ProjectID: proj.ID, Name: "api", Type: domain.MonitorHTTP, Target: "https://x", IntervalSeconds: 60, TimeoutSeconds: 5})
	page, _ := st.CreateStatusPage(ctx, domain.StatusPage{OrgID: org.ID, Slug: "s", Title: "S", Visibility: domain.VisibilityPublic})
	if _, err := st.CreateComponent(ctx, domain.Component{StatusPageID: page.ID, Name: "API", MonitorID: mon.ID}); err != nil {
		t.Fatalf("component: %v", err)
	}

	// Unconfirmed subscriber does not appear in the fan-out.
	sub, err := st.CreateSubscriber(ctx, domain.Subscriber{StatusPageID: page.ID, Email: "a@x.com", ConfirmToken: "tok1"})
	if err != nil || sub.ConfirmedAt != nil {
		t.Fatalf("create = %+v (err %v)", sub, err)
	}
	if emails, _ := st.ConfirmedSubscriberEmailsForProject(ctx, proj.ID); len(emails) != 0 {
		t.Fatalf("unconfirmed appeared: %v", emails)
	}

	// Re-subscribing the same email re-issues the token on the same row.
	sub2, _ := st.CreateSubscriber(ctx, domain.Subscriber{StatusPageID: page.ID, Email: "a@x.com", ConfirmToken: "tok2"})
	if sub2.ID != sub.ID || sub2.ConfirmToken != "tok2" {
		t.Fatalf("re-subscribe = %+v, want same id / new token", sub2)
	}

	// Confirm → appears; idempotent; unknown token → ErrNotFound.
	if _, err := st.ConfirmSubscriber(ctx, "tok2"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	emails, _ := st.ConfirmedSubscriberEmailsForProject(ctx, proj.ID)
	if len(emails) != 1 || emails[0] != "a@x.com" {
		t.Fatalf("fanout = %v, want [a@x.com]", emails)
	}
	if _, err := st.ConfirmSubscriber(ctx, "nope"); err != store.ErrNotFound {
		t.Fatalf("confirm unknown = %v, want ErrNotFound", err)
	}

	// Unsubscribe removes it from the fan-out; deleting again is ErrNotFound.
	if err := st.DeleteSubscriberByToken(ctx, "tok2"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if emails, _ := st.ConfirmedSubscriberEmailsForProject(ctx, proj.ID); len(emails) != 0 {
		t.Fatalf("after unsubscribe = %v, want empty", emails)
	}
	if err := st.DeleteSubscriberByToken(ctx, "tok2"); err != store.ErrNotFound {
		t.Fatalf("delete again = %v, want ErrNotFound", err)
	}
}

func TestAuditRecordAndList(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	other, _ := st.CreateOrganization(ctx, "globex", "Globex")
	actor, _ := st.UpsertUserByOIDCSub(ctx, "sub-a", "ada@x.com", "Ada")

	if err := st.RecordAudit(ctx, domain.AuditEntry{OrgID: org.ID, ActorUserID: actor.ID, Action: "member.add", Target: "viewer"}); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	if err := st.RecordAudit(ctx, domain.AuditEntry{OrgID: org.ID, ActorUserID: actor.ID, ViaToken: true, Action: "token.create", Target: "ci"}); err != nil {
		t.Fatalf("record 2: %v", err)
	}
	// A machine actor without a user resolves to NULL.
	if err := st.RecordAudit(ctx, domain.AuditEntry{OrgID: org.ID, Action: "member.remove", Target: "user x"}); err != nil {
		t.Fatalf("record 3: %v", err)
	}
	// A different org's entry must not leak.
	if err := st.RecordAudit(ctx, domain.AuditEntry{OrgID: other.ID, ActorUserID: actor.ID, Action: "member.add", Target: "editor"}); err != nil {
		t.Fatalf("record other: %v", err)
	}

	entries, err := st.ListAuditByOrg(ctx, org.ID, 100)
	if err != nil || len(entries) != 3 {
		t.Fatalf("list = %d (err %v), want 3", len(entries), err)
	}
	// Newest first: the NULL-actor member.remove is last written → first.
	if entries[0].Action != "member.remove" || entries[0].ActorUserID != "" {
		t.Fatalf("first = %+v, want member.remove with no actor", entries[0])
	}
	// Actor identity is joined for entries that have one.
	if entries[1].Action != "token.create" || !entries[1].ViaToken || entries[1].ActorEmail != "ada@x.com" {
		t.Fatalf("second = %+v, want token.create by ada via token", entries[1])
	}
	// Limit is honoured.
	if lim, _ := st.ListAuditByOrg(ctx, org.ID, 1); len(lim) != 1 {
		t.Fatalf("limit 1 returned %d", len(lim))
	}
}

func TestMonitorConfigAndStatuses(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	a, _ := st.CreateMonitor(ctx, domain.Monitor{ProjectID: proj.ID, Name: "a", Type: domain.MonitorHTTP, Target: "https://a", IntervalSeconds: 60, TimeoutSeconds: 5})
	b, _ := st.CreateMonitor(ctx, domain.Monitor{ProjectID: proj.ID, Name: "b", Type: domain.MonitorHTTP, Target: "https://b", IntervalSeconds: 60, TimeoutSeconds: 5})

	// A composite persists and round-trips its config map.
	comp, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "group", Type: domain.MonitorComposite, IntervalSeconds: 60,
		Config: map[string]string{"children": a.ID + "," + b.ID, "mode": "any"},
	})
	if err != nil {
		t.Fatalf("create composite: %v", err)
	}
	got, _ := st.GetMonitor(ctx, comp.ID)
	if got.Config["mode"] != "any" || len(got.ChildIDs()) != 2 {
		t.Fatalf("config round-trip = %+v", got.Config)
	}

	// MonitorStatuses returns each child's status (both pending initially); a
	// valid-but-absent id is simply not in the map.
	const absent = "00000000-0000-0000-0000-000000000000"
	statuses, err := st.MonitorStatuses(ctx, []string{a.ID, b.ID, absent})
	if err != nil {
		t.Fatalf("statuses: %v", err)
	}
	if statuses[a.ID] != domain.StatusPending || statuses[b.ID] != domain.StatusPending {
		t.Fatalf("statuses = %+v, want both pending", statuses)
	}
	if _, ok := statuses[absent]; ok {
		t.Fatal("absent id should not be in the map")
	}
}

func TestTOTPSecretAndRecoveryCodes(t *testing.T) {
	st, ctx := testStore(t)
	u, err := st.CreateLocalUser(ctx, "2fa@x", "TwoFA", "hash-abc", false)
	if err != nil {
		t.Fatalf("create local user: %v", err)
	}

	// Fresh account: no secret, disabled.
	if sec, en, err := st.GetTOTP(ctx, u.ID); err != nil || sec != "" || en {
		t.Fatalf("fresh totp = (%q,%v,%v), want empty/false/nil", sec, en, err)
	}

	// Set a pending secret (encrypted at rest, decrypted on read), still disabled.
	if err := st.SetTOTPSecret(ctx, u.ID, "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatalf("set secret: %v", err)
	}
	if sec, en, _ := st.GetTOTP(ctx, u.ID); sec != "JBSWY3DPEHPK3PXP" || en {
		t.Fatalf("pending totp = (%q,%v), want secret/false", sec, en)
	}
	// Credential lookup must surface the decrypted secret + enabled flag for login.
	cred, err := st.LocalCredentialByEmail(ctx, "2fa@x")
	if err != nil || cred.TOTPEnabled || cred.TOTPSecret != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("cred pre-enable: %+v err=%v", cred, err)
	}

	// Enable, store recovery codes.
	if err := st.EnableTOTP(ctx, u.ID); err != nil {
		t.Fatalf("enable: %v", err)
	}
	hashes := []string{store.HashToken("code-a"), store.HashToken("code-b")}
	if err := st.ReplaceRecoveryCodes(ctx, u.ID, hashes); err != nil {
		t.Fatalf("replace recovery: %v", err)
	}
	if cred, _ := st.LocalCredentialByEmail(ctx, "2fa@x"); !cred.TOTPEnabled {
		t.Fatal("cred should be TOTPEnabled after enable")
	}

	// Consume a recovery code once; a second attempt fails.
	if ok, err := st.ConsumeRecoveryCode(ctx, u.ID, store.HashToken("code-a")); err != nil || !ok {
		t.Fatalf("first consume = (%v,%v), want true", ok, err)
	}
	if ok, _ := st.ConsumeRecoveryCode(ctx, u.ID, store.HashToken("code-a")); ok {
		t.Fatal("reused recovery code should not consume again")
	}
	// Unknown code never matches.
	if ok, _ := st.ConsumeRecoveryCode(ctx, u.ID, store.HashToken("nope")); ok {
		t.Fatal("unknown recovery code should not consume")
	}

	// ReplaceRecoveryCodes wipes the old set (used + unused).
	if err := st.ReplaceRecoveryCodes(ctx, u.ID, []string{store.HashToken("code-c")}); err != nil {
		t.Fatalf("replace again: %v", err)
	}
	if ok, _ := st.ConsumeRecoveryCode(ctx, u.ID, store.HashToken("code-b")); ok {
		t.Fatal("old recovery code should be gone after replace")
	}

	// Disable clears secret + all recovery codes.
	if err := st.DisableTOTP(ctx, u.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if sec, en, _ := st.GetTOTP(ctx, u.ID); sec != "" || en {
		t.Fatalf("post-disable totp = (%q,%v), want empty/false", sec, en)
	}
	if ok, _ := st.ConsumeRecoveryCode(ctx, u.ID, store.HashToken("code-c")); ok {
		t.Fatal("recovery codes should be cleared on disable")
	}
}

func TestPasswordResetTokens(t *testing.T) {
	st, ctx := testStore(t)
	u, err := st.CreateLocalUser(ctx, "reset@x", "Reset", "hash-abc", false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Valid token → consumes once, returns the user id.
	if err := st.CreatePasswordResetToken(ctx, u.ID, store.HashToken("tok-a"), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create token: %v", err)
	}
	got, err := st.ConsumePasswordResetToken(ctx, store.HashToken("tok-a"))
	if err != nil || got != u.ID {
		t.Fatalf("consume = (%q,%v), want %q/nil", got, err, u.ID)
	}
	// Second use of the same token → ErrNotFound (single-use).
	if _, err := st.ConsumePasswordResetToken(ctx, store.HashToken("tok-a")); err != store.ErrNotFound {
		t.Fatalf("reused token err = %v, want ErrNotFound", err)
	}

	// Expired token → ErrNotFound.
	if err := st.CreatePasswordResetToken(ctx, u.ID, store.HashToken("tok-exp"), time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("create expired token: %v", err)
	}
	if _, err := st.ConsumePasswordResetToken(ctx, store.HashToken("tok-exp")); err != store.ErrNotFound {
		t.Fatalf("expired token err = %v, want ErrNotFound", err)
	}

	// Unknown token → ErrNotFound.
	if _, err := st.ConsumePasswordResetToken(ctx, store.HashToken("tok-nope")); err != store.ErrNotFound {
		t.Fatalf("unknown token err = %v, want ErrNotFound", err)
	}
}

func TestRecordCheckStatusConfirmationsAndMaintenance(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "api", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true, FailureThreshold: 2,
	})
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	// First success: pending → up.
	if prev, cur, sup, err := st.RecordCheckStatus(ctx, mon.ID, true); err != nil || cur != domain.StatusUp || sup {
		t.Fatalf("up: (%q→%q sup=%v err=%v), want →up", prev, cur, sup, err)
	}
	// First failure (threshold 2): stays up, no flip.
	if prev, cur, _, err := st.RecordCheckStatus(ctx, mon.ID, false); err != nil || prev != domain.StatusUp || cur != domain.StatusUp {
		t.Fatalf("1st fail: (%q→%q err=%v), want up→up (unconfirmed)", prev, cur, err)
	}
	// Second consecutive failure: confirmed down.
	if prev, cur, sup, err := st.RecordCheckStatus(ctx, mon.ID, false); err != nil || prev != domain.StatusUp || cur != domain.StatusDown || sup {
		t.Fatalf("2nd fail: (%q→%q sup=%v err=%v), want up→down not suppressed", prev, cur, sup, err)
	}
	// Recovery is immediate and resets the counter.
	if _, cur, _, _ := st.RecordCheckStatus(ctx, mon.ID, true); cur != domain.StatusUp {
		t.Fatalf("recovery cur=%q, want up", cur)
	}
	if _, cur, _, _ := st.RecordCheckStatus(ctx, mon.ID, false); cur != domain.StatusUp {
		t.Fatalf("counter should reset after recovery: 1 fail cur=%q, want up", cur)
	}

	// With an active maintenance window over the project, a down flip is suppressed.
	now := time.Now()
	if _, err := st.CreateMaintenanceWindow(ctx, domain.MaintenanceWindow{
		ProjectID: proj.ID, Reason: "maint", StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("create maintenance: %v", err)
	}
	// threshold now needs one more failure to confirm (counter is at 1 from above).
	if _, cur, sup, err := st.RecordCheckStatus(ctx, mon.ID, false); err != nil || cur != domain.StatusDown || !sup {
		t.Fatalf("down in maintenance: (cur=%q sup=%v err=%v), want down + suppressed", cur, sup, err)
	}
}

func TestRenotifyReminders(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	// One monitor with re-notify on, one with it off (default).
	on, _ := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "on", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true, FailureThreshold: 1, RenotifySeconds: 1,
	})
	off, _ := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "off", Type: domain.MonitorHTTP, Target: "https://y",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true, FailureThreshold: 1, RenotifySeconds: 0,
	})
	// Both go down (threshold 1 → immediate); this stamps last_notified_at.
	if _, cur, _, _ := st.RecordCheckStatus(ctx, on.ID, false); cur != domain.StatusDown {
		t.Fatalf("on should be down")
	}
	if _, cur, _, _ := st.RecordCheckStatus(ctx, off.ID, false); cur != domain.StatusDown {
		t.Fatalf("off should be down")
	}

	// Not due yet (just stamped).
	if n, err := st.EnqueueRenotifyReminders(ctx); err != nil || n != 0 {
		t.Fatalf("immediate renotify = (%d,%v), want 0", n, err)
	}
	// After the interval, only the re-notify-enabled monitor is due.
	time.Sleep(1200 * time.Millisecond)
	if n, err := st.EnqueueRenotifyReminders(ctx); err != nil || n != 1 {
		t.Fatalf("due renotify = (%d,%v), want 1 (only 'on')", n, err)
	}
	// Bumped → not due again immediately.
	if n, _ := st.EnqueueRenotifyReminders(ctx); n != 0 {
		t.Fatalf("renotify should have bumped last_notified_at, got %d due", n)
	}
	// Recovery clears last_notified_at → never due again.
	if _, cur, _, _ := st.RecordCheckStatus(ctx, on.ID, true); cur != domain.StatusUp {
		t.Fatalf("on should recover")
	}
	time.Sleep(1200 * time.Millisecond)
	if n, _ := st.EnqueueRenotifyReminders(ctx); n != 0 {
		t.Fatalf("recovered monitor must not renotify, got %d", n)
	}
}

// TestSearchScopeBeforeLimit proves tenant scoping is applied in SQL BEFORE the
// per-type LIMIT: an allowed monitor that sorts AFTER a full page of another
// tenant's matches must still be returned. With the old post-LIMIT filtering it
// would be crowded out (the global top-8 were all the other tenant's, then removed).
func TestSearchScopeBeforeLimit(t *testing.T) {
	st, ctx := testStore(t)
	orgA, _ := st.CreateOrganization(ctx, "acme", "Acme")
	orgB, _ := st.CreateOrganization(ctx, "globex", "Globex")
	pA, _ := st.CreateProject(ctx, orgA.ID, "a", "A")
	pB, _ := st.CreateProject(ctx, orgB.ID, "b", "B")

	mk := func(projectID, name string) {
		if _, err := st.CreateMonitor(ctx, domain.Monitor{
			ProjectID: projectID, Name: name, Type: domain.MonitorHTTP, Target: "https://x",
			IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true,
		}); err != nil {
			t.Fatalf("create monitor %s: %v", name, err)
		}
	}
	// 9 matches in org B that sort BEFORE the single org-A match (limit is 8).
	for i := 0; i < 9; i++ {
		mk(pB.ID, fmt.Sprintf("widget-b-%02d", i))
	}
	mk(pA.ID, "widget-z-target") // sorts last by name

	// Caller sees only org A.
	scope := store.SearchScope{OrgIDs: []string{orgA.ID}}
	hits, err := st.Search(ctx, "widget", 8, scope)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var gotTarget bool
	for _, h := range hits {
		if h.OrgID != orgA.ID {
			t.Fatalf("search leaked a non-visible org's hit: %+v", h)
		}
		if h.Type == "monitor" && h.Label == "widget-z-target" {
			gotTarget = true
		}
	}
	if !gotTarget {
		t.Fatalf("the allowed monitor was crowded out by another tenant (scope applied after LIMIT?); hits=%+v", hits)
	}

	// A global admin (AllOrgs) sees both tenants' matches.
	admin, err := st.Search(ctx, "widget", 8, store.SearchScope{AllOrgs: true})
	if err != nil {
		t.Fatalf("admin search: %v", err)
	}
	if len(admin) == 0 {
		t.Fatal("admin search returned nothing")
	}
}
