package store

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

// testSecretActor is the machine actor used by these tests: audit_logs.actor_user_id is a
// soft FK to users, so an empty ActorUserID (→ NULL) needs no user fixture.
var testSecretActor = SecretActor{}

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

// createRefMonitor creates a credentialed monitor whose config references refName.
func createRefMonitor(t *testing.T, st *Store, ctx context.Context, projID, name, refName string, enabled bool) domain.Monitor {
	t.Helper()
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: projID, Name: name, Type: domain.MonitorPostgres, Target: "db:5432",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: enabled,
		Config: map[string]string{"username": "cerbix", "database": "app", "password_ref": refName},
	})
	if err != nil {
		t.Fatalf("create monitor %s: %v", name, err)
	}
	return mon
}

// monitorFenceState reads the two D-0142 fence columns of a monitor.
func monitorFenceState(t *testing.T, st *Store, ctx context.Context, monitorID string) (rev int64, lastResult *time.Time) {
	t.Helper()
	if err := st.pool.QueryRow(ctx,
		`SELECT execution_revision, last_result_ts FROM monitors WHERE id = $1`, monitorID).Scan(&rev, &lastResult); err != nil {
		t.Fatalf("fence state of %s: %v", monitorID, err)
	}
	return rev, lastResult
}

// countSecretAudit counts audit_logs rows for an action, and returns the newest target.
func countSecretAudit(t *testing.T, st *Store, ctx context.Context, orgID, action string) (int, string) {
	t.Helper()
	var n int
	var target string
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(max(target), '') FROM audit_logs WHERE org_id = $1 AND action = $2`,
		orgID, action).Scan(&n, &target); err != nil {
		t.Fatalf("count audit %s: %v", action, err)
	}
	return n, target
}

func TestProjectSecretCRUDRoundTrip(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projID := secretsFixture(t, st, ctx, "acme", "api")

	created, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "db-password", "s3cr3t-value")
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

	id, val, err := st.resolveProjectSecret(ctx, projID, "db-password")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id != created.ID || string(val) != "s3cr3t-value" {
		t.Fatalf("resolve = (%q, %q)", id, val)
	}

	// Duplicate name in the same project.
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "db-password", "other"); !errors.Is(err, ErrSecretExists) {
		t.Fatalf("duplicate create err = %v, want ErrSecretExists", err)
	}

	// Validation: slug (owned by the domain), emptiness, encoding, size.
	for _, bad := range []string{"", "Upper", "1leading", "has_underscore", "-dash", strings.Repeat("a", 64)} {
		if _, err := st.CreateProjectSecret(ctx, testSecretActor, projID, bad, "v"); !errors.Is(err, ErrSecretNameInvalid) {
			t.Fatalf("name %q err = %v, want ErrSecretNameInvalid", bad, err)
		}
	}
	// Only true emptiness is invalid — a whitespace-only value is the owner's business
	// (spec §5 bounds bytes, not content).
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "ok-name", ""); !errors.Is(err, ErrSecretValueInvalid) {
		t.Fatalf("empty value err = %v, want ErrSecretValueInvalid", err)
	}
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "ws-only", "   \t\n"); err != nil {
		t.Fatalf("whitespace-only value err = %v, want accepted", err)
	}
	if _, val, err := st.resolveProjectSecret(ctx, projID, "ws-only"); err != nil || string(val) != "   \t\n" {
		t.Fatalf("whitespace-only round trip = (%q, %v)", val, err)
	}
	// Invalid UTF-8 is rejected (spec §5: UTF-8 bytes).
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "bad-utf8", "ok\xff\xfe"); !errors.Is(err, ErrSecretValueInvalid) {
		t.Fatalf("invalid-utf8 value err = %v, want ErrSecretValueInvalid", err)
	}
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "ok-name", strings.Repeat("x", 4097)); !errors.Is(err, ErrSecretValueInvalid) {
		t.Fatalf("oversize value err = %v, want ErrSecretValueInvalid", err)
	}
	// Exactly 4096 bytes is fine.
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "max-size", strings.Repeat("x", 4096)); err != nil {
		t.Fatalf("4096-byte value: %v", err)
	}
}

func TestProjectSecretQuota(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projID := secretsFixture(t, st, ctx, "acme", "api")

	for i := 0; i < 100; i++ {
		if _, err := st.CreateProjectSecret(ctx, testSecretActor, projID, fmt.Sprintf("s-%03d", i), "v"); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "s-100", "v"); !errors.Is(err, ErrSecretQuota) {
		t.Fatalf("101st create err = %v, want ErrSecretQuota", err)
	}
	// The quota is per project: a sibling project is unaffected.
	var otherID string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO projects (org_id, slug, name) SELECT org_id, 'other', 'Other' FROM projects WHERE id = $1 RETURNING id`,
		projID).Scan(&otherID); err != nil {
		t.Fatalf("sibling project: %v", err)
	}
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, otherID, "s-000", "v"); err != nil {
		t.Fatalf("sibling create: %v", err)
	}
}

