package webhook

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.example.com/monitoring/cerbix/internal/domain"
)

func TestSignDeterministic(t *testing.T) {
	a := Sign("secret", []byte("body"))
	b := Sign("secret", []byte("body"))
	if a != b {
		t.Fatal("signing should be deterministic")
	}
	if a == Sign("other", []byte("body")) {
		t.Fatal("different secrets should sign differently")
	}
	if len(a) < len("sha256=") || a[:7] != "sha256=" {
		t.Fatalf("signature missing prefix: %q", a)
	}
}

type fakeStore struct{ hooks []domain.Webhook }

func (f *fakeStore) ListEnabledWebhooksForProject(_ context.Context, _ string) ([]domain.Webhook, error) {
	return f.hooks, nil
}

func TestDeliverSignsAndPosts(t *testing.T) {
	type received struct {
		sig, event string
		body       []byte
	}
	got := make(chan received, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- received{sig: r.Header.Get(SignatureHeader), event: r.Header.Get(EventHeader), body: b}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := &fakeStore{hooks: []domain.Webhook{{ID: "wh1", URL: srv.URL, Secret: "topsecret", Enabled: true}}}
	d := New(store, srv.Client())

	ev := domain.IncidentEvent{Type: domain.EventIncidentOpened, Incident: domain.Incident{ID: "inc1", ProjectID: "p1", Title: "down"}}
	if err := d.Deliver(context.Background(), ev); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	r := <-got
	if r.event != domain.EventIncidentOpened {
		t.Fatalf("event header = %q", r.event)
	}
	if want := Sign("topsecret", r.body); r.sig != want {
		t.Fatalf("signature mismatch: got %q want %q", r.sig, want)
	}
	if !strings.Contains(string(r.body), "inc1") {
		t.Fatalf("body missing incident id: %s", r.body)
	}
}

type errStore struct{}

func (errStore) ListEnabledWebhooksForProject(_ context.Context, _ string) ([]domain.Webhook, error) {
	return nil, io.ErrUnexpectedEOF
}

// TestDeliverReportsFailures confirms Deliver returns an error the outbox can act
// on (store error, non-2xx, and a bad URL) rather than swallowing it.
func TestDeliverReportsFailures(t *testing.T) {
	ev := domain.IncidentEvent{Type: domain.EventIncidentOpened, Incident: domain.Incident{ID: "i", ProjectID: "p"}}

	if err := New(errStore{}, nil).Deliver(context.Background(), ev); err == nil {
		t.Fatal("store error should surface")
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	if err := New(&fakeStore{hooks: []domain.Webhook{{ID: "w", URL: bad.URL, Secret: "s", Enabled: true}}}, bad.Client()).
		Deliver(context.Background(), ev); err == nil {
		t.Fatal("non-2xx should surface")
	}

	if err := New(&fakeStore{hooks: []domain.Webhook{{ID: "w", URL: "http://%zz", Secret: "s", Enabled: true}}}, nil).
		Deliver(context.Background(), ev); err == nil {
		t.Fatal("bad URL should surface")
	}
}

// TestDeliverNoWebhooksIsSuccess confirms an event with no matching webhooks is a
// successful no-op (so the outbox marks it delivered, not retried).
func TestDeliverNoWebhooksIsSuccess(t *testing.T) {
	ev := domain.IncidentEvent{Type: domain.EventIncidentOpened, Incident: domain.Incident{ID: "i", ProjectID: "p"}}
	if err := New(&fakeStore{}, nil).Deliver(context.Background(), ev); err != nil {
		t.Fatalf("no webhooks should be a no-op, got %v", err)
	}
}
