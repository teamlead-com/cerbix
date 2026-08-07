package store_test

import (
	"errors"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

func TestListAllUsers(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	admin, _ := st.CreateLocalUser(ctx, "root@x.com", "Root", "hash", true)
	ada, _ := st.UpsertUserByOIDCSub(ctx, "sub-ada", "ada@x.com", "Ada")
	orphan, _ := st.UpsertUserByOIDCSub(ctx, "sub-orphan", "lone@x.com", "Lone Wolf")

	if _, err := st.CreateMembership(ctx, domain.Membership{UserID: admin.ID, OrgID: org.ID, Role: domain.RoleOrgAdmin}); err != nil {
		t.Fatalf("org membership: %v", err)
	}
	if _, err := st.CreateMembership(ctx, domain.Membership{UserID: ada.ID, OrgID: org.ID, ProjectID: proj.ID, Role: domain.RoleViewer}); err != nil {
		t.Fatalf("project membership: %v", err)
	}

	users, err := st.ListAllUsers(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("got %d users, want 3", len(users))
	}
	byEmail := map[string]domain.AdminUser{}
	for _, u := range users {
		byEmail[u.Email] = u
	}

	// The org-less OIDC user is visible with an empty (non-nil) membership list.
	lo, ok := byEmail["lone@x.com"]
	if !ok || lo.ID != orphan.ID {
		t.Fatalf("orphan user missing: %+v", users)
	}
	if lo.AuthType != "oidc" || lo.Memberships == nil || len(lo.Memberships) != 0 {
		t.Fatalf("orphan = %+v, want oidc with empty memberships", lo)
	}

	ad := byEmail["ada@x.com"]
	if ad.AuthType != "oidc" {
		t.Fatalf("ada auth_type = %q, want oidc", ad.AuthType)
	}
	if len(ad.Memberships) != 1 || ad.Memberships[0].ProjectID != proj.ID ||
		ad.Memberships[0].ProjectName != "API" || ad.Memberships[0].OrgName != "Acme" {
		t.Fatalf("ada memberships = %+v", ad.Memberships)
	}

	ro := byEmail["root@x.com"]
	if ro.AuthType != "local" || !ro.IsGlobalAdmin {
		t.Fatalf("root = %+v, want local global admin", ro)
	}
	if len(ro.Memberships) != 1 || ro.Memberships[0].Role != domain.RoleOrgAdmin || ro.Memberships[0].ProjectID != "" {
		t.Fatalf("root memberships = %+v", ro.Memberships)
	}

	// Substring search, case-insensitive, matches display name too.
	found, err := st.ListAllUsers(ctx, "WOLF")
	if err != nil || len(found) != 1 || found[0].Email != "lone@x.com" {
		t.Fatalf("search = %+v (err %v), want just lone@x.com", found, err)
	}
}

func TestDeleteUserCascades(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	u, _ := st.CreateLocalUser(ctx, "gone@x.com", "Gone", "hash", false)
	if _, err := st.CreateMembership(ctx, domain.Membership{UserID: u.ID, OrgID: org.ID, Role: domain.RoleEditor}); err != nil {
		t.Fatalf("membership: %v", err)
	}
	// An audit entry by this user must survive with a NULL actor.
	if err := st.RecordAudit(ctx, domain.AuditEntry{OrgID: org.ID, ActorUserID: u.ID, Action: "member.add", Target: "x"}); err != nil {
		t.Fatalf("audit: %v", err)
	}

	if err := st.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetUser(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("user still readable: %v", err)
	}
	if members, _ := st.ListOrgMembers(ctx, org.ID); len(members) != 0 {
		t.Fatalf("memberships not cascaded: %+v", members)
	}
	entries, err := st.ListAuditByOrg(ctx, org.ID, 10)
	if err != nil || len(entries) != 1 || entries[0].ActorUserID != "" {
		t.Fatalf("audit after delete = %+v (err %v), want 1 entry with NULL actor", entries, err)
	}
	if err := st.DeleteUser(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

func TestCountGlobalAdminsAndNullOrgAudit(t *testing.T) {
	st, ctx := testStore(t)
	if n, err := st.CountGlobalAdmins(ctx); err != nil || n != 0 {
		t.Fatalf("empty count = %d (err %v)", n, err)
	}
	a, _ := st.CreateLocalUser(ctx, "a@x.com", "A", "hash", true)
	if _, err := st.CreateLocalUser(ctx, "b@x.com", "B", "hash", false); err != nil {
		t.Fatalf("create b: %v", err)
	}
	if n, _ := st.CountGlobalAdmins(ctx); n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}
	if err := st.SetGlobalAdmin(ctx, a.ID, false); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if n, _ := st.CountGlobalAdmins(ctx); n != 0 {
		t.Fatalf("count after demote = %d, want 0", n)
	}
	// Instance-level audit rows (empty org) are accepted after migration 00047.
	if err := st.RecordAudit(ctx, domain.AuditEntry{ActorUserID: a.ID, Action: "user.global_admin", Target: "user " + a.ID + " → false"}); err != nil {
		t.Fatalf("null-org audit: %v", err)
	}
}

func TestInsertHeartbeatForDeletedMonitor(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "gone", Type: domain.MonitorTCP, Target: "localhost:1",
		IntervalSeconds: 30, TimeoutSeconds: 5, Region: domain.DefaultRegion,
	})
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	if err := st.DeleteMonitor(ctx, mon.ID); err != nil {
		t.Fatalf("delete monitor: %v", err)
	}
	// The in-flight result must be dropped as a quiet not-found, not an error.
	err = st.InsertHeartbeat(ctx, domain.Heartbeat{MonitorID: mon.ID, Up: true})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("insert after delete = %v, want ErrNotFound", err)
	}
}

func TestInsertHeartbeatsBulkSkipsDeletedMonitor(t *testing.T) {
	st, ctx := testStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	live, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "live", Type: domain.MonitorTCP, Target: "localhost:1",
		IntervalSeconds: 30, TimeoutSeconds: 5, Region: domain.DefaultRegion,
	})
	if err != nil {
		t.Fatalf("create live: %v", err)
	}
	gone, _ := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "gone", Type: domain.MonitorTCP, Target: "localhost:2",
		IntervalSeconds: 30, TimeoutSeconds: 5, Region: domain.DefaultRegion,
	})
	if err := st.DeleteMonitor(ctx, gone.ID); err != nil {
		t.Fatalf("delete gone: %v", err)
	}
	// A batch mixing a live and a deleted monitor must not abort — the live
	// heartbeats land, the deleted one is skipped.
	n, err := st.InsertHeartbeatsBulk(ctx, []domain.Heartbeat{
		{MonitorID: live.ID, Up: true},
		{MonitorID: gone.ID, Up: false},
		{MonitorID: live.ID, Up: true}, // dup ts within the call → one lands
	})
	if err != nil {
		t.Fatalf("bulk insert: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected at least the live heartbeat inserted, got %d", n)
	}
}
