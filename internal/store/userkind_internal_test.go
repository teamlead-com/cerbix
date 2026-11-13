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

// sec-hardening-2 H-1 (fixes 4 and 5): users.kind, its narrow backfill, and the two OIDC upserts —
// the machine one refuses a human row and writes nothing; the human one flips a machine row and
// strips everything granted to it.

const kindIssuer = "https://idp.kind.test"

// userRowJSON is the whole row as JSON, so "byte-identical" covers every column.
func userRowJSON(t *testing.T, st *Store, ctx context.Context, id string) string {
	t.Helper()
	var raw string
	if err := st.pool.QueryRow(ctx, `SELECT row_to_json(u)::text FROM users u WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("row json: %v", err)
	}
	return raw
}

func userKind(t *testing.T, st *Store, ctx context.Context, id string) (kind string, admin bool) {
	t.Helper()
	if err := st.pool.QueryRow(ctx, `SELECT kind, is_global_admin FROM users WHERE id = $1`, id).Scan(&kind, &admin); err != nil {
		t.Fatalf("read kind: %v", err)
	}
	return kind, admin
}

func membershipCount(t *testing.T, st *Store, ctx context.Context, userID string) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	return n
}

func rerunMigrationUp(t *testing.T, st *Store, ctx context.Context, file string) {
	t.Helper()
	raw, err := migrationsFS.ReadFile("migrations/" + file)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	up := string(raw)
	i, j := strings.Index(up, "-- +goose Up"), strings.Index(up, "-- +goose Down")
	if i < 0 || j < i {
		t.Fatalf("migration has no Up/Down sections")
	}
	if _, err := st.pool.Exec(ctx, up[i:j]); err != nil {
		t.Fatalf("re-run Up: %v", err)
	}
}

func TestUsersKindBackfill(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	ins := func(sub *string, email string, lastSeen bool, password bool, admin bool) string {
		t.Helper()
		var id string
		var pw *string
		if password {
			h := "hash"
			pw = &h
		}
		seen := "NULL"
		if lastSeen {
			seen = "now()"
		}
		if err := st.pool.QueryRow(ctx,
			`INSERT INTO users (oidc_issuer, oidc_sub, email, display_name, password_hash, is_global_admin, last_seen_at)
			 VALUES (CASE WHEN $1::text IS NULL THEN NULL ELSE '`+kindIssuer+`' END, $1, $2, '', $3, $4, `+seen+`)
			 RETURNING id`, sub, email, pw, admin).Scan(&id); err != nil {
			t.Fatalf("insert %s: %v", email, err)
		}
		return id
	}
	s := func(v string) *string { return &v }
	sa := ins(s("sa-1"), "sa-1@clients", false, false, false)
	dormantAdmin := ins(s("ada"), "ada@x.com", false, false, true)
	dormantHuman := ins(s("bob"), "bob@x.com", false, false, false)
	seenClients := ins(s("sa-2"), "sa-2@clients", true, false, false)
	local := ins(nil, "root@clients", false, true, false)

	if _, err := st.pool.Exec(ctx, `UPDATE users SET kind = 'human'`); err != nil {
		t.Fatalf("reset kind: %v", err)
	}
	rerunMigrationUp(t, st, ctx, "00112_users_kind_machine_clients.sql")

	want := map[string]string{
		sa: domain.UserKindMachine, dormantAdmin: domain.UserKindHuman, dormantHuman: domain.UserKindHuman,
		seenClients: domain.UserKindHuman, local: domain.UserKindHuman,
	}
	for id, w := range want {
		if got, _ := userKind(t, st, ctx, id); got != w {
			t.Errorf("user %s: kind %q, want %q", id, got, w)
		}
	}
}

func TestMachineUpsertRefusesHumanRowUnchanged(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	human, _, err := st.UpsertHumanUserByOIDCIdentity(ctx, kindIssuer, "sub-h", "h@x.com", "H")
	if err != nil {
		t.Fatalf("human upsert: %v", err)
	}
	before := userRowJSON(t, st, ctx, human.ID)
	_, err = st.UpsertMachineUserByOIDCIdentity(ctx, kindIssuer, "sub-h", "other@clients", "svc")
	if !errors.Is(err, ErrSubjectIsHuman) {
		t.Fatalf("machine upsert over a human: err %v, want ErrSubjectIsHuman", err)
	}
	if after := userRowJSON(t, st, ctx, human.ID); after != before {
		t.Fatalf("human row changed:\nbefore %s\nafter  %s", before, after)
	}
}

func TestMachineUpsertLegacyNullIssuerHumanUntouched(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	var id string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO users (oidc_issuer, oidc_sub, email, display_name) VALUES (NULL, 's1', 'legacy@x.com', 'L') RETURNING id`).
		Scan(&id); err != nil {
		t.Fatalf("legacy row: %v", err)
	}
	before := userRowJSON(t, st, ctx, id)
	if _, err := st.UpsertMachineUserByOIDCIdentity(ctx, kindIssuer, "s1", "s1@clients", "svc"); !errors.Is(err, ErrSubjectIsHuman) {
		t.Fatalf("err %v, want ErrSubjectIsHuman", err)
	}
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE oidc_sub = 's1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows for s1 = %d (%v), want 1", n, err)
	}
	if after := userRowJSON(t, st, ctx, id); after != before {
		t.Fatalf("legacy row changed:\nbefore %s\nafter  %s", before, after)
	}
}

