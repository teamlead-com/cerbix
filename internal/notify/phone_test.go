package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/metrics"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// FR-041a §11 "Outbox — reasons" (P6), D19, D20, D31 rule 1 at send, D33's budget, P1, P3: DeliverPhone
// with the fake provider (or both real providers against httptest servers) and a store fake. The
// reservation itself — its locks, the storm count, D32 — is the store's and is tested there.

// The production implementations Task 14 wires (the store's is asserted by the store's DB test).
var (
	_ TelephonySource = (*telephony.Service)(nil)
	_ PhoneMetrics    = (*metrics.Registry)(nil)
)

const (
	numA = "+905551236869"
	numB = "+447700900199"
	numC = "+12025550147"
)

type unsentRow struct {
	in     domain.PhoneReserveInput
	reason string
}

// phoneStore is a store fake for DeliverPhone: the targets as PhoneTargets would read them, scripted
// reservations per channel, and every write recorded.
type phoneStore struct {
	*fakeStore
	targets []domain.PhoneTarget

	mu            sync.Mutex
	reserveStatus map[string]domain.PhoneReservationStatus // by channel; default Proceed
	existing      map[string]string                        // by channel: the reason of an Existing row
	// unsentExisting is, by channel, the reason of a row an earlier attempt of the event wrote: the
	// unsent path finds it and writes nothing.
	unsentExisting map[string]string
	reserveErr     map[string]error // by channel
	unsentErr      map[string]error // by channel
	reserveHook    func(ctx context.Context, in domain.PhoneReserveInput)
	finishErr      map[string]error // by channel
	reserved       []domain.PhoneReserveInput
	unsent         []unsentRow
	finished       map[string]domain.PhoneOutcome // by channel
	deliveryCh     map[string]string              // delivery id → channel
	nextID         int
}

func newPhoneStore(targets ...domain.PhoneTarget) *phoneStore {
	return &phoneStore{fakeStore: &fakeStore{}, targets: targets,
		reserveStatus: map[string]domain.PhoneReservationStatus{}, existing: map[string]string{},
		unsentExisting: map[string]string{},
		reserveErr:     map[string]error{}, finishErr: map[string]error{},
		finished: map[string]domain.PhoneOutcome{}, deliveryCh: map[string]string{}}
}

func (s *phoneStore) PhoneTargets(_ context.Context, ids []string) ([]domain.PhoneTarget, error) {
	var out []domain.PhoneTarget
	for _, id := range ids {
		for _, t := range s.targets {
			if t.ChannelID == id {
				out = append(out, t)
			}
		}
	}
	return out, nil
}

func (s *phoneStore) ReservePhoneDelivery(ctx context.Context, in domain.PhoneReserveInput) (domain.PhoneReservation, error) {
	if s.reserveHook != nil {
		s.reserveHook(ctx, in)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserved = append(s.reserved, in)
	if err := s.reserveErr[in.ChannelID]; err != nil {
		return domain.PhoneReservation{}, err
	}
	s.nextID++
	id := fmt.Sprintf("d%d", s.nextID)
	s.deliveryCh[id] = in.ChannelID
	switch st := s.reserveStatus[in.ChannelID]; st {
	case domain.PhoneReservationStormLimited:
		return domain.PhoneReservation{ID: id, Status: st, Reason: domain.PhoneReasonStormLimited}, nil
	case domain.PhoneReservationClosedStale:
		return domain.PhoneReservation{ID: id, Status: st, Reason: domain.PhoneReasonOutcomeUnknown}, nil
	case domain.PhoneReservationExisting:
		return domain.PhoneReservation{ID: id, Status: st, Reason: s.existing[in.ChannelID]}, nil
	case "", domain.PhoneReservationProceed:
		return domain.PhoneReservation{ID: id, Status: domain.PhoneReservationProceed, Reason: domain.PhoneReasonSending, Attempts: 1}, nil
	default: // a status this package does not know
		return domain.PhoneReservation{ID: id, Status: st}, nil
	}
}

// RecordUnsentPhoneDelivery writes the row unless the event already has one for the channel
// (unsentExisting, by channel: that row's reason), as the real store's ON CONFLICT does.
func (s *phoneStore) RecordUnsentPhoneDelivery(_ context.Context, in domain.PhoneReserveInput, reason string) (bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.unsentErr[in.ChannelID]; err != nil {
		return false, "", err
	}
	if prev, ok := s.unsentExisting[in.ChannelID]; ok {
		return false, prev, nil
	}
	s.unsent = append(s.unsent, unsentRow{in: in, reason: reason})
	return true, reason, nil
}

// FinishPhoneDelivery fails on a spent context as the real store would: the answer must be stored
// with a context of its own, or a call the provider accepted at the deadline is lost.
func (s *phoneStore) FinishPhoneDelivery(ctx context.Context, id string, out domain.PhoneOutcome) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := s.deliveryCh[id]
	if err := s.finishErr[ch]; err != nil {
		return err
	}
	s.finished[ch] = out
	return nil
}

// fakeTelephony is the telephony in force: settings and a provider (or the error building it).
type fakeTelephony struct {
	ts       domain.TelephonySettings
	provider telephony.Provider
	err      error
	built    atomic.Int32
}

func (f *fakeTelephony) Settings() domain.TelephonySettings { return f.ts }
func (f *fakeTelephony) Provider() (telephony.Provider, error) {
	f.built.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.provider, nil
}

