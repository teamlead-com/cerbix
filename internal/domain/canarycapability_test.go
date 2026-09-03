package domain

import "testing"

// FR-029 invariant 6. The announcement crosses three wires, so what is asserted here is the SHAPE
// both ends depend on: the token, and the queue-name split that carries it over AMQP.

func TestTheCapabilityTokenCarriesKindAndVersion(t *testing.T) {
	if got := CanaryCapabilityToken("async_transaction_v1", 1); got != "async_transaction_v1@1" {
		t.Fatalf("token = %q", got)
	}
	if got := CanaryCapabilityOfThisBinary(); got != CanaryCapabilityToken(CanaryWorkflowKind, CanaryWorkflowVersion) {
		t.Fatalf("this binary announces %q, which is not its own kind and version", got)
	}
	// The token a JOB needs comes from the document's kind, not from the constant: otherwise a
	// workflow of a kind this core does not run would ask for a runner of the kind it does.
	if got := CanaryCapabilityRequiredBy(CanaryWorkflow{Kind: "async_transaction_v9"}); got != "async_transaction_v9@1" {
		t.Fatalf("required = %q, want the DOCUMENT's kind", got)
	}
}

func TestAnAnnouncementMatchesExactlyInBothDirections(t *testing.T) {
	if !CanaryCapabilityAnnounced([]string{"other@1", "async_transaction_v1@1"}, "async_transaction_v1@1") {
		t.Fatal("an exact token in the set must match")
	}
	// A newer runner does not silently accept an older contract, and an older one does not
	// satisfy a newer job. Both are a mismatch, which is a bounded refusal, not a hopeful send.
	if CanaryCapabilityAnnounced([]string{"async_transaction_v1@2"}, "async_transaction_v1@1") {
		t.Fatal("a v2 runner must not satisfy a v1 job")
	}
	if CanaryCapabilityAnnounced([]string{"async_transaction_v1@1"}, "async_transaction_v1@2") {
		t.Fatal("a v1 runner must not satisfy a v2 job")
	}
	if CanaryCapabilityAnnounced(nil, "async_transaction_v1@1") {
		t.Fatal("an empty announcement announces nothing")
	}
	// Prefix or substring is not membership: `async_transaction_v1@11` must not answer for `@1`.
	if CanaryCapabilityAnnounced([]string{"async_transaction_v1@11"}, "async_transaction_v1@1") {
		t.Fatal("@11 must not satisfy @1")
	}
}

func TestTheQueueSuffixRoundTrips(t *testing.T) {
	token, region := CanaryCapabilityOfThisBinary(), "eu-west-1"
	got, gotRegion, ok := SplitCanaryQueueSuffix(CanaryQueueSuffix(token, region))
	if !ok || got != token || gotRegion != region {
		t.Fatalf("round trip = %q/%q ok=%v", got, gotRegion, ok)
	}
	// A region containing dots still round-trips, because the FIRST dot is the separator and a
	// token contains none.
	if _, r, ok := SplitCanaryQueueSuffix(CanaryQueueSuffix(token, "eu.west.1")); !ok || r != "eu.west.1" {
		t.Fatalf("dotted region = %q ok=%v", r, ok)
	}
	for _, bad := range []string{"", "no-dot", ".region", "token.", "."} {
		if _, _, ok := SplitCanaryQueueSuffix(bad); ok {
			t.Fatalf("%q was accepted as a canary queue suffix", bad)
		}
	}
}

// One rule, three call sites: the scheduler's dispatch filter, the pull job's required capability and
// the AMQP queue a job goes to must ask for the SAME token, or they disagree exactly for the
// documents that matter — the ones of an unfamiliar kind.
func TestTheRequiredTokenIsReadFromTheStoredDocument(t *testing.T) {
	future := map[string]string{CanaryWorkflowKey: `{"kind":"async_transaction_v9"}`}
	if got := CanaryCapabilityRequiredByConfig(future); got != "async_transaction_v9@1" {
		t.Fatalf("required = %q, want the document's own kind", got)
	}
	// An unparseable config asks for the runner this binary knows: the failure then happens at the
	// executor's gate, with its own reason, instead of being reported as a capability problem.
	for _, cfg := range []map[string]string{nil, {}, {CanaryWorkflowKey: "{"}} {
		if got := CanaryCapabilityRequiredByConfig(cfg); got != CanaryCapabilityOfThisBinary() {
			t.Fatalf("unparseable config asked for %q", got)
		}
	}
}