// TestProjectSecretQuotaConcurrentSpellings drives the quota boundary with two REAL
// concurrent creates that spell the same project uuid differently. The tx canonicalizes
// the id before taking the advisory lock, so both spellings serialize on the SAME lock
// and exactly one create passes the boundary (P0-2b).
func TestProjectSecretQuotaConcurrentSpellings(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projID := secretsFixture(t, st, ctx, "acme", "api")

	for i := 0; i < 99; i++ {
		if _, err := st.CreateProjectSecret(ctx, testSecretActor, projID, fmt.Sprintf("s-%03d", i), "v"); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	spellings := []string{projID, strings.ToUpper(projID)}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = st.CreateProjectSecret(ctx, testSecretActor, spellings[i], fmt.Sprintf("race-%d", i), "v")
		}(i)
	}
	wg.Wait()

	quotaHits := 0
	for i, err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, ErrSecretQuota):
			quotaHits++
		default:
			t.Fatalf("racer %d unexpected err: %v", i, err)
		}
	}
	if quotaHits != 1 {
		t.Fatalf("quota hits = %d, want exactly 1 (one racer past the boundary)", quotaHits)
	}
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM project_secrets WHERE project_id = $1`, projID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 100 {
		t.Fatalf("row count = %d, want exactly 100 (quota held under concurrency)", n)
	}
}

// TestProjectSecretCanonicalProjectSpellings proves the AAD and lock key bind the
// CANONICAL project uuid, not the caller's spelling (P0-2a): a create through an
// uppercase spelling resolves through the canonical one, and a rotate through a braced
// spelling is visible to every other spelling.
func TestProjectSecretCanonicalProjectSpellings(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projID := secretsFixture(t, st, ctx, "acme", "api")

	upper := strings.ToUpper(projID)
	braced := "{" + projID + "}"

	if _, err := st.CreateProjectSecret(ctx, testSecretActor, upper, "db-password", "v1"); err != nil {
		t.Fatalf("create via uppercase spelling: %v", err)
	}
	// AAD stability: the canonical spelling decrypts what the uppercase spelling wrote.
	if _, val, err := st.resolveProjectSecret(ctx, projID, "db-password"); err != nil || string(val) != "v1" {
		t.Fatalf("canonical resolve = (%q, %v), want v1", val, err)
	}
	// Rotate via a third spelling; the canonical read sees the new value.
	nv := "v2"
	if _, rotated, _, err := st.UpdateProjectSecret(ctx, testSecretActor, braced, "db-password", nil, &nv); err != nil || !rotated {
		t.Fatalf("rotate via braced spelling = (rotated=%t, %v)", rotated, err)
	}
	if _, val, err := st.resolveProjectSecret(ctx, projID, "db-password"); err != nil || string(val) != "v2" {
		t.Fatalf("post-rotate canonical resolve = (%q, %v), want v2", val, err)
	}
	// A spelling that is not a uuid at all is a clean not-found, not a SQL error.
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, "not-a-uuid", "x-name", "v"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-uuid project err = %v, want ErrNotFound", err)
	}
}

func TestProjectSecretsNilCipher(t *testing.T) {
	st, ctx := outboxTestStore(t) // no WithCipher
	_, projID := secretsFixture(t, st, ctx, "acme", "api")

	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "db-password", "v"); !errors.Is(err, ErrSecretsUnavailable) {
		t.Fatalf("create err = %v, want ErrSecretsUnavailable", err)
	}
	nn := "new-name"
	if _, _, _, err := st.UpdateProjectSecret(ctx, testSecretActor, projID, "db-password", &nn, nil); !errors.Is(err, ErrSecretsUnavailable) {
		t.Fatalf("update err = %v, want ErrSecretsUnavailable", err)
	}
	if _, _, err := st.resolveProjectSecret(ctx, projID, "db-password"); !errors.Is(err, ErrSecretsUnavailable) {
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

	secA, err := st.CreateProjectSecret(ctx, testSecretActor, projA, "shared", "plaintext-a")
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	secB, err := st.CreateProjectSecret(ctx, testSecretActor, projB, "shared", "plaintext-b")
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
	if _, _, err := st.resolveProjectSecret(ctx, projA, "shared"); err == nil {
		t.Fatal("resolve of transplanted ciphertext in project A succeeded, want auth failure")
	}
	if _, _, err := st.resolveProjectSecret(ctx, projB, "shared"); err == nil {
		t.Fatal("resolve of transplanted ciphertext in project B succeeded, want auth failure")
	}
}

func TestProjectSecretDeleteAndRenameGuards(t *testing.T) {
	st, ctx := secretsTestStore(t)
	orgID, projID := secretsFixture(t, st, ctx, "acme", "api")

	sec, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "db-password", "v1")
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	mon := createRefMonitor(t, st, ctx, projID, "db", "db-password", true)
	insertSecretRef(t, st, ctx, mon.ID, projID, "password_ref", sec.ID)

	// Delete guard: referenced → typed error with the exact under-lock count, no delete.
	err = st.DeleteProjectSecret(ctx, testSecretActor, projID, "db-password")
	var inUse SecretInUseError
	if !errors.As(err, &inUse) || inUse.Count != 1 {
		t.Fatalf("delete err = %v, want SecretInUseError{Count: 1}", err)
	}
	list, err := st.ListProjectSecrets(ctx, projID)
	if err != nil || len(list) != 1 || list[0].UsedByTotal != 1 || list[0].UsedByFileManaged != 0 {
		t.Fatalf("post-guard list = %+v (err %v)", list, err)
	}
	// A refused delete leaves no audit row (audit commits with the mutation, P0-3).
	if n, _ := countSecretAudit(t, st, ctx, orgID, "secret.delete"); n != 0 {
		t.Fatalf("refused delete wrote %d audit rows, want 0", n)
	}

	// Rename guard: file-managed ref (managed_monitors row) blocks the rename.
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO managed_monitors (monitor_id, provider_id, org_id, project_id, source_uid)
		 VALUES ($1, 'platform', $2, $3, 'db')`,
		mon.ID, orgID, projID); err != nil {
		t.Fatalf("mark file-managed: %v", err)
	}
	newName := "db-pass-renamed"
	_, _, _, err = st.UpdateProjectSecret(ctx, testSecretActor, projID, "db-password", &newName, nil)
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
	renamed, rotated, repointed, err := st.UpdateProjectSecret(ctx, testSecretActor, projID, "db-password", &newName, nil)
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
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "taken", "v"); err != nil {
		t.Fatalf("create taken: %v", err)
	}
	taken := "taken"
	if _, _, _, err := st.UpdateProjectSecret(ctx, testSecretActor, projID, "db-pass-renamed", &taken, nil); !errors.Is(err, ErrSecretExists) {
		t.Fatalf("rename-to-taken err = %v, want ErrSecretExists", err)
	}

	// Drop the ref → delete succeeds.
	if _, err := st.pool.Exec(ctx, `DELETE FROM monitor_secret_refs WHERE monitor_id = $1`, mon.ID); err != nil {
		t.Fatalf("drop ref: %v", err)
	}
	if err := st.DeleteProjectSecret(ctx, testSecretActor, projID, "db-pass-renamed"); err != nil {
		t.Fatalf("delete after unref: %v", err)
	}
	if _, _, err := st.resolveProjectSecret(ctx, projID, "db-pass-renamed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve deleted err = %v, want ErrNotFound", err)
	}
}

