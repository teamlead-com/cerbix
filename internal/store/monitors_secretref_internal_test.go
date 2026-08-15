package store

import (
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// TestPartialUpdateKeepsTheStoredCredential is the regression for the audit P1 where the
// API and the store disagreed about an omitted credential slot. The value is write-only, so
// a client that reads a monitor back never had it to resend; demanding exactly-one-of on
// every PATCH made changing any OTHER setting impossible without sending `"password": ""`
// as a placeholder — a workaround worse than the rule it worked around.
func TestPartialUpdateKeepsTheStoredCredential(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projectID := secretsFixture(t, st, ctx, "partial-org", "payments")

	inline, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: projectID, Name: "inline-db", Type: domain.MonitorPostgres,
		Target: "db.internal:5432", IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true,
		Config: map[string]string{"username": "u", "database": "d", "password": "original-value", "sslmode": "require"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// A safe reader never sees the credential, so this is exactly what a client can send.
	safe, err := st.GetMonitor(ctx, inline.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := safe.Config["password"]; present {
		t.Fatal("the read surface exposed the credential; the rest of this test is meaningless")
	}
	safe.Config["sslmode"] = "verify-full" // change something else entirely
	if _, err := st.UpdateMonitor(ctx, safe); err != nil {
		t.Fatalf("partial update without the credential slot was rejected: %v", err)
	}

	// The other setting changed and the stored credential survived, byte for byte.
	var stored string
	if err := st.pool.QueryRow(ctx, `SELECT config->>'password' FROM monitors WHERE id = $1`, inline.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var originalCiphertext string
	if err := st.pool.QueryRow(ctx, `SELECT config->>'sslmode' FROM monitors WHERE id = $1`, inline.ID).Scan(&originalCiphertext); err != nil {
		t.Fatal(err)
	}
	if originalCiphertext != "verify-full" {
		t.Fatalf("the non-credential setting did not change: %q", originalCiphertext)
	}
	plain, err := st.cipher.Decrypt(stored)
	if err != nil {
		t.Fatalf("stored credential is no longer decryptable: %v", err)
	}
	if plain != "original-value" {
		t.Fatalf("stored credential = %q, want the original preserved", plain)
	}

	// Sending BOTH forms is still exactly-one-of: preservation is about omission, not about
	// relaxing the rule.
	both := safe
	both.Config = map[string]string{
		"username": "u", "database": "d", "sslmode": "require",
		"password": "x", "password_ref": "y",
	}
	if _, err := st.UpdateMonitor(ctx, both); err == nil {
		t.Fatal("an update carrying both a value and a ref was accepted")
	}
}

// TestPartialUpdateKeepsAStoredReference is the ref half the first version of this
// regression missed: it seeded only an INLINE credential, so the preserve path was never
// exercised on a row whose credential is a REFERENCE. On such a row the store looked for an
// old inline password, found none, and returned an internal error the API surfaced as a
// 500 — an ordinary partial edit of a ref-based monitor failed with a server error.
func TestPartialUpdateKeepsAStoredReference(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projectID := secretsFixture(t, st, ctx, "partial-ref-org", "payments")
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projectID, "app-db", "s3cret"); err != nil {
		t.Fatal(err)
	}
	ref, err := st.CreateMonitor(ctx, postgresRefMonitor(projectID, "ref-db", "app-db"))
	if err != nil {
		t.Fatal(err)
	}

	safe, err := st.GetMonitor(ctx, ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A safe reader DOES see the reference name — it is metadata, not the secret — but the
	// point is a client that drops it while editing something else, which is exactly what a
	// form that only renders changed fields does.
	delete(safe.Config, "password_ref")
	safe.Config["sslmode"] = "verify-full"
	if _, err := st.UpdateMonitor(ctx, safe); err != nil {
		t.Fatalf("partial update of a ref-based monitor failed: %v", err)
	}

	var storedRef, storedSSL string
	if err := st.pool.QueryRow(ctx,
		`SELECT COALESCE(config->>'password_ref',''), COALESCE(config->>'sslmode','') FROM monitors WHERE id = $1`, ref.ID).
		Scan(&storedRef, &storedSSL); err != nil {
		t.Fatal(err)
	}
	if storedRef != "app-db" {
		t.Fatalf("stored reference = %q, want it preserved", storedRef)
	}
	if storedSSL != "verify-full" {
		t.Fatalf("the edited setting did not persist: %q", storedSSL)
	}
	// The normalized ref table must still agree with the config, or a rotation would fence
	// nothing and a delete guard would see no consumer.
	var refRows int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM monitor_secret_refs WHERE monitor_id = $1 AND project_id = $2`, ref.ID, projectID).
		Scan(&refRows); err != nil {
		t.Fatal(err)
	}
	if refRows != 1 {
		t.Fatalf("monitor_secret_refs rows = %d, want the reference still tracked", refRows)
	}
}
