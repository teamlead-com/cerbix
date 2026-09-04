package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/teamlead-com/cerbix/internal/api"
)

// FR-032 invariants 10g (pull half) and 10h.
//
// The generation-4 endpoint is gated on its OWN capability, and a v3 claim cannot reach a
// generation-4 row: the claim predicate selects `protocol_version <= 3`, so the row is outside the
// result set rather than filtered out of it.

func ledgerHandler(t *testing.T) http.Handler {
	t.Helper()
	fs := seededStore()
	fs.pullJobs = map[string][]fakePullRow{"secure": {
		{payload: []byte(`{"Monitor":{"id":"plain"}}`), generation: 1},
		{payload: []byte(`{"Monitor":{"id":"identified"}}`), generation: 4},
	}}
	return api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithAgentToken("s3cr3t").AgentRouter()
}

func claimedIDs(t *testing.T, body string) []string {
	t.Helper()
	var out struct {
		Jobs []json.RawMessage `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode claim response: %v (%s)", err, body)
	}
	ids := make([]string, 0, len(out.Jobs))
	for _, raw := range out.Jobs {
		var job struct {
			Monitor struct {
				ID string `json:"id"`
			} `json:"Monitor"`
		}
		if err := json.Unmarshal(raw, &job); err != nil {
			t.Fatalf("decode job: %v", err)
		}
		ids = append(ids, job.Monitor.ID)
	}
	return ids
}

// 10g, pull half: physical unreachability, not a filter.
func TestAV3ClaimCannotReachAGenerationFourRow(t *testing.T) {
	rec := agentReqWithHeaders(ledgerHandler(t), http.MethodGet, "/api/v1/agent/v3/jobs?region=secure",
		"s3cr3t", "", map[string]string{"X-Cerbix-Credential-Envelope": "2"})
	if rec.Code != http.StatusOK {
		t.Fatalf("v3 claim status %d: %s", rec.Code, rec.Body.String())
	}
	for _, id := range claimedIDs(t, rec.Body.String()) {
		if id == "identified" {
			t.Fatal("a v3 claim returned the generation-4 row; its predicate must leave that row " +
				"outside the result set, not exclude it after selection")
		}
	}
}

// The announced ledger capability is a CLOSED domain: 0 or 1. It is persisted into an existential
// query that decides whether core may raise a region to generation 4, so a value nobody defined
// must be refused at the boundary rather than stored and interpreted later.
func TestTheAnnouncedLedgerCapabilityIsAClosedDomain(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{name: "absent", body: `{"capabilities":{"credential_envelope":2}}`, want: http.StatusNoContent},
		{name: "zero", body: `{"capabilities":{"credential_envelope":2,"ledger":0}}`, want: http.StatusNoContent},
		{name: "one", body: `{"capabilities":{"credential_envelope":2,"ledger":1}}`, want: http.StatusNoContent},
		{name: "two", body: `{"capabilities":{"credential_envelope":2,"ledger":2}}`, want: http.StatusBadRequest},
		{name: "negative", body: `{"capabilities":{"credential_envelope":2,"ledger":-1}}`, want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := agentReqWithHeaders(ledgerHandler(t), http.MethodPost,
				"/api/v1/agent/heartbeat?region=secure&agent_id=a1", "s3cr3t", tc.body, nil)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// The v4 endpoint requires the LEDGER capability, declared per claim. Announcing the credential
// envelope is not that declaration: job identity applies to every monitor, so an agent with no
// secrets must be able to announce one without the other (§13.0).
func TestTheGenerationFourEndpointRequiresTheLedgerCapability(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{name: "no-declaration", headers: nil, want: http.StatusBadRequest},
		{name: "credential-envelope-is-not-a-ledger-declaration",
			headers: map[string]string{"X-Cerbix-Credential-Envelope": "2"}, want: http.StatusBadRequest},
		// A v4 claim also returns every OLDER generation, envelope-bearing ones included, so the
		// envelope floor still applies. The two capabilities are announced independently; this is
		// the claim's cumulative RANGE needing what its widest older generation needs.
		{name: "ledger-without-the-envelope-floor",
			headers: map[string]string{"X-Cerbix-Ledger": "1"}, want: http.StatusBadRequest},
		{name: "both-declared", headers: map[string]string{
			"X-Cerbix-Ledger": "1", "X-Cerbix-Credential-Envelope": "2"}, want: http.StatusOK},
		// EXACT, not a floor. There is no ledger capability above 1 in this generation, so a
		// higher declaration is a claim about something nobody has defined — and the heartbeat
		// that announces the same capability is already closed to 0|1.
		{name: "a-higher-ledger-declaration-is-not-a-silent-yes", headers: map[string]string{
			"X-Cerbix-Ledger": "2", "X-Cerbix-Credential-Envelope": "2"}, want: http.StatusBadRequest},
		{name: "a-nonsense-ledger-declaration", headers: map[string]string{
			"X-Cerbix-Ledger": "999", "X-Cerbix-Credential-Envelope": "2"}, want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := agentReqWithHeaders(ledgerHandler(t), http.MethodGet,
				"/api/v1/agent/v4/jobs?region=secure", "s3cr3t", "", tc.headers)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// And a declared v4 claim does reach it — without which the test above cannot tell a working
// endpoint from one that refuses everything.
func TestADeclaredGenerationFourClaimReachesTheRow(t *testing.T) {
	rec := agentReqWithHeaders(ledgerHandler(t), http.MethodGet, "/api/v1/agent/v4/jobs?region=secure",
		"s3cr3t", "", map[string]string{"X-Cerbix-Ledger": "1", "X-Cerbix-Credential-Envelope": "2"})
	if rec.Code != http.StatusOK {
		t.Fatalf("v4 claim status %d: %s", rec.Code, rec.Body.String())
	}
	var saw bool
	for _, id := range claimedIDs(t, rec.Body.String()) {
		saw = saw || id == "identified"
	}
	if !saw {
		t.Fatalf("the v4 claim did not return the generation-4 row: %s", rec.Body.String())
	}
}
