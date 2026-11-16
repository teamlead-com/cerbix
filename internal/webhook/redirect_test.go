package webhook

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/prober"
)

// TestWebhookDeliveryFollowsNoRedirect (sec-hardening-3 T-2, S17): for every redirect status and both
// clients, a signed incident event never reaches the Location host and Deliver names the status.
// Before, B received the body and a valid X-Cerbix-Signature for it on 307/308, and a GET with
// Referer = A's URL on 301/302/303, with Deliver returning nil.
func TestWebhookDeliveryFollowsNoRedirect(t *testing.T) {
	clients := map[string]func() *http.Client{
		"guard":    func() *http.Client { return prober.NewGuard(true, false).HTTPClient(5 * time.Second) },
		"fallback": func() *http.Client { return nil },
	}
	for _, status := range []int{301, 302, 303, 307, 308} {
		for name, client := range clients {
			t.Run(fmt.Sprintf("%d/%s", status, name), func(t *testing.T) {
				var hits atomic.Int32
				var gotSignature atomic.Bool
				b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					if r.Header.Get(SignatureHeader) != "" {
						gotSignature.Store(true)
					}
					w.WriteHeader(http.StatusOK)
				}))
				defer b.Close()
				a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Location", b.URL+"/landing")
					w.WriteHeader(status)
				}))
				defer a.Close()

				store := &fakeStore{hooks: []domain.Webhook{{ID: "wh1", URL: a.URL + "/hook?token=TOKEN", Secret: "topsecret", Enabled: true}}}
				ev := domain.IncidentEvent{Type: domain.EventIncidentOpened, Incident: domain.Incident{ID: "inc1", ProjectID: "p1", Title: "down"}}
				err := New(store, client()).Deliver(context.Background(), ev)
				if got := hits.Load(); got != 0 {
					t.Errorf("the Location host received %d request(s) (signature carried: %v)", got, gotSignature.Load())
				}
				if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("non-2xx status %d", status)) {
					t.Errorf("Deliver = %v, want an error naming status %d", err, status)
				}
			})
		}
	}
}
