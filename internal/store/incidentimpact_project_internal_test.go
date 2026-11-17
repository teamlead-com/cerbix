package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-042 §4.4 — the project default impact: read on every project read, written under a lock with
// its audit row in the same transaction.

func projectAuditRows(t *testing.T, st *Store, ctx context.Context, orgID string) []string {
	t.Helper()
	rows, err := st.pool.Query(ctx,
		`SELECT target FROM audit_logs WHERE org_id = $1 AND action = $2 ORDER BY created_at`,
		orgID, ProjectAuditDefaultIncidentImpact)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestProjectReadsCarryTheDefault(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	orgID, projID := seedTenant(t, st, ctx)
	user, err := st.CreateLocalUser(ctx, "reader@example.com", "Reader", "x", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO memberships (user_id, org_id, project_id, role) VALUES ($1, $2, NULL, 'org_admin')`,
		user.ID, orgID); err != nil {
		t.Fatalf("membership: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE projects SET default_incident_impact = 'minor' WHERE id = $1`, projID); err != nil {
		t.Fatal(err)
	}
	p2, err := st.CreateProject(ctx, orgID, "second", "Second")
	if err != nil {
		t.Fatal(err)
	}
	if p2.DefaultIncidentImpact != domain.ImpactMajor {
		t.Fatalf("a new project starts at %q, want major", p2.DefaultIncidentImpact)
	}
	got, err := st.GetProject(ctx, projID)
	if err != nil || got.DefaultIncidentImpact != domain.ImpactMinor {
		t.Fatalf("GetProject = %q, %v; want minor", got.DefaultIncidentImpact, err)
	}
	byOrg, err := st.ListProjectsByOrg(ctx, orgID)
	if err != nil || len(byOrg) != 2 {
		t.Fatalf("ListProjectsByOrg = %d, %v", len(byOrg), err)
	}
	forUser, err := st.ListProjectsForUser(ctx, user.ID)
	if err != nil || len(forUser) != 2 {
		t.Fatalf("ListProjectsForUser = %d, %v", len(forUser), err)
	}
	for name, list := range map[string][]domain.Project{"org": byOrg, "user": forUser} {
		for _, p := range list {
			want := domain.ImpactMajor
			if p.ID == projID {
				want = domain.ImpactMinor
			}
			if p.DefaultIncidentImpact != want {
				t.Errorf("%s list: %s = %q, want %q", name, p.Slug, p.DefaultIncidentImpact, want)
			}
		}
	}
}

func TestSettingTheProjectDefaultAuditsInItsTransaction(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	orgID, projID := seedTenant(t, st, ctx)
	user, err := st.CreateLocalUser(ctx, "ops@example.com", "Ops", "x", false)
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.GetProject(ctx, projID)
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.SetProjectDefaultIncidentImpact(ctx, projID, domain.ImpactCritical,
		AuditActor{ActorUserID: user.ID, Label: "ops@example.com"})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if got.DefaultIncidentImpact != domain.ImpactCritical {
		t.Fatalf("returned %q, want critical", got.DefaultIncidentImpact)
	}
	if !got.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("updated_at did not move: %v -> %v", before.UpdatedAt, got.UpdatedAt)
	}
	if _, err := st.SetProjectDefaultIncidentImpact(ctx, projID, domain.ImpactMinor,
		AuditActor{ViaToken: true, Label: "token:ops"}); err != nil {
		t.Fatalf("token set: %v", err)
	}
	rows := projectAuditRows(t, st, ctx, orgID)
	want := []string{"payments major→critical", "payments critical→minor · actor=token:ops"}
	if len(rows) != 2 || rows[0] != want[0] || rows[1] != want[1] {
		t.Fatalf("audit targets = %q, want %q", rows, want)
	}
}

