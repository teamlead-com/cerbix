package outbox

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/metrics"
	"github.com/teamlead-com/cerbix/internal/notify"
	"github.com/teamlead-com/cerbix/internal/secret"
	"github.com/teamlead-com/cerbix/internal/settings"
	"github.com/teamlead-com/cerbix/internal/store"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// Final review M-6: TestP14NoPhoneProjectGolden was captured, and still runs, over a dispatcher with
// no phone wiring and telephony off. This twin runs the SAME outage against the SAME golden with the
// wiring cli.serveRuntime builds: an at-rest key, telephony saved ENABLED through settings.Service,
// notify.WithPhone over the real store with the metrics registry, and outbox.WithPhoneLinks reading
// both public base URLs from settings at each send. Only the provider is a fake, and the network
// below the telephony service refuses every request: a project without phone channels must reach
// neither. The golden is never regenerated from here.

// p14ProductionTelephony is the telephony in force for the twin: production's settings through
// telephony.Service, and a fake provider whose every build and request is counted.
type p14ProductionTelephony struct {
	svc      *telephony.Service
	provider *telephony.FakeProvider
	builds   atomic.Int32
}

func (p *p14ProductionTelephony) Settings() domain.TelephonySettings { return p.svc.Settings() }

func (p *p14ProductionTelephony) Provider() (telephony.Provider, error) {
	p.builds.Add(1)
	return p.provider, nil
}

// refuseRoundTrip fails every request the telephony service's client would make.
type refuseRoundTrip struct{ used *atomic.Int32 }

func (r refuseRoundTrip) RoundTrip(*http.Request) (*http.Response, error) {
	r.used.Add(1)
	return nil, errors.New("p14 twin: no network")
}

func TestP14NoPhoneProjectGoldenProductionWiring(t *testing.T) {
	var (
		settingsSvc *settings.Service
		tel         = &p14ProductionTelephony{provider: telephony.NewFakeProvider()}
		network     atomic.Int32
		registry    = metrics.New(buildinfo.Current(), "all")
	)
	r := newP14RigWired(t, p14Wiring{
		prepare: func(ctx context.Context, st *store.Store) {
			key := make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				t.Fatal(err)
			}
			c, err := secret.New(key)
			if err != nil {
				t.Fatal(err)
			}
			st.WithCipher(c)
			settingsSvc = settings.New(st, settings.Bootstrap{
				Mail: domain.MailSettings{PublicBaseURL: "https://mail.p14.example"},
			}, slog.New(slog.DiscardHandler))
			if err := settingsSvc.Load(ctx); err != nil {
				t.Fatalf("load settings: %v", err)
			}
			if err := settingsSvc.SaveTelephony(ctx, domain.TelephonySettings{
				Enabled: true, Provider: domain.TelephonyProviderTwilio, PublicBaseURL: "https://cerbix.p14.example",
				Twilio: domain.TelephonyTwilio{AccountSID: "AC0123456789abcdef0123456789abcdef",
					AuthToken: "p14-token", From: "+15005550006"},
			}, domain.TelephonySaveOptions{}); err != nil {
				t.Fatalf("save telephony: %v", err)
			}
			if ts := settingsSvc.Telephony(); !ts.Enabled || ts.Provider != domain.TelephonyProviderTwilio {
				t.Fatalf("telephony in force: %+v; want it enabled with twilio", ts)
			}
			tel.svc = telephony.NewService(settingsSvc, &http.Client{Transport: refuseRoundTrip{used: &network}})
		},
		worker: func(st *store.Store, logger *slog.Logger) *Worker {
			notifier := notify.New(st, nil).WithPhone(st, tel, registry, logger)
			return New(st, &fakeWebhook{}, notifier, &fakeMetrics{}, logger).
				WithPhoneLinks(func() string { return settingsSvc.Telephony().PublicBaseURL },
					func() string { return settingsSvc.Mail().PublicBaseURL })
		},
	})

	r.compareGolden(r.runScenario())

	if n := tel.builds.Load(); n != 0 {
		t.Fatalf("a project without phone channels built the provider %d times", n)
	}
	if calls := tel.provider.Calls(); len(calls) != 0 {
		t.Fatalf("a project without phone channels reached the provider: %d requests", len(calls))
	}
	if n := network.Load(); n != 0 {
		t.Fatalf("the telephony client was used %d times", n)
	}
	var out strings.Builder
	registry.WritePrometheus(&out)
	if strings.Contains(out.String(), "cerbix_phone") {
		t.Fatal("the metrics show a phone family for a project without phone channels")
	}
	var rows int
	if err := r.conn.QueryRow(r.ctx, `SELECT count(*) FROM phone_deliveries`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("%d phone delivery rows for a project without phone channels", rows)
	}
}
