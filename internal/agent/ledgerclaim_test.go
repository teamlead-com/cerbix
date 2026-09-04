package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-032: announcement and consumption must be the SAME condition.
//
// An agent that announces `capabilities.ledger` without claiming generation 4 is worse than one
// that announces nothing: core is authorized to select a v4 row into a region that will never
// claim it, and the monitor has no outcome until the row's TTL — the generation-3 failure
// `scheduler.go` records, repeated one generation later.

func testWorkerRing(t *testing.T) *dispatch.CredentialKeyring {
	t.Helper()
	ring, err := dispatch.NewCredentialKeyring(
		dispatch.CredentialKeyMaterial{ID: "worker", Key: bytes.Repeat([]byte{1}, 32)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

type claimRecord struct {
	mu      sync.Mutex
	paths   []string
	ledger  []string
	handled int
}

func (c *claimRecord) note(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paths = append(c.paths, r.URL.Path)
	c.ledger = append(c.ledger, r.Header.Get("X-Cerbix-Ledger"))
	c.handled++
}

func (c *claimRecord) snapshot() ([]string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.paths...), append([]string(nil), c.ledger...)
}

// A capable agent claims the generation-4 endpoint, declares the ledger capability on the request
// itself, and executes the row it receives.
func TestACapableAgentClaimsGenerationFourAndExecutesTheRow(t *testing.T) {
	monitor := domain.Monitor{ID: "m1", Type: domain.MonitorHTTP, Target: "https://x", IntervalSeconds: 60}
	rec := &claimRecord{}
	var posted int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/v4/jobs":
			rec.note(r)
			body, _ := json.Marshal(dispatch.CheckJob{Monitor: monitor, ProtocolVersion: dispatch.ProtocolV4})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs": []json.RawMessage{body}, "tokens": []string{"lease"}})
		case "/api/v1/agent/results":
			posted++
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := New(srv.URL, "tok", "pull1", fixedRunner{hb: domain.Heartbeat{MonitorID: "m1", Up: true}},
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithCredentialKeyring(testWorkerRing(t))
	a.poll(context.Background())

	paths, ledger := rec.snapshot()
	if len(paths) != 1 || paths[0] != "/api/v1/agent/v4/jobs" {
		t.Fatalf("claim path %v, want /api/v1/agent/v4/jobs — an agent that announces the ledger "+
			"capability must claim the carrier it announced", paths)
	}
	if ledger[0] != "1" {
		t.Fatalf("X-Cerbix-Ledger on the claim was %q, want \"1\": the declaration is re-asserted "+
			"per request, not taken from the heartbeat", ledger[0])
	}
	if posted != 1 {
		t.Fatalf("the generation-4 row was claimed but not executed (%d results posted)", posted)
	}
	// Only NOW may it announce: the endpoint has answered. Before this claim it advertised
	// nothing, so core could not have selected a v4 row it had not yet proven it can take.
	if got := a.ledgerCapability(); got != 1 {
		t.Fatalf("after a served v4 claim the agent announces ledger=%d", got)
	}
}

// The announcement follows PROOF, not eligibility to try. A fresh agent has proven nothing.
func TestAnAgentAnnouncesNothingBeforeItsFirstServedV4Claim(t *testing.T) {
	a := New("http://x", "tok", "pull1", fixedRunner{}, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithCredentialKeyring(testWorkerRing(t))
	if got := a.ledgerCapability(); got != 0 {
		t.Fatalf("a fresh agent announces ledger=%d before any v4 claim has been served; core "+
			"would select a row it has not shown it can claim", got)
	}
	if !a.mayTryLedgerEndpoint() {
		t.Fatal("a fresh capable agent must still ATTEMPT v4 — trying costs one 404, announcing " +
			"costs an unclaimable row")
	}
}

// The rollout case §16.1 calls normal: an executor NEWER than its core. A core predating
// `agentJobsV4` answers 404, and the agent must fall back rather than stop claiming — with the
// retry immediate, so the poll is not lost.
func TestAnAgentNewerThanItsCoreFallsBackWithoutLosingThePoll(t *testing.T) {
	monitor := domain.Monitor{ID: "m1", Type: domain.MonitorHTTP, Target: "https://x", IntervalSeconds: 60}
	rec := &claimRecord{}
	var posted int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/v4/jobs":
			rec.note(r)
			w.WriteHeader(http.StatusNotFound) // a core that predates this endpoint
		case "/api/v1/agent/v3/jobs":
			rec.note(r)
			body, _ := json.Marshal(dispatch.CheckJob{Monitor: monitor, ProtocolVersion: dispatch.ProtocolV3})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs": []json.RawMessage{body}, "tokens": []string{"lease"}})
		case "/api/v1/agent/results":
			posted++
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := New(srv.URL, "tok", "pull1", fixedRunner{hb: domain.Heartbeat{MonitorID: "m1", Up: true}},
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithCredentialKeyring(testWorkerRing(t))

	a.poll(context.Background())
	paths, _ := rec.snapshot()
	if len(paths) != 2 || paths[0] != "/api/v1/agent/v4/jobs" || paths[1] != "/api/v1/agent/v3/jobs" {
		t.Fatalf("first poll took %v, want the v4 attempt then an immediate v3 retry", paths)
	}
	if posted != 1 {
		t.Fatalf("the fallback lost the poll: %d results posted, want 1", posted)
	}

	// The 404 is paid ONCE within the window: the second poll goes straight to the older path.
	a.poll(context.Background())
	paths, _ = rec.snapshot()
	if len(paths) != 3 || paths[2] != "/api/v1/agent/v3/jobs" {
		t.Fatalf("after the downgrade the agent took %v; the 404 must be paid once, not per poll", paths)
	}
	// And while downgraded it must announce NOTHING, or core would select a v4 row for a region
	// that is currently claiming v3.
	if got := a.ledgerCapability(); got != 0 {
		t.Fatalf("a downgraded agent still announces ledger=%d; the announcement and the claim read "+
			"one predicate precisely so they cannot disagree", got)
	}
}

// The rollout continues: core is upgraded UNDERNEATH the running agent. A permanent downgrade
// would leave this agent claiming v3 for the rest of its life while core, seeing an announcement
// it never withdrew, selected v4 for its region — the same unclaimable row, one step later
// (reviewer [432]).
func TestACoreUpgradedUnderARunningAgentIsFoundAgain(t *testing.T) {
	monitor := domain.Monitor{ID: "m1", Type: domain.MonitorHTTP, Target: "https://x", IntervalSeconds: 60}
	rec := &claimRecord{}
	var v4Live bool
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/v4/jobs":
			rec.note(r)
			mu.Lock()
			live := v4Live
			mu.Unlock()
			if !live {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			body, _ := json.Marshal(dispatch.CheckJob{Monitor: monitor, ProtocolVersion: dispatch.ProtocolV4})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs": []json.RawMessage{body}, "tokens": []string{"lease"}})
		case "/api/v1/agent/v3/jobs":
			rec.note(r)
			_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []json.RawMessage{}, "tokens": []string{}})
		case "/api/v1/agent/results":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	clock := time.Now()
	a := New(srv.URL, "tok", "pull1", fixedRunner{hb: domain.Heartbeat{MonitorID: "m1", Up: true}},
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithCredentialKeyring(testWorkerRing(t))
	a.now = func() time.Time { return clock }

	a.poll(context.Background()) // v4 → 404, falls back
	if got := a.ledgerCapability(); got != 0 {
		t.Fatalf("announcing ledger=%d while downgraded", got)
	}

	// Core is upgraded. Before the window expires the agent must NOT re-probe — the downgrade is
	// bounded, not absent.
	mu.Lock()
	v4Live = true
	mu.Unlock()
	a.poll(context.Background())
	paths, _ := rec.snapshot()
	if paths[len(paths)-1] != "/api/v1/agent/v3/jobs" {
		t.Fatalf("the agent re-probed inside the window: %v", paths)
	}

	// The window expires while core is STILL absent — the case that made the false announcement
	// periodic rather than permanent. Eligibility to try must not become an announcement.
	mu.Lock()
	v4Live = false
	mu.Unlock()
	clock = clock.Add(ledgerReprobeAfter + time.Second)
	a.poll(context.Background())
	if got := a.ledgerCapability(); got != 0 {
		t.Fatalf("the cooldown expired against a core that still 404s and the agent announced "+
			"ledger=%d; being allowed to TRY is not being allowed to ANNOUNCE", got)
	}
	paths, _ = rec.snapshot()
	if paths[len(paths)-2] != "/api/v1/agent/v4/jobs" {
		t.Fatalf("the agent did not re-probe after the window: %v", paths)
	}

	// Now core really is upgraded. The next probe is served, and only then does the announcement
	// come back — consumption first, announcement after it is proven.
	mu.Lock()
	v4Live = true
	mu.Unlock()
	clock = clock.Add(ledgerReprobeAfter + time.Second)
	a.poll(context.Background())
	paths, ledger := rec.snapshot()
	if paths[len(paths)-1] != "/api/v1/agent/v4/jobs" {
		t.Fatalf("after the window the agent took %v; an upgraded core must be found again", paths)
	}
	if ledger[len(ledger)-1] != "1" {
		t.Fatalf("the recovered claim carried X-Cerbix-Ledger=%q", ledger[len(ledger)-1])
	}
	if got := a.ledgerCapability(); got != 1 {
		t.Fatalf("the agent claims v4 again but announces ledger=%d", got)
	}
}

// The mirror of the upgrade: core is ROLLED BACK under an agent that had already proven v4. The
// announcement must be withdrawn in the same breath as the fallback, or core keeps selecting v4
// rows for a region that has gone back to claiming v3. Found by a surviving mutation — my tests
// only ever 404'd an agent that had never proven the endpoint, so clearing the proof was untested.
func TestACoreRolledBackUnderAProvenAgentWithdrawsTheAnnouncement(t *testing.T) {
	monitor := domain.Monitor{ID: "m1", Type: domain.MonitorHTTP, Target: "https://x", IntervalSeconds: 60}
	rec := &claimRecord{}
	var v4Live = true
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/v4/jobs":
			rec.note(r)
			mu.Lock()
			live := v4Live
			mu.Unlock()
			if !live {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			body, _ := json.Marshal(dispatch.CheckJob{Monitor: monitor, ProtocolVersion: dispatch.ProtocolV4})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs": []json.RawMessage{body}, "tokens": []string{"lease"}})
		case "/api/v1/agent/v3/jobs":
			rec.note(r)
			_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []json.RawMessage{}, "tokens": []string{}})
		case "/api/v1/agent/results":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	clock := time.Now()
	a := New(srv.URL, "tok", "pull1", fixedRunner{hb: domain.Heartbeat{MonitorID: "m1", Up: true}},
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithCredentialKeyring(testWorkerRing(t))
	a.now = func() time.Time { return clock }

	a.poll(context.Background())
	if got := a.ledgerCapability(); got != 1 {
		t.Fatalf("the agent proved v4 but announces ledger=%d", got)
	}

	// Core rolls back. The very next claim 404s, and the announcement must go with it.
	mu.Lock()
	v4Live = false
	mu.Unlock()
	a.poll(context.Background())
	if got := a.ledgerCapability(); got != 0 {
		t.Fatalf("core stopped serving v4 and the agent still announces ledger=%d; core would keep "+
			"selecting rows for a region that has gone back to claiming v3", got)
	}
	paths, _ := rec.snapshot()
	if paths[len(paths)-1] != "/api/v1/agent/v3/jobs" {
		t.Fatalf("after the rollback the agent took %v, want a fallback to v3", paths)
	}
}

// An agent that cannot open envelope v2 must NOT announce the ledger capability, because it must
// not claim generation 4: that claim also returns envelope-bearing rows it cannot read.
func TestAnAgentThatCannotOpenEnvelopesAnnouncesNoLedgerCapability(t *testing.T) {
	a := New("http://x", "tok", "pull1", fixedRunner{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := a.ledgerCapability(); got != 0 {
		t.Fatalf("ledgerCapability()=%d for an agent with no keyring; announcing what it cannot "+
			"claim authorizes selection into a region that will not claim it", got)
	}
	if got := a.jobClaimPath(); got != "/api/v1/agent/jobs" {
		t.Fatalf("job claim path %q for an agent with no capability", got)
	}
}

// B1 ships `agentJobsV4` and NO v4 test endpoint. The two paths are derived separately so the job
// claim can move ahead without redirecting test RPC to a route that does not exist.
func TestTheTestEndpointIsNotDraggedToGenerationFour(t *testing.T) {
	a := New("http://x", "tok", "pull1", fixedRunner{}, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithCredentialKeyring(testWorkerRing(t))
	if got := a.jobClaimPath(); got != "/api/v1/agent/v4/jobs" {
		t.Fatalf("job claim path %q, want the v4 endpoint", got)
	}
	if got := a.claimPath("tests"); got != "/api/v1/agent/v3/tests" {
		t.Fatalf("test claim path %q — B1 specifies no v4 test endpoint, so test RPC must stay on "+
			"the envelope-derived generation", got)
	}
}
