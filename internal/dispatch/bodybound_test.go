package dispatch

import (
	"errors"
	"maps"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// sec-hardening-3 T-1c, S19. On a body-bound job (envelope v2) an absent defaulted key digests as its
// canonical default, while the prober reads the absence as the legacy value (sslmode → prefer,
// tls → off). Deleting the key in transit therefore kept a valid envelope and downgraded the probe.

// bodyBoundCase is one credentialed schema with every writeDefault field stated.
type bodyBoundCase struct {
	typ       domain.MonitorType
	config    map[string]string
	defaulted []string // the writeDefault fields of the effective schema, in sorted order
}

func bodyBoundCases() []bodyBoundCase {
	return []bodyBoundCase{
		{domain.MonitorPostgres, map[string]string{"username": "u", "database": "d", "sslmode": "require", "query": "SELECT 1"}, []string{"query", "sslmode"}},
		{domain.MonitorMySQL, map[string]string{"username": "u", "database": "d", "tls": "true", "query": "SELECT 1"}, []string{"query", "tls"}},
		{domain.MonitorRedis, map[string]string{"tls": "true"}, []string{"tls"}},
		{domain.MonitorRabbitMQ, map[string]string{"mode": "management", "username": "u", "path": "/api/overview", "tls": "true"}, []string{"path", "tls"}},
	}
}

// sealedJob seals a full body for a carrier and returns the delivered job with `drop` removed from
// the body afterwards, as a carrier writer would.
func sealedJob(t *testing.T, ring *CredentialKeyring, c bodyBoundCase, carrier int, drop string) DeliveredJob {
	t.Helper()
	m := domain.Monitor{
		ID: "11111111-1111-1111-1111-111111111111", Type: c.typ, Target: "db.example:1", Region: "geo1",
		ExecutionRevision: 3, TimeoutSeconds: 10, Config: maps.Clone(c.config),
	}
	job := CheckJob{ProtocolVersion: carrier, JobID: "job-bodybound", Monitor: m}
	if carrier >= ProtocolV4 {
		job.IssuedAt = time.Now()
		job.DueAt = time.Now()
	}
	if carrier == ProtocolV1 {
		job.Monitor.Config["password"] = "pw" // the legacy wire carries the credential inline
	} else {
		version, err := EnvelopeForCarrier(carrier)
		if err != nil {
			t.Fatal(err)
		}
		env, err := ring.Seal(SealContext{EnvelopeVersion: version, Region: m.Region, JobID: job.JobID,
			MonitorID: m.ID, Revision: m.ExecutionRevision, Body: m}, map[string][]byte{"password": []byte("pw")})
		if err != nil {
			t.Fatalf("seal %s on carrier %d: %v", c.typ, carrier, err)
		}
		job.CredentialEnvelope = env
	}
	delete(job.Monitor.Config, drop)
	return DeliveredJob{Job: job, CarrierGeneration: carrier}
}

func TestBodyBoundJobWithoutADefaultedKeyIsRefused(t *testing.T) {
	ring := credentialTestRing(t)
	for _, c := range bodyBoundCases() {
		// The rule is stated over the registry: the registry must name exactly these keys, so a new
		// defaulted field joins this test (and the gate) by construction.
		got, err := domain.DefaultedExecutionKeys(c.typ, c.config)
		if err != nil {
			t.Fatalf("%s: %v", c.typ, err)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, c.defaulted) {
			t.Errorf("%s: the registry's defaulted keys are %v, want %v", c.typ, got, c.defaulted)
		}
		for _, key := range c.defaulted {
			for _, carrier := range []int{ProtocolV3, ProtocolV4} {
				d := sealedJob(t, ring, c, carrier, key)
				m, err := ValidateAndMaterialize(ring, d)
				if err == nil {
					m.Cleanup()
					t.Errorf("%s without %q on carrier %d (envelope v2): accepted, password materialized", c.typ, key, carrier)
					continue
				}
				if !errors.Is(err, ErrBodyOmitsDefaultedSetting) || !strings.Contains(err.Error(), `"`+key+`"`) {
					t.Errorf("%s without %q on carrier %d: refused with %v, want the defaulted-setting refusal naming the key", c.typ, key, carrier, err)
				}
			}
			// Envelope v1 and the legacy wire do not bind the body: absence keeps today's reading.
			for _, carrier := range []int{ProtocolV2, ProtocolV1} {
				d := sealedJob(t, ring, c, carrier, key)
				m, err := ValidateAndMaterialize(ring, d)
				if err != nil {
					t.Errorf("%s without %q on carrier %d: refused (%v), want accepted", c.typ, key, carrier, err)
					continue
				}
				m.Cleanup()
			}
		}
		// The full body is accepted on every carrier.
		for _, carrier := range []int{ProtocolV1, ProtocolV2, ProtocolV3, ProtocolV4} {
			m, err := ValidateAndMaterialize(ring, sealedJob(t, ring, c, carrier, ""))
			if err != nil {
				t.Errorf("%s full body on carrier %d: %v", c.typ, carrier, err)
				continue
			}
			m.Cleanup()
		}
	}
}

func TestBodyBoundRefusalKeepsTheExecutorReady(t *testing.T) {
	ring := credentialTestRing(t)
	c := bodyBoundCases()[0]
	var health CredentialHealth
	var last error
	for i := 0; i < 3; i++ {
		m, err := ValidateAndMaterialize(ring, sealedJob(t, ring, c, ProtocolV3, "sslmode"))
		if err == nil {
			m.Cleanup()
			t.Fatalf("refusal %d: a body-bound job without sslmode was accepted", i+1)
		}
		last = err
		if reason := CredentialProbeErrorReason(err); reason != domain.ProbeErrorUnsupportedVersion {
			t.Fatalf("refusal %d: reason %q, want %q", i+1, reason, domain.ProbeErrorUnsupportedVersion)
		}
		if health.Failure(CredentialProbeErrorReason(err)) {
			t.Fatalf("refusal %d marked the executor credential-unready", i+1)
		}
	}
	// The contrast: the same refusal without its sentinel is decrypt_auth_failed, and the second one
	// costs the executor its readiness — which would leave the region with no credentialed work.
	var unmapped CredentialHealth
	bare := errors.New(last.Error())
	if reason := CredentialProbeErrorReason(bare); reason != domain.ProbeErrorDecryptAuthFailed {
		t.Fatalf("the unmapped refusal reads %q, want %q", reason, domain.ProbeErrorDecryptAuthFailed)
	}
	if unmapped.Failure(CredentialProbeErrorReason(bare)) {
		t.Fatal("the first unmapped refusal already marked the executor unready")
	}
	if !unmapped.Failure(CredentialProbeErrorReason(bare)) {
		t.Fatal("the second unmapped refusal did not mark the executor unready; the contrast is gone")
	}
}
