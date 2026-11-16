package outbox

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

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/notify"
)

// hookRecorder is a webhook channel endpoint that keeps every monitor.transition body it receives.
type hookRecorder struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func newHookRecorder(t *testing.T) (*hookRecorder, *httptest.Server) {
	t.Helper()
	h := &hookRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Errorf("non-JSON webhook body %q", raw)
		}
		h.mu.Lock()
		h.bodies = append(h.bodies, m)
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return h, srv
}

func (h *hookRecorder) texts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, b := range h.bodies {
		s, _ := b["text"].(string)
		out = append(out, s)
	}
	return out
}

// TestRecoveryEventRendersFromItsSnapshot (FR-043 §4.6, I14): the worker hands Deliver the snapshot
// the EVENT carries — never one rebuilt from the monitor row, which the confirming commit already
// reset and whose threshold was edited here before delivery (the row says 5, the event 3/3). Through
// the real dispatcher, the webhook channel's text carries the sentence; the payload keys stay
// monitor.transition's.
func TestRecoveryEventRendersFromItsSnapshot(t *testing.T) {
	firstOK := time.Date(2026, 11, 16, 14, 4, 5, 0, time.UTC)
	payload, err := json.Marshal(domain.MonitorTransition{
		MonitorID: "m1", Prev: domain.StatusDown, Cur: domain.StatusUp, Seq: 4,
		RecoveryRun: 3, RecoveryThreshold: 3, RecoveryFirstOK: &firstOK,
	})
	if err != nil {
		t.Fatal(err)
	}
	row := domain.Monitor{ID: "m1", Name: "API", Status: domain.StatusUp, StateSequence: 4, RecoveryThreshold: 5}

	// The seam: the snapshot reaches Deliver unchanged, through Deliver and not DeliverText.
	fs := &fakeStore{monitors: map[string]domain.Monitor{"m1": row},
		pending: []domain.OutboxEvent{{ID: "e1", Topic: domain.TopicMonitorTransition, Payload: payload, Attempts: 1}}}
	nf := &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
	want := &domain.RecoverySnapshot{Run: 3, Threshold: 3, FirstOK: firstOK}
	if nf.called != 1 || !nf.up || nf.text != "" || nf.recovery == nil || *nf.recovery != *want {
		t.Fatalf("Deliver called %d, up %v, text %q, recovery %+v; want once, up, via Deliver, %+v",
			nf.called, nf.up, nf.text, nf.recovery, want)
	}

	// The rendering, through the real dispatcher.
	hooks, srv := newHookRecorder(t)
	fs2 := &fakeStore{monitors: map[string]domain.Monitor{"m1": row},
		pending: []domain.OutboxEvent{{ID: "e1", Topic: domain.TopicMonitorTransition, Payload: payload, Attempts: 1}}}
	nd := notify.New(channelStore{channels: []domain.NotificationChannel{
		{ID: "nc1", Type: domain.ChannelWebhook, Enabled: true, Config: map[string]string{"url": srv.URL + "/hook"}},
	}}, nil)
	New(fs2, nil, nd, &fakeMetrics{}, slog.New(slog.NewTextHandler(io.Discard, nil))).drain(context.Background())
	if len(fs2.delivered) != 1 {
		t.Fatalf("delivered=%v failed=%v reasons=%q", fs2.delivered, fs2.failed, fs2.failReasons)
	}
	wantText := "🟢 API recovered — it is back UP. Recovered: 3/3 consecutive checks passed; first OK at 2026-11-16T14:04:05Z"
	if got := hooks.texts(); len(got) != 1 || got[0] != wantText {
		t.Fatalf("webhook texts = %q, want [%q]", got, wantText)
	}
	if b := hooks.bodies[0]; b["event"] != "monitor.transition" || b["up"] != true || len(b) != 6 {
		t.Fatalf("webhook body = %v, want monitor.transition, up true, the six keys of today", b)
	}
}

// TestLegacyRecoveryEventRendersTodaysText (FR-043 §4.6, I1): an up event without the snapshot —
// every event already in the queue at upgrade — reaches Deliver with no snapshot and renders today's
// text byte for byte, even when the monitor row now says threshold 3.
func TestLegacyRecoveryEventRendersTodaysText(t *testing.T) {
	row := domain.Monitor{ID: "m1", Name: "API", Status: domain.StatusUp, RecoveryThreshold: 3}
	// A literal payload as an older binary wrote it: no recovery_* keys at all.
	payload := []byte(`{"monitor_id":"m1","prev":"down","cur":"up","at":"2026-11-16T14:00:00Z","seq":2}`)

	fs := &fakeStore{monitors: map[string]domain.Monitor{"m1": row},
		pending: []domain.OutboxEvent{{ID: "e1", Topic: domain.TopicMonitorTransition, Payload: payload, Attempts: 1}}}
	nf := &fakeNotify{}
	newWorker(fs, &fakeWebhook{}, nf, &fakeMetrics{}).drain(context.Background())
	if nf.called != 1 || !nf.up || nf.recovery != nil {
		t.Fatalf("Deliver called %d, up %v, recovery %+v; want once, up, no snapshot", nf.called, nf.up, nf.recovery)
	}

	hooks, srv := newHookRecorder(t)
	fs2 := &fakeStore{monitors: map[string]domain.Monitor{"m1": row},
		pending: []domain.OutboxEvent{{ID: "e1", Topic: domain.TopicMonitorTransition, Payload: payload, Attempts: 1}}}
	nd := notify.New(channelStore{channels: []domain.NotificationChannel{
		{ID: "nc1", Type: domain.ChannelWebhook, Enabled: true, Config: map[string]string{"url": srv.URL + "/hook"}},
	}}, nil)
	New(fs2, nil, nd, &fakeMetrics{}, slog.New(slog.NewTextHandler(io.Discard, nil))).drain(context.Background())
	if got := hooks.texts(); len(got) != 1 || got[0] != "🟢 API recovered — it is back UP." {
		t.Fatalf("webhook texts = %q, want today's text only", got)
	}
}
