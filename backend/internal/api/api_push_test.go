package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"git.example.com/monitoring/cerbix/internal/api"
	"git.example.com/monitoring/cerbix/internal/domain"
)

type fakeResultSink struct {
	mu  sync.Mutex
	hbs []domain.Heartbeat
}

func (s *fakeResultSink) PublishResult(_ context.Context, hb domain.Heartbeat) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hbs = append(s.hbs, hb)
	return nil
}

func (s *fakeResultSink) last() (domain.Heartbeat, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.hbs) == 0 {
		return domain.Heartbeat{}, false
	}
	return s.hbs[len(s.hbs)-1], true
}

func pushHandler(fs *fakeStore, sink *fakeResultSink) http.Handler {
	return api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithResultSink(sink).PublicRouter()
}

func TestPushHeartbeat(t *testing.T) {
	sink := &fakeResultSink{}
	h := pushHandler(seededStore(), sink)

	// A push records an up heartbeat for the token's monitor.
	if rec := do(h, outsider, http.MethodPost, "/api/v1/public/push/tok-push", ""); rec.Code != http.StatusOK {
		t.Fatalf("push = %d, want 200", rec.Code)
	}
	hb, ok := sink.last()
	if !ok || hb.MonitorID != "monp" || !hb.Up {
		t.Fatalf("expected an up heartbeat for monp, got %+v (ok=%v)", hb, ok)
	}

	// ?status=down records a down heartbeat.
	if rec := do(h, outsider, http.MethodPost, "/api/v1/public/push/tok-push?status=down", ""); rec.Code != http.StatusOK {
		t.Fatalf("push down = %d, want 200", rec.Code)
	}
	if hb, _ := sink.last(); hb.Up {
		t.Fatal("status=down should record a down heartbeat")
	}

	// An unknown token is hidden.
	if rec := do(h, outsider, http.MethodPost, "/api/v1/public/push/nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token = %d, want 404", rec.Code)
	}
}

func TestCreatePushMonitorGetsToken(t *testing.T) {
	h := newHandler(seededStore())
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors", `{"name":"cron","type":"push","interval_seconds":600}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create push monitor = %d, want 201", rec.Code)
	}
	var mon domain.Monitor
	_ = json.Unmarshal(rec.Body.Bytes(), &mon)
	if mon.PushToken == "" {
		t.Fatal("push monitor should be issued a push_token")
	}
}