func TestMachineUpsertCreatesAndUpdatesMachine(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	u, err := st.UpsertMachineUserByOIDCIdentity(ctx, kindIssuer, "svc-1", "svc-1@clients", "ci")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if u.Kind != domain.UserKindMachine || u.IsGlobalAdmin {
		t.Fatalf("created %+v, want a non-admin machine", u)
	}
	u2, err := st.UpsertMachineUserByOIDCIdentity(ctx, kindIssuer, "svc-1", "renamed@clients", "ci")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if u2.ID != u.ID || u2.Email != "renamed@clients" || u2.Kind != domain.UserKindMachine {
		t.Fatalf("updated %+v, want same id %s with the new email", u2, u.ID)
	}
}

func TestHumanUpsertFlipsMachineAndStripsGrants(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	org, err := st.CreateOrganization(ctx, "kind-org", "Kind")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "kind-proj", "Kind")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	m, err := st.UpsertMachineUserByOIDCIdentity(ctx, kindIssuer, "pre-claimed", "pre-claimed@clients", "svc")
	if err != nil {
		t.Fatalf("machine: %v", err)
	}
	if _, err := st.CreateMembership(ctx, domain.Membership{UserID: m.ID, OrgID: org.ID, Role: domain.RoleOrgAdmin}); err != nil {
		t.Fatalf("org grant: %v", err)
	}
	if _, err := st.CreateMembership(ctx, domain.Membership{UserID: m.ID, OrgID: org.ID, ProjectID: proj.ID, Role: domain.RoleViewer}); err != nil {
		t.Fatalf("project grant: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE users SET is_global_admin = true WHERE id = $1`, m.ID); err != nil {
		t.Fatalf("fixture admin: %v", err)
	}

	u, flip, err := st.UpsertHumanUserByOIDCIdentity(ctx, kindIssuer, "pre-claimed", "person@x.com", "Person")
	if err != nil {
		t.Fatalf("human upsert: %v", err)
	}
	if u.ID != m.ID || !flip.Flipped || !flip.WasGlobalAdmin || len(flip.Revoked) != 2 {
		t.Fatalf("flip = %+v (user %s), want flipped, was admin, 2 revoked, same id", flip, u.ID)
	}
	if kind, admin := userKind(t, st, ctx, m.ID); kind != domain.UserKindHuman || admin {
		t.Fatalf("after flip kind=%q admin=%v, want human, not admin", kind, admin)
	}
	if u.Kind != domain.UserKindHuman || u.IsGlobalAdmin || u.Email != "person@x.com" {
		t.Fatalf("returned user %+v", u)
	}
	if n := membershipCount(t, st, ctx, m.ID); n != 0 {
		t.Fatalf("%d memberships survived the flip", n)
	}
	var instanceRows, orgRows int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE action = 'user.kind_human' AND org_id IS NULL),
		        count(*) FILTER (WHERE action = 'member.revoke_machine_grants' AND org_id = $2)
		   FROM audit_logs WHERE actor_user_id = $1`, m.ID, org.ID).Scan(&instanceRows, &orgRows); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if instanceRows != 1 || orgRows != 1 {
		t.Fatalf("audit rows: instance %d, org %d; want 1 and 1", instanceRows, orgRows)
	}
}

func TestHumanUpsertKeepsHumanGrants(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	org, _ := st.CreateOrganization(ctx, "keep-org", "Keep")
	h, _, err := st.UpsertHumanUserByOIDCIdentity(ctx, kindIssuer, "keeper", "k@x.com", "K")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := st.CreateMembership(ctx, domain.Membership{UserID: h.ID, OrgID: org.ID, Role: domain.RoleOrgAdmin}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	again, flip, err := st.UpsertHumanUserByOIDCIdentity(ctx, kindIssuer, "keeper", "k2@x.com", "K")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if flip.Flipped || again.ID != h.ID || again.Email != "k2@x.com" {
		t.Fatalf("second sign-in: %+v flip %+v", again, flip)
	}
	if n := membershipCount(t, st, ctx, h.ID); n != 1 {
		t.Fatalf("human lost grants: %d", n)
	}
}

func TestHumanUpsertLegacyClaim(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	var id string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO users (oidc_issuer, oidc_sub, email, display_name) VALUES (NULL, 'legacy-h', 'l@x.com', 'L') RETURNING id`).
		Scan(&id); err != nil {
		t.Fatalf("legacy row: %v", err)
	}
	u, flip, err := st.UpsertHumanUserByOIDCIdentity(ctx, kindIssuer, "legacy-h", "l@x.com", "L")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if u.ID != id || flip.Flipped || u.Kind != domain.UserKindHuman {
		t.Fatalf("legacy claim: got %+v flip %+v, want id %s, human", u, flip, id)
	}
	var issuer *string
	if err := st.pool.QueryRow(ctx, `SELECT oidc_issuer FROM users WHERE id = $1`, id).Scan(&issuer); err != nil || issuer == nil || *issuer != kindIssuer {
		t.Fatalf("issuer not claimed: %v (%v)", issuer, err)
	}
}