// TestProjectSecretRepointBrokenInvariant corrupts the persisted `*_ref` name so it no
// longer matches the ref row, then attempts a rename: the whole transaction must fail and
// roll back — the §4.3 ref/config invariant is never papered over by skipping (P1-1).
func TestProjectSecretRepointBrokenInvariant(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projID := secretsFixture(t, st, ctx, "acme", "api")

	sec, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "db-password", "v1")
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	mon := createRefMonitor(t, st, ctx, projID, "db", "db-password", true)
	insertSecretRef(t, st, ctx, mon.ID, projID, "password_ref", sec.ID)

	// Corrupt the config value behind the ref row's back.
	var raw []byte
	if err := st.pool.QueryRow(ctx, `SELECT config FROM monitors WHERE id = $1`, mon.ID).Scan(&raw); err != nil {
		t.Fatalf("read config: %v", err)
	}
	cfg := map[string]string{}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	cfg["password_ref"] = "drifted-elsewhere"
	corrupted, _ := json.Marshal(cfg)
	if _, err := st.pool.Exec(ctx, `UPDATE monitors SET config = $1 WHERE id = $2`, corrupted, mon.ID); err != nil {
		t.Fatalf("corrupt config: %v", err)
	}

	newName := "db-pass-renamed"
	if _, _, _, err := st.UpdateProjectSecret(ctx, testSecretActor, projID, "db-password", &newName, nil); err == nil {
		t.Fatal("rename over a broken ref/config invariant succeeded, want error")
	} else if !strings.Contains(err.Error(), "invariant") {
		t.Fatalf("rename err = %v, want the broken-invariant error", err)
	}
	// Rolled back: secret name unchanged, monitor config unchanged.
	if _, _, err := st.resolveProjectSecret(ctx, projID, "db-password"); err != nil {
		t.Fatalf("secret renamed despite rollback: %v", err)
	}
	got, err := st.GetMonitor(ctx, mon.ID)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	if got.Config["password_ref"] != "drifted-elsewhere" {
		t.Fatalf("monitor config changed despite rollback: %q", got.Config["password_ref"])
	}
}