func enabledTelephony(p telephony.Provider) *fakeTelephony {
	ts := domain.TelephonySettings{Configured: true, Enabled: true, Provider: domain.TelephonyProviderTwilio,
		PublicBaseURL: "https://cerbix.example"}
	ts.Normalize()
	return &fakeTelephony{ts: ts, provider: p}
}

// phoneMetrics counts series as "kind|provider|reason" and "scope|kind|event".
type phoneMetrics struct {
	mu         sync.Mutex
	deliveries map[string]int
	quota      map[string]int
}

func (m *phoneMetrics) RecordPhoneDelivery(kind, provider, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deliveries == nil {
		m.deliveries = map[string]int{}
	}
	m.deliveries[kind+"|"+provider+"|"+reason]++
}

func (m *phoneMetrics) RecordPhoneQuotaEvent(scope, kind, event string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.quota == nil {
		m.quota = map[string]int{}
	}
	m.quota[scope+"|"+kind+"|"+event]++
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)) }

func smsTarget(id, number string) domain.PhoneTarget {
	return domain.PhoneTarget{ChannelID: id, ProjectID: "p1", Kind: domain.ChannelSMS,
		Recipient: domain.PhoneRecipientNumber, Number: number}
}

func voiceTarget(id, userID, number string) domain.PhoneTarget {
	return domain.PhoneTarget{ChannelID: id, ProjectID: "p1", Kind: domain.ChannelVoice,
		Recipient: domain.PhoneRecipientUser, UserID: userID, Number: number}
}

var stepAlert = telephony.PhoneAlert{Kind: telephony.KindEscalationStep, Step: 2, SubjectName: "payments-api",
	ProjectName: "prod", IncidentRef: "X7K2P9QM", PublicBaseURL: "https://cerbix.example"}

var stepEvent = domain.PhoneEventRef{OutboxEventID: "ev1", IncidentID: "inc1"}

