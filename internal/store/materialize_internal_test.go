package store

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

func materializerRing(t *testing.T) *dispatch.CredentialKeyring {
	t.Helper()
	ring, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{
		ID: "core-2026a", Key: bytes.Repeat([]byte{7}, 32),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func TestMaterializerSnapshotAndPayloadPlaintextAbsence(t *testing.T) {
	st, ctx := secretsTestStore(t)
	ring := materializerRing(t)
	st.WithCredentialKeyrings(dispatch.CredentialKeyrings{Regions: map[string]*dispatch.CredentialKeyring{"core": ring}})
	_, projectID := secretsFixture(t, st, ctx, "materialize-org", "app")
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projectID, "db-password", "inventory-plaintext"); err != nil {
		t.Fatal(err)
	}
	ref, err := st.CreateMonitor(ctx, postgresRefMonitor(projectID, "ref-db", "db-password"))
	if err != nil {
		t.Fatal(err)
	}
	inline, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: projectID, Name: "inline-db", Type: domain.MonitorPostgres,
		Target: "inline.internal:5432", IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true,
		Config: map[string]string{"username": "cerbix", "database": "app", "password": "inline-plaintext"},
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := st.ListEnabledMonitorSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range snapshot {
		if _, ok := m.Config["password"]; ok {
			t.Fatalf("snapshot contains credential key for %s: %#v", m.ID, m.Config)
		}
		blob, _ := json.Marshal(m)
		if bytes.Contains(blob, []byte("plaintext")) || bytes.Contains(blob, []byte("enc:v")) {
			t.Fatalf("snapshot contains plaintext/ciphertext for %s: %s", m.ID, blob)
		}
	}

	items, err := st.MaterializeExecutionConfigs(ctx, []string{ref.ID, inline.ID})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{ref.ID: "inventory-plaintext", inline.ID: "inline-plaintext"}
	for _, item := range items {
		if item.Reason != "" || item.Job.ProtocolVersion != dispatch.ProtocolV2 || item.Job.CredentialEnvelope == nil {
			t.Fatalf("materialized item = %+v", item)
		}
		body, _ := json.Marshal(item.Job)
		if bytes.Contains(body, []byte(want[item.MonitorID])) {
			t.Fatalf("transport payload leaked plaintext: %s", body)
		}
		probeMonitor, cleanup, err := ring.MaterializeForProbe(item.Job)
		if err != nil {
			t.Fatalf("executor open %s: %v", item.MonitorID, err)
		}
		if probeMonitor.Config["password"] != want[item.MonitorID] {
			t.Fatalf("executor password %q, want %q", probeMonitor.Config["password"], want[item.MonitorID])
		}
		cleanup()
	}
}

func TestMaterializerCurrentStateAndTenantIntegrity(t *testing.T) {
	st, ctx := secretsTestStore(t)
	ring := materializerRing(t)
	st.WithCredentialKeyrings(dispatch.CredentialKeyrings{Regions: map[string]*dispatch.CredentialKeyring{"core": ring}})
	_, projectA := secretsFixture(t, st, ctx, "materialize-a", "app")
	_, projectB := secretsFixture(t, st, ctx, "materialize-b", "app")
	secretA, _ := st.CreateProjectSecret(ctx, testSecretActor, projectA, "db-password", "value-a")
	secretB, _ := st.CreateProjectSecret(ctx, testSecretActor, projectB, "db-password", "value-b")
	mon, err := st.CreateMonitor(ctx, postgresRefMonitor(projectA, "db", "db-password"))
	if err != nil {
		t.Fatal(err)
	}

	// Swap the at-rest ciphertext across tenants: the tenant-safe join still selects A's
	// row, but its AAD authentication fails and no job is emitted.
	var ciphertextB string
	if err := st.pool.QueryRow(ctx, `SELECT value_encrypted FROM project_secrets WHERE id=$1`, secretB.ID).Scan(&ciphertextB); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE project_secrets SET value_encrypted=$1 WHERE id=$2 AND project_id=$3`, ciphertextB, secretA.ID, projectA); err != nil {
		t.Fatal(err)
	}
	item, err := st.MaterializeExecutionConfig(ctx, mon.ID)
	if err != nil || item.Reason != MaterializeDecryptFailed || item.Job.CredentialEnvelope != nil {
		t.Fatalf("AAD swap materialization = %+v err=%v", item, err)
	}

	if _, err := st.pool.Exec(ctx, `UPDATE monitors SET enabled=false WHERE id=$1`, mon.ID); err != nil {
		t.Fatal(err)
	}
	item, err = st.MaterializeExecutionConfig(ctx, mon.ID)
	if err != nil || item.Reason != MaterializeSkippedCurrentState {
		t.Fatalf("disabled materialization = %+v err=%v", item, err)
	}
	if strings.Contains(item.Reason, secretA.Name) {
		t.Fatal("bounded reason leaked secret metadata")
	}
}