func TestProjectSecretRotate(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projID := secretsFixture(t, st, ctx, "acme", "api")

	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "api-key", "old-value"); err != nil {
		t.Fatalf("create: %v", err)
	}
	newVal := "new-value"
	renamed, rotated, repointed, err := st.UpdateProjectSecret(ctx, testSecretActor, projID, "api-key", nil, &newVal)
	if err != nil || renamed || !rotated || repointed != 0 {
		t.Fatalf("rotate = (%v, %v, %d, %v), want (false, true, 0, nil)", renamed, rotated, repointed, err)
	}
	list, err := st.ListProjectSecrets(ctx, projID)
	if err != nil || len(list) != 1 || list[0].RotatedAt == nil {
		t.Fatalf("post-rotate list = %+v (err %v)", list, err)
	}
	if _, val, err := st.resolveProjectSecret(ctx, projID, "api-key"); err != nil || string(val) != "new-value" {
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
	renamed, rotated, _, err = st.UpdateProjectSecret(ctx, testSecretActor, projID, "api-key", &nn, &nv)
	if err != nil || !renamed || !rotated {
		t.Fatalf("rename+rotate = (%v, %v, %v), want (true, true, nil)", renamed, rotated, err)
	}
	if _, val, err := st.resolveProjectSecret(ctx, projID, "api-key-v2"); err != nil || string(val) != "third-value" {
		t.Fatalf("resolve renamed = (%q, %v)", val, err)
	}
	if _, _, err := st.resolveProjectSecret(ctx, projID, "api-key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve old name err = %v, want ErrNotFound", err)
	}
}

