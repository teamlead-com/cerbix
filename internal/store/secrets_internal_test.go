package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

// secretsTestStore is outboxTestStore plus a fresh random cipher — the secret inventory is
// fail-closed and refuses to operate without one (spec func-secret-inventory §4.1).
func secretsTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	st, ctx := outboxTestStore(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.New(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	st.WithCipher(cipher)
	return st, ctx
}

// secretsFixture creates an org + project pair.
func secretsFixture(t *testing.T, st *Store, ctx context.Context, orgSlug, projSlug string) (orgID, projID string) {
	t.Helper()
	org, err := st.CreateOrganization(ctx, orgSlug, orgSlug)
	if err != nil {
		t.Fatalf("org %s: %v", orgSlug, err)
	}
	proj, err := st.CreateProject(ctx, org.ID, projSlug, projSlug)
	if err != nil {
		t.Fatalf("project %s: %v", projSlug, err)
	}
	return org.ID, proj.ID
}

// insertSecretRef seeds a monitor_secret_refs row the way a future monitor save would
// (iteration 2 wires that path; here the schema contract itself is under test).
func insertSecretRef(t *testing.T, st *Store, ctx context.Context, monitorID, projectID, settingKey, secretID string) {
	t.Helper()
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO monitor_secret_refs (monitor_id, project_id, setting_key, secret_id)
		 VALUES ($1, $2, $3, $4)`,
		monitorID, projectID, settingKey, secretID); err != nil {
		t.Fatalf("insert secret ref: %v", err)
	}
}

func TestProjectSecretCRUDRoundTrip(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projID := secretsFixture(t, st, ctx, "acme", "api")

	created, err := st.CreateProjectSecret(ctx, projID, "db-password", "s3cr3t-value")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" || created.Name != "db-password" || created.CreatedAt.IsZero() {
		t.Fatalf("create returned %+v", created)
	}

	list, err := st.ListProjectSecrets(ctx, projID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}
	got := list[0]
	if got.ID != created.ID || got.Name != "db-password" || got.RotatedAt != nil ||
		got.UsedByTotal != 0 || got.UsedByFileManaged != 0 {
		t.Fatalf("list[0] = %+v", got)
	}
	// The DTO has no value field by construction; belt-and-braces: nothing in it carries
	// the plaintext.
	if strings.Contains(fmt.Sprintf("%+v", got), "s3cr3t-value") {
		t.Fatalf("listing leaked the plaintext: %+v", got)
	}

	id, val, err := st.ResolveProjectSecret(ctx, projID, "db-password")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id != created.ID || string(val) != "s3cr3t-value" {
		t.Fatalf("resolve = (%q, %q)", id, val)
	}

	// Duplicate name in the same project.
	if _, err := st.CreateProjectSecret(ctx, projID, "db-password", "other"); !errors.Is(err, ErrSecretExists) {
		t.Fatalf("duplicate create err = %v, want ErrSecretExists", err)
	}

	// Validation: slug, emptiness, size.
	for _, bad := range []string{"", "Upper", "1leading", "has_underscore", "-dash", strings.Repeat("a", 64)} {
		if _, err := st.CreateProjectSecret(ctx, projID, bad, "v"); !errors.Is(err, ErrSecretNameInvalid) {
			t.Fatalf("name %q err = %v, want ErrSecretNameInvalid", bad, err)
		}
	}
	for _, bad := range []string{"", "   ", "\t\n"} {
		if _, err := st.CreateProjectSecret(ctx, projID, "ok-name", bad); !errors.Is(err, ErrSecretValueInvalid) {
			t.Fatalf("value %q err = %v, want ErrSecretValueInvalid", bad, err)
		}
	}
	if _, err := st.CreateProjectSecret(ctx, projID, "ok-name", strings.Repeat("x", 4097)); !errors.Is(err, ErrSecretValueInvalid) {
		t.Fatalf("oversize value err = %v, want ErrSecretValueInvalid", err)
	}
	// Exactly 4096 bytes is fine.
	if _, err := st.CreateProjectSecret(ctx, projID, "max-size", strings.Repeat("x", 4096)); err != nil {
		t.Fatalf("4096-byte value: %v", err)
	}
}

func TestProjectSecretQuota(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projID := secretsFixture(t, st, ctx, "acme", "api")

	for i := 0; i < 100; i++ {
		if _, err := st.CreateProjectSecret(ctx, projID, fmt.Sprintf("s-%03d", i), "v"); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, err := st.CreateProjectSecret(ctx, projID, "s-100", "v"); !errors.Is(err, ErrSecretQuota) {
		t.Fatalf("101st create err = %v, want ErrSecretQuota", err)
	}
	// The quota is per project: a sibling project is unaffected.
	var otherID string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO projects (org_id, slug, name) SELECT org_id, 'other', 'Other' FROM projects WHERE id = $1 RETURNING id`,
		projID).Scan(&otherID); err != nil {
		t.Fatalf("sibling project: %v", err)
	}
	if _, err := st.CreateProjectSecret(ctx, otherID, "s-000", "v"); err != nil {
		t.Fatalf("sibling create: %v", err)
	}
}

