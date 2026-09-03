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

	items, err := st.MaterializeExecutionConfigs(ctx, []string{ref.ID, inline.ID}, nil)
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
		if err := st.EnqueuePullJobV2(ctx, item.Job.Monitor.Region, body, item.Job.Monitor.IntervalSeconds, 0, ""); err != nil {
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
	persisted, err := st.ClaimPullJobsV2(ctx, "core", len(items), 30, nil)
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
	for _, tc := range []struct {
		carrier int
		want    int
	}{
		{dispatch.ProtocolV1, dispatch.EnvelopeV1},
		// Generation 2 already means envelope v1 to every deployed executor.
		{dispatch.ProtocolV2, dispatch.EnvelopeV1},
		{dispatch.ProtocolV3, dispatch.EnvelopeV2},
	} {
		got, err := envelopeForCarrier(tc.carrier)
		if err != nil || got != tc.want {
			t.Fatalf("carrier %d → envelope %d (err=%v), want %d", tc.carrier, got, err, tc.want)
		}
	}
	// A carrier we do not know is a wiring bug, not something to guess at: neither the
	// newest envelope (nobody downstream could open it) nor the oldest (it would ship
	// under a binding the caller did not ask for).
	for _, unknown := range []int{0, 4, 99, -1} {
		if _, err := envelopeForCarrier(unknown); err == nil {
			t.Fatalf("carrier %d silently mapped to an envelope", unknown)
		}
	}
}

// TestCarrierComesFromTheAuthoritativeRegion is the regression for a P0 the gen3 review
// found: the scheduler nominates a batch by SNAPSHOT region, but a monitor may have moved
// since. Choosing the carrier from the snapshot meant a monitor moved from a capability-2
// region to a capability-1 one got the right row and the right key but the WRONG carrier,
// and its job then sat on a queue nobody in the new region consumes until TTL. §4.4.3 says
// regroup by the authoritative region and only then select the transport — the carrier has
// to come from the same row as the keyring.
func TestCarrierComesFromTheAuthoritativeRegion(t *testing.T) {
	st, ctx := secretsTestStore(t)
	st.WithCredentialKeyrings(dispatch.CredentialKeyrings{Regions: map[string]*dispatch.CredentialKeyring{
		"core":    materializerRing(t),
		"geo-new": materializerRingWithByte(t, "geo-new-key", 9),
	}})
	_, projectID := secretsFixture(t, st, ctx, "carrier-org", "app")
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projectID, "db-password", "inventory-plaintext"); err != nil {
		t.Fatal(err)
	}
	ref, err := st.CreateMonitor(ctx, postgresRefMonitor(projectID, "ref-db", "db-password"))
	if err != nil {
		t.Fatal(err)
	}
	// The monitor has ALREADY moved to a region that can only take generation 2; the caller
	// still believes (from its stale snapshot) that it lives in the generation-3 region.
	if _, err := st.pool.Exec(ctx, `UPDATE monitors SET region = 'geo-new' WHERE id = $1`, ref.ID); err != nil {
		t.Fatal(err)
	}
	policy := map[string]int{"core": dispatch.ProtocolV3, "geo-new": dispatch.ProtocolV2}

	items, err := st.MaterializeExecutionConfigs(ctx, []string{ref.ID}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Reason != "" {
		t.Fatalf("materialize: %+v", items)
	}
	job := items[0].Job
	if job.Monitor.Region != "geo-new" {
		t.Fatalf("authoritative region = %q, want geo-new", job.Monitor.Region)
	}
	if job.ProtocolVersion != dispatch.ProtocolV2 {
		t.Fatalf("carrier = %d, want 2 — it must follow the authoritative region, not the snapshot's",
			job.ProtocolVersion)
	}
	if job.CredentialEnvelope == nil || job.CredentialEnvelope.V != dispatch.EnvelopeV1 {
		t.Fatalf("envelope generation must follow the carrier: %+v", job.CredentialEnvelope)
	}

	// And the other direction: a region established at generation 3 gets the
	// execution-bound envelope, so the amendment is reachable rather than merely specified.
	items, err = st.MaterializeExecutionConfigs(ctx, []string{ref.ID},
		map[string]int{"geo-new": dispatch.ProtocolV3})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Reason != "" {
		t.Fatalf("materialize at generation 3: %+v", items)
	}
	if got := items[0].Job.ProtocolVersion; got != dispatch.ProtocolV3 {
		t.Fatalf("carrier = %d, want 3", got)
	}
	if e := items[0].Job.CredentialEnvelope; e == nil || e.V != dispatch.EnvelopeV2 {
		t.Fatalf("generation-3 carrier must seal envelope v2: %+v", e)
	}
}

// Every materialized job carries its own identity (func-result-protocol §9, iter-0155): an id and the
// instant the CORE issued it, both from the database in the statement that materialized the job.
//
// Why the database and not the scheduler's clock: the ordering check compares an executor's
// `observed_at` against this instant, so it has to come from the one clock the core trusts. And why
// per JOB rather than per monitor: a monitor probed every 30 seconds produces a new job each time, and
// an id that repeated would make "which dispatch does this result answer" unanswerable exactly when it
// matters — a retry, a duplicate delivery, a slow region.
func TestEveryMaterializedJobCarriesItsOwnIdentity(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	var ids []string
	for _, name := range []string{"a", "b"} {
		m, err := st.CreateMonitor(ctx, domain.Monitor{
			ProjectID: proj.ID, Name: name, Type: domain.MonitorHTTP, Target: "https://x",
			IntervalSeconds: 60, TimeoutSeconds: 5, FailureThreshold: 1, Enabled: true,
		})
		if err != nil {
			t.Fatalf("monitor %s: %v", name, err)
		}
		ids = append(ids, m.ID)
	}

	before := time.Now().UTC().Add(-time.Minute)
	items, err := st.MaterializeExecutionConfigs(ctx, ids, nil)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	batch := map[string]MaterializedExecution{}
	for _, it := range items {
		batch[it.MonitorID] = it
	}
	seen := map[string]bool{}
	for _, id := range ids {
		entry, ok := batch[id]
		if !ok || entry.Reason != "" {
			t.Fatalf("monitor %s not materialized: %+v", id, entry)
		}
		if entry.Job.JobID == "" {
			t.Fatalf("monitor %s got a job with no id — a result answering it could never be correlated "+
				"with the dispatch that asked for it", id)
		}
		if seen[entry.Job.JobID] {
			t.Fatalf("two jobs in one batch share the id %s — an id must identify a JOB, not a monitor",
				entry.Job.JobID)
		}
		seen[entry.Job.JobID] = true
		if entry.Job.IssuedAt.IsZero() || entry.Job.IssuedAt.Before(before) {
			t.Fatalf("job %s issued at %s, want the database clock of this materialization — a zero or "+
				"stale instant makes the ordering check compare against nothing", entry.Job.JobID, entry.Job.IssuedAt)
		}
	}

	// A second materialization of the SAME monitor is a different job.
	againItems, err := st.MaterializeExecutionConfigs(ctx, ids[:1], nil)
	if err != nil || len(againItems) != 1 {
		t.Fatalf("re-materialize: %v (%d items)", err, len(againItems))
	}
	if againItems[0].Job.JobID == batch[ids[0]].Job.JobID {
		t.Error("the same monitor materialized twice produced the same job id — every dispatch is its own job")
	}
}