// TestProjectSecretRotationFence is the §4.5/P0-1 regression: rotating a secret bumps the
// execution_revision of exactly the referencing monitors (disabled included) through the
// same D-0142 semantics UpdateMonitor uses (revision +1, scheduled freshness watermark →
// NULL), leaves non-referencing monitors untouched, and sends the in-tx
// monitor_config_changed NOTIFY on commit. The ingest fence itself (a stale-revision
// result being rejected) is already proven by the D-0142 result-protocol tests — the
// revision bump asserted here is what arms it.
func TestProjectSecretRotationFence(t *testing.T) {
	st, ctx := secretsTestStore(t)
	orgID, projID := secretsFixture(t, st, ctx, "acme", "api")

	sec, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "db-password", "v1")
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	refEnabled := createRefMonitor(t, st, ctx, projID, "db-enabled", "db-password", true)
	refDisabled := createRefMonitor(t, st, ctx, projID, "db-disabled", "db-password", false)
	other := createRefMonitor(t, st, ctx, projID, "db-other", "db-password", true) // config mentions the name but has NO ref row
	insertSecretRef(t, st, ctx, refEnabled.ID, projID, "password_ref", sec.ID)
	insertSecretRef(t, st, ctx, refDisabled.ID, projID, "password_ref", sec.ID)

	// Give every monitor a freshness watermark so the reset is observable.
	if _, err := st.pool.Exec(ctx,
		`UPDATE monitors SET last_result_ts = now() WHERE project_id = $1`, projID); err != nil {
		t.Fatalf("seed last_result_ts: %v", err)
	}
	revEnabledBefore, _ := monitorFenceState(t, st, ctx, refEnabled.ID)
	revDisabledBefore, _ := monitorFenceState(t, st, ctx, refDisabled.ID)
	revOtherBefore, _ := monitorFenceState(t, st, ctx, other.ID)

	// LISTEN before the rotate; the NOTIFY is delivered on COMMIT of the rotate tx.
	lconn, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire listen conn: %v", err)
	}
	defer lconn.Release()
	if _, err := lconn.Exec(ctx, `LISTEN `+FileConfigChannel); err != nil {
		t.Fatalf("listen: %v", err)
	}

	nv := "v2"
	renamed, rotated, _, err := st.UpdateProjectSecret(ctx, testSecretActor, projID, "db-password", nil, &nv)
	if err != nil || renamed || !rotated {
		t.Fatalf("rotate = (%v, %v, %v), want (false, true, nil)", renamed, rotated, err)
	}

	// Referencing monitors fenced (D-0142): revision +1, scheduled watermark reset.
	revEnabledAfter, lastEnabled := monitorFenceState(t, st, ctx, refEnabled.ID)
	if revEnabledAfter != revEnabledBefore+1 || lastEnabled != nil {
		t.Fatalf("enabled ref monitor: rev %d→%d last_result_ts=%v, want rev+1 and NULL",
			revEnabledBefore, revEnabledAfter, lastEnabled)
	}
	revDisabledAfter, lastDisabled := monitorFenceState(t, st, ctx, refDisabled.ID)
	if revDisabledAfter != revDisabledBefore+1 || lastDisabled != nil {
		t.Fatalf("DISABLED ref monitor: rev %d→%d last_result_ts=%v, want rev+1 and NULL (disabled included)",
			revDisabledBefore, revDisabledAfter, lastDisabled)
	}
	// Non-referencing monitor untouched — no ref row means no fence, whatever the config says.
	revOtherAfter, lastOther := monitorFenceState(t, st, ctx, other.ID)
	if revOtherAfter != revOtherBefore || lastOther == nil {
		t.Fatalf("non-referencing monitor touched: rev %d→%d last_result_ts=%v",
			revOtherBefore, revOtherAfter, lastOther)
	}

	// The in-tx NOTIFY arrived after commit.
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	note, err := lconn.Conn().WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("no %s notification after rotate: %v", FileConfigChannel, err)
	}
	if note.Channel != FileConfigChannel || !strings.Contains(note.Payload, "secret_rotation") {
		t.Fatalf("notification = %q on %q, want a secret_rotation payload on %s", note.Payload, note.Channel, FileConfigChannel)
	}
	if strings.Contains(note.Payload, "v2") || strings.Contains(note.Payload, "v1") {
		t.Fatalf("notification payload leaks a value: %q", note.Payload)
	}

	// The audit row committed with the fence.
	if n, target := countSecretAudit(t, st, ctx, orgID, "secret.update"); n != 1 || !strings.Contains(target, "rotated=true") {
		t.Fatalf("secret.update audit = (count %d, target %q), want 1 row with rotated=true", n, target)
	}
}