func TestAnUnchangedProjectDefaultWritesNothing(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	orgID, projID := seedTenant(t, st, ctx)
	before, err := st.GetProject(ctx, projID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	got, err := st.SetProjectDefaultIncidentImpact(ctx, projID, domain.ImpactMajor,
		AuditActor{ViaToken: true, Label: "token:ops"})
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultIncidentImpact != domain.ImpactMajor || !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("no-op changed the project: %+v", got)
	}
	after, err := st.GetProject(ctx, projID)
	if err != nil {
		t.Fatalf("get project after: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Error("updated_at moved on a no-op")
	}
	if rows := projectAuditRows(t, st, ctx, orgID); len(rows) != 0 {
		t.Fatalf("a no-op wrote audit rows: %q", rows)
	}
}

func TestAnInvalidProjectDefaultIsRefusedBeforeAnyWrite(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	orgID, projID := seedTenant(t, st, ctx)
	for _, bad := range []domain.IncidentImpact{"severe", ""} {
		if _, err := st.SetProjectDefaultIncidentImpact(ctx, projID, bad,
			AuditActor{ViaToken: true, Label: "token:ops"}); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
	got, _ := st.GetProject(ctx, projID)
	if got.DefaultIncidentImpact != domain.ImpactMajor {
		t.Fatalf("stored %q after refusals", got.DefaultIncidentImpact)
	}
	if rows := projectAuditRows(t, st, ctx, orgID); len(rows) != 0 {
		t.Fatalf("audit rows after refusals: %q", rows)
	}
}

func TestAProjectWithFileManagedObjectsTakesTheDefault(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	orgID, projID := seedTenant(t, st, ctx)
	applyServiceBundle(t, st, ctx, svcBundle)
	if err := st.DeleteProject(ctx, orgID, projID); !errors.Is(err, ErrManagedByFile) {
		t.Fatalf("DeleteProject = %v, want ErrManagedByFile (the applied bundle did not make the project file-managed)", err)
	}
	got, err := st.SetProjectDefaultIncidentImpact(ctx, projID, domain.ImpactNone,
		AuditActor{ViaToken: true, Label: "token:ops"})
	if err != nil {
		t.Fatalf("set on a file-managed project: %v", err)
	}
	if got.DefaultIncidentImpact != domain.ImpactNone {
		t.Fatalf("returned %q", got.DefaultIncidentImpact)
	}
}

func TestAZeroActorCannotSetTheProjectDefault(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	orgID, projID := seedTenant(t, st, ctx)
	if _, err := st.SetProjectDefaultIncidentImpact(ctx, projID, domain.ImpactCritical, AuditActor{}); err == nil {
		t.Fatal("a zero actor was accepted")
	}
	got, _ := st.GetProject(ctx, projID)
	if got.DefaultIncidentImpact != domain.ImpactMajor {
		t.Fatalf("stored %q: the write survived its failed audit", got.DefaultIncidentImpact)
	}
	if rows := projectAuditRows(t, st, ctx, orgID); len(rows) != 0 {
		t.Fatalf("audit rows: %q", rows)
	}
}

// The write reads the stored value under FOR UPDATE. A second connection holds the row, changes the
// value and commits while the set waits; the set must then see the COMMITTED value as its old side.
// Without the lock the set reads `major` before the blocker commits and audits `major→critical`.
func TestTheProjectDefaultWriteLocksTheRowBeforeReadingIt(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	orgID, projID := seedTenant(t, st, ctx)

	blocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	var blockerPID int
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM projects WHERE id = $1 FOR UPDATE`, projID); err != nil {
		t.Fatalf("blocker lock: %v", err)
	}

	type result struct {
		p   domain.Project
		err error
	}
	done := make(chan result, 1)
	go func() {
		p, serr := st.SetProjectDefaultIncidentImpact(ctx, projID, domain.ImpactCritical,
			AuditActor{ViaToken: true, Label: "token:ops"})
		done <- result{p, serr}
	}()

	// Some backend must be genuinely blocked on the blocker's lock; a sleep would prove nothing.
	deadline := time.Now().Add(10 * time.Second)
	for blocked := false; !blocked; {
		if time.Now().After(deadline) {
			t.Fatal("the set never blocked on the row lock: it did not lock the row before reading it")
		}
		if err := st.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid)))`,
			blockerPID).Scan(&blocked); err != nil {
			t.Fatalf("poll blocking pids: %v", err)
		}
		if !blocked {
			select {
			case r := <-done:
				t.Fatalf("the set finished while the row was locked: %+v, %v", r.p, r.err)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}

	if _, err := blocker.Exec(ctx, `UPDATE projects SET default_incident_impact = 'minor' WHERE id = $1`, projID); err != nil {
		t.Fatalf("blocker update: %v", err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("commit blocker: %v", err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("set: %v", r.err)
	}
	if r.p.DefaultIncidentImpact != domain.ImpactCritical {
		t.Fatalf("returned %q, want critical", r.p.DefaultIncidentImpact)
	}
	rows := projectAuditRows(t, st, ctx, orgID)
	want := "payments minor→critical · actor=token:ops"
	if len(rows) != 1 || rows[0] != want {
		t.Fatalf("audit targets = %q, want [%q]", rows, want)
	}
}
