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

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

type fixedRunner struct{ hb domain.Heartbeat }

func (f fixedRunner) Run(context.Context, domain.Monitor) domain.Heartbeat { return f.hb }

type fakeCredentialHealth struct {
	mu     sync.Mutex
	ready  bool
	reason string
	errors []string
}

func (f *fakeCredentialHealth) SetCredentialReady(ready bool, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ready, f.reason = ready, reason
}

func (f *fakeCredentialHealth) RecordExecutorProbeError(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errors = append(f.errors, reason)
}

func TestCredentialHealthDegradesAndRecovers(t *testing.T) {
	workerRing, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{ID: "worker", Key: bytes.Repeat([]byte{1}, 32)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	otherRing, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{ID: "other", Key: bytes.Repeat([]byte{2}, 32)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	monitor := domain.Monitor{ID: "m1", Type: domain.MonitorPostgres, Region: "pull1", ExecutionRevision: 3}
	badEnvelope, _ := otherRing.Seal(dispatch.SealContext{EnvelopeVersion: dispatch.EnvelopeV1, Region: "pull1", JobID: "job-bad", MonitorID: monitor.ID, Revision: monitor.ExecutionRevision, Body: monitor}, map[string][]byte{"password": []byte("secret")})
	goodEnvelope, _ := workerRing.Seal(dispatch.SealContext{EnvelopeVersion: dispatch.EnvelopeV1, Region: "pull1", JobID: "job-good", MonitorID: monitor.ID, Revision: monitor.ExecutionRevision, Body: monitor}, map[string][]byte{"password": []byte("secret")})
	jobs := []dispatch.CheckJob{
		{Monitor: monitor, ProtocolVersion: dispatch.ProtocolV2, CredentialEnvelope: badEnvelope},
		{Monitor: monitor, ProtocolVersion: dispatch.ProtocolV2, CredentialEnvelope: goodEnvelope},
	}
	claim := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/v3/jobs":
			body, _ := json.Marshal(jobs[claim])
			claim++
			_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []json.RawMessage{body}, "tokens": []string{"lease"}})
		case "/api/v1/agent/results":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	health := &fakeCredentialHealth{}
	a := New(srv.URL, "tok", "pull1", fixedRunner{hb: domain.Heartbeat{MonitorID: "m1", Up: true}}, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithCredentialKeyring(workerRing).
		WithCredentialHealth(health)
	a.poll(context.Background())
	health.mu.Lock()
	if health.ready || health.reason != domain.ProbeErrorUnknownKeyID || len(health.errors) != 1 {
		t.Fatalf("health after mismatch: ready=%v reason=%q errors=%v", health.ready, health.reason, health.errors)
	}
	health.mu.Unlock()

	a.poll(context.Background())
	health.mu.Lock()
	defer health.mu.Unlock()
	if !health.ready || health.reason != "" || len(health.errors) != 1 {
		t.Fatalf("health after recovery: ready=%v reason=%q errors=%v", health.ready, health.reason, health.errors)
	}
}

func TestFutureCredentialEnvelopeIsProbeErrorWithoutReadinessDowngrade(t *testing.T) {
	ring, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{ID: "worker", Key: bytes.Repeat([]byte{1}, 32)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	monitor := domain.Monitor{ID: "m-future", Type: domain.MonitorPostgres, Region: "pull1", ExecutionRevision: 3}
	envelope, err := ring.Seal(dispatch.SealContext{EnvelopeVersion: dispatch.EnvelopeV1, Region: "pull1", JobID: "job-future", MonitorID: monitor.ID, Revision: monitor.ExecutionRevision, Body: monitor}, map[string][]byte{"password": []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	// A genuinely FUTURE generation: v2 is now a real, supported binding, so the
	// unsupported-version path has to be probed above the newest one we understand.
	envelope.V = dispatch.EnvelopeV2 + 1
	job := dispatch.CheckJob{Monitor: monitor, ProtocolVersion: dispatch.ProtocolV2, CredentialEnvelope: envelope}

	var result domain.Heartbeat
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/v3/jobs":
			body, _ := json.Marshal(job)
			_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []json.RawMessage{body}, "tokens": []string{"lease"}})
		case "/api/v1/agent/results":
			var request struct {
				Results []domain.Heartbeat `json:"results"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			if len(request.Results) == 1 {
				result = request.Results[0]
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	health := &fakeCredentialHealth{}
	a := New(srv.URL, "tok", "pull1", fixedRunner{}, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithCredentialKeyring(ring).
		WithCredentialHealth(health)
	a.poll(context.Background())

	if result.ProbeError == nil || result.ProbeError.Reason != domain.ProbeErrorUnsupportedVersion || result.ProbeError.JobID != "job-future" {
		t.Fatalf("future envelope result = %+v", result)
	}
	health.mu.Lock()
	defer health.mu.Unlock()
	if !health.ready || health.reason != "" || len(health.errors) != 1 || health.errors[0] != domain.ProbeErrorUnsupportedVersion {
		t.Fatalf("future envelope health: ready=%v reason=%q errors=%v", health.ready, health.reason, health.errors)
	}
}

// TestEdgeBufferFlush: when /results fails, the cycle's results are buffered; on the next
// successful cycle they are flushed to /backfill (historical), never re-posted as live.
func TestEdgeBufferFlush(t *testing.T) {
	var mu sync.Mutex
	resultsFail := true
	var backfilled, liveResults int
	var acked []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/jobs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs":   []json.RawMessage{json.RawMessage(`{"Monitor":{"id":"m1","type":"http","region":"pull1"}}`)},
				"tokens": []string{"lease-m1"},
			})
		case "/api/v1/agent/results":
			var body struct {
				Results []domain.Heartbeat `json:"results"`
				Ack     []string           `json:"ack"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			fail := resultsFail
			if !fail {
				liveResults++
				acked = append(acked, body.Ack...)
			}
			mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		case "/api/v1/agent/backfill":
			var body struct {
				Results []domain.Heartbeat `json:"results"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			backfilled += len(body.Results)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	a := New(srv.URL, "tok", "pull1", fixedRunner{hb: domain.Heartbeat{MonitorID: "m1", Up: true}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	// Cycle 1: /results fails → the result is buffered, nothing backfilled yet, and
	// crucially the job is NOT acked (its lease must lapse so it re-delivers).
	a.poll(ctx)
	if len(a.buf) != 1 {
		t.Fatalf("after failed post, buffer = %d, want 1", len(a.buf))
	}
	if backfilled != 0 {
		t.Fatalf("no backfill expected yet, got %d", backfilled)
	}
	if len(acked) != 0 {
		t.Fatalf("a failed results post must not ack, got %v", acked)
	}

	// Cycle 2: connectivity restored → live post succeeds AND the buffer is flushed as backfill.
	mu.Lock()
	resultsFail = false
	mu.Unlock()
	a.poll(ctx)
	if len(a.buf) != 0 {
		t.Fatalf("buffer should be drained, still %d", len(a.buf))
	}
	if backfilled != 1 {
		t.Fatalf("backfilled = %d, want 1 (the buffered result)", backfilled)
	}
	if liveResults != 1 {
		t.Fatalf("liveResults = %d, want 1 (cycle 2 live post)", liveResults)
	}
	// The successful live post carried the lease token as an ack.
	mu.Lock()
	gotAck := append([]string(nil), acked...)
	mu.Unlock()
	if len(gotAck) != 1 || gotAck[0] != "lease-m1" {
		t.Fatalf("ack on success = %v, want [lease-m1]", gotAck)
	}
}

func TestBufferRingDropsOldest(t *testing.T) {
	a := New("http://x", "t", "r", fixedRunner{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	over := make([]domain.Heartbeat, bufferCap+50)
	a.bufferResults(over)
	if len(a.buf) != bufferCap {
		t.Fatalf("buffer len = %d, want capped at %d", len(a.buf), bufferCap)
	}
	if a.dropped != 50 {
		t.Fatalf("dropped = %d, want 50", a.dropped)
	}
}

// TestPollDoesNotAckMalformedJob proves a claimed-but-unparseable job is NOT acked (its
// token is withheld), so its lease lapses and it re-delivers rather than being silently
// deleted with no result. Only the well-formed job's token is acked.
func TestPollDoesNotAckMalformedJob(t *testing.T) {
	var mu sync.Mutex
	var acked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/jobs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs": []json.RawMessage{
					json.RawMessage(`"garbage"`), // valid JSON, wrong type → Unmarshal into CheckJob fails → skipped, token withheld
					json.RawMessage(`{"Monitor":{"id":"good","type":"http","region":"pull1"}}`),
				},
				"tokens": []string{"tok-bad", "tok-good"},
			})
		case "/api/v1/agent/results":
			var body struct {
				Ack []string `json:"ack"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			acked = append(acked, body.Ack...)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	a := New(srv.URL, "tok", "pull1", fixedRunner{hb: domain.Heartbeat{MonitorID: "good", Up: true}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.poll(context.Background())

	mu.Lock()
	got := append([]string(nil), acked...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "tok-good" {
		t.Fatalf("ack = %v, want only [tok-good] (malformed job's token withheld)", got)
	}
}

// recordingRunner captures which monitors actually reached a prober.
type recordingRunner struct {
	mu  sync.Mutex
	ran []string
}

func (r *recordingRunner) Run(_ context.Context, m domain.Monitor) domain.Heartbeat {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ran = append(r.ran, m.ID)
	return domain.Heartbeat{MonitorID: m.ID, Up: true}
}

// TestCapableAgentExecutesMixedGenerationClaim is the agent-side half of the r7
// availability regression (D-0160). A capable claim now returns both an ordinary
// generation-1 job and a credentialed generation-2 one; the agent must probe BOTH. Before
// the fix an `enforced` region's agent never received the generation-1 rows at all, so
// every ordinary monitor there silently stopped being probed.
func TestCapableAgentExecutesMixedGenerationClaim(t *testing.T) {
	ring, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{ID: "agent", Key: bytes.Repeat([]byte{1}, 32)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plain := domain.Monitor{ID: "m-plain", Type: domain.MonitorHTTP, Region: "pull1", ExecutionRevision: 1}
	credentialed := domain.Monitor{ID: "m-cred", Type: domain.MonitorPostgres, Region: "pull1", ExecutionRevision: 3}
	envelope, err := ring.Seal(dispatch.SealContext{EnvelopeVersion: dispatch.EnvelopeV1, Region: "pull1", JobID: "job-cred", MonitorID: credentialed.ID, Revision: credentialed.ExecutionRevision, Body: credentialed}, map[string][]byte{"password": []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	plainJob, _ := json.Marshal(dispatch.CheckJob{Monitor: plain})
	credJob, _ := json.Marshal(dispatch.CheckJob{
		Monitor: credentialed, ProtocolVersion: dispatch.ProtocolV2, CredentialEnvelope: envelope,
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/v3/jobs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs":              []json.RawMessage{plainJob, credJob},
				"tokens":            []string{"lease-plain", "lease-cred"},
				"protocol_versions": []int{1, 2},
			})
		case "/api/v1/agent/results":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	runner := &recordingRunner{}
	a := New(srv.URL, "tok", "pull1", runner, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithCredentialKeyring(ring)
	a.poll(context.Background())

	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.ran) != 2 || runner.ran[0] != "m-plain" || runner.ran[1] != "m-cred" {
		t.Fatalf("capable agent probed %v, want both the ordinary and the credentialed monitor", runner.ran)
	}
}

// A credential envelope on a row the SERVER stamped as generation 1 is a carrier/payload
// mismatch: the generation is transport metadata and the payload is the attacker-editable
// part, so the job is refused before any probe rather than opened on the payload's word.
func TestEnvelopeOnGeneration1CarrierIsRejectedBeforeProbing(t *testing.T) {
	ring, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{ID: "agent", Key: bytes.Repeat([]byte{1}, 32)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	monitor := domain.Monitor{ID: "m-mismatch", Type: domain.MonitorPostgres, Region: "pull1", ExecutionRevision: 3}
	envelope, err := ring.Seal(dispatch.SealContext{EnvelopeVersion: dispatch.EnvelopeV1, Region: "pull1", JobID: "job-mismatch", MonitorID: monitor.ID, Revision: monitor.ExecutionRevision, Body: monitor}, map[string][]byte{"password": []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	job, _ := json.Marshal(dispatch.CheckJob{
		Monitor: monitor, ProtocolVersion: dispatch.ProtocolV2, CredentialEnvelope: envelope,
	})

	var results []domain.Heartbeat
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/v3/jobs":
			// The row is stamped generation 1 by the server while its body claims v2.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs":              []json.RawMessage{job},
				"tokens":            []string{"lease"},
				"protocol_versions": []int{1},
			})
		case "/api/v1/agent/results":
			var request struct {
				Results []domain.Heartbeat `json:"results"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			results = request.Results
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	runner := &recordingRunner{}
	a := New(srv.URL, "tok", "pull1", runner, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithCredentialKeyring(ring)
	a.poll(context.Background())

	runner.mu.Lock()
	ran := append([]string(nil), runner.ran...)
	runner.mu.Unlock()
	if len(ran) != 0 {
		t.Fatalf("mismatched job reached a prober: %v", ran)
	}
	if len(results) != 1 || results[0].ProbeError == nil ||
		results[0].ProbeError.Reason != domain.ProbeErrorDecryptAuthFailed {
		t.Fatalf("carrier/payload mismatch result = %+v", results)
	}
}

// TestClaimResponseDesyncIsRefusedNotGuessed covers the fail-open the pull-claim review
// found. The parallel arrays let a response disagree with itself, and filling a missing
// element with the endpoint default silently promotes a truncated array to "generation 2"
// — which is permission to open an envelope. Absent (an older core) and present-but-
// malformed (a wire disagreement) are different situations and only the first has a safe
// fallback.
func TestClaimResponseDesyncIsRefusedNotGuessed(t *testing.T) {
	ring, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{ID: "agent", Key: bytes.Repeat([]byte{1}, 32)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	monitor := domain.Monitor{ID: "m-desync", Type: domain.MonitorPostgres, Region: "pull1", ExecutionRevision: 3}
	envelope, err := ring.Seal(dispatch.SealContext{
		EnvelopeVersion: dispatch.EnvelopeV1, Region: "pull1", JobID: "job-desync",
		MonitorID: monitor.ID, Revision: monitor.ExecutionRevision, Body: monitor,
	}, map[string][]byte{"password": []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	job, _ := json.Marshal(dispatch.CheckJob{
		Monitor: monitor, ProtocolVersion: dispatch.ProtocolV2, CredentialEnvelope: envelope,
	})

	cases := []struct {
		name     string
		body     map[string]any
		wantRuns int
	}{
		{"stamps shorter than jobs", map[string]any{
			"jobs": []json.RawMessage{job, job}, "tokens": []string{"a", "b"}, "protocol_versions": []int{2},
		}, 0},
		{"stamps longer than jobs", map[string]any{
			"jobs": []json.RawMessage{job}, "tokens": []string{"a"}, "protocol_versions": []int{2, 2},
		}, 0},
		{"stamped generation zero", map[string]any{
			"jobs": []json.RawMessage{job}, "tokens": []string{"a"}, "protocol_versions": []int{0},
		}, 0},
		{"stamped generation above capability", map[string]any{
			"jobs": []json.RawMessage{job}, "tokens": []string{"a"}, "protocol_versions": []int{99},
		}, 0},
		{"tokens desynced from jobs", map[string]any{
			"jobs": []json.RawMessage{job, job}, "tokens": []string{"a"}, "protocol_versions": []int{2, 2},
		}, 0},
		// An explicit JSON null is PRESENT, not absent: a *[]int cannot tell the two
		// apart, so decoding presence off a pointer would let null take the legacy
		// fallback — the same pad-and-guess bypass through a different door.
		{"stamps present as JSON null", map[string]any{
			"jobs": []json.RawMessage{job}, "tokens": []string{"a"}, "protocol_versions": nil,
		}, 0},
		{"stamps present but not an array", map[string]any{
			"jobs": []json.RawMessage{job}, "tokens": []string{"a"}, "protocol_versions": "2",
		}, 0},
		// The one legitimate fallback: an older core that does not stamp at all. The
		// endpoint this agent chose to poll is out-of-band knowledge it already holds.
		{"field absent entirely (older core)", map[string]any{
			"jobs": []json.RawMessage{job}, "tokens": []string{"a"},
		}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/agent/v3/jobs":
					_ = json.NewEncoder(w).Encode(tc.body)
				case "/api/v1/agent/results":
					w.WriteHeader(http.StatusOK)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			runner := &recordingRunner{}
			a := New(srv.URL, "tok", "pull1", runner, slog.New(slog.NewTextHandler(io.Discard, nil))).
				WithCredentialKeyring(ring)
			a.poll(context.Background())

			runner.mu.Lock()
			defer runner.mu.Unlock()
			if len(runner.ran) != tc.wantRuns {
				t.Fatalf("probed %v, want %d execution(s)", runner.ran, tc.wantRuns)
			}
		})
	}
}

// The test-RPC path gets the executor regression the jobs path already had: a server-
// stamped legacy generation carrying an envelope is refused before any probe.
func TestEnvelopeOnGeneration1TestCarrierIsRejectedBeforeProbing(t *testing.T) {
	ring, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{ID: "agent", Key: bytes.Repeat([]byte{1}, 32)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	monitor := domain.Monitor{ID: "m-test-mismatch", Type: domain.MonitorPostgres, Region: "pull1", ExecutionRevision: 3}
	envelope, err := ring.Seal(dispatch.SealContext{
		EnvelopeVersion: dispatch.EnvelopeV1, Region: "pull1", JobID: "job-test-mismatch",
		MonitorID: monitor.ID, Revision: monitor.ExecutionRevision, Body: monitor,
	}, map[string][]byte{"password": []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	job, _ := json.Marshal(dispatch.CheckJob{
		Monitor: monitor, ProtocolVersion: dispatch.ProtocolV2, CredentialEnvelope: envelope,
	})

	var posted domain.Heartbeat
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/v3/tests":
			_ = json.NewEncoder(w).Encode(map[string]any{"test": map[string]any{
				"id": "t1", "job": json.RawMessage(job), "protocol_version": 1,
			}})
		case "/api/v1/agent/test-results":
			var body struct {
				Result domain.Heartbeat `json:"result"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			posted = body.Result
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	runner := &recordingRunner{}
	a := New(srv.URL, "tok", "pull1", runner, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithCredentialKeyring(ring)
	a.pollTest(context.Background())

	runner.mu.Lock()
	ran := append([]string(nil), runner.ran...)
	runner.mu.Unlock()
	if len(ran) != 0 {
		t.Fatalf("mismatched test reached a prober: %v", ran)
	}
	if posted.ProbeError == nil || posted.ProbeError.Reason != domain.ProbeErrorDecryptAuthFailed {
		t.Fatalf("test carrier mismatch result = %+v", posted)
	}
}

// A malformed stamp on the test claim is refused rather than guessed, same contract as
// jobs — including an explicit null, which must not read as "absent".
func TestTestClaimStampOutsideRangeIsRefused(t *testing.T) {
	ring, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{ID: "agent", Key: bytes.Repeat([]byte{1}, 32)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	job, _ := json.Marshal(dispatch.CheckJob{Monitor: domain.Monitor{ID: "m1", Type: domain.MonitorHTTP}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/v3/tests" {
			_ = json.NewEncoder(w).Encode(map[string]any{"test": map[string]any{
				"id": "t1", "job": json.RawMessage(job), "protocol_version": 99,
			}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	runner := &recordingRunner{}
	a := New(srv.URL, "tok", "pull1", runner, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithCredentialKeyring(ring)
	a.pollTest(context.Background())

	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.ran) != 0 {
		t.Fatalf("test with an out-of-range stamp reached a prober: %v", runner.ran)
	}
}

// The test claim's presence contract, including the null case a pointer cannot express.
func TestTestClaimStampPresenceContract(t *testing.T) {
	ring, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{ID: "agent", Key: bytes.Repeat([]byte{1}, 32)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	monitor := domain.Monitor{ID: "m-null", Type: domain.MonitorPostgres, Region: "pull1", ExecutionRevision: 3}
	envelope, err := ring.Seal(dispatch.SealContext{
		EnvelopeVersion: dispatch.EnvelopeV1, Region: "pull1", JobID: "job-null",
		MonitorID: monitor.ID, Revision: monitor.ExecutionRevision, Body: monitor,
	}, map[string][]byte{"password": []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	job, _ := json.Marshal(dispatch.CheckJob{
		Monitor: monitor, ProtocolVersion: dispatch.ProtocolV2, CredentialEnvelope: envelope,
	})

	for _, tc := range []struct {
		name     string
		test     map[string]any
		wantRuns int
	}{
		{"protocol_version present as null", map[string]any{
			"id": "t1", "job": json.RawMessage(job), "protocol_version": nil,
		}, 0},
		{"protocol_version present but not a number", map[string]any{
			"id": "t1", "job": json.RawMessage(job), "protocol_version": "2",
		}, 0},
		{"protocol_version absent (older core)", map[string]any{
			"id": "t1", "job": json.RawMessage(job),
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/agent/v3/tests":
					_ = json.NewEncoder(w).Encode(map[string]any{"test": tc.test})
				case "/api/v1/agent/test-results":
					w.WriteHeader(http.StatusOK)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			runner := &recordingRunner{}
			a := New(srv.URL, "tok", "pull1", runner, slog.New(slog.NewTextHandler(io.Discard, nil))).
				WithCredentialKeyring(ring)
			a.pollTest(context.Background())

			runner.mu.Lock()
			defer runner.mu.Unlock()
			if len(runner.ran) != tc.wantRuns {
				t.Fatalf("probed %v, want %d execution(s)", runner.ran, tc.wantRuns)
			}
		})
	}
}

// The pull half of the feature-off contract: an agent with no keyring, claiming from the
// legacy endpoint, must still probe a credentialed monitor whose credential is inline.
func TestFeatureOffCredentialJobReachesThePullProber(t *testing.T) {
	monitor := domain.Monitor{
		ID: "m-legacy-pull", Type: domain.MonitorPostgres, Region: "pull1", ExecutionRevision: 2,
		Target: "db.internal:5432",
		Config: map[string]string{"username": "ro", "database": "app", "sslmode": "require", "password": "inline"},
	}
	job, _ := json.Marshal(dispatch.CheckJob{Monitor: monitor})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/jobs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs": []json.RawMessage{job}, "tokens": []string{"lease"}, "protocol_versions": []int{1},
			})
		case "/api/v1/agent/results":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	runner := &recordingRunner{}
	// No keyring: the feature-off agent profile, which polls the legacy claim endpoint.
	a := New(srv.URL, "tok", "pull1", runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.poll(context.Background())

	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.ran) != 1 || runner.ran[0] != "m-legacy-pull" {
		t.Fatalf("legacy credentialed pull job did not reach the prober: %v", runner.ran)
	}
}