// Every part-(a) reason of §4.5, each produced by its cause, with what the call returns (D19), what
// it counts (D20, D31) and the metric of the row it finished.
func TestDeliverPhoneReasons(t *testing.T) {
	rejected := telephony.Result{Outcome: telephony.Rejected, HTTPStatus: 400, Code: "21211"}
	unreachable := telephony.Result{Outcome: telephony.Unreachable, HTTPStatus: 503}
	cases := []struct {
		name   string
		target domain.PhoneTarget
		setup  func(s *phoneStore, src *fakeTelephony, p *telephony.FakeProvider)
		calls  int
		// finished is the provider outcome stored, "" for none; unsent is the unsent row's reason
		// and notes.
		finished       string
		unsent         string
		unsentNotes    []string
		wantErr        error // nil: no error; errAny: any error
		resolved       int
		delivered      int
		metric         string // kind|provider|reason, "" for none
		storm          bool
		providerBuilds int32
	}{
		{name: "sent", target: smsTarget("c1", numA), calls: 1, finished: domain.PhoneReasonSent,
			resolved: 1, delivered: 1, metric: "sms|fake|sent", providerBuilds: 1},
		{name: "provider_unreachable", target: smsTarget("c1", numA),
			setup: func(_ *phoneStore, _ *fakeTelephony, p *telephony.FakeProvider) { p.ScriptSMS(unreachable) },
			calls: 1, finished: domain.PhoneReasonProviderUnreachable, wantErr: errAny, resolved: 1,
			metric: "sms|fake|provider_unreachable", providerBuilds: 1},
		{name: "provider_rejected", target: smsTarget("c1", numA),
			setup: func(_ *phoneStore, _ *fakeTelephony, p *telephony.FakeProvider) { p.ScriptSMS(rejected) },
			calls: 1, finished: domain.PhoneReasonProviderRejected, resolved: 1,
			metric: "sms|fake|provider_rejected", providerBuilds: 1},
		{name: "voice rejected", target: voiceTarget("c1", "u1", numA),
			setup: func(_ *phoneStore, _ *fakeTelephony, p *telephony.FakeProvider) { p.ScriptCall(rejected) },
			calls: 1, finished: domain.PhoneReasonProviderRejected, resolved: 1,
			metric: "voice|fake|provider_rejected", providerBuilds: 1},
		{name: "outcome_unknown (a stale sending row met by the retry)", target: smsTarget("c1", numA),
			setup: func(s *phoneStore, _ *fakeTelephony, _ *telephony.FakeProvider) {
				s.reserveStatus["c1"] = domain.PhoneReservationClosedStale
			},
			resolved: 1, metric: "sms|fake|outcome_unknown", providerBuilds: 1},
		{name: "storm_limited", target: smsTarget("c1", numA),
			setup: func(s *phoneStore, _ *fakeTelephony, _ *telephony.FakeProvider) {
				s.reserveStatus["c1"] = domain.PhoneReservationStormLimited
			},
			resolved: 1, metric: "sms|fake|storm_limited", storm: true, providerBuilds: 1},
		{name: "sending row too young: in flight", target: smsTarget("c1", numA),
			setup: func(s *phoneStore, _ *fakeTelephony, _ *telephony.FakeProvider) {
				s.reserveErr["c1"] = domain.ErrPhoneDeliveryInFlight
			},
			wantErr: domain.ErrPhoneDeliveryInFlight, resolved: 1, providerBuilds: 1},
		{name: "already sent (a retry)", target: smsTarget("c1", numA),
			setup: func(s *phoneStore, _ *fakeTelephony, _ *telephony.FakeProvider) {
				s.reserveStatus["c1"], s.existing["c1"] = domain.PhoneReservationExisting, domain.PhoneReasonSent
			},
			resolved: 1, delivered: 1, providerBuilds: 1},
		{name: "already rejected (a retry)", target: smsTarget("c1", numA),
			setup: func(s *phoneStore, _ *fakeTelephony, _ *telephony.FakeProvider) {
				s.reserveStatus["c1"], s.existing["c1"] = domain.PhoneReservationExisting, domain.PhoneReasonProviderRejected
			},
			resolved: 1, providerBuilds: 1},
		{name: "answer for a row closed meanwhile", target: smsTarget("c1", numA),
			setup: func(s *phoneStore, _ *fakeTelephony, _ *telephony.FakeProvider) {
				s.finishErr["c1"] = fmt.Errorf("store: finish: %w", domain.ErrPhoneDeliveryClosed)
			},
			calls: 1, resolved: 1, providerBuilds: 1},
		{name: "provider cannot be built", target: smsTarget("c1", numA),
			setup: func(_ *phoneStore, src *fakeTelephony, _ *telephony.FakeProvider) {
				src.err = telephony.ErrSecretsUnreadable
			},
			finished: domain.PhoneReasonProviderUnreachable, wantErr: errAny, resolved: 1,
			metric: "sms|twilio|provider_unreachable", providerBuilds: 1},
		{name: "not_deliverable: no verified number", target: domain.PhoneTarget{ChannelID: "c1", ProjectID: "p1",
			Kind: domain.ChannelSMS, Recipient: domain.PhoneRecipientUser, UserID: "u1",
			NotRouteReason: domain.DeliverableNoVerifiedNumber},
			unsent: domain.PhoneReasonNotDeliverable, unsentNotes: []string{domain.PhoneNoteNoVerifiedNumber},
			metric: "sms|twilio|not_deliverable"},
		{name: "not_deliverable: recipient not eligible", target: domain.PhoneTarget{ChannelID: "c1", ProjectID: "p1",
			Kind: domain.ChannelVoice, Recipient: domain.PhoneRecipientUser, UserID: "u1", Number: numA,
			NotRouteReason: domain.DeliverableRecipientNotEligible},
			unsent: domain.PhoneReasonNotDeliverable, unsentNotes: []string{domain.PhoneNoteRecipientNotEligible},
			metric: "voice|twilio|not_deliverable"},
		{name: "not_deliverable: recipient deleted", target: domain.PhoneTarget{ChannelID: "c1", ProjectID: "p1",
			Kind: domain.ChannelSMS, Recipient: domain.PhoneRecipientUser,
			NotRouteReason: domain.DeliverableRecipientDeleted},
			unsent: domain.PhoneReasonNotDeliverable, unsentNotes: []string{domain.PhoneNoteRecipientNotEligible},
			metric: "sms|twilio|not_deliverable"},
		{name: "not_deliverable: channel number unverified", target: domain.PhoneTarget{ChannelID: "c1", ProjectID: "p1",
			Kind: domain.ChannelSMS, Recipient: domain.PhoneRecipientNumber,
			NotRouteReason: domain.DeliverableNumberUnverified},
			unsent: domain.PhoneReasonNotDeliverable, unsentNotes: []string{domain.PhoneNoteNoVerifiedNumber},
			metric: "sms|twilio|not_deliverable"},
		{name: "not_deliverable: a route without a number (P1)", target: smsTarget("c1", ""),
			unsent: domain.PhoneReasonNotDeliverable, unsentNotes: []string{domain.PhoneNoteNoVerifiedNumber},
			metric: "sms|twilio|not_deliverable"},
		{name: "telephony_disabled: no provider built", target: domain.PhoneTarget{ChannelID: "c1", ProjectID: "p1",
			Kind: domain.ChannelSMS, Recipient: domain.PhoneRecipientNumber, Number: numA,
			NotRouteReason: domain.DeliverableTelephonyDisabled},
			unsent: domain.PhoneReasonTelephonyDisabled, metric: "sms|twilio|telephony_disabled"},
		{name: "voice_unsupported_by_provider", target: domain.PhoneTarget{ChannelID: "c1", ProjectID: "p1",
			Kind: domain.ChannelVoice, Recipient: domain.PhoneRecipientNumber, Number: numA,
			NotRouteReason: domain.DeliverableVoiceUnsupportedByProvider},
			unsent: domain.PhoneReasonVoiceUnsupportedByProvider, metric: "voice|twilio|voice_unsupported_by_provider"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newPhoneStore(c.target)
			p := telephony.NewFakeProvider()
			src := enabledTelephony(p)
			if c.setup != nil {
				c.setup(st, src, p)
			}
			m := &phoneMetrics{}
			d := New(st, nil).WithPhone(st, src, m, discardLogger())
			res, err := d.DeliverPhone(context.Background(), []string{"c1"}, stepAlert, stepEvent)

			switch {
			case c.wantErr == nil && err != nil:
				t.Fatalf("err = %v; want nil (a final outcome)", err)
			case c.wantErr == errAny && err == nil:
				t.Fatal("err = nil; want an error the outbox retries")
			case c.wantErr != nil && c.wantErr != errAny && !errors.Is(err, c.wantErr):
				t.Fatalf("err = %v; want %v", err, c.wantErr)
			}
			if got := len(p.Calls()); got != c.calls {
				t.Fatalf("provider calls = %d; want %d", got, c.calls)
			}
			if got := src.built.Load(); got != c.providerBuilds {
				t.Fatalf("provider built %d times; want %d", got, c.providerBuilds)
			}
			want := domain.ChannelDelivery{Requested: 1, Resolved: c.resolved, Delivered: c.delivered}
			if res != want {
				t.Fatalf("delivery = %+v; want %+v", res, want)
			}
			if got := st.finished["c1"].Reason; got != c.finished {
				t.Fatalf("finished %q; want %q", got, c.finished)
			}
			if c.unsent == "" {
				if len(st.unsent) != 0 {
					t.Fatalf("unsent rows %+v; want none", st.unsent)
				}
			} else {
				if len(st.unsent) != 1 || st.unsent[0].reason != c.unsent {
					t.Fatalf("unsent rows %+v; want one %s", st.unsent, c.unsent)
				}
				if len(st.reserved) != 0 {
					t.Fatal("a channel that is no route was reserved")
				}
				for _, n := range c.unsentNotes {
					if !contains(st.unsent[0].in.Notes, n) {
						t.Fatalf("unsent notes %v; want %s", st.unsent[0].in.Notes, n)
					}
				}
			}
			var wantMetrics map[string]int
			if c.metric != "" {
				wantMetrics = map[string]int{c.metric: 1}
			}
			if fmt.Sprint(m.deliveries) != fmt.Sprint(wantMetrics) {
				t.Fatalf("delivery metrics %v; want %v", m.deliveries, wantMetrics)
			}
			wantStorm := map[string]int(nil)
			if c.storm {
				wantStorm = map[string]int{"storm|" + string(c.target.Kind) + "|exceeded": 1}
			}
			if fmt.Sprint(m.quota) != fmt.Sprint(wantStorm) {
				t.Fatalf("storm metric %v; want %v", m.quota, wantStorm)
			}
			// P6: never reported as sent unless the provider accepted.
			if c.finished != domain.PhoneReasonSent && res.Delivered > 0 && c.name != "already sent (a retry)" {
				t.Fatal("delivered without an accepted call")
			}
		})
	}
}

