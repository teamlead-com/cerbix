package store

import (
	"context"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// TestUpdateWithoutConfigKeepsTheStoredConfig (sec-hardening-3 T-1d, T-5; S20): an update that
// carries no `config` runs no settings preparation and rewrites neither the config column nor
// monitor_secret_refs. Before, the store re-prepared the stored config on every update: a beta row
// with no sslmode was silently written `require`, and one with `prefer` was refused.
func TestUpdateWithoutConfigKeepsTheStoredConfig(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projectID := secretsFixture(t, st, ctx, "partial-org", "app")
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projectID, "db-password", "inventory-plaintext"); err != nil {
		t.Fatal(err)
	}
	inline := map[string]string{"username": "u", "database": "d", "password": "inline-pw"}
	cases := []struct {
		name    string
		monitor domain.Monitor
		legacy  string // SQL that makes the stored row what a beta (or a direct write) left behind
	}{
		{"postgres, sslmode absent", pgRow(projectID, "absent", inline), `UPDATE monitors SET config = config - 'sslmode' WHERE id = $1`},
		{"postgres, sslmode prefer", pgRow(projectID, "prefer", inline), `UPDATE monitors SET config = jsonb_set(config, '{sslmode}', '"prefer"') WHERE id = $1`},
		{"postgres, a beta free-form value", pgRow(projectID, "freeform", inline), `UPDATE monitors SET config = jsonb_set(config, '{sslmode}', '"require host=a"') WHERE id = $1`},
		{"postgres by reference, sslmode prefer", postgresRefMonitor(projectID, "ref-prefer", "db-password"), `UPDATE monitors SET config = jsonb_set(config, '{sslmode}', '"prefer"') WHERE id = $1`},
		{"synthetic", domain.Monitor{
			ProjectID: projectID, Name: "synthetic", Type: domain.MonitorSynthetic,
			IntervalSeconds: 60, TimeoutSeconds: 30, Enabled: true,
			Config: map[string]string{"scenario": `{"steps":[{"url":"https://example.test/health"}]}`},
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created, err := st.CreateMonitor(ctx, tc.monitor)
			if err != nil {
				t.Fatal(err)
			}
			if tc.legacy != "" {
				if _, err := st.pool.Exec(ctx, tc.legacy, created.ID); err != nil {
					t.Fatal(err)
				}
			}
			before := storedConfigAndRefs(ctx, t, st, created.ID)

			// What the handler now reads for a principal who may write the monitor.
			m, err := st.GetMonitorForWriter(ctx, created.ID)
			if err != nil {
				t.Fatal(err)
			}
			m.Enabled = !m.Enabled
			updated, err := st.updateMonitor(ctx, m, nil, false)
			if err != nil {
				t.Fatalf("an update without config was refused: %v", err)
			}
			if updated.Enabled != m.Enabled {
				t.Fatalf("enabled = %v, want %v", updated.Enabled, m.Enabled)
			}
			if after := storedConfigAndRefs(ctx, t, st, created.ID); after != before {
				t.Fatalf("the stored config or its references changed:\n before %s\n after  %s", before, after)
			}
		})
	}
}

func pgRow(projectID, name string, config map[string]string) domain.Monitor {
	c := make(map[string]string, len(config))
	for k, v := range config {
		c[k] = v
	}
	return domain.Monitor{
		ProjectID: projectID, Name: name, Type: domain.MonitorPostgres, Target: "db.internal:5432",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true, Config: c,
	}
}

// storedConfigAndRefs is the config column byte for byte, then the monitor's secret references.
func storedConfigAndRefs(ctx context.Context, t *testing.T, st *Store, id string) string {
	t.Helper()
	var out string
	if err := st.pool.QueryRow(ctx, `
		SELECT m.config::text || ' | ' || COALESCE((
		         SELECT string_agg(r.setting_key || '=' || r.secret_id::text, ',' ORDER BY r.setting_key)
		           FROM monitor_secret_refs r WHERE r.monitor_id = m.id), '')
		  FROM monitors m WHERE m.id = $1`, id).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}