// TestProjectSecretAuditInTx pins the P0-3 contract: every successful mutation leaves its
// audit row (written inside the mutation tx), targets carry names/flags but never values,
// a failed mutation leaves none, and a no-op update records nothing.
func TestProjectSecretAuditInTx(t *testing.T) {
	st, ctx := secretsTestStore(t)
	orgID, projID := secretsFixture(t, st, ctx, "acme", "api")
	const value = "audit-must-never-see-this"

	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "db-pass", value); err != nil {
		t.Fatalf("create: %v", err)
	}
	if n, target := countSecretAudit(t, st, ctx, orgID, "secret.create"); n != 1 || target != "db-pass" {
		t.Fatalf("secret.create audit = (count %d, target %q), want (1, db-pass)", n, target)
	}

	nn, nv := "db-pass-2", value+"-rotated"
	if _, _, _, err := st.UpdateProjectSecret(ctx, testSecretActor, projID, "db-pass", &nn, &nv); err != nil {
		t.Fatalf("update: %v", err)
	}
	n, target := countSecretAudit(t, st, ctx, orgID, "secret.update")
	if n != 1 {
		t.Fatalf("secret.update audit count = %d, want 1", n)
	}
	for _, want := range []string{"db-pass → db-pass-2", "renamed=true", "rotated=true", "repointed=0"} {
		if !strings.Contains(target, want) {
			t.Fatalf("secret.update target = %q, want it to contain %q", target, want)
		}
	}
	if strings.Contains(target, value) {
		t.Fatalf("secret.update target leaks the value: %q", target)
	}

	// A no-op update (rename to the current name) records NOTHING.
	same := "db-pass-2"
	if renamed, rotated, _, err := st.UpdateProjectSecret(ctx, testSecretActor, projID, "db-pass-2", &same, nil); err != nil || renamed || rotated {
		t.Fatalf("no-op update = (%v, %v, %v)", renamed, rotated, err)
	}
	if n, _ := countSecretAudit(t, st, ctx, orgID, "secret.update"); n != 1 {
		t.Fatalf("no-op update wrote an audit row (count %d), want 1", n)
	}
	// A failed rename (duplicate) rolls its audit back with the tx.
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "taken", "v"); err != nil {
		t.Fatalf("create taken: %v", err)
	}
	taken := "taken"
	if _, _, _, err := st.UpdateProjectSecret(ctx, testSecretActor, projID, "db-pass-2", &taken, nil); !errors.Is(err, ErrSecretExists) {
		t.Fatalf("rename-to-taken err = %v, want ErrSecretExists", err)
	}
	if n, _ := countSecretAudit(t, st, ctx, orgID, "secret.update"); n != 1 {
		t.Fatalf("failed rename left an audit row (count %d), want 1", n)
	}

	if err := st.DeleteProjectSecret(ctx, testSecretActor, projID, "db-pass-2"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, target := countSecretAudit(t, st, ctx, orgID, "secret.delete"); n != 1 || target != "db-pass-2" {
		t.Fatalf("secret.delete audit = (count %d, target %q), want (1, db-pass-2)", n, target)
	}

	// Belt and braces: no audit target anywhere carries the value.
	var leaks int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs WHERE target LIKE '%' || $1 || '%'`, value).Scan(&leaks); err != nil {
		t.Fatalf("leak scan: %v", err)
	}
	if leaks != 0 {
		t.Fatalf("%d audit rows contain the value", leaks)
	}
}

func TestProjectSecretTenantIsolation(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projA := secretsFixture(t, st, ctx, "acme", "api")
	_, projB := secretsFixture(t, st, ctx, "globex", "web")

	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projA, "db-password", "v"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := st.resolveProjectSecret(ctx, projB, "db-password"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant resolve err = %v, want ErrNotFound", err)
	}
	if err := st.DeleteProjectSecret(ctx, testSecretActor, projB, "db-password"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant delete err = %v, want ErrNotFound", err)
	}
	nn := "stolen"
	if _, _, _, err := st.UpdateProjectSecret(ctx, testSecretActor, projB, "db-password", &nn, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant update err = %v, want ErrNotFound", err)
	}
	list, err := st.ListProjectSecrets(ctx, projB)
	if err != nil || len(list) != 0 {
		t.Fatalf("cross-tenant list = %+v (err %v), want empty", list, err)
	}
	// A's secret is untouched.
	if _, val, err := st.resolveProjectSecret(ctx, projA, "db-password"); err != nil || string(val) != "v" {
		t.Fatalf("owner resolve = (%q, %v)", val, err)
	}
}

func TestProjectSecretProjectDeleteCascade(t *testing.T) {
	st, ctx := secretsTestStore(t)
	orgID, projID := secretsFixture(t, st, ctx, "acme", "api")

	sec, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "db-password", "v")
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	mon := createRefMonitor(t, st, ctx, projID, "db", "db-password", true)
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
	// The deleted project no longer resolves.
	if _, err := st.ListProjectSecrets(ctx, projID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("list of deleted project err = %v, want ErrNotFound", err)
	}
}

// TestProjectSecretAuditFailureRollsBackMutation is the reviewer-required real-PG proof in
// the OTHER direction: when the IN-TX audit insert itself fails, the secret mutation commits
// nothing. A non-UUID actor id makes the audit_logs.actor_user_id cast fail inside the tx.
func TestProjectSecretAuditFailureRollsBackMutation(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projID := secretsFixture(t, st, ctx, "acme", "api")
	badActor := SecretActor{ActorUserID: "not-a-uuid", ViaToken: false}

	if _, err := st.CreateProjectSecret(ctx, badActor, projID, "db-pass", "v"); err == nil {
		t.Fatal("create with a failing audit insert must error")
	}
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM project_secrets WHERE project_id = $1`, projID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("audit failure must roll back the secret mutation; %d rows persisted", n)
	}

	// Same for rotate: seed with a good actor, rotate with the failing one.
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "db-pass", "v1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	nv := "v2"
	if _, _, _, err := st.UpdateProjectSecret(ctx, badActor, projID, "db-pass", nil, &nv); err == nil {
		t.Fatal("rotate with a failing audit insert must error")
	}
	if _, got, err := st.resolveProjectSecret(ctx, projID, "db-pass"); err != nil || string(got) != "v1" {
		t.Fatalf("rotate must be rolled back with the audit: value=%q err=%v", got, err)
	}
}
