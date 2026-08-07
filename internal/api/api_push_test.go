package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// fakeResultSink captures results published to the ingest pipeline — still used by the
// AGENT results endpoint (push migrated to fakePushRecorder below).
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

// fakePushRecorder captures the Record calls the push handler makes (it replaces the old
// ResultSink path: push now goes through the dedicated recorder, not the dispatcher).
type fakePushRecorder struct {
	mu    sync.Mutex
	calls []pushCall
}

type pushCall struct {
	monitorID  string
	up         bool
	msg        string
	receivedAt time.Time
}

func (r *fakePushRecorder) Record(_ context.Context, monitorID string, up bool, msg string, receivedAt, _ time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, pushCall{monitorID, up, msg, receivedAt})
}

func (r *fakePushRecorder) last() (pushCall, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return pushCall{}, false
	}
	return r.calls[len(r.calls)-1], true
}

func pushHandler(fs *fakeStore, rec *fakePushRecorder) http.Handler {
	return api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithPushRecorder(rec).PublicRouter()
}

func TestPushHeartbeat(t *testing.T) {
	pr := &fakePushRecorder{}
	h := pushHandler(seededStore(), pr)

	// A push records an up result for the token's monitor, carrying the ingress received_at.
	if rec := do(h, outsider, http.MethodPost, "/api/v1/public/push/tok-push", ""); rec.Code != http.StatusOK {
		t.Fatalf("push = %d, want 200", rec.Code)
	}
	c, ok := pr.last()
	if !ok || c.monitorID != "monp" || !c.up {
		t.Fatalf("expected an up push for monp, got %+v (ok=%v)", c, ok)
	}
	if c.receivedAt.IsZero() {
		t.Fatal("push must carry the ingress received_at from the token lookup")
	}

	// ?status=down records a down result.
	if rec := do(h, outsider, http.MethodPost, "/api/v1/public/push/tok-push?status=down", ""); rec.Code != http.StatusOK {
		t.Fatalf("push down = %d, want 200", rec.Code)
	}
	if c, _ := pr.last(); c.up {
		t.Fatal("status=down should record a down result")
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
