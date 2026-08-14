package dispatch_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// TestAMQPRoundTrip requires a real RabbitMQ. It is opt-in via
// CERBIX_TEST_RABBITMQ_URL and skipped otherwise, so default `go test ./...` and
// CI stay hermetic (the in-process dispatcher covers the seam there).
func TestAMQPRoundTrip(t *testing.T) {
	url := os.Getenv("CERBIX_TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("set CERBIX_TEST_RABBITMQ_URL to run the AMQP dispatcher test")
	}
	d, err := dispatch.NewAMQP(url, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		_ = d.Close()
		deleteTestQueue(t, url, "checks.jobs.roundtrip")
		deleteTestQueue(t, url, "checks.jobs.v2.roundtrip")
		deleteTestQueue(t, url, "checks.jobs.wirebarrier")
		deleteTestQueue(t, url, "checks.jobs.v2.wirebarrier")
	})
	d.WithJobRegion("roundtrip").WithProtocolV2(true)
	ctx := context.Background()

	jobs := d.Jobs()
	want := "00000000-0000-4000-8000-000000000117"
	if err := d.PublishJob(ctx, dispatch.CheckJob{Monitor: domain.Monitor{ID: want, Name: "x", Region: "roundtrip"}}); err != nil {
		t.Fatalf("publish job: %v", err)
	}
	if got := awaitJob(t, jobs, want); got != want {
		t.Fatalf("job round-trip = %q, want %q", got, want)
	}

	results := d.Results()
	if err := d.PublishResult(ctx, domain.Heartbeat{MonitorID: want, Up: true, Code: 200}); err != nil {
		t.Fatalf("publish result: %v", err)
	}
	if got := awaitResult(t, results, want); !got {
		t.Fatal("result round-trip: heartbeat not up")
	}

	keyring, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{
		ID:  "roundtrip-key",
		Key: bytes.Repeat([]byte{0x11}, 32),
	}, nil)
	if err != nil {
		t.Fatalf("credential keyring: %v", err)
	}
	// Inspect exact physical queues in a region that has no consumer. The merged
	// Jobs() channel above cannot by itself prove the v2 envelope never crossed the
	// v1 queue boundary.
	barrierV1 := dispatch.CheckJob{Monitor: domain.Monitor{ID: "00000000-0000-4000-8000-000000000122", Region: "wirebarrier"}}
	if err := d.PublishJob(ctx, barrierV1); err != nil {
		t.Fatalf("publish wire-barrier v1 job: %v", err)
	}
	barrierMonitor := domain.Monitor{ID: "00000000-0000-4000-8000-000000000123", Region: "wirebarrier", ExecutionRevision: 1}
	barrierEnvelope, err := keyring.Seal("wirebarrier", "wire-barrier-v2", barrierMonitor.ID, barrierMonitor.ExecutionRevision, map[string][]byte{"password": []byte("barrier")})
	if err != nil {
		t.Fatalf("seal wire-barrier v2 job: %v", err)
	}
	if err := d.PublishJob(ctx, dispatch.CheckJob{Monitor: barrierMonitor, ProtocolVersion: dispatch.ProtocolV2, CredentialEnvelope: barrierEnvelope}); err != nil {
		t.Fatalf("publish wire-barrier v2 job: %v", err)
	}
	rawV1 := getQueuedJob(t, url, "checks.jobs.wirebarrier")
	if rawV1.ProtocolVersion == dispatch.ProtocolV2 || rawV1.CredentialEnvelope != nil {
		t.Fatalf("v1 physical queue received envelope-bearing job: %+v", rawV1)
	}
	rawV2 := getQueuedJob(t, url, "checks.jobs.v2.wirebarrier")
	if rawV2.ProtocolVersion != dispatch.ProtocolV2 || rawV2.CredentialEnvelope == nil {
		t.Fatalf("v2 physical queue did not receive envelope-bearing job: %+v", rawV2)
	}
	barrierFields, err := keyring.Open(rawV2)
	if err != nil {
		t.Fatalf("open wire-barrier envelope: %v", err)
	}
	dispatch.WipeCredentialFields(barrierFields)

	v2Monitor := domain.Monitor{
		ID: "00000000-0000-4000-8000-000000000119", Name: "v2", Region: "roundtrip",
		ExecutionRevision: 2,
	}
	v2Envelope, err := keyring.Seal("roundtrip", "job-roundtrip-v2", v2Monitor.ID, v2Monitor.ExecutionRevision, map[string][]byte{"password": []byte("v2-secret")})
	if err != nil {
		t.Fatalf("seal v2 job: %v", err)
	}
	v2Job := dispatch.CheckJob{Monitor: v2Monitor, ProtocolVersion: dispatch.ProtocolV2, CredentialEnvelope: v2Envelope}
	if err := d.PublishJob(ctx, v2Job); err != nil {
		t.Fatalf("publish v2 job: %v", err)
	}
	receivedV2 := awaitJobPayload(t, jobs, v2Monitor.ID)
	fields, err := keyring.Open(receivedV2)
	if err != nil {
		t.Fatalf("open received v2 envelope: %v", err)
	}
	if string(fields["password"]) != "v2-secret" {
		t.Errorf("received v2 credential = %q, want v2-secret", fields["password"])
	}
	dispatch.WipeCredentialFields(fields)

	// Exercise both shared regional test queues without pre-declaring them. The
	// synchronous Serve calls are the worker-readiness barrier: an immediate RPC
	// after they return must route on RabbitMQ 4.3 rather than time out silently.
	if err := d.ServeTests(func(_ context.Context, m domain.Monitor) domain.Heartbeat {
		return domain.Heartbeat{MonitorID: m.ID, Up: true, Code: 204}
	}); err != nil {
		t.Fatalf("start v1 test consumer: %v", err)
	}
	testResult, err := d.RunTest(ctx, domain.Monitor{ID: "00000000-0000-4000-8000-000000000118", Region: "roundtrip", TimeoutSeconds: 1})
	if err != nil {
		t.Fatalf("test RPC round-trip: %v", err)
	}
	if !testResult.Up || testResult.Code != 204 {
		t.Fatalf("test RPC result = %+v, want up/204", testResult)
	}

	if err := d.ServeTestsV2(func(_ context.Context, job dispatch.CheckJob) (domain.Heartbeat, error) {
		fields, openErr := keyring.Open(job)
		if openErr != nil {
			return domain.Heartbeat{}, openErr
		}
		defer dispatch.WipeCredentialFields(fields)
		if string(fields["password"]) != "v2-test-secret" {
			t.Errorf("received v2 test credential = %q, want v2-test-secret", fields["password"])
		}
		return domain.Heartbeat{MonitorID: job.Monitor.ID, Up: true, Code: 205}, nil
	}); err != nil {
		t.Fatalf("start v2 test consumer: %v", err)
	}
	v2TestMonitor := domain.Monitor{
		ID: "00000000-0000-4000-8000-000000000120", Region: "roundtrip",
		ExecutionRevision: 3, TimeoutSeconds: 1,
	}
	v2TestEnvelope, err := keyring.Seal("roundtrip", "test-roundtrip-v2", v2TestMonitor.ID, v2TestMonitor.ExecutionRevision, map[string][]byte{"password": []byte("v2-test-secret")})
	if err != nil {
		t.Fatalf("seal v2 test job: %v", err)
	}
	v2TestResult, err := d.RunJobTest(ctx, dispatch.CheckJob{Monitor: v2TestMonitor, ProtocolVersion: dispatch.ProtocolV2, CredentialEnvelope: v2TestEnvelope})
	if err != nil {
		t.Fatalf("v2 test RPC round-trip: %v", err)
	}
	if !v2TestResult.Up || v2TestResult.Code != 205 {
		t.Fatalf("v2 test RPC result = %+v, want up/205", v2TestResult)
	}

	unroutableAt := time.Now()
	_, err = d.RunTest(ctx, domain.Monitor{ID: "00000000-0000-4000-8000-000000000121", Region: "missing-roundtrip", TimeoutSeconds: 1})
	if err == nil {
		t.Fatal("test RPC to a missing regional queue unexpectedly succeeded")
	}
	if elapsed := time.Since(unroutableAt); elapsed > time.Second {
		t.Fatalf("mandatory unroutable RPC took %s instead of failing promptly: %v", elapsed, err)
	}
}

