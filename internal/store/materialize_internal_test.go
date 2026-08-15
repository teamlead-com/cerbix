package store

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

func materializerRingWithByte(t *testing.T, id string, b byte) *dispatch.CredentialKeyring {
	t.Helper()
	ring, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{ID: id, Key: bytes.Repeat([]byte{b}, 32)}, nil)
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

	items, err := st.MaterializeExecutionConfigs(ctx, []string{ref.ID, inline.ID}, dispatch.ProtocolV2)
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
		if err := st.EnqueuePullJobV2(ctx, item.Job.Monitor.Region, body, item.Job.Monitor.IntervalSeconds); err != nil {
			t.Fatalf("persist v2 pull payload: %v", err)
		}
		materialized, err := dispatch.ValidateAndMaterialize(ring,
			dispatch.DeliveredJob{Job: item.Job, CarrierGeneration: dispatch.ProtocolV2})
		if err != nil {
			t.Fatalf("executor open %s: %v", item.MonitorID, err)
		}
		probeMonitor, cleanup := materialized.Monitor, materialized.Cleanup
		if probeMonitor.Config["password"] != want[item.MonitorID] {
			t.Fatalf("executor password %q, want %q", probeMonitor.Config["password"], want[item.MonitorID])
		}
		cleanup()
	}

	// The physical pull-queue row is the transport persistence boundary. It must carry
	// only the envelope ciphertext; neither inventory nor legacy inline plaintext may
	// survive the authoritative materialization step.
	persisted, err := st.ClaimPullJobsV2(ctx, "core", len(items), 30)
	if err != nil || len(persisted) != len(items) {
		t.Fatalf("claim persisted v2 payloads: count=%d err=%v", len(persisted), err)
	}
	for _, claimed := range persisted {
		if bytes.Contains(claimed.Payload, []byte("inventory-plaintext")) || bytes.Contains(claimed.Payload, []byte("inline-plaintext")) {
			t.Fatalf("pull_jobs payload leaked plaintext: %s", claimed.Payload)
		}
		var job dispatch.CheckJob
		if err := json.Unmarshal(claimed.Payload, &job); err != nil {
			t.Fatalf("decode persisted v2 payload: %v", err)
		}
		if job.ProtocolVersion != dispatch.ProtocolV2 || job.CredentialEnvelope == nil || len(job.CredentialEnvelope.Fields) != 1 {
			t.Fatalf("persisted payload lost the v2 envelope: %+v", job)
		}
		if _, exists := job.Monitor.Config["password"]; exists {
			t.Fatalf("persisted monitor config contains password: %#v", job.Monitor.Config)
		}
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

func TestMaterializerAuthoritativeReadBoundaries(t *testing.T) {
	st, ctx := secretsTestStore(t)
	coreRing := materializerRingWithByte(t, "core-key", 7)
	geoRing := materializerRingWithByte(t, "geo-key", 8)
	st.WithCredentialKeyrings(dispatch.CredentialKeyrings{Regions: map[string]*dispatch.CredentialKeyring{
		"core": coreRing,
		"geo":  geoRing,
	}})
	_, projectID := secretsFixture(t, st, ctx, "materialize-authoritative", "app")
	createdSecret, err := st.CreateProjectSecret(ctx, testSecretActor, projectID, "db-password", "old-value")
	if err != nil {
		t.Fatal(err)
	}
	mon, err := st.CreateMonitor(ctx, postgresRefMonitor(projectID, "db", createdSecret.Name))
	if err != nil {
		t.Fatal(err)
	}

	// Both an inventory rotation and an ordinary config/routing edit commit before the
	// authoritative read. The job must contain one coherent NEW row/value/revision.
	newValue := "rotated-value"
	if _, rotated, _, err := st.UpdateProjectSecret(ctx, testSecretActor, projectID, createdSecret.Name, nil, &newValue); err != nil || !rotated {
		t.Fatalf("rotate: rotated=%v err=%v", rotated, err)
	}
	current, err := st.GetMonitor(ctx, mon.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.Target = "new.internal:5432"
	current.Region = "geo"
	current.IntervalSeconds = 123
	updated, err := st.UpdateMonitor(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	item, err := st.MaterializeExecutionConfig(ctx, mon.ID)
	if err != nil || item.Reason != "" {
		t.Fatalf("materialize: item=%+v err=%v", item, err)
	}
	if item.Job.Monitor.Target != updated.Target || item.Job.Monitor.Region != "geo" || item.Job.Monitor.IntervalSeconds != 123 || item.Job.Monitor.ExecutionRevision != updated.ExecutionRevision {
		t.Fatalf("job is not the authoritative row: %+v, update=%+v", item.Job.Monitor, updated)
	}
	materialized, err := dispatch.ValidateAndMaterialize(geoRing,
		dispatch.DeliveredJob{Job: item.Job, CarrierGeneration: dispatch.ProtocolV2})
	if err != nil {
		t.Fatalf("geo decrypt: %v", err)
	}
	probeMonitor, cleanup := materialized.Monitor, materialized.Cleanup
	if probeMonitor.Config["password"] != newValue {
		t.Fatalf("materialized password = %q, want rotated value", probeMonitor.Config["password"])
	}
	cleanup()
	if _, err := dispatch.ValidateAndMaterialize(coreRing,
		dispatch.DeliveredJob{Job: item.Job, CarrierGeneration: dispatch.ProtocolV2}); err == nil {
		t.Fatal("old-region keyring opened a job routed to the new region")
	}

	// A change committed AFTER the read cannot recall the payload, but its revision bump
	// must fence the old job's result before any heartbeat or state mutation.
	afterRead, err := st.GetMonitor(ctx, mon.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterRead.Target = "newer.internal:5432"
	if _, err := st.UpdateMonitor(ctx, afterRead); err != nil {
		t.Fatal(err)
	}
	outcome, err := st.RecordScheduledResult(ctx, domain.Heartbeat{
		MonitorID: item.Job.Monitor.ID, ExecutionRevision: item.Job.Monitor.ExecutionRevision,
		Ts: time.Now().UTC(), Up: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Reason != ReasonStaleRevision || outcome.Applied || outcome.Inserted {
		t.Fatalf("post-read old result = %+v", outcome)
	}
}

// TestMaterializeSealsTheEnvelopeItsCarrierCarries pins the generation mapping at the point
// where it matters: the envelope version is derived from the carrier the caller has
// ESTABLISHED the region can consume, never chosen independently. Emitting envelope v2 into
// a region whose executors only understand v1 is the rolling-upgrade break the whole
// generational design exists to prevent (§4.7, D-0160).
func TestMaterializeSealsTheEnvelopeItsCarrierCarries(t *testing.T) {
	if envelopeForCarrier(dispatch.ProtocolV1) != dispatch.EnvelopeV1 {
		t.Fatal("generation 1 must map to the oldest envelope")
	}
	if envelopeForCarrier(dispatch.ProtocolV2) != dispatch.EnvelopeV1 {
		t.Fatal("generation 2 carries envelope v1 — it already means that to every deployed executor")
	}
	if envelopeForCarrier(dispatch.ProtocolV3) != dispatch.EnvelopeV2 {
		t.Fatal("generation 3 must carry envelope v2")
	}
	// An unknown carrier degrades to what everyone can open, not to what nobody can.
	if envelopeForCarrier(99) != dispatch.EnvelopeV2 {
		t.Fatal("a carrier newer than we know should still map to our newest envelope")
	}
	if envelopeForCarrier(0) != dispatch.EnvelopeV1 {
		t.Fatal("an unset carrier must degrade to the oldest envelope")
	}
}
