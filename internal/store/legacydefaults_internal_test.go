package store

import (
	"testing"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// TestMaterializeStatesTheLegacyValueOfAnAbsentKey (sec-hardening-3 T-1c, S19): a row written by a
// v0.1.0 beta has no `sslmode` (postgres) or `tls` (mysql). Core states the value the executor has
// always applied to that absence — `prefer`, `false` — in the job body before sealing, on every
// carrier, so the executor's body-bound gate accepts it and executes exactly what it executed before.
// The stored row is not changed.
func TestMaterializeStatesTheLegacyValueOfAnAbsentKey(t *testing.T) {
	st, ctx := secretsTestStore(t)
	ring := materializerRing(t)
	st.WithCredentialKeyrings(dispatch.CredentialKeyrings{Regions: map[string]*dispatch.CredentialKeyring{"core": ring}})
	_, projectID := secretsFixture(t, st, ctx, "legacy-org", "app")

	for _, tc := range []struct {
		typ    domain.MonitorType
		config map[string]string
		key    string
		want   string
	}{
		{domain.MonitorPostgres, map[string]string{"username": "u", "database": "d", "password": "inline-pw"}, "sslmode", "prefer"},
		{domain.MonitorMySQL, map[string]string{"username": "u", "database": "d", "password": "inline-pw"}, "tls", "false"},
	} {
		m, err := st.CreateMonitor(ctx, domain.Monitor{
			ProjectID: projectID, Name: "beta-" + string(tc.typ), Type: tc.typ, Target: "db.internal:5432",
			IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true, Config: tc.config,
		})
		if err != nil {
			t.Fatal(err)
		}
		// As a beta wrote it: no defaulted key at all.
		if _, err := st.pool.Exec(ctx, `UPDATE monitors SET config = config - $2 WHERE id = $1`, m.ID, tc.key); err != nil {
			t.Fatal(err)
		}
		// A schedule row gives the dispatch a window, which is what lets generation 4 carry it. Creating
		// the monitor normally writes one; this makes sure of it.
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO monitor_schedule (project_id, monitor_id, next_due_at, interval_in_force, execution_revision)
			 SELECT project_id, id, now(), interval_seconds, execution_revision FROM monitors WHERE id = $1
			 ON CONFLICT (monitor_id) DO NOTHING`, m.ID); err != nil {
			t.Fatal(err)
		}
		var before string
		if err := st.pool.QueryRow(ctx, `SELECT config::text FROM monitors WHERE id = $1`, m.ID).Scan(&before); err != nil {
			t.Fatal(err)
		}

		for _, carrier := range []int{dispatch.ProtocolV2, dispatch.ProtocolV3, dispatch.ProtocolV4} {
			items, err := st.MaterializeExecutionConfigs(ctx, []string{m.ID}, map[string]int{"core": carrier})
			if err != nil {
				t.Fatal(err)
			}
			item := items[0]
			if item.Reason != "" || item.CarrierGeneration != carrier {
				t.Fatalf("%s on carrier %d: reason %q, carrier %d", tc.typ, carrier, item.Reason, item.CarrierGeneration)
			}
			if got, ok := item.Job.Monitor.Config[tc.key]; !ok || got != tc.want {
				t.Errorf("%s on carrier %d: job body %s = %q (present %v), want %q", tc.typ, carrier, tc.key, got, ok, tc.want)
				continue
			}
			opened, err := dispatch.ValidateAndMaterialize(ring, dispatch.DeliveredJob{Job: item.Job, CarrierGeneration: item.CarrierGeneration})
			if err != nil {
				t.Errorf("%s on carrier %d: the executor refused the stated body: %v", tc.typ, carrier, err)
				continue
			}
			if opened.Monitor.Config["password"] != "inline-pw" {
				t.Errorf("%s on carrier %d: the credential did not open", tc.typ, carrier)
			}
			opened.Cleanup()
		}
		var after string
		if err := st.pool.QueryRow(ctx, `SELECT config::text FROM monitors WHERE id = $1`, m.ID).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Errorf("%s: the stored row changed:\n before %s\n after  %s", tc.typ, before, after)
		}
	}
}