// errAny marks a case that must return SOME error.
var errAny = errors.New("any error")

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// What the reservation and the provider are handed: the event's keys (D33), a monitor DOWN's seq
// (D32), the caller's notes plus the renderer's, the rendered text in the instance's encoding, and
// the voice in the recipient's language else the instance's. Segments are stored on a sent SMS.
func TestDeliverPhoneHandsTheRightFacts(t *testing.T) {
	sms := smsTarget("c1", numA)
	voice := voiceTarget("c2", "u2", numB)
	voice.Language = "tr-TR"
	plainVoice := voiceTarget("c3", "u3", numC)
	st := newPhoneStore(sms, voice, plainVoice)
	p := telephony.NewFakeProvider()
	src := enabledTelephony(p)
	src.ts.VoiceLanguage = "de-DE"
	src.ts.SMSMaxSegments = 1
	d := New(st, nil).WithPhone(st, src, nil, discardLogger())

	// A name long enough that one segment cannot hold the link: link_omitted joins the caller's note.
	alert := telephony.PhoneAlert{Kind: telephony.KindMonitorDown, SubjectName: strings.Repeat("payments ", 20),
		ProjectName: "prod", IncidentRef: "X7K2P9QM", PublicBaseURL: "https://cerbix.example/" + strings.Repeat("x", 120)}
	ev := domain.PhoneEventRef{OutboxEventID: "ev9", MonitorID: "m1", MonitorSeq: 42, ServiceID: "",
		Notes: []string{domain.PhoneNoteNoPublicURL}}
	res, err := d.DeliverPhone(context.Background(), []string{"c1", "c2", "c3"}, alert, ev)
	if err != nil || res.Resolved != 3 || res.Delivered != 3 {
		t.Fatalf("DeliverPhone = %+v, %v", res, err)
	}
	byCh := map[string]domain.PhoneReserveInput{}
	for _, in := range st.reserved {
		byCh[in.ChannelID] = in
	}
	in := byCh["c1"]
	if in.ProjectID != "p1" || in.OutboxEventID != "ev9" || in.Kind != domain.ChannelSMS ||
		in.AlertKind != domain.PhoneAlertMonitorDown || in.MonitorID != "m1" || in.MonitorSeq != 42 ||
		in.Number != numA || in.UserID != "" {
		t.Fatalf("sms reservation %+v", in)
	}
	if !contains(in.Notes, domain.PhoneNoteNoPublicURL) || !contains(in.Notes, domain.PhoneNoteLinkOmitted) {
		t.Fatalf("sms notes %v; want the caller's and link_omitted", in.Notes)
	}
	if v := byCh["c2"]; v.UserID != "u2" || v.Number != numB || v.Kind != domain.ChannelVoice || contains(v.Notes, domain.PhoneNoteLinkOmitted) {
		t.Fatalf("voice reservation %+v", v)
	}
	if len(ev.Notes) != 1 {
		t.Fatalf("the caller's notes were modified: %v", ev.Notes)
	}

	wantSMS, err := telephony.RenderSMS(alert, telephony.EncodingTransliterate, 1)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]telephony.FakeCall{}
	for _, c := range p.Calls() {
		body[c.To] = c
	}
	if body[numA].Kind != "sms" || body[numA].Body != wantSMS.Text {
		t.Fatalf("sms sent %+v; want %q", body[numA], wantSMS.Text)
	}
	if body[numB].Body != telephony.RenderVoice(alert, "tr-TR") || body[numC].Body != telephony.RenderVoice(alert, "de-DE") {
		t.Fatalf("voice bodies: %q / %q", body[numB].Body, body[numC].Body)
	}
	if got := st.finished["c1"]; got.Segments != wantSMS.Segments || got.Reason != domain.PhoneReasonSent || got.Ref == "" || got.HTTPStatus != 201 {
		t.Fatalf("sms finished %+v; want %d segments and the provider's answer", got, wantSMS.Segments)
	}
	if got := st.finished["c2"]; got.Segments != 0 {
		t.Fatalf("voice finished %+v; a call has no segments", got)
	}
}

