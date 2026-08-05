package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.example.com/monitoring/cerbix/internal/api"
	"git.example.com/monitoring/cerbix/internal/auth"
	"git.example.com/monitoring/cerbix/internal/domain"
	"git.example.com/monitoring/cerbix/internal/events"
)

func TestEventsDisabledWithoutSource(t *testing.T) {
	if rec := do(newHandler(seededStore()), o1Viewer, http.MethodGet, "/api/v1/events", ""); rec.Code != http.StatusNotImplemented {
		t.Fatalf("events without source = %d, want 501", rec.Code)
	}
}

func TestEventsStreamFiltersByVisibility(t *testing.T) {
	fs := seededStore()
	fs.userOrgs["u1"] = []domain.Organization{fs.orgs["o1"]} // u1 (o1Viewer) sees o1 projects p1,p2
	broker := events.NewBroker()
	handler := api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithEvents(broker).Router()

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(auth.WithPrincipal(ctx, o1Viewer))
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handler.ServeHTTP(rec, req); close(done) }()

	time.Sleep(30 * time.Millisecond) // let the handler subscribe
	broker.Publish(events.Event{Type: "status", MonitorID: "mon1", ProjectID: "p1", Status: "down"})
	broker.Publish(events.Event{Type: "status", MonitorID: "mon3", ProjectID: "p3", Status: "down"})
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, "text/event-stream") && rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content-type = %q", rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(body, `"monitor_id":"mon1"`) {
		t.Fatalf("expected visible mon1 event, got: %q", body)
	}
	if strings.Contains(body, "mon3") {
		t.Fatalf("p3 event (other org) should be filtered out, got: %q", body)
	}
}
