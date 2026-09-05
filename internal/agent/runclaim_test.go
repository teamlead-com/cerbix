package agent

import (
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

// FR-032 phase C — the pull agent claims a whole BATCH before it probes any of it (§8.4).
//
// The AMQP worker publishes one claim per job because it takes jobs one at a time; an agent claims
// a batch in one poll, so batching the claims is the same rule expressed for the transport that
// exists here. It costs one round trip per POLL rather than per job — §8.4's "one genuinely new
// per-run round trip" is an upper bound, and the pull transport comes in under it.

// recordingServer captures every POST the agent makes, in order, so a test can assert the ORDER of
// the claim post against the probes rather than merely that both happened.
type recordingServer struct {
	mu       sync.Mutex
	posts    []recordedPost
	probesAt []int // how many posts had been made when each probe started
}

type recordedPost struct {
	path    string
	results []domain.Heartbeat
	ack     []string
}

func (s *recordingServer) record(path string, body struct {
	Results []domain.Heartbeat `json:"results"`
	Ack     []string           `json:"ack"`
}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posts = append(s.posts, recordedPost{path: path, results: body.Results, ack: body.Ack})
}

func (s *recordingServer) noteProbe() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probesAt = append(s.probesAt, len(s.posts))
}

func (s *recordingServer) snapshot() ([]recordedPost, []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedPost(nil), s.posts...), append([]int(nil), s.probesAt...)
}

// countingRunner tells the server when each probe begins.
type countingRunner struct{ srv *recordingServer }

func (r countingRunner) Run(_ context.Context, m domain.Monitor) domain.Heartbeat {
	r.srv.noteProbe()
	return domain.Heartbeat{MonitorID: m.ID, Up: true}
}

func (countingRunner) Supports(domain.MonitorType) bool { return true }

func claimTestJob(id string) dispatch.CheckJob {
	at := time.Now().UTC()
	return dispatch.CheckJob{
		Monitor:         domain.Monitor{ID: id, Type: domain.MonitorHTTP, Region: "pull1", ExecutionRevision: 4},
		ProtocolVersion: dispatch.ProtocolV4,
		JobID:           "11111111-1111-4111-8111-11111111111" + id[len(id)-1:],
		IssuedAt:        at,
		DueAt:           at.Add(-time.Minute),
	}
}