// TestBearerAndCallbackRace drives the bearer (machine) and callback (human) upserts for one
// subject at the same instant, with and without a pre-existing machine row. No interleaving may
// leave anything but one human row with no grants, and the callback never fails.
func TestBearerAndCallbackRace(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	org, err := st.CreateOrganization(ctx, "race-org", "Race")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	for _, preexisting := range []bool{true, false} {
		for i := 0; i < 50; i++ {
			sub := fmt.Sprintf("race-%v-%d", preexisting, i)
			if preexisting {
				m, err := st.UpsertMachineUserByOIDCIdentity(ctx, kindIssuer, sub, sub+"@clients", "svc")
				if err != nil {
					t.Fatalf("seed machine: %v", err)
				}
				if _, err := st.CreateMembership(ctx, domain.Membership{UserID: m.ID, OrgID: org.ID, Role: domain.RoleOrgAdmin}); err != nil {
					t.Fatalf("seed grant: %v", err)
				}
			}
			var (
				wg              sync.WaitGroup
				start           = make(chan struct{})
				machine, human  domain.User
				machErr, humErr error
			)
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				machine, machErr = st.UpsertMachineUserByOIDCIdentity(context.Background(), kindIssuer, sub, sub+"@clients", "svc")
			}()
			go func() {
				defer wg.Done()
				<-start
				human, _, humErr = st.UpsertHumanUserByOIDCIdentity(context.Background(), kindIssuer, sub, "p@x.com", "P")
			}()
			close(start)
			wg.Wait()

			if humErr != nil {
				t.Fatalf("%s: callback failed: %v", sub, humErr)
			}
			if machErr != nil && !errors.Is(machErr, ErrSubjectIsHuman) {
				t.Fatalf("%s: bearer failed unexpectedly: %v", sub, machErr)
			}
			var n int
			if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE oidc_issuer = $1 AND oidc_sub = $2`, kindIssuer, sub).Scan(&n); err != nil || n != 1 {
				t.Fatalf("%s: %d rows (%v), want 1", sub, n, err)
			}
			kind, admin := userKind(t, st, ctx, human.ID)
			if kind != domain.UserKindHuman || admin {
				t.Fatalf("%s: final kind=%q admin=%v", sub, kind, admin)
			}
			if c := membershipCount(t, st, ctx, human.ID); c != 0 {
				t.Fatalf("%s: %d grants survived", sub, c)
			}
			if machErr == nil && machine.ID != human.ID {
				t.Fatalf("%s: bearer got user %s, callback %s", sub, machine.ID, human.ID)
			}
			// No row beforehand and the bearer succeeded: the machine was inserted first, so the
			// callback flipped it — exactly once, with exactly one instance audit row.
			if !preexisting && machErr == nil {
				var flips int
				if err := st.pool.QueryRow(ctx,
					`SELECT count(*) FROM audit_logs WHERE actor_user_id = $1 AND action = 'user.kind_human'`,
					human.ID).Scan(&flips); err != nil {
					t.Fatalf("%s: audit: %v", sub, err)
				}
				if flips != 1 {
					t.Fatalf("%s: %d user.kind_human audit rows, want exactly 1", sub, flips)
				}
			}
		}
	}
}

// External review of iter-0209, P2-1: a machine token's email is not verified, so adding a member
// by email must prefer the human who owns that address over an older machine row carrying it.
func TestGetUserByEmailPrefersHuman(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	m, err := st.UpsertMachineUserByOIDCIdentity(ctx, kindIssuer, "claims-dup", "dup@x.com", "svc")
	if err != nil {
		t.Fatalf("machine: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE users SET created_at = now() - interval '1 day' WHERE id = $1`, m.ID); err != nil {
		t.Fatalf("age the machine row: %v", err)
	}
	// Only the machine carries the address: it is what the lookup finds.
	got, err := st.GetUserByEmail(ctx, "dup@x.com")
	if err != nil || got.ID != m.ID {
		t.Fatalf("machine only: got %+v (%v), want the machine %s", got, err, m.ID)
	}
	h, _, err := st.UpsertHumanUserByOIDCIdentity(ctx, kindIssuer, "person-dup", "dup@x.com", "Person")
	if err != nil {
		t.Fatalf("human: %v", err)
	}
	got, err = st.GetUserByEmail(ctx, "dup@x.com")
	if err != nil || got.ID != h.ID || got.Kind != domain.UserKindHuman {
		t.Fatalf("older machine and a human share the email: got %+v (%v), want the human %s", got, err, h.ID)
	}
}

