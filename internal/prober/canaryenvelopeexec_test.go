package prober

import (
	"bytes"
	"context"
	"testing"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// The LAST link of the chain: a credential that arrives in a real sealed envelope is opened by the
// real executor gate and reaches the target, and the journey succeeds (reviewer AC, party [396]).
//
// Every other canary test in this file puts the binding's value into the monitor's config directly,
// which proves the prober substitutes it and proves nothing about where it came from. Here the value
// exists ONLY inside a `dispatch` envelope sealed by a real keyring; the monitor handed to the
// prober is the one `ValidateAndMaterialize` produces, and the assertion is that the controlled
// target received the secret — so the envelope, the gate and the execution are one causal statement
// rather than three facts.
//
// What this does NOT reach, said here rather than implied: the scheduler and the store. Those are
// covered by `TestACanaryWithADeclaredBindingIsNominatedForItsEnvelope` (nomination) and
// `TestACanaryBindingIsSealedIntoAGenerationThreeEnvelope` (real materializer → generation 3 +
// `EnvelopeV2` → gate opens). The joints between the three are the same real types, not fakes; a
// single process spanning all of them is not reachable without a product-level test seam, and that
// seam is the policy's own bypass — which is why the canary's live E2E targets are `.invalid` and
// no canary has ever executed successfully on a live stack here.
// canaryExecRing is a real keyring: the envelope below is sealed and opened by product code, not by
// a stub that agrees with itself.
func canaryExecRing(t *testing.T) *dispatch.CredentialKeyring {
	t.Helper()
	r, err := dispatch.NewCredentialKeyring(
		dispatch.CredentialKeyMaterial{ID: "k-canary", Key: bytes.Repeat([]byte{7}, 32)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestASealedCanaryBindingIsOpenedByTheGateAndReachesTheTarget(t *testing.T) {
	f := newCanaryFixture(t)
	m := canaryMonitor(t, f.URL, nil)

	// The value is REMOVED from the config: from here on it exists only inside the envelope.
	delete(m.Config, domain.CanaryBindingField("upload"))

	// The envelope context must be complete — region, job, monitor and a revision of at least one.
	// The fixture monitor carries no region or revision of its own, so they are set here.
	if m.Region == "" {
		m.Region = "core"
	}
	m.ExecutionRevision = 1

	ring := canaryExecRing(t)
	envelope, err := ring.Seal(dispatch.SealContext{
		EnvelopeVersion: dispatch.EnvelopeV2,
		Region:          m.Region,
		JobID:           "00000000-0000-4000-8000-00000000cafe",
		MonitorID:       m.ID,
		Revision:        m.ExecutionRevision,
		Body:            m,
	}, map[string][]byte{domain.CanaryBindingField("upload"): []byte("s3cr3t-canary-token")})
	if err != nil {
		t.Fatalf("sealing a canary binding must work: %v", err)
	}

	materialized, err := dispatch.ValidateAndMaterialize(ring, dispatch.DeliveredJob{
		Job: dispatch.CheckJob{
			Monitor: m, ProtocolVersion: dispatch.ProtocolV3, CredentialEnvelope: envelope,
		},
		CarrierGeneration: dispatch.ProtocolV3,
	})
	if err != nil {
		t.Fatalf("the executor gate refused a correctly sealed canary: %v", err)
	}
	if !materialized.UsedCredential {
		t.Fatal("the gate opened the job without using the credential")
	}
	defer materialized.Cleanup()

	res := canaryTestProber().Probe(context.Background(), materialized.Monitor)
	if !res.Connected || res.Msg != "" {
		t.Fatalf("the journey must succeed on a materialized envelope: connected=%v msg=%q",
			res.Connected, res.Msg)
	}
	// The credential the TARGET saw came from the envelope and nowhere else — the config no longer
	// holds it, which is what makes this an end-to-end statement rather than a substitution test.
	if f.lastAuth != "s3cr3t-canary-token" {
		t.Fatalf("authorization = %q, want the value carried by the envelope", f.lastAuth)
	}
	if f.lastBody["token"] != "s3cr3t-canary-token" {
		t.Fatalf("body token = %v, want the value carried by the envelope", f.lastBody["token"])
	}
}