func getQueuedJob(t *testing.T, url, name string) dispatch.CheckJob {
	t.Helper()
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatalf("dial queue inspection: %v", err)
	}
	defer func() { _ = conn.Close() }()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("open queue-inspection channel: %v", err)
	}
	defer func() { _ = ch.Close() }()
	msg, ok, err := ch.Get(name, false)
	if err != nil {
		t.Fatalf("get queue %s: %v", name, err)
	}
	if !ok {
		t.Fatalf("queue %s is empty", name)
	}
	if err := msg.Ack(false); err != nil {
		t.Fatalf("ack queue %s: %v", name, err)
	}
	var job dispatch.CheckJob
	if err := json.Unmarshal(msg.Body, &job); err != nil {
		t.Fatalf("decode queue %s job: %v", name, err)
	}
	return job
}

func deleteTestQueue(t *testing.T, url, name string) {
	t.Helper()
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Errorf("dial queue cleanup: %v", err)
		return
	}
	defer func() { _ = conn.Close() }()
	ch, err := conn.Channel()
	if err != nil {
		t.Errorf("open queue-cleanup channel: %v", err)
		return
	}
	defer func() { _ = ch.Close() }()
	if _, err := ch.QueueDelete(name, false, false, false); err != nil {
		t.Errorf("delete test queue %s: %v", name, err)
	}
}

func awaitJob(t *testing.T, jobs <-chan dispatch.CheckJob, want string) string {
	return awaitJobPayload(t, jobs, want).Monitor.ID
}

func awaitJobPayload(t *testing.T, jobs <-chan dispatch.CheckJob, want string) dispatch.CheckJob {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case j := <-jobs:
			if j.Monitor.ID == want {
				return j
			}
		case <-deadline:
			t.Fatal("timed out waiting for job")
			return dispatch.CheckJob{}
		}
	}
}

func awaitResult(t *testing.T, results <-chan domain.Heartbeat, want string) bool {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case hb := <-results:
			if hb.MonitorID == want {
				return hb.Up
			}
		case <-deadline:
			t.Fatal("timed out waiting for result")
			return false
		}
	}
}
