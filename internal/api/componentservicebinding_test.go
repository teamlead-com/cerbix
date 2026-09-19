package api_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/store"
)

const componentBindingID = "00000000-0000-4000-8000-000000000001"

// The reported defect, at the surface an operator meets it: adding a SERVICE-backed component to a
// status page answered `400 invalid JSON body`.
//
// The body was valid JSON. `openapi.yaml` declared `service_id` on `CreateComponent`, the SPA sent
// it, `domain.Component` carried it, the store inserted it — and the handler's body struct was the
// single place that did not name the field. `decodeJSON` disallows unknown fields, so the request
// died in the decoder with a message about malformed JSON, which sends whoever reads it after the
// wrong thing entirely.
//
// Worth recording beside the fix: `fakeStore.CreateComponent` in this package already derived the
// service source, with the comment "a service binding wins over a leftover monitor one". The test
// double implemented the contract the product did not — so nothing in this suite could fail.
//
// The mutation that must kill this: remove `ServiceID` from the create body struct.
func TestAServiceBackedComponentCanBeCreated(t *testing.T) {
	h := newHandler(seededStore())

	rec := do(h, o1Admin, http.MethodPost, "/api/v1/status-pages/sp1/components",
		`{"name":"Billing API","service_id":"`+componentBindingID+`","group":"Services","description":"availability","position":1}`)
	if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "invalid JSON body") {
		t.Fatalf("the decoder refused a valid body — `service_id` is unsupported again: %s", rec.Body.String())
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("create service component = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	// And the SOURCE is derived from the binding, which is what makes the component render as a
	// service rather than as an unbound manual row.
	if !strings.Contains(rec.Body.String(), `"source":"service"`) {
		t.Fatalf("a service binding must derive the service source: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"service_id":"`+componentBindingID+`"`) {
		t.Fatalf("the binding must survive the round trip: %s", rec.Body.String())
	}
}

// An infrastructure failure is not a bad request, and must not be reported as one.
//
// The store classifies: a binding that is absent or outside the organization is
// `ErrComponentBindingNotFound`, a page-scope or dormant-pair conflict is
// `ErrComponentConversionTarget`, and anything else is the database not answering. The first
// version of this repair collapsed every lookup error into `ErrNotFound`, so a connection failure
// reached the operator as `400 binding not found` — an
// answer that is not merely unhelpful but false, and one that would send them looking for a deleted
// service while the database was down (reviewer P1).
//
// The mutation that must kill this: return 400 for the default case in the handler's switch.
func TestAnInfrastructureFailureIsNotABadRequest(t *testing.T) {
	fs := seededStore()
	fs.createComponentErr = errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	h := newHandler(fs)

	rec := do(h, o1Admin, http.MethodPost, "/api/v1/status-pages/sp1/components",
		`{"name":"Billing API","service_id":"`+componentBindingID+`"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("a database failure = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	// And the cause is not echoed to the caller: `serverError` logs it and answers a fixed message.
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Fatalf("the response leaked the internal cause: %s", rec.Body.String())
	}
}

func TestComponentBindingIDsAreValidatedAtTheTransport(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		field string
	}{
		{name: "service", body: `{"name":"Billing API","service_id":"not-a-uuid"}`, field: "service_id"},
		{name: "monitor", body: `{"name":"Billing API","monitor_id":"not-a-uuid"}`, field: "monitor_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(newHandler(seededStore()), o1Admin, http.MethodPost,
				"/api/v1/status-pages/sp1/components", tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("malformed %s = %d, want 400: %s", tt.field, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.field+" must be a UUID") {
				t.Fatalf("response does not name malformed %s: %s", tt.field, rec.Body.String())
			}
		})
	}
}

func TestTheStoreOwnsMonitorBindingScopeValidation(t *testing.T) {
	h := newHandler(seededStore())
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/status-pages/sp1/components",
		`{"name":"API","monitor_id":"`+componentBindingID+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create monitor component = %d, want store-owned validation path: %s", rec.Code, rec.Body.String())
	}
}

func TestAnUnavailableBindingIsABadRequest(t *testing.T) {
	fs := seededStore()
	fs.createComponentErr = store.ErrComponentBindingNotFound
	rec := do(newHandler(fs), o1Admin, http.MethodPost, "/api/v1/status-pages/sp1/components",
		`{"name":"Billing API","service_id":"`+componentBindingID+`"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "binding not found") {
		t.Fatalf("missing binding = %d, want 400 binding not found: %s", rec.Code, rec.Body.String())
	}
}

func TestAPageDeletedDuringCreateRemainsNotFound(t *testing.T) {
	fs := seededStore()
	fs.createComponentErr = store.ErrNotFound
	rec := do(newHandler(fs), o1Admin, http.MethodPost, "/api/v1/status-pages/sp1/components",
		`{"name":"manual"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deleted page = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "binding not found") {
		t.Fatalf("a deleted page was mislabeled as a binding error: %s", rec.Body.String())
	}
}
