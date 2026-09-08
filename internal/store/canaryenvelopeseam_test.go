package store

import (
	"context"
	"testing"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// canaryWithBinding builds a canary whose workflow declares one binding, plus the flat reference
// key that is its persisted identity. Both halves are one declaration, and the product treats them
// as one.
func canaryWithBinding(t *testing.T, projectID, slug, secretName string) domain.Monitor {
	t.Helper()
	doc, err := domain.CanaryCanonicalJSON(domain.CanaryWorkflow{
		Kind:    domain.CanaryWorkflowKind,
		Secrets: map[string]string{"upload": ""},
		Submit: domain.CanarySubmit{
			Kind: domain.CanarySubmitHTTPJSON, Method: "POST",
			URL: "https://files.example.com/files/upload", SubmitTimeout: 10,
			AcceptedStatus: []int{202},
			Headers:        []domain.CanaryHeader{{Name: "x-api-key", SecretRef: "upload"}},
			Body:           map[string]domain.CanaryValue{"tenant": {Kind: domain.CanaryValueString, Str: "canary"}},
		},
		Correlate: domain.CanaryCorrelate{Source: domain.CanaryCorrelateResponseJSON, Path: "task_id"},
		Completion: domain.CanaryCompletion{
			Kind: domain.CanaryCompletionPollJSON, URL: "https://files.example.com/t/{{ correlation_id }}",
			Timeout: 60,
			Poll: &domain.CanaryPoll{Interval: 5, MaxAttempts: 12,
				Success: domain.CanaryPollMatch{Path: "status", Value: "completed"}},
		},
		Result:  domain.CanaryResult{MaxLatency: 60, RequiredJSONFields: []string{"s3_path"}, LifecyclePath: "s3_path"},
		Cleanup: domain.CanaryCleanup{Kind: domain.CanaryCleanupNone, Acknowledged: true},
	})
	if err != nil {
		t.Fatalf("canonical workflow: %v", err)
	}
	return domain.Monitor{
		ProjectID: projectID, Name: slug, Slug: slug, Type: domain.MonitorAsyncCanary,
		Region: "core", Enabled: true, IntervalSeconds: 120, TimeoutSeconds: 60,
		Config: map[string]string{"workflow": doc, "canary_secret_upload_ref": secretName},
	}
}

// The CAUSAL CHAIN a canary binding travels, against the real materializer and the real executor
// gate rather than against a fake (reviewer P0, party [394]).
//
// The scheduler-side test proves only that such a monitor is NOMINATED; a fake materializer that
// records an id proves nothing about what the nomination buys. This is the other half and it uses
// no fakes: the store seals the envelope, and `dispatch.ValidateAndMaterialize` — the same gate
// every executor crosses — opens it and reports the credential as USED.
//
// Before the repairs this failed for TWO reasons, and naming only the first was the incomplete
// cause an earlier version of this comment gave (reviewer, party [396]): the scheduler never
// nominated such a monitor, AND — reached directly, as this test reaches it — the materializer got
// as far as sealing and failed there, because the execution digest asks for the canonical value of
// every binding key and the canary's keys had no arm in that lookup. Fixing the nomination alone
// would have left this test red.
//
// The mutations that must kill this: seal a generation-2 envelope on the generation-3 carrier, or
// drop the binding from the expected-field set.
func TestACanaryBindingIsSealedIntoAGenerationThreeEnvelope(t *testing.T) {
	st, ctx := secretsTestStore(t)
	st.WithCredentialKeyrings(dispatch.CredentialKeyrings{Regions: map[string]*dispatch.CredentialKeyring{
		"core": materializerRing(t),
	}})
	_, projectID := secretsFixture(t, st, ctx, "canary-org", "app")
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projectID, "api-token", "inventory-plaintext"); err != nil {
		t.Fatal(err)
	}
	mon, err := st.CreateMonitor(ctx, canaryWithBinding(t, projectID, "canary-seam", "api-token"))
	if err != nil {
		t.Fatalf("create canary: %v", err)
	}

	// The nomination's own question, asked of the domain rather than of the scheduler, so this file
	// fails too if the predicate stops answering for a canary.
	if !domain.RequiresExecutionEnvelope(mon) {
		t.Fatal("a canary declaring a binding must require an execution envelope")
	}

	items, err := st.MaterializeExecutionConfigs(ctx, []string{mon.ID},
		map[string]int{"core": dispatch.ProtocolV3})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(items) != 1 || items[0].Reason != "" {
		t.Fatalf("materialize refused the canary: %+v", items)
	}
	job := items[0].Job
	if job.ProtocolVersion != dispatch.ProtocolV3 {
		t.Fatalf("carrier = %d, want 3 — a binding needs the execution-bound generation",
			job.ProtocolVersion)
	}
	if job.CredentialEnvelope == nil {
		t.Fatal("no envelope was sealed; the binding would travel as a placeholder or not at all")
	}
	if job.CredentialEnvelope.V != dispatch.EnvelopeV2 {
		t.Fatalf("envelope generation = %d, want %d (execution-bound)",
			job.CredentialEnvelope.V, dispatch.EnvelopeV2)
	}

	// EXECUTION SUCCESS: the gate every executor crosses opens the envelope and reports the
	// credential as used. Before the repair this monitor never reached a carrier that could hold
	// one, and this call is what that cost.
	m, err := dispatch.ValidateAndMaterialize(materializerRing(t),
		dispatch.DeliveredJob{Job: job, CarrierGeneration: dispatch.ProtocolV3})
	if err != nil {
		t.Fatalf("the executor gate refused a correctly sealed canary: %v", err)
	}
	if !m.UsedCredential {
		t.Fatal("the gate opened the job without using the credential; the binding was not resolved")
	}
	if m.Cleanup != nil {
		m.Cleanup()
	}
	_ = context.Background()
}