// A render error is a programming error (settings validate the base URL, refs come from the DB): the
// event fails and nothing is reserved or sent.
func TestDeliverPhoneRenderErrorFailsTheEvent(t *testing.T) {
	st := newPhoneStore(smsTarget("c1", numA))
	p := telephony.NewFakeProvider()
	d := New(st, nil).WithPhone(st, enabledTelephony(p), nil, discardLogger())
	bad := stepAlert
	bad.IncidentRef = "not a ref!"
	if _, err := d.DeliverPhone(context.Background(), []string{"c1"}, bad, stepEvent); err == nil {
		t.Fatal("a malformed incident ref rendered")
	}
	if len(st.reserved) != 0 || len(p.Calls()) != 0 {
		t.Fatal("reserved or sent despite the render error")
	}
}

// §4.5's classification, for both providers, through DeliverPhone: only a 2xx is `sent` and counted
// delivered (D20); 408, 429, 5xx and a network error are `provider_unreachable` and fail the event
// (D19); any other status, a 3xx included, is `provider_rejected` and final.
func TestDeliverPhoneClassifiesBothProviders(t *testing.T) {
	type answer struct {
		status int
		reason string
	}
	answers := []answer{
		{201, domain.PhoneReasonSent}, {200, domain.PhoneReasonSent},
		{408, domain.PhoneReasonProviderUnreachable}, {429, domain.PhoneReasonProviderUnreachable},
		{500, domain.PhoneReasonProviderUnreachable}, {503, domain.PhoneReasonProviderUnreachable},
		{302, domain.PhoneReasonProviderRejected}, {400, domain.PhoneReasonProviderRejected},
		{404, domain.PhoneReasonProviderRejected},
		{0, domain.PhoneReasonProviderUnreachable}, // the server is gone: a network error
	}
	providers := map[string]func(url string) telephony.Provider{
		"twilio": func(url string) telephony.Provider {
			orig := telephony.TwilioBaseURL
			telephony.TwilioBaseURL = url
			defer func() { telephony.TwilioBaseURL = orig }()
			return telephony.NewTwilio(telephony.TwilioConfig{AccountSID: "AC1", AuthToken: "tok", From: "+15005550006"}, nil)
		},
		"generic_http": func(url string) telephony.Provider {
			g, err := telephony.NewGenericHTTP(telephony.GenericHTTPConfig{Method: "POST", URL: url + "/send",
				ContentType: telephony.ContentTypeJSON, BodyTemplate: `{"to":"{{to}}","text":"{{text}}"}`}, nil)
			if err != nil {
				panic(err)
			}
			return g
		},
	}
	for name, build := range providers {
		for _, a := range answers {
			t.Run(fmt.Sprintf("%s %d", name, a.status), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if a.status == http.StatusFound {
						w.Header().Set("Location", "/elsewhere")
					}
					w.WriteHeader(a.status)
					_, _ = w.Write([]byte(`{"sid":"SM123","code":21211,"message":"the number +905551236869 is bad"}`))
				}))
				p := build(srv.URL)
				if a.status == 0 {
					srv.Close()
				} else {
					defer srv.Close()
				}
				st := newPhoneStore(smsTarget("c1", numA))
				src := enabledTelephony(p)
				d := New(st, nil).WithPhone(st, src, nil, discardLogger())
				res, err := d.DeliverPhone(context.Background(), []string{"c1"}, stepAlert, stepEvent)
				got := st.finished["c1"]
				if got.Reason != a.reason {
					t.Fatalf("reason %q; want %q", got.Reason, a.reason)
				}
				if a.status != 0 && got.HTTPStatus != a.status {
					t.Fatalf("status %d; want %d", got.HTTPStatus, a.status)
				}
				if (err != nil) != (a.reason == domain.PhoneReasonProviderUnreachable) {
					t.Fatalf("err = %v for %s", err, a.reason)
				}
				if wantDelivered := map[bool]int{true: 1}[a.reason == domain.PhoneReasonSent]; res.Delivered != wantDelivered {
					t.Fatalf("delivered %d for %s", res.Delivered, a.reason)
				}
				if strings.Contains(got.Code+got.Ref, "+90") || (err != nil && strings.Contains(err.Error(), "555")) {
					t.Fatalf("the response text reached the row or the error: %+v, %v", got, err)
				}
			})
		}
	}
}

// slowProvider answers per number after a delay, honouring the context as the real providers do
// (a request cut by the deadline is Unreachable), and records each call's deadline and the most
// calls it ever had in flight at once.
type slowProvider struct {
	delay     map[string]time.Duration // by number; a negative delay waits for the context
	mu        sync.Mutex
	inFlight  int
	maxFlight int
	deadlines map[string]time.Duration // by number: the call context's remaining time
	called    map[string]bool
}

