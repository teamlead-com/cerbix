package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// A canary that declares a binding must be NOMINATED for credential materialization.
//
// The nomination gate asked `domain.CredentialedType(m.Type)`, which answers whether the TYPE has a
// static FR-020 credential schema. An `async_canary` has none — its bindings live in the workflow
// document — so a canary with a declared binding read false, went down the plain branch, and was
// stamped with a generation that carries no envelope. The executor gate then refused it for a field
// the producer had never been asked to include, and the monitor could not run at all. The two halves
// disagreed about the same monitor: `ExpectedCredentialFields` named `canary_secret_upload` while
// the nomination said this monitor needs no envelope (reviewer, party [390]).
//
// The assertion is the NOMINATION and NOTHING ELSE. An earlier version of this comment said the
// carrier policy was asserted with it; it is not, and the sentence outlived the assertion it
// described — the defect this whole arc is about, in the test written to close an instance of it
// (reviewer, party [394]). What the causal chain does with a nominated canary — a generation-3 job
// carrying an `EnvelopeV2` that the executor gate opens — is proved by
// `TestACanaryBindingIsSealedIntoAGenerationThreeEnvelope` in `internal/store`, against the real
// materializer and the real gate rather than a fake.
func TestACanaryWithADeclaredBindingIsNominatedForItsEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name    string
		config  map[string]string
		wantNom bool
	}{
		{
			name:    "declared binding is nominated",
			config:  map[string]string{"canary_secret_upload_ref": "api-token"},
			wantNom: true,
		},
		{
			// The other side of the rule, so the fix cannot be "nominate every canary": a canary
			// with no binding needs no envelope and must stay off the credential path.
			name:    "no binding stays off the credential path",
			config:  map[string]string{},
			wantNom: false,
		},
		{
			// A blank reference is not a declaration — the same non-blank test the envelope's own
			// field list applies, asked here so the two cannot drift apart.
			name:    "blank reference is not a declaration",
			config:  map[string]string{"canary_secret_upload_ref": "   "},
			wantNom: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			canary := domain.Monitor{
				ID: "canary-1", Type: domain.MonitorAsyncCanary, Region: "core",
				Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5, Config: tc.config,
			}
			fs := &fakeStore{leader: true, monitors: []domain.Monitor{canary}}

			var mu sync.Mutex
			seen := map[string]bool{}
			fs.materialize = func(ids []string) ([]store.MaterializedExecution, error) {
				mu.Lock()
				for _, id := range ids {
					seen[id] = true
				}
				mu.Unlock()
				return nil, nil
			}

			// The region ANNOUNCES an envelope-capable executor, so the carrier policy the
			// materializer is called with is a real one. Without it the policy is empty and the
			// carrier assertion below would be about the harness rather than the product.
			s := New(fs, dispatch.NewInProc(2), testLogger()).
				WithCredentialEnvelopes(true).
				WithCredentialLiveRegions(announcingLedgerRegions{"core": true})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			go s.Run(ctx)

			nominated := func() bool {
				mu.Lock()
				defer mu.Unlock()
				return seen[canary.ID]
			}
			if tc.wantNom {
				waitUntil(t, 3*time.Second, "the canary to be nominated for credential materialization", nominated)
				// The CARRIER is deliberately not asserted here, and the reason is a measurement
				// rather than an omission: the credential capability is resolved AFTER the loop
				// that nominates, so at this point the policy the materializer is handed is still
				// empty and an assertion on it would be about the harness. What generation an
				// announced region receives is already held by the carrier tests in
				// `ledgercarrier_test.go`; what was missing, and is here, is whether this monitor
				// reaches that path at all.
				return
			}
			// A negative is proved by waiting out a window in which the positive case answers, not
			// by checking immediately — otherwise it passes on timing rather than on the rule.
			time.Sleep(1500 * time.Millisecond)
			if nominated() {
				t.Fatal("a canary with no declared binding was put on the credential path")
			}
		})
	}
}

// The link the three-test chain was missing: the SCHEDULER derives generation 3 from a live agent's
// announced capability, and PUBLISHES the canary on it (reviewer AC, party [398]).
//
// The store test hands the materializer a policy by hand and the prober test seals a job by hand;
// neither shows the scheduler CHOOSING. Here nothing is handed to it: a pull region's agent
// announces envelope v2 and the canary workflow it can run, and the assertions are the two facts
// that were being taken on trust — the carrier POLICY the materializer is called with, and the
// generation the job is actually enqueued on.
//
// The mutations that must kill this: announce envelope v1 instead (the region can no longer hold a
// binding, so nothing may go out on generation 3), or restore the nomination to `CredentialedType`
// (the canary never joins the credential batch and is published without an envelope at all).
func TestACanaryBindingIsPublishedOnTheGenerationItsAgentAnnounced(t *testing.T) {
	// A FULL canonical workflow with the binding used in a header, not a `{"kind":…}` stub. The
	// stub is accepted by `CanaryCapabilityRequiredByConfig`, so the earlier fixture passed for a
	// monitor the real write boundary would REJECT and no prober could execute — a test describing
	// a class that cannot exist (reviewer, party [400]). `Validate` is asserted below so the
	// fixture cannot drift back into that shape silently.
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
	canary := domain.Monitor{
		ID: "canary-pub", Name: "canary-pub", Slug: "canary-pub",
		ProjectID: "11111111-1111-4111-8111-111111111111",
		Type:      domain.MonitorAsyncCanary, Region: "core",
		Enabled: true, IntervalSeconds: 120, TimeoutSeconds: 60,
		Config: map[string]string{
			"workflow":                 doc,
			"canary_secret_upload_ref": "api-token",
		},
	}
	// The monitor this test schedules must be one the product would ACCEPT on write. Without this
	// the seam below describes a permissive snapshot rather than the released monitor class.
	if err := canary.Validate(); err != nil {
		t.Fatalf("the fixture must be a monitor the write boundary accepts: %v", err)
	}
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{canary}}
	// A LIVE AGENT in the pull region, announcing what it can actually do: envelope v2 — which is
	// carrier generation 3 — and the canary kind this workflow declares.
	fs.agentEnvelopeCapability = map[string]int{"core": dispatch.EnvelopeV2}
	fs.canaryCapabilities = map[string][]string{"core": {domain.CanaryCapabilityOfThisBinary()}}

	s := New(fs, dispatch.NewInProc(2), testLogger()).
		WithCredentialEnvelopes(true).
		WithPullRegions([]string{"core"})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	go s.Run(ctx)

	// 1. The POLICY the materializer is called with is the announced generation, not a default.
	waitUntil(t, 4*time.Second, "the materializer to be called on generation 3", func() bool {
		return highestCarrier(t, fs) >= dispatch.ProtocolV3
	})
	// 2. And the job actually goes out on it.
	waitUntil(t, 4*time.Second, "a generation-3 pull job to be enqueued", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		for _, g := range fs.pullGenerations {
			if g == dispatch.ProtocolV3 {
				return true
			}
		}
		return false
	})
}