func TestMemberListsCarryKind(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	org, _ := st.CreateOrganization(ctx, "list-org", "List")
	proj, _ := st.CreateProject(ctx, org.ID, "list-proj", "List")
	m, err := st.UpsertMachineUserByOIDCIdentity(ctx, kindIssuer, "list-svc", "list-svc@clients", "svc")
	if err != nil {
		t.Fatalf("machine: %v", err)
	}
	h, _, err := st.UpsertHumanUserByOIDCIdentity(ctx, kindIssuer, "list-h", "h@x.com", "H")
	if err != nil {
		t.Fatalf("human: %v", err)
	}
	for _, mem := range []domain.Membership{
		{UserID: m.ID, OrgID: org.ID, Role: domain.RoleOrgAdmin},
		{UserID: h.ID, OrgID: org.ID, Role: domain.RoleOrgAdmin},
		{UserID: m.ID, OrgID: org.ID, ProjectID: proj.ID, Role: domain.RoleViewer},
		{UserID: h.ID, OrgID: org.ID, ProjectID: proj.ID, Role: domain.RoleViewer},
	} {
		if _, err := st.CreateMembership(ctx, mem); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}
	wantByEmail := map[string]string{"list-svc@clients": domain.UserKindMachine, "h@x.com": domain.UserKindHuman}

	orgMembers, err := st.ListOrgMembers(ctx, org.ID)
	if err != nil || len(orgMembers) != 4 {
		t.Fatalf("org members: %d (%v)", len(orgMembers), err)
	}
	for _, mm := range orgMembers {
		if mm.Kind != wantByEmail[mm.Email] {
			t.Errorf("org list %s: kind %q", mm.Email, mm.Kind)
		}
	}
	projMembers, err := st.ListProjectMembers(ctx, proj.ID)
	if err != nil || len(projMembers) != 2 {
		t.Fatalf("project members: %d (%v)", len(projMembers), err)
	}
	for _, mm := range projMembers {
		if mm.Kind != wantByEmail[mm.Email] {
			t.Errorf("project list %s: kind %q", mm.Email, mm.Kind)
		}
	}
	inherited, err := st.ListProjectInheritedMembers(ctx, proj.ID)
	if err != nil || len(inherited) != 2 {
		t.Fatalf("inherited: %d (%v)", len(inherited), err)
	}
	for _, im := range inherited {
		if im.Kind != wantByEmail[im.Email] {
			t.Errorf("inherited list %s: kind %q", im.Email, im.Kind)
		}
	}
	all, err := st.ListAllUsers(ctx, "")
	if err != nil {
		t.Fatalf("all users: %v", err)
	}
	for _, u := range all {
		if w, ok := wantByEmail[u.Email]; ok && u.Kind != w {
			t.Errorf("admin list %s: kind %q", u.Email, u.Kind)
		}
	}
}
