package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// FR-041a Task 14: the wiring that makes phone alerts run in a served process.

type liveTelephony struct {
	ts   domain.TelephonySettings
	mail domain.MailSettings
}

func (l *liveTelephony) Telephony() domain.TelephonySettings { return l.ts }
func (l *liveTelephony) Mail() domain.MailSettings           { return l.mail }
func (l *liveTelephony) CurrentTelephony(context.Context) (domain.TelephonySettings, error) {
	return l.ts, nil
}

// The provider's HTTP client is the notification egress guard's, as for every outbound alert: a
// generic_http URL is typed by an admin, and must not reach loopback or a private range unless the
// deployment opted in (notification_egress). httptest binds 127.0.0.1.
func TestTelephonyProviderDialsThroughTheNotificationEgressGuard(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	src := &liveTelephony{ts: domain.TelephonySettings{Configured: true, Enabled: true,
		Provider: domain.TelephonyProviderGenericHTTP, GenericHTTP: domain.TelephonyGenericHTTP{
			Method: http.MethodPost, URL: srv.URL, ContentType: telephony.ContentTypeJSON,
			BodyTemplate: `{"to":"{{to}}","text":"{{text}}"}`}}}
	send := func(cfg *config.Config) telephony.Result {
		t.Helper()
		p, err := newTelephonyService(src, notificationEgressGuard(cfg)).Provider()
		if err != nil {
			t.Fatalf("provider: %v", err)
		}
		return p.SendSMS(context.Background(), "+905551236869", "[cerbix] test")
	}

	def, err := config.Parse([]byte("log:\n  level: info\n"))
	if err != nil {
		t.Fatal(err)
	}
	if res := send(def); res.Outcome == telephony.Sent || hits.Load() != 0 {
		t.Fatalf("default notification egress: outcome %s, %d requests; want the loopback gateway unreached", res.Outcome, hits.Load())
	}
	open, err := config.Parse([]byte("notification_egress:\n  allow_private_ips: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if res := send(open); res.Outcome != telephony.Sent || hits.Load() != 1 {
		t.Fatalf("explicit opt-in: outcome %s, %d requests; want the gateway reached once", res.Outcome, hits.Load())
	}
}

// A phone message's link base is the telephony public_base_url, else mail's (§4.1), read at each send:
// a settings save is in force for the next message without a restart.
func TestPhoneLinkBasesAreReadLive(t *testing.T) {
	src := &liveTelephony{}
	tel, mail := phoneLinkBases(src)
	if tel() != "" || mail() != "" {
		t.Fatalf("nothing stored: %q, %q", tel(), mail())
	}
	src.ts.PublicBaseURL = "https://phone.example"
	src.mail.PublicBaseURL = "https://mail.example"
	if tel() != "https://phone.example" || mail() != "https://mail.example" {
		t.Fatalf("after a save: %q, %q; want the saved values", tel(), mail())
	}
}

// serveRuntime wires every piece phone delivery needs, for every role that delivers: the dispatcher's
// phone half over the store and the live telephony, the outbox's link bases, the API's provider (code
// sends and the test SMS answer 501 without it) and the leader's outcome_unknown count on every
// scheduler construction.
func TestServeWiresPhoneDelivery(t *testing.T) {
	src, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatalf("read cli.go: %v", err)
	}
	text := string(src)
	for _, want := range []string{
		"telSvc = newTelephonyService(settingsSvc, egress)",
		".WithPhone(st, telSvc, registry, logger)",
		"WithPhoneLinks(phoneLinkBases(settingsSvc))",
		"apiHandler.WithTelephony(telSvc)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("cli.go does not wire %q", want)
		}
	}
	sites := regexp.MustCompile(`scheduler\.New\(`).FindAllStringIndex(text, -1)
	if len(sites) == 0 {
		t.Fatal("no scheduler.New( construction found in cli.go — this guard is now blind")
	}
	for i, at := range sites {
		end := len(text)
		if i+1 < len(sites) {
			end = sites[i+1][0]
		}
		if !strings.Contains(text[at[0]:end], "WithPhoneMetrics(registry)") {
			t.Errorf("scheduler construction #%d does not call WithPhoneMetrics(registry): the rows its stale "+
				"sweep closes would be counted nowhere (P7)", i+1)
		}
	}
	if got := strings.Count(text, "WithPhoneMetrics("); got != len(sites) {
		t.Errorf("WithPhoneMetrics( appears %d time(s) against %d scheduler construction(s)", got, len(sites))
	}
}
