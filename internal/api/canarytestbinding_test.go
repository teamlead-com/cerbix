package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// A canary that declares a binding is not testable before it is saved, and the refusal says why.
//
// The sibling of iter-0180's nomination defect, found by asking where else a TYPE-level predicate
// answers a MONITOR-level question. `MaterializeTestExecutionConfig` returns a generation-1 job —
// no envelope — for every type `CredentialedType` reports false for, and it reports false for
// `async_canary`, whose bindings live in the workflow document rather than in a schema. The test
// path had the matching rule for a SCENARIO binding (FR-028 D10) and none for a canary one, so a
// canary declaring a binding was sent to a prober with nothing to resolve it and met a message about
// a missing envelope field somewhere far from the person who pressed Test.
//
// The mutation that must kill this: delete the canary arm of the refusal.
func TestACanaryDeclaringABindingIsNotTestableBeforeItIsSaved(t *testing.T) {
	// A tester IS wired: without one the handler answers 501 before reaching any rule, and the
	// assertion below would pass for the wrong reason on a deployment that cannot probe at all.
	h := api.New(seededStore(), slog.New(slog.NewTextHandler(io.Discard, nil)), 8).
		WithTester(fakeProbe{hb: domain.Heartbeat{Up: true}}).Router()

	// A REAL workflow document, because a canary without one is refused by ordinary validation
	// long before the rule under test — and a fixture that fails earlier proves nothing about it.
	// The binding is declared in the document and in its flat reference key, which is the pair the
	// product treats as one declaration.
	doc := canaryWorkflowJSON(t, func(w *domain.CanaryWorkflow) {
		w.Secrets = map[string]string{"upload": ""}
		w.Submit.Headers = []domain.CanaryHeader{{Name: "x-api-key", SecretRef: "upload"}}
	})
	payload, err := json.Marshal(map[string]any{
		"type": "async_canary", "region": "core",
		// The completion budget is 60 s, and validation requires the monitor's timeout to hold it.
		"interval_seconds": 120, "timeout_seconds": 60,
		"config": map[string]string{"workflow": doc, "canary_secret_upload_ref": "api-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors/test", string(payload))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a canary carrying a binding must be refused before the probe: %d (%s)",
			rec.Code, rec.Body.String())
	}
	// The refusal names the BINDING and tells the operator what to do, which is the whole
	// difference between this and the executor's "missing field" arriving later.
	if !strings.Contains(rec.Body.String(), "upload") ||
		!strings.Contains(rec.Body.String(), "save the monitor before testing it") {
		t.Fatalf("the refusal must name the binding and the way forward: %s", rec.Body.String())
	}
	// And it must not echo the secret's NAME from the inventory: the binding is the operator's
	// word, the reference is not theirs to leak.
	if strings.Contains(rec.Body.String(), "api-token") {
		t.Fatalf("the refusal echoed the secret reference: %s", rec.Body.String())
	}
}