func (s *slowProvider) Name() string        { return "fake" }
func (s *slowProvider) SupportsVoice() bool { return true }
func (s *slowProvider) PlaceCall(ctx context.Context, to, _ string) telephony.Result {
	return s.SendSMS(ctx, to, "")
}
func (s *slowProvider) SendSMS(ctx context.Context, to, _ string) telephony.Result {
	s.mu.Lock()
	s.inFlight++
	s.maxFlight = max(s.maxFlight, s.inFlight)
	if s.deadlines == nil {
		s.deadlines, s.called = map[string]time.Duration{}, map[string]bool{}
	}
	s.called[to] = true
	if dl, ok := ctx.Deadline(); ok {
		s.deadlines[to] = time.Until(dl)
	}
	delay := s.delay[to]
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.inFlight--; s.mu.Unlock() }()
	var wait <-chan time.Time
	if delay >= 0 {
		wait = time.After(delay)
	}
	select {
	case <-wait:
		return telephony.Result{Outcome: telephony.Sent, HTTPStatus: 201}
	case <-ctx.Done():
		return telephony.Result{Outcome: telephony.Unreachable, TransportClass: "timeout"}
	}
}

// D33's budget with three phone channels: two answer at once, the third gets no answer before the
// budget ends — it is provider_unreachable, the call returns when the budget does (not after 5 s),
// and the event fails for the retry. Each call is bounded by min(5 s, what is left).
func TestDeliverPhoneBudget(t *testing.T) {
	st := newPhoneStore(smsTarget("a", numA), smsTarget("b", numB), smsTarget("c", numC))
	p := &slowProvider{delay: map[string]time.Duration{numA: 0, numB: 0, numC: -1}}
	d := New(st, nil).WithPhone(st, enabledTelephony(p), nil, discardLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := d.DeliverPhone(ctx, []string{"a", "b", "c"}, stepAlert, stepEvent)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("took %s; the budget was 300 ms", took)
	}
	if err == nil {
		t.Fatal("the unreached channel did not fail the event")
	}
	if res.Resolved != 3 || res.Delivered != 2 {
		t.Fatalf("delivery = %+v; want 3 resolved, 2 delivered", res)
	}
	if st.finished["a"].Reason != domain.PhoneReasonSent || st.finished["b"].Reason != domain.PhoneReasonSent ||
		st.finished["c"].Reason != domain.PhoneReasonProviderUnreachable {
		t.Fatalf("finished %+v", st.finished)
	}
	for to, left := range p.deadlines {
		if left > 300*time.Millisecond {
			t.Fatalf("call to %s had %s; more than the budget", to, left)
		}
	}

	// A long budget still gives one call at most 5 s.
	st = newPhoneStore(smsTarget("a", numA))
	p = &slowProvider{}
	d = New(st, nil).WithPhone(st, enabledTelephony(p), nil, discardLogger())
	long, cancelLong := context.WithTimeout(context.Background(), time.Minute)
	defer cancelLong()
	if _, err := d.DeliverPhone(long, []string{"a"}, stepAlert, stepEvent); err != nil {
		t.Fatal(err)
	}
	if left := p.deadlines[numA]; left <= 0 || left > 5*time.Second {
		t.Fatalf("call had %s; want at most 5 s", left)
	}
}

