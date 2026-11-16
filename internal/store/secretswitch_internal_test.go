package store

import (
	"errors"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// TestAConfigLessUpdateChecksTheLockedRowForReferences (code review of sec-hardening-3, second pass,
// P2-1): with `secrets.enabled: false` the refusal of a config-less update is decided on the LOCKED row
// — a reference in monitor_secret_refs, or one in the stored config — never on the config the caller
// read before the transaction, because the write keeps the stored config (COALESCE) and what was checked
// must be what is kept.
func TestAConfigLessUpdateChecksTheLockedRowForReferences(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projectID := secretsFixture(t, st, ctx, "switch-org", "app")
	secretRow, err := st.CreateProjectSecret(ctx, testSecretActor, projectID, "db-password", "inventory-plaintext")
	if err != nil {
		t.Fatal(err)
	}
	inline := map[string]string{"username": "u", "database": "d", "password": "inline-pw", "sslmode": "disable"}
	snapshot := func(id string) string {
		t.Helper()
		var s string
		if err := st.pool.QueryRow(ctx, `
			SELECT m.target || ' | ' || m.enabled::text || ' | ' || m.config::text || ' | ' || COALESCE((
			         SELECT string_agg(r.setting_key || '=' || r.secret_id::text, ',' ORDER BY r.setting_key)
			           FROM monitor_secret_refs r WHERE r.monitor_id = m.id), '')
			  FROM monitors m WHERE m.id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	refuse := func(name string, m domain.Monitor) {
		t.Helper()
		before := snapshot(m.ID)
		m.Enabled = !m.Enabled
		m.Target = "elsewhere.example:5432"
		_, err := st.UpdateMonitorByPrincipal(ctx, m, tokenActor, false)
		if !errors.Is(err, ErrSecretsFeatureDisabled) {
			t.Errorf("%s: update = %v, want ErrSecretsFeatureDisabled", name, err)
		}
		if after := snapshot(m.ID); after != before {
			t.Errorf("%s: the row changed:\n before %s\n after  %s", name, before, after)
		}
	}

	// (a) a reference only in monitor_secret_refs; the config has no `password_ref`.
	onlyRefsRow, err := st.CreateMonitor(ctx, pgRow(projectID, "refs-row-only", inline))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO monitor_secret_refs (monitor_id, project_id, setting_key, secret_id)
		VALUES ($1, $2, 'password_ref', $3)`, onlyRefsRow.ID, projectID, secretRow.ID); err != nil {
		t.Fatal(err)
	}
	// (b) a reference in config and refs, and a caller that passes no config at all.
	withRef, err := st.CreateMonitor(ctx, postgresRefMonitor(projectID, "with-ref", "db-password"))
	if err != nil {
		t.Fatal(err)
	}
	// (c) the caller read the monitor before another write added a reference.
	late, err := st.CreateMonitor(ctx, pgRow(projectID, "late-ref", inline))
	if err != nil {
		t.Fatal(err)
	}
	stale, err := st.GetMonitorForWriter(ctx, late.ID)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := st.GetMonitorForWriter(ctx, late.ID)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Config = map[string]string{"username": "u", "database": "d", "password_ref": "db-password", "sslmode": "disable"}
	if _, err := st.UpdateMonitorByPrincipal(ctx, fresh, tokenActor, true); err != nil {
		t.Fatalf("adding the reference: %v", err)
	}
	// (d) a monitor without references.
	plain, err := st.CreateMonitor(ctx, pgRow(projectID, "no-ref", inline))
	if err != nil {
		t.Fatal(err)
	}

	st.WithSecretsEnabled(false)
	m, err := st.GetMonitorForWriter(ctx, onlyRefsRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	refuse("(a) reference only in monitor_secret_refs", m)
	m, err = st.GetMonitorForWriter(ctx, withRef.ID)
	if err != nil {
		t.Fatal(err)
	}
	m.Config = nil
	refuse("(b) the caller passes no config", m)
	refuse("(c) the caller read the monitor before the reference was added", stale)

	m, err = st.GetMonitorForWriter(ctx, plain.ID)
	if err != nil {
		t.Fatal(err)
	}
	m.Enabled = !m.Enabled
	if _, err := st.UpdateMonitorByPrincipal(ctx, m, tokenActor, false); err != nil {
		t.Errorf("(d) a monitor without references, switch off: %v", err)
	}

	// (d) the switch on: the update succeeds and the config and its references are untouched (S20).
	st.WithSecretsEnabled(true)
	m, err = st.GetMonitorForWriter(ctx, withRef.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := storedConfigAndRefs(ctx, t, st, withRef.ID)
	m.Enabled = !m.Enabled
	if _, err := st.UpdateMonitorByPrincipal(ctx, m, tokenActor, false); err != nil {
		t.Fatalf("(d) switch on: %v", err)
	}
	if after := storedConfigAndRefs(ctx, t, st, withRef.ID); after != before {
		t.Errorf("(d) switch on: S20 broken:\n before %s\n after  %s", before, after)
	}
}