func TestProjectSecretsNilCipher(t *testing.T) {
	st, ctx := outboxTestStore(t) // no WithCipher
	_, projID := secretsFixture(t, st, ctx, "acme", "api")

	if _, err := st.CreateProjectSecret(ctx, projID, "db-password", "v"); !errors.Is(err, ErrSecretsUnavailable) {
		t.Fatalf("create err = %v, want ErrSecretsUnavailable", err)
	}
	nn := "new-name"
	if _, _, _, err := st.UpdateProjectSecret(ctx, projID, "db-password", &nn, nil); !errors.Is(err, ErrSecretsUnavailable) {
		t.Fatalf("update err = %v, want ErrSecretsUnavailable", err)
	}
	if _, _, err := st.ResolveProjectSecret(ctx, projID, "db-password"); !errors.Is(err, ErrSecretsUnavailable) {
		t.Fatalf("resolve err = %v, want ErrSecretsUnavailable", err)
	}
}

func TestProjectSecretEncryptedAtRestAndAADBinding(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projA := secretsFixture(t, st, ctx, "acme", "api")
	orgB, err := st.CreateOrganization(ctx, "globex", "Globex")
	if err != nil {
		t.Fatalf("org b: %v", err)
	}
	projBrow, err := st.CreateProject(ctx, orgB.ID, "web", "Web")
	if err != nil {
		t.Fatalf("project b: %v", err)
	}
	projB := projBrow.ID

	secA, err := st.CreateProjectSecret(ctx, projA, "shared", "plaintext-a")
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	secB, err := st.CreateProjectSecret(ctx, projB, "shared", "plaintext-b")
	if err != nil {
		t.Fatalf("create b: %v", err)
	}

	// Encrypted at rest: AAD-bound prefix, no plaintext in the column.
	var rawA, rawB string
	if err := st.pool.QueryRow(ctx, `SELECT value_encrypted FROM project_secrets WHERE id = $1`, secA.ID).Scan(&rawA); err != nil {
		t.Fatalf("raw a: %v", err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT value_encrypted FROM project_secrets WHERE id = $1`, secB.ID).Scan(&rawB); err != nil {
		t.Fatalf("raw b: %v", err)
	}
	for raw, plain := range map[string]string{rawA: "plaintext-a", rawB: "plaintext-b"} {
		if !strings.HasPrefix(raw, "enc:v2a:") {
			t.Fatalf("value_encrypted %q lacks the enc:v2a: prefix", raw)
		}
		if strings.Contains(raw, plain) {
			t.Fatalf("value_encrypted contains the plaintext: %q", raw)
		}
	}

	// Cross-project transplant: swap the two ciphertexts row-for-row. The AAD binds
	// (project_id, secret_id), so BOTH resolves must now fail authentication — project A
	// can never be made to dispatch project B's credential (spec §4.7/§9).
	if _, err := st.pool.Exec(ctx,
		`UPDATE project_secrets SET value_encrypted = $1 WHERE id = $2`, rawB, secA.ID); err != nil {
		t.Fatalf("transplant a: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE project_secrets SET value_encrypted = $1 WHERE id = $2`, rawA, secB.ID); err != nil {
		t.Fatalf("transplant b: %v", err)
	}
	if _, _, err := st.ResolveProjectSecret(ctx, projA, "shared"); err == nil {
		t.Fatal("resolve of transplanted ciphertext in project A succeeded, want auth failure")
	}
	if _, _, err := st.ResolveProjectSecret(ctx, projB, "shared"); err == nil {
		t.Fatal("resolve of transplanted ciphertext in project B succeeded, want auth failure")
	}
}

func TestProjectSecretDeleteAndRenameGuards(t *testing.T) {
	st, ctx := secretsTestStore(t)
	orgID, projID := secretsFixture(t, st, ctx, "acme", "api")

	sec, err := st.CreateProjectSecret(ctx, projID, "db-password", "v1")
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: projID, Name: "db", Type: domain.MonitorPostgres, Target: "db:5432",
		IntervalSeconds: 60, TimeoutSeconds: 5,
		Config: map[string]string{"username": "cerbix", "database": "app", "password_ref": "db-password"},
	})
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	insertSecretRef(t, st, ctx, mon.ID, projID, "password_ref", sec.ID)

	// Delete guard: referenced → typed error with the exact under-lock count, no delete.
	err = st.DeleteProjectSecret(ctx, projID, "db-password")
	var inUse SecretInUseError
	if !errors.As(err, &inUse) || inUse.Count != 1 {
		t.Fatalf("delete err = %v, want SecretInUseError{Count: 1}", err)
	}
	list, err := st.ListProjectSecrets(ctx, projID)
	if err != nil || len(list) != 1 || list[0].UsedByTotal != 1 || list[0].UsedByFileManaged != 0 {
		t.Fatalf("post-guard list = %+v (err %v)", list, err)
	}

	// Rename guard: file-managed ref (managed_monitors row) blocks the rename.
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO managed_monitors (monitor_id, provider_id, org_id, project_id, source_uid)
		 VALUES ($1, 'platform', $2, $3, 'db')`,
		mon.ID, orgID, projID); err != nil {
		t.Fatalf("mark file-managed: %v", err)
	}
	newName := "db-pass-renamed"
	_, _, _, err = st.UpdateProjectSecret(ctx, projID, "db-password", &newName, nil)
	var renamedInUse SecretRenamedInUseError
	if !errors.As(err, &renamedInUse) || renamedInUse.Count != 1 {
		t.Fatalf("rename err = %v, want SecretRenamedInUseError{Count: 1}", err)
	}
	if list, _ := st.ListProjectSecrets(ctx, projID); list[0].UsedByFileManaged != 1 {
		t.Fatalf("file-managed count = %d, want 1", list[0].UsedByFileManaged)
	}

	// UI-managed again (drop the ownership row): rename succeeds, the monitor's config ref
	// NAME is re-pointed, and execution semantics stay untouched.
	if _, err := st.pool.Exec(ctx, `DELETE FROM managed_monitors WHERE monitor_id = $1`, mon.ID); err != nil {
		t.Fatalf("unmark file-managed: %v", err)
	}
	var revBefore int64
	var updatedBefore time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT execution_revision, updated_at FROM monitors WHERE id = $1`, mon.ID).Scan(&revBefore, &updatedBefore); err != nil {
		t.Fatalf("pre-rename monitor row: %v", err)
	}
	renamed, rotated, repointed, err := st.UpdateProjectSecret(ctx, projID, "db-password", &newName, nil)
	if err != nil || !renamed || rotated || repointed != 1 {
		t.Fatalf("rename = (%v, %v, %d, %v), want (true, false, 1, nil)", renamed, rotated, repointed, err)
	}
	got, err := st.GetMonitor(ctx, mon.ID)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	if got.Config["password_ref"] != "db-pass-renamed" {
		t.Fatalf("password_ref = %q, want re-pointed to db-pass-renamed", got.Config["password_ref"])
	}
	var revAfter int64
	var updatedAfter time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT execution_revision, updated_at FROM monitors WHERE id = $1`, mon.ID).Scan(&revAfter, &updatedAfter); err != nil {
		t.Fatalf("post-rename monitor row: %v", err)
	}
	if revAfter != revBefore || !updatedAfter.Equal(updatedBefore) {
		t.Fatalf("re-point bumped execution semantics: rev %d→%d, updated_at %v→%v",
			revBefore, revAfter, updatedBefore, updatedAfter)
	}
	// Rename to a taken name → duplicate.
	if _, err := st.CreateProjectSecret(ctx, projID, "taken", "v"); err != nil {
		t.Fatalf("create taken: %v", err)
	}
	taken := "taken"
	if _, _, _, err := st.UpdateProjectSecret(ctx, projID, "db-pass-renamed", &taken, nil); !errors.Is(err, ErrSecretExists) {
		t.Fatalf("rename-to-taken err = %v, want ErrSecretExists", err)
	}

	// Drop the ref → delete succeeds.
	if _, err := st.pool.Exec(ctx, `DELETE FROM monitor_secret_refs WHERE monitor_id = $1`, mon.ID); err != nil {
		t.Fatalf("drop ref: %v", err)
	}
	if err := st.DeleteProjectSecret(ctx, projID, "db-pass-renamed"); err != nil {
		t.Fatalf("delete after unref: %v", err)
	}
	if _, _, err := st.ResolveProjectSecret(ctx, projID, "db-pass-renamed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve deleted err = %v, want ErrNotFound", err)
	}
}

func TestProjectSecretRotate(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projID := secretsFixture(t, st, ctx, "acme", "api")

	if _, err := st.CreateProjectSecret(ctx, projID, "api-key", "old-value"); err != nil {
		t.Fatalf("create: %v", err)
	}
	newVal := "new-value"
	renamed, rotated, repointed, err := st.UpdateProjectSecret(ctx, projID, "api-key", nil, &newVal)
	if err != nil || renamed || !rotated || repointed != 0 {
		t.Fatalf("rotate = (%v, %v, %d, %v), want (false, true, 0, nil)", renamed, rotated, repointed, err)
	}
	list, err := st.ListProjectSecrets(ctx, projID)
	if err != nil || len(list) != 1 || list[0].RotatedAt == nil {
		t.Fatalf("post-rotate list = %+v (err %v)", list, err)
	}
	if _, val, err := st.ResolveProjectSecret(ctx, projID, "api-key"); err != nil || string(val) != "new-value" {
		t.Fatalf("resolve = (%q, %v), want new-value", val, err)
	}
	var raw string
	if err := st.pool.QueryRow(ctx,
		`SELECT value_encrypted FROM project_secrets WHERE project_id = $1 AND name = 'api-key'`, projID).Scan(&raw); err != nil {
		t.Fatalf("raw: %v", err)
	}
	if strings.Contains(raw, "old-value") || strings.Contains(raw, "new-value") {
		t.Fatalf("rotated ciphertext leaks a plaintext: %q", raw)
	}

	// Rename + rotate in one call.
	nn := "api-key-v2"
	nv := "third-value"
	renamed, rotated, _, err = st.UpdateProjectSecret(ctx, projID, "api-key", &nn, &nv)
	if err != nil || !renamed || !rotated {
		t.Fatalf("rename+rotate = (%v, %v, %v), want (true, true, nil)", renamed, rotated, err)
	}
	if _, val, err := st.ResolveProjectSecret(ctx, projID, "api-key-v2"); err != nil || string(val) != "third-value" {
		t.Fatalf("resolve renamed = (%q, %v)", val, err)
	}
	if _, _, err := st.ResolveProjectSecret(ctx, projID, "api-key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve old name err = %v, want ErrNotFound", err)
	}
}

func TestProjectSecretTenantIsolation(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projA := secretsFixture(t, st, ctx, "acme", "api")
	_, projB := secretsFixture(t, st, ctx, "globex", "web")

	if _, err := st.CreateProjectSecret(ctx, projA, "db-password", "v"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := st.ResolveProjectSecret(ctx, projB, "db-password"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant resolve err = %v, want ErrNotFound", err)
	}
	if err := st.DeleteProjectSecret(ctx, projB, "db-password"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant delete err = %v, want ErrNotFound", err)
	}
	nn := "stolen"
	if _, _, _, err := st.UpdateProjectSecret(ctx, projB, "db-password", &nn, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant update err = %v, want ErrNotFound", err)
	}
	list, err := st.ListProjectSecrets(ctx, projB)
	if err != nil || len(list) != 0 {
		t.Fatalf("cross-tenant list = %+v (err %v), want empty", list, err)
	}
	// A's secret is untouched.
	if _, val, err := st.ResolveProjectSecret(ctx, projA, "db-password"); err != nil || string(val) != "v" {
		t.Fatalf("owner resolve = (%q, %v)", val, err)
	}
}

func TestProjectSecretProjectDeleteCascade(t *testing.T) {
	st, ctx := secretsTestStore(t)
	orgID, projID := secretsFixture(t, st, ctx, "acme", "api")

	sec, err := st.CreateProjectSecret(ctx, projID, "db-password", "v")
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: projID, Name: "db", Type: domain.MonitorPostgres, Target: "db:5432",
		IntervalSeconds: 60, TimeoutSeconds: 5,
		Config: map[string]string{"username": "cerbix", "database": "app", "password_ref": "db-password"},
	})
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	insertSecretRef(t, st, ctx, mon.ID, projID, "password_ref", sec.ID)

	// The deferred NO ACTION FK keeps the cascade order-independent: secrets and
	// monitors (→ refs) both vanish inside the delete tx, and the commit-time check
	// passes because both sides are gone (spec §4.3).
	if err := st.DeleteProject(ctx, orgID, projID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	for _, q := range []string{
		`SELECT count(*) FROM project_secrets WHERE project_id = $1`,
		`SELECT count(*) FROM monitor_secret_refs WHERE project_id = $1`,
		`SELECT count(*) FROM monitors WHERE project_id = $1`,
	} {
		var n int
		if err := st.pool.QueryRow(ctx, q, projID).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n != 0 {
			t.Fatalf("%s = %d rows after project delete, want 0", q, n)
		}
	}
}