// A channel whose turn comes after the budget ended is not called: it is provider_unreachable, and
// its answer is still stored although the budget's context is spent.
func TestDeliverPhoneBudgetEndsBeforeTheCall(t *testing.T) {
	st := newPhoneStore(smsTarget("a", numA))
	st.reserveHook = func(context.Context, domain.PhoneReserveInput) { time.Sleep(200 * time.Millisecond) } // a slow reservation
	p := &slowProvider{}
	d := New(st, nil).WithPhone(st, enabledTelephony(p), nil, discardLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := d.DeliverPhone(ctx, []string{"a"}, stepAlert, stepEvent)
	if err == nil {
		t.Fatal("an unreached channel did not fail the event")
	}
	if p.called[numA] {
		t.Fatal("the provider was called after the budget ended")
	}
	if got := st.finished["a"].Reason; got != domain.PhoneReasonProviderUnreachable {
		t.Fatalf("finished %q; want provider_unreachable stored after the budget", got)
	}
}

// D33: phone channels of one event are called concurrently, at most four at a time.
func TestDeliverPhoneConcurrency(t *testing.T) {
	var targets []domain.PhoneTarget
	var ids []string
	delay := map[string]time.Duration{}
	for i := 0; i < 10; i++ {
		id, num := fmt.Sprintf("c%d", i), fmt.Sprintf("+9055512300%02d", i)
		targets, ids = append(targets, smsTarget(id, num)), append(ids, id)
		delay[num] = 60 * time.Millisecond
	}
	st := newPhoneStore(targets...)
	p := &slowProvider{delay: delay}
	d := New(st, nil).WithPhone(st, enabledTelephony(p), nil, discardLogger())
	res, err := d.DeliverPhone(context.Background(), ids, stepAlert, stepEvent)
	if err != nil || res.Delivered != 10 {
		t.Fatalf("DeliverPhone = %+v, %v", res, err)
	}
	if p.maxFlight != 4 {
		t.Fatalf("at most %d calls in flight; want 4", p.maxFlight)
	}
}

// P3: no number in any log line, any error, any formatted or serialized target — over every path
// that logs.
func TestDeliverPhoneNeverLogsANumber(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	targets := []domain.PhoneTarget{
		smsTarget("ok", numA), smsTarget("down", numB), smsTarget("no", numC),
		voiceTarget("closed", "u1", "+905551110001"), smsTarget("storm", "+905551110002"),
		{ChannelID: "off", ProjectID: "p1", Kind: domain.ChannelSMS, Recipient: domain.PhoneRecipientUser,
			UserID: "u2", Number: "+905551110003", NotRouteReason: domain.DeliverableRecipientNotEligible},
	}
	st := newPhoneStore(targets...)
	st.reserveStatus["storm"] = domain.PhoneReservationStormLimited
	st.finishErr["closed"] = domain.ErrPhoneDeliveryClosed
	p := &scriptedByNumber{FakeProvider: telephony.NewFakeProvider(), results: map[string]telephony.Result{
		numB: {Outcome: telephony.Unreachable, TransportClass: "connection refused"},
		numC: {Outcome: telephony.Rejected, HTTPStatus: 400, Code: "21211"},
	}}
	ids := []string{"ok", "down", "no", "closed", "storm", "off"}
	var errs []error
	_, err := New(st, nil).WithPhone(st, enabledTelephony(p), nil, logger).DeliverPhone(context.Background(), ids, stepAlert, stepEvent)
	errs = append(errs, err)
	broken := enabledTelephony(nil)
	broken.err = telephony.ErrSecretsUnreadable
	bst := newPhoneStore(targets...)
	_, err = New(bst, nil).WithPhone(bst, broken, nil, logger).DeliverPhone(context.Background(), ids, stepAlert, stepEvent)
	errs = append(errs, err)

	if logs.Len() == 0 {
		t.Fatal("nothing was logged; the test would prove nothing")
	}
	var text strings.Builder
	text.WriteString(logs.String())
	for _, e := range errs {
		if e != nil {
			text.WriteString(e.Error())
		}
	}
	for _, tg := range targets {
		raw, _ := json.Marshal(tg)
		fmt.Fprintf(&text, "%v %+v %#v %s %s", tg, tg, tg, tg, raw)
		var rec bytes.Buffer
		slog.New(slog.NewTextHandler(&rec, nil)).Info("target", "t", tg)
		text.WriteString(rec.String())
	}
	for _, tg := range targets {
		national := tg.Number[len(tg.Number)-7:]
		if strings.Contains(text.String(), national) {
			t.Fatalf("number of %s in the output:\n%s", tg.ChannelID, text.String())
		}
	}
}

// scriptedByNumber answers per destination; every other number gets the fake's default.
type scriptedByNumber struct {
	*telephony.FakeProvider
	results map[string]telephony.Result
}

func (s *scriptedByNumber) SendSMS(ctx context.Context, to, text string) telephony.Result {
	if r, ok := s.results[to]; ok {
		return r
	}
	return s.FakeProvider.SendSMS(ctx, to, text)
}

// D16: a phone channel among a text path's channels is skipped — the others deliver as before and
// DeliverChannelsReporting counts only them as resolved. Before FR-041a's notify change, the sms
// channel failed the whole event ("unknown channel type").
func TestTextPathsSkipPhoneChannels(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	slack := domain.NotificationChannel{ID: "s", Type: domain.ChannelSlack, Enabled: true, Config: map[string]string{"url": srv.URL}}
	sms := domain.NotificationChannel{ID: "p1", Type: domain.ChannelSMS, Enabled: true, Recipient: domain.PhoneRecipientNumber}
	voice := domain.NotificationChannel{ID: "p2", Type: domain.ChannelVoice, Enabled: true, Recipient: domain.PhoneRecipientUser}
	fs := &fakeStore{channels: []domain.NotificationChannel{sms, slack, voice}}
	d := New(fs, nil)
	ctx := context.Background()
	mon := domain.Monitor{ID: "m1", Name: "API", ProjectID: "p"}

	for name, deliver := range map[string]func() error{
		"Deliver":            func() error { return d.Deliver(ctx, mon, false, nil) },
		"DeliverText":        func() error { return d.DeliverText(ctx, mon, "burn") },
		"DeliverProjectText": func() error { return d.DeliverProjectText(ctx, "p", "report") },
		"DeliverChannels":    func() error { return d.DeliverChannels(ctx, []string{"p1", "s", "p2"}, "step") },
	} {
		before := posts.Load()
		if err := deliver(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := posts.Load() - before; got != 1 {
			t.Fatalf("%s: %d posts; want the Slack channel's one", name, got)
		}
	}
	res, err := d.DeliverChannelsReporting(ctx, []string{"p1", "s", "p2"}, "alert")
	if err != nil {
		t.Fatal(err)
	}
	if want := (domain.ChannelDelivery{Requested: 3, Resolved: 1, Delivered: 1}); res != want {
		t.Fatalf("reporting = %+v; want %+v (the phone channels are DeliverPhone's)", res, want)
	}
}

// A set with no phone channel: DeliverPhone writes nothing, builds no provider and returns no error
// — wired or not. A phone channel without the wiring is an error, never a silent drop.
func TestDeliverPhoneWithoutPhoneChannels(t *testing.T) {
	st := newPhoneStore() // PhoneTargets finds nothing: the ids are text channels
	src := enabledTelephony(telephony.NewFakeProvider())
	for name, d := range map[string]*Dispatcher{
		"wired":   New(st, nil).WithPhone(st, src, nil, discardLogger()),
		"unwired": New(st, nil),
	} {
		res, err := d.DeliverPhone(context.Background(), []string{"slack", "hook"}, stepAlert, stepEvent)
		if err != nil || res != (domain.ChannelDelivery{Requested: 2}) {
			t.Fatalf("%s: %+v, %v", name, res, err)
		}
	}
	if src.built.Load() != 0 || len(st.reserved)+len(st.unsent) != 0 {
		t.Fatal("a set without phone channels touched the provider or the store")
	}
	unwired := &fakeStore{channels: []domain.NotificationChannel{{ID: "c1", Type: domain.ChannelSMS, Enabled: true}}}
	if _, err := New(unwired, nil).DeliverPhone(context.Background(), []string{"c1"}, stepAlert, stepEvent); err == nil {
		t.Fatal("an unwired dispatcher dropped a phone channel silently")
	}
}

// D16 / D31: one snapshot with text and phone recipients. The text call resolves and delivers the
// text channels; the phone call resolves the routes among the phone channels; nothing is counted
// twice and nothing fails.
func TestMixedRecipientsSplitBetweenThePaths(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	st := newPhoneStore(smsTarget("p1", numA), domain.PhoneTarget{ChannelID: "p2", ProjectID: "p1",
		Kind: domain.ChannelVoice, Recipient: domain.PhoneRecipientUser, UserID: "u",
		NotRouteReason: domain.DeliverableNoVerifiedNumber})
	st.channels = []domain.NotificationChannel{
		{ID: "s", Type: domain.ChannelSlack, Enabled: true, Config: map[string]string{"url": srv.URL}},
		{ID: "p1", Type: domain.ChannelSMS, Enabled: true},
		{ID: "p2", Type: domain.ChannelVoice, Enabled: true},
	}
	d := New(st, nil).WithPhone(st, enabledTelephony(telephony.NewFakeProvider()), nil, discardLogger())
	ids := []string{"s", "p1", "p2"}
	text, err := d.DeliverChannelsReporting(context.Background(), ids, "alert")
	if err != nil {
		t.Fatal(err)
	}
	phone, err := d.DeliverPhone(context.Background(), ids, stepAlert, stepEvent)
	if err != nil {
		t.Fatal(err)
	}
	if text.Resolved+phone.Resolved != 2 || text.Delivered+phone.Delivered != 2 {
		t.Fatalf("text %+v + phone %+v; want 2 resolved (slack, the sms route), 2 delivered", text, phone)
	}
	reasons := []string{st.finished["p1"].Reason}
	for _, u := range st.unsent {
		reasons = append(reasons, u.in.ChannelID+":"+u.reason)
	}
	sort.Strings(reasons)
	if fmt.Sprint(reasons) != fmt.Sprint([]string{"p2:not_deliverable", "sent"}) {
		t.Fatalf("phone rows %v", reasons)
	}
}

// FR-041a revision 7 §4.5: the service state an alert announced and the escalation step that handed
// it over reach the row, on the reservation and on an unsent row alike.
func TestDeliverPhoneRecordsServiceStateAndStep(t *testing.T) {
	route := smsTarget("c1", numA)
	noNumber := smsTarget("c2", "")
	noNumber.NotRouteReason = domain.DeliverableNumberUnverified
	st := newPhoneStore(route, noNumber)
	d := New(st, nil).WithPhone(st, enabledTelephony(telephony.NewFakeProvider()), nil, discardLogger())
	ev := domain.PhoneEventRef{OutboxEventID: "ev1", IncidentID: "inc1", ServiceID: "s1",
		ServiceState: string(domain.ServiceAlertDown), EscalationStep: 3}
	if _, err := d.DeliverPhone(context.Background(), []string{"c1", "c2"}, stepAlert, ev); err != nil {
		t.Fatal(err)
	}
	if len(st.reserved) != 1 || st.reserved[0].ServiceState != "down" || st.reserved[0].EscalationStep != 3 {
		t.Fatalf("reserved %+v; want service state down and step 3", st.reserved)
	}
	if len(st.unsent) != 1 || st.unsent[0].in.ServiceState != "down" || st.unsent[0].in.EscalationStep != 3 {
		t.Fatalf("unsent %+v; want service state down and step 3", st.unsent)
	}
}

// FR-041a review P3-4: a channel deleted after the targets were read — its reservation or its unsent
// row refused with domain.ErrPhoneChannelGone — ends its delivery quietly: no error for the event, not
// counted in Resolved or in a metric, one log line; the other channels are delivered as usual.
func TestDeliverPhoneToADeletedChannelEndsQuietly(t *testing.T) {
	gone := domain.PhoneTarget{ChannelID: "c2", ProjectID: "p1", Kind: domain.ChannelSMS,
		Recipient: domain.PhoneRecipientNumber, NotRouteReason: domain.DeliverableTelephonyDisabled}
	st := newPhoneStore(smsTarget("c1", numA), gone, smsTarget("c3", numA))
	st.reserveErr["c1"] = domain.ErrPhoneChannelGone
	st.unsentErr = map[string]error{"c2": domain.ErrPhoneChannelGone}
	p := telephony.NewFakeProvider()
	m := &phoneMetrics{}
	var logs bytes.Buffer
	d := New(st, nil).WithPhone(st, enabledTelephony(p), m, slog.New(slog.NewTextHandler(&logs, nil)))
	res, err := d.DeliverPhone(context.Background(), []string{"c1", "c2", "c3"}, stepAlert, stepEvent)
	if err != nil {
		t.Fatalf("err = %v; want nil: a deleted channel does not fail the event", err)
	}
	if want := (domain.ChannelDelivery{Requested: 3, Resolved: 1, Delivered: 1}); res != want {
		t.Fatalf("delivery = %+v; want %+v (the deleted channels counted in nothing)", res, want)
	}
	if n := len(p.Calls()); n != 1 {
		t.Fatalf("provider calls %d; want the one to c3", n)
	}
	if len(m.deliveries) != 1 || m.deliveries["sms|fake|sent"] != 1 {
		t.Fatalf("metrics %v; want only c3's sent", m.deliveries)
	}
	if got := strings.Count(logs.String(), "phone channel deleted during its delivery"); got != 2 {
		t.Fatalf("log lines %d; want one per deleted channel:\n%s", got, logs.String())
	}
}
