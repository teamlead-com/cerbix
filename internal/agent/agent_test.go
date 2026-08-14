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
	badEnvelope, _ := otherRing.Seal("pull1", "job-bad", monitor.ID, monitor.ExecutionRevision, map[string][]byte{"password": []byte("secret")})
	goodEnvelope, _ := workerRing.Seal("pull1", "job-good", monitor.ID, monitor.ExecutionRevision, map[string][]byte{"password": []byte("secret")})
	jobs := []dispatch.CheckJob{
		{Monitor: monitor, ProtocolVersion: dispatch.ProtocolV2, CredentialEnvelope: badEnvelope},
		{Monitor: monitor, ProtocolVersion: dispatch.ProtocolV2, CredentialEnvelope: goodEnvelope},
	}
	claim := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/v2/jobs":
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
	envelope, err := ring.Seal("pull1", "job-future", monitor.ID, monitor.ExecutionRevision, map[string][]byte{"password": []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	envelope.V++
	job := dispatch.CheckJob{Monitor: monitor, ProtocolVersion: dispatch.ProtocolV2, CredentialEnvelope: envelope}

	var result domain.Heartbeat
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/v2/jobs":
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
