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

	"github.com/teamlead-com/cerbix/internal/domain"
)

type fixedRunner struct{ hb domain.Heartbeat }

func (f fixedRunner) Run(context.Context, domain.Monitor) domain.Heartbeat { return f.hb }

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