func claimTestServer(t *testing.T, rec *recordingServer, jobs []dispatch.CheckJob) *httptest.Server {
	t.Helper()
	served := false
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/v4/jobs":
			if served {
				_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []json.RawMessage{}, "tokens": []string{}, "protocol_versions": []int{}})
				return
			}
			served = true
			raw := make([]json.RawMessage, 0, len(jobs))
			tokens := make([]string, 0, len(jobs))
			versions := make([]int, 0, len(jobs))
			for i, job := range jobs {
				body, _ := json.Marshal(job)
				raw = append(raw, body)
				tokens = append(tokens, "lease-"+string(rune('a'+i)))
				versions = append(versions, dispatch.ProtocolV4)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jobs": raw, "tokens": tokens, "protocol_versions": versions})
		case "/api/v1/agent/results":
			var body struct {
				Results []domain.Heartbeat `json:"results"`
				Ack     []string           `json:"ack"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			rec.record(r.URL.Path, body)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestTheAgentClaimsTheWholeBatchBeforeProbingAnyOfIt(t *testing.T) {
	rec := &recordingServer{}
	jobs := []dispatch.CheckJob{claimTestJob("m1"), claimTestJob("m2")}
	srv := claimTestServer(t, rec, jobs)
	defer srv.Close()

	// The envelope floor still applies to a v4 claim, because that claim returns every OLDER
	// generation too — envelope-bearing ones included. Without a keyring this agent polls the v1
	// endpoint and never sees a generation-4 job at all, which is B1's rule and not a test detail.
	a := New(srv.URL, "tok", "pull1", countingRunner{srv: rec}, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithCredentialKeyring(testWorkerRing(t))
	a.poll(context.Background())

	posts, probesAt := rec.snapshot()
	if len(posts) != 2 {
		t.Fatalf("%d posts were made, want 2 — one for the batch's claims and one for its results: %+v",
			len(posts), posts)
	}
	claims, results := posts[0], posts[1]
	if len(claims.results) != 2 {
		t.Fatalf("the first post carried %d messages, want one claim per job", len(claims.results))
	}
	for _, hb := range claims.results {
		if hb.Claim == nil {
			t.Fatalf("the first post carried a non-claim: %+v", hb)
		}
		if hb.JobID == "" || hb.DueAt.IsZero() {
			t.Errorf("a claim lost its correlation: %+v", hb)
		}
	}
	// The claim post carries NO ack tokens: acking DELETES the leased job, and these jobs have not
	// run yet. The mutation this kills is reusing the results post's argument list.
	if len(claims.ack) != 0 {
		t.Fatalf("the claim post acked %v — acking deletes the leased job, and none of them has run",
			claims.ack)
	}
	if len(results.ack) != 2 {
		t.Errorf("the results post acked %v, want both leases", results.ack)
	}
	// EVERY probe began after the claims were posted. This is the assertion the whole harness
	// exists for: a claim sent after its probe would never exist for a run that crashed mid-probe.
	if len(probesAt) != 2 {
		t.Fatalf("%d probes ran, want 2", len(probesAt))
	}
	for i, posts := range probesAt {
		if posts < 1 {
			t.Fatalf("probe %d began before any claim was posted", i)
		}
	}
}

// A batch of jobs carrying no window produces NO request at all: a region below the ledger carrier
// pays nothing. The mutation that must kill this: post unconditionally, which sends an empty
// results body every poll.
func TestTheAgentSendsNoClaimRequestForABatchWithNoWindows(t *testing.T) {
	rec := &recordingServer{}
	plain := dispatch.CheckJob{Monitor: domain.Monitor{ID: "m1", Type: domain.MonitorHTTP, Region: "pull1"}}
	srv := claimTestServer(t, rec, []dispatch.CheckJob{plain})
	defer srv.Close()

	a := New(srv.URL, "tok", "pull1", countingRunner{srv: rec}, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithCredentialKeyring(testWorkerRing(t))
	a.poll(context.Background())

	posts, _ := rec.snapshot()
	if len(posts) != 1 {
		t.Fatalf("%d posts were made for a batch with no windows, want 1 (results only): %+v",
			len(posts), posts)
	}
	for _, hb := range posts[0].results {
		if hb.Claim != nil {
			t.Fatalf("a claim was sent for a job with no window: %+v", hb)
		}
	}
}

// A failed claim post does NOT stop the batch, and is NOT buffered.
//
// Best-effort is a specification (§8.4): if the publish fails the probe still runs, so a missing
// claim means "unwitnessed" and never "did not happen". And a claim is a diagnostic whose value is
// its timing — replaying one from the edge buffer an hour later would assert that the run started
// then, which is why `bufferResults` is not on this path.
func TestAFailedClaimPostNeitherStopsTheBatchNorIsBuffered(t *testing.T) {
	rec := &recordingServer{}
	var posts int
	var mu sync.Mutex
	jobs := []dispatch.CheckJob{claimTestJob("m1")}
	served := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/v4/jobs":
			if served {
				_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []json.RawMessage{}, "tokens": []string{}, "protocol_versions": []int{}})
				return
			}
			served = true
			body, _ := json.Marshal(jobs[0])
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs": []json.RawMessage{body}, "tokens": []string{"lease"},
				"protocol_versions": []int{dispatch.ProtocolV4}})
		case "/api/v1/agent/results":
			mu.Lock()
			posts++
			first := posts == 1
			mu.Unlock()
			if first {
				// The claim post fails.
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var body struct {
				Results []domain.Heartbeat `json:"results"`
				Ack     []string           `json:"ack"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			rec.record(r.URL.Path, body)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := New(srv.URL, "tok", "pull1", countingRunner{srv: rec}, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithCredentialKeyring(testWorkerRing(t))
	a.poll(context.Background())

	recorded, probesAt := rec.snapshot()
	if len(probesAt) != 1 {
		t.Fatalf("the batch did not run after its claim post failed: %d probes", len(probesAt))
	}
	if len(recorded) != 1 || len(recorded[0].results) != 1 {
		t.Fatalf("the results post did not carry the probe's result: %+v", recorded)
	}
	if recorded[0].results[0].Claim != nil {
		t.Error("the failed claim was replayed alongside the result")
	}
	if len(a.buf) != 0 {
		t.Fatalf("%d messages were buffered: a claim's value is its timing, and replaying one later "+
			"asserts that the run started then", len(a.buf))
	}
}

// The regression for a defect B1 shipped and could NOT observe: the ceiling on what the server may
// stamp must be the generation of the endpoint the agent actually called.
//
// `resolveStampedGenerations` read `claimGeneration()`, which is derived from the ENVELOPE
// capability and caps at 3, while `jobClaimGeneration()` sends a capable agent to `/v4/jobs`. The
// first real generation-4 row a core stamped therefore made the agent reject the WHOLE response —
// "stamped generation 4 for job 0, outside 1..3" — claim nothing, and try again next poll. A pull
// region on the ledger carrier would have stopped executing entirely, which is the same shape as
// the generation-3 failure `scheduler.go` records: work placed where nothing will take it.
//
// B1 could not see it because nothing stamped 4 (its config refused the flag) and its own tests
// omitted `protocol_versions`, taking the legacy fallback for an older core. It surfaced the moment
// phase C put a real v4 batch through the function.
//
// The mutation that must kill this: restore `claimGeneration()` as the ceiling.
func TestAStampedGenerationFourIsAcceptedFromTheEndpointThatServesIt(t *testing.T) {
	rec := &recordingServer{}
	srv := claimTestServer(t, rec, []dispatch.CheckJob{claimTestJob("m1")})
	defer srv.Close()

	a := New(srv.URL, "tok", "pull1", countingRunner{srv: rec}, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithCredentialKeyring(testWorkerRing(t))
	a.poll(context.Background())

	_, probesAt := rec.snapshot()
	if len(probesAt) != 1 {
		t.Fatalf("a generation-4 row stamped by the server was not executed (%d probes): the agent "+
			"rejected the whole claim response, so the region stops executing while the carrier is on",
			len(probesAt))
	}

	// And the ceiling still BINDS: a generation above the endpoint's own is refused, because the
	// stamp is what tells the executor which contract applies and a server claiming more than the
	// endpoint serves is a wiring fault, not a rolling upgrade.
	if _, err := a.resolveStampedGenerationsAt(dispatch.ProtocolV4, 1, 1, json.RawMessage(`[5]`)); err == nil {
		t.Fatal("a stamp above the endpoint's generation was accepted")
	}
	// The converse for the older endpoints, unchanged: a v3 claim may not be handed a v4 row.
	if _, err := a.resolveStampedGenerationsAt(dispatch.ProtocolV3, 1, 1, json.RawMessage(`[4]`)); err == nil {
		t.Fatal("a generation-4 stamp was accepted from the v3 endpoint")
	}
}
