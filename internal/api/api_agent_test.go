package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/api"
)

func agentReq(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAgentEndpoints(t *testing.T) {
	fs := seededStore()
	fs.pullJobs = map[string][][]byte{
		"geo3": {[]byte(`{"Monitor":{"id":"m1","type":"http","target":"https://x","region":"geo3"}}`)},
	}
	sink := &fakeResultSink{}
	h := api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).
		WithAgentToken("s3cr3t").WithResultSink(sink).AgentRouter()

	// Wrong / missing token → 401.
	if rec := agentReq(h, http.MethodGet, "/api/v1/agent/jobs?region=geo3", "wrong", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token = %d, want 401", rec.Code)
	}

	// Claim jobs for geo3 → one job, delivered once.
	rec := agentReq(h, http.MethodGet, "/api/v1/agent/jobs?region=geo3", "s3cr3t", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("claim = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var claimed struct {
		Jobs   []json.RawMessage `json:"jobs"`
		Tokens []string          `json:"tokens"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &claimed)
	if len(claimed.Jobs) != 1 || len(claimed.Tokens) != 1 || claimed.Tokens[0] == "" {
		t.Fatalf("claimed %d jobs / %d tokens, want 1 job with a lease token", len(claimed.Jobs), len(claimed.Tokens))
	}
	if again := agentReq(h, http.MethodGet, "/api/v1/agent/jobs?region=geo3", "s3cr3t", ""); strings.Contains(again.Body.String(), "Monitor") {
		t.Fatalf("re-claim returned a job, want none: %s", again.Body.String())
	}

	// Post a result with the lease token as ack → it reaches the sink AND the job is
	// acked (deleted). m1 lives in core, so the agent scopes the post to region=core.
	body := `{"results":[{"monitor_id":"m1","up":true,"latency_ms":12,"code":200}],"ack":["` + claimed.Tokens[0] + `"]}`
	if rec := agentReq(h, http.MethodPost, "/api/v1/agent/results?region=core", "s3cr3t", body); rec.Code != http.StatusOK {
		t.Fatalf("results = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if len(fs.acked) != 1 || fs.acked[0] != claimed.Tokens[0] {
		t.Fatalf("results handler acked %v, want [%s]", fs.acked, claimed.Tokens[0])
	}
	// A region-less result post is rejected — every agent ingest is region-scoped.
	if rec := agentReq(h, http.MethodPost, "/api/v1/agent/results", "s3cr3t", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("region-less results = %d, want 400", rec.Code)
	}
	if hb, ok := sink.last(); !ok || hb.MonitorID != "m1" || !hb.Up {
		t.Fatalf("sink did not receive the heartbeat: %+v ok=%v", hb, ok)
	}

	// Heartbeat records the region as live.
	if rec := agentReq(h, http.MethodPost, "/api/v1/agent/heartbeat?region=geo3&agent_id=a1", "s3cr3t", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("heartbeat = %d, want 204", rec.Code)
	}
	if fs.agentHeartbeats["geo3"] != "a1" {
		t.Fatalf("heartbeat not recorded: %#v", fs.agentHeartbeats)
	}
}

type fakeWaiter struct{ onWait func() }

func (f fakeWaiter) Wait(_ context.Context, _ string, _ time.Duration) {
	if f.onWait != nil {
		f.onWait()
	}
}

func TestAgentJobsLongPoll(t *testing.T) {
	fs := seededStore()
	fs.pullJobs = map[string][][]byte{} // empty initially
	// The waiter simulates a NOTIFY arriving mid-hold: a job appears, then the handler
	// re-claims and returns it.
	waiter := fakeWaiter{onWait: func() {
		fs.pullJobs["geo3"] = [][]byte{[]byte(`{"Monitor":{"id":"m1"}}`)}
	}}
	h := api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).
		WithAgentToken("s3cr3t").WithPullWaiter(waiter).AgentRouter()

	rec := agentReq(h, http.MethodGet, "/api/v1/agent/jobs?region=geo3", "s3cr3t", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("long-poll jobs = %d, want 200", rec.Code)
	}
	var out struct {
		Jobs []json.RawMessage `json:"jobs"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Jobs) != 1 {
		t.Fatalf("long-poll returned %d jobs, want 1 (re-claim after wake)", len(out.Jobs))
	}
}

func TestAgentTestEndpoints(t *testing.T) {
	fs := seededStore()
	fs.pullTests = map[string]fakePullTest{
		"t1": {region: "geo2", payload: []byte(`{"Monitor":{"id":"m1"}}`)},
	}
	h := api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithAgentToken("s3cr3t").AgentRouter()

	// Claim the pending test for geo2.
	rec := agentReq(h, http.MethodGet, "/api/v1/agent/tests?region=geo2", "s3cr3t", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("claim test = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Test *struct {
			ID  string          `json:"id"`
			Job json.RawMessage `json:"job"`
		} `json:"test"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Test == nil || out.Test.ID != "t1" || len(out.Test.Job) == 0 {
		t.Fatalf("claimed test = %+v", out.Test)
	}
	// A region with no test gets an empty response.
	if rec := agentReq(h, http.MethodGet, "/api/v1/agent/tests?region=nope", "s3cr3t", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"test":null`) {
		t.Fatalf("empty test claim = %d %s", rec.Code, rec.Body.String())
	}
	// A cross-region test-result post (right id, wrong region) does not populate the
	// test — the store scopes the write to the agent's own region.
	if rec := agentReq(h, http.MethodPost, "/api/v1/agent/test-results?region=nope", "s3cr3t", `{"id":"t1","result":{"up":true}}`); rec.Code != http.StatusNoContent {
		t.Fatalf("cross-region test result = %d, want 204", rec.Code)
	}
	if string(fs.pullTests["t1"].result) != "" {
		t.Fatalf("cross-region result must not be stored: %+v", fs.pullTests["t1"])
	}
	// A region-less test-result post is rejected.
	if rec := agentReq(h, http.MethodPost, "/api/v1/agent/test-results", "s3cr3t", `{"id":"t1","result":{"up":true}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("region-less test result = %d, want 400", rec.Code)
	}
	// Post the result for the test's own region.
	if rec := agentReq(h, http.MethodPost, "/api/v1/agent/test-results?region=geo2", "s3cr3t", `{"id":"t1","result":{"up":true,"code":200}}`); rec.Code != http.StatusNoContent {
		t.Fatalf("post test result = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	if string(fs.pullTests["t1"].result) == "" {
		t.Fatalf("result not stored: %+v", fs.pullTests["t1"])
	}
}

func TestAgentBackfill(t *testing.T) {
	fs := seededStore()
	h := api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithAgentToken("s3cr3t").AgentRouter()
	body := `{"results":[{"monitor_id":"m1","ts":"2026-02-01T00:00:00Z","up":false,"msg":"down"},{"monitor_id":"m1","ts":"2026-02-01T00:01:00Z","up":true}]}`
	rec := agentReq(h, http.MethodPost, "/api/v1/agent/backfill?region=core", "s3cr3t", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("backfill = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if len(fs.backfilled) != 2 {
		t.Fatalf("backfilled = %d, want 2 (historical bulk, bypassing pipeline)", len(fs.backfilled))
	}
	// A region-less backfill is rejected — historical ingest is region-scoped too.
	if rec := agentReq(h, http.MethodPost, "/api/v1/agent/backfill", "s3cr3t", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("region-less backfill = %d, want 400", rec.Code)
	}
	// Unauthed is rejected.
	if rec := agentReq(h, http.MethodPost, "/api/v1/agent/backfill?region=core", "", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthed backfill = %d, want 401", rec.Code)
	}
}

func TestAgentResultsRegionScope(t *testing.T) {
	fs := seededStore() // mon1 belongs to region core
	sink := &fakeResultSink{}
	h := api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithAgentToken("s3cr3t").WithResultSink(sink).AgentRouter()
	body := `{"results":[{"monitor_id":"mon1","up":true}]}`
	// Posting mon1 (core) under region=geo3 is rejected: cross-region forgery.
	if rec := agentReq(h, http.MethodPost, "/api/v1/agent/results?region=geo3", "s3cr3t", body); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-region results = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	// Posting under the monitor's own region is accepted.
	if rec := agentReq(h, http.MethodPost, "/api/v1/agent/results?region=core", "s3cr3t", body); rec.Code != http.StatusOK {
		t.Fatalf("same-region results = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func TestAgentDBTokens(t *testing.T) {
	fs := seededStore()
	fs.pullJobs = map[string][][]byte{"geo3": {[]byte(`{"Monitor":{"id":"m1"}}`)}}
	admin := newHandler(fs) // authed Router for issuing/revoking
	agentH := api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithAgentDBTokens().AgentRouter()

	// Non-admin may not issue.
	if rec := do(admin, o1Viewer, http.MethodPost, "/api/v1/agent-tokens", `{"name":"a","region":"geo3"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer issue = %d, want 403", rec.Code)
	}
	// Global admin issues a geo3 token.
	rec := do(admin, globalAdmin, http.MethodPost, "/api/v1/agent-tokens", `{"name":"a","region":"geo3"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var issued struct{ ID, Token string }
	_ = json.Unmarshal(rec.Body.Bytes(), &issued)
	if issued.Token == "" || issued.ID == "" {
		t.Fatalf("issued = %+v", issued)
	}
	// The DB token authorizes geo3 claims...
	if rec := agentReq(agentH, http.MethodGet, "/api/v1/agent/jobs?region=geo3", issued.Token, ""); rec.Code != http.StatusOK {
		t.Fatalf("db-token claim = %d, want 200", rec.Code)
	}
	// ...but not another region.
	if rec := agentReq(agentH, http.MethodGet, "/api/v1/agent/jobs?region=geo5", issued.Token, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("db-token wrong region = %d, want 401", rec.Code)
	}
	// Revoke → the token stops working.
	if rec := do(admin, globalAdmin, http.MethodDelete, "/api/v1/agent-tokens/"+issued.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204", rec.Code)
	}
	if rec := agentReq(agentH, http.MethodGet, "/api/v1/agent/jobs?region=geo3", issued.Token, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token = %d, want 401", rec.Code)
	}
}

func TestAgentPerRegionTokenScope(t *testing.T) {
	fs := seededStore()
	fs.pullJobs = map[string][][]byte{"geo3": {[]byte(`{"Monitor":{"id":"m1"}}`)}}
	h := api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).
		WithResultSink(&fakeResultSink{}).
		WithAgentRegionTokens(map[string]string{"geo3": "tok3", "geo5": "tok5"}).AgentRouter()

	// geo3 token claims geo3 → 200.
	if rec := agentReq(h, http.MethodGet, "/api/v1/agent/jobs?region=geo3", "tok3", ""); rec.Code != http.StatusOK {
		t.Fatalf("geo3 token on geo3 = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	// geo3 token may NOT claim geo5 → 401.
	if rec := agentReq(h, http.MethodGet, "/api/v1/agent/jobs?region=geo5", "tok3", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("geo3 token on geo5 = %d, want 401", rec.Code)
	}
	// A per-region token posts results for its OWN region → 200.
	if rec := agentReq(h, http.MethodPost, "/api/v1/agent/results?region=geo5", "tok5", `{"results":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("geo5 token results on geo5 = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	// A per-region token may NOT post results for another region → 401 (the old
	// region-less bypass, where any per-region token authorized any results, is closed).
	if rec := agentReq(h, http.MethodPost, "/api/v1/agent/results?region=geo3", "tok5", `{"results":[]}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("geo5 token results on geo3 = %d, want 401", rec.Code)
	}
	// A region-less results post is rejected outright (only the catch-all token even
	// reaches the handler, and the handler requires a region).
	if rec := agentReq(h, http.MethodPost, "/api/v1/agent/results", "tok5", `{"results":[]}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("region-less per-region token results = %d, want 401", rec.Code)
	}
	// An unknown token is rejected on results too.
	if rec := agentReq(h, http.MethodPost, "/api/v1/agent/results?region=geo5", "nope", `{"results":[]}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token results = %d, want 401", rec.Code)
	}
}

func TestAgentEndpointsDisabledWithoutToken(t *testing.T) {
	h := api.New(seededStore(), slog.New(slog.NewTextHandler(io.Discard, nil)), 8).AgentRouter()
	if rec := agentReq(h, http.MethodGet, "/api/v1/agent/jobs?region=geo3", "anything", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("no token configured = %d, want 404", rec.Code)
	}
}
