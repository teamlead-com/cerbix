package notify

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

// redirectPair is server A, which answers every request with `status` and `Location: B`, and server
// B, which counts what reaches it.
type redirectPair struct {
	a, b *httptest.Server
	hits atomic.Int32
}

func newRedirectPair(t *testing.T, status int) *redirectPair {
	t.Helper()
	p := &redirectPair{}
	p.b = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		p.hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	p.a = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", p.b.URL+"/landing")
		w.WriteHeader(status)
	}))
	t.Cleanup(func() { p.a.Close(); p.b.Close() })
	return p
}

var redirectStatuses = []int{
	http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
	http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
}

// TestDeliveryFollowsNoRedirect (sec-hardening-3 T-2, S17): for every redirect status, every HTTP
// channel kind and both clients — the guard's (private addresses allowed, so only the redirect rule is
// under test) and the nil fallback of New — the Location host receives nothing and Deliver names the
// status. Before, B received the POST with its body on 307/308 and a bodiless GET with Referer = A's
// URL on 301/302/303, which Deliver then counted as delivered.
func TestDeliveryFollowsNoRedirect(t *testing.T) {
	clients := map[string]func() *http.Client{
		"guard":    func() *http.Client { return prober.NewGuard(true, false).HTTPClient(5 * time.Second) },
		"fallback": func() *http.Client { return nil },
	}
	origAPI := telegramAPI
	t.Cleanup(func() { telegramAPI = origAPI })
	for _, status := range redirectStatuses {
		for _, kind := range []domain.ChannelType{domain.ChannelWebhook, domain.ChannelSlack, domain.ChannelTelegram} {
			for name, client := range clients {
				t.Run(fmt.Sprintf("%d/%s/%s", status, kind, name), func(t *testing.T) {
					p := newRedirectPair(t, status)
					ch := domain.NotificationChannel{ID: "nc1", Type: kind, Enabled: true}
					switch kind {
					case domain.ChannelTelegram:
						telegramAPI = p.a.URL
						ch.Config = map[string]string{"bot_token": "123:TOKEN", "chat_id": "42"}
					default:
						ch.Config = map[string]string{"url": p.a.URL + "/hook/TOKEN"}
					}
					err := New(&fakeStore{channels: []domain.NotificationChannel{ch}}, client()).
						Deliver(context.Background(), domain.Monitor{ID: "m1", Name: "API"}, false, nil)
					if got := p.hits.Load(); got != 0 {
						t.Errorf("the Location host received %d request(s)", got)
					}
					if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("non-2xx status %d", status)) {
						t.Errorf("Deliver = %v, want an error naming status %d", err, status)
					}
				})
			}
		}
	}
}
