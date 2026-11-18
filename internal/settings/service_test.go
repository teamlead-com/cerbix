package settings

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

type fakeStore struct {
	s             domain.InstanceSettings
	noKey         bool
	telephonyErr  error
	telephonySave []domain.TelephonySaveOptions
	getErr        error
}

func (f *fakeStore) EncryptionKeyConfigured() bool { return !f.noKey }
func (f *fakeStore) SaveTelephonySettings(_ context.Context, ts domain.TelephonySettings, opts domain.TelephonySaveOptions) error {
	f.telephonySave = append(f.telephonySave, opts)
	if f.telephonyErr != nil {
		return f.telephonyErr
	}
	ts.Configured = true
	f.s.Telephony = ts
	return nil
}

func (f *fakeStore) GetInstanceSettings(context.Context) (domain.InstanceSettings, error) {
	if f.getErr != nil {
		return domain.InstanceSettings{}, f.getErr
	}
	return f.s, nil
}
func (f *fakeStore) UpsertBranding(_ context.Context, b domain.Branding) error {
	f.s.Branding = b
	return nil
}
func (f *fakeStore) UpsertAuthPolicy(_ context.Context, p domain.AuthPolicy) error {
	f.s.AuthPolicy = p
	return nil
}
func (f *fakeStore) UpsertAlerting(_ context.Context, a domain.Alerting) error {
	f.s.Alerting = a
	return nil
}
func (f *fakeStore) UpsertMonitorDefaults(_ context.Context, d domain.MonitorDefaults) error {
	f.s.MonitorDefaults = d
	return nil
}
func (f *fakeStore) UpsertMail(_ context.Context, m domain.MailSettings) error {
	f.s.Mail = m
	return nil
}

func testService(fs *fakeStore) *Service {
	return New(fs, Bootstrap{MinPasswordLen: 10, SessionTTLSeconds: 7200}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestResolveBootstrapThenOverride(t *testing.T) {
	fs := &fakeStore{}
	svc := testService(fs)
	ctx := context.Background()
	if err := svc.Load(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	// Unconfigured → bootstrap/defaults.
	if svc.AuthPolicy().MinPasswordLen != 10 || svc.AuthPolicy().SessionTTLSeconds != 7200 {
		t.Fatalf("bootstrap auth policy = %+v", svc.AuthPolicy())
	}
	if svc.Branding().ProductName != "cerbix" {
		t.Fatalf("default branding = %+v", svc.Branding())
	}
	if svc.MonitorDefaults().IntervalSeconds != 60 || !svc.MonitorDefaults().AutoIncident {
		t.Fatalf("default monitor defaults = %+v", svc.MonitorDefaults())
	}
	if svc.Alerting().Silenced(time.Now()) {
		t.Fatal("default alerting should not be silenced")
	}

	// Saving a group overrides the bootstrap and refreshes the snapshot.
	if err := svc.SaveAuthPolicy(ctx, domain.AuthPolicy{MinPasswordLen: 16, SessionTTLSeconds: 1800, RequireTOTP: domain.TOTPAll}); err != nil {
		t.Fatalf("save auth policy: %v", err)
	}
	if svc.AuthPolicy().MinPasswordLen != 16 || svc.AuthPolicy().RequireTOTP != domain.TOTPAll || !svc.AuthPolicy().Configured {
		t.Fatalf("override auth policy = %+v", svc.AuthPolicy())
	}
	// A bad group is rejected and does not change the snapshot.
	if err := svc.SaveBranding(ctx, domain.Branding{AccentColor: "notacolor"}); err == nil {
		t.Fatal("invalid branding should be rejected")
	}
	if svc.Branding().ProductName != "cerbix" {
		t.Fatalf("snapshot changed after failed save: %+v", svc.Branding())
	}
}

func TestRunReturnsAfterContextCancellation(t *testing.T) {
	svc := testService(&fakeStore{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.Run(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("settings Run did not return after cancellation")
	}
}

// D36: telephony has no config-file seed. An absent group is disabled with §4.1's defaults.
func TestTelephonyAbsentIsDisabled(t *testing.T) {
	svc := testService(&fakeStore{})
	// Before Load, and after it on an empty row.
	for _, label := range []string{"before load", "after load"} {
		got := svc.Telephony()
		if got.Configured || got.Enabled || got.Provider != "" || got.VoiceLanguage != "en-US" ||
			got.SMSEncoding != "transliterate" || got.SMSMaxSegments != 2 || got.Storm.PerNumberPerHour != 20 {
			t.Fatalf("%s: telephony = %+v", label, got)
		}
		if err := svc.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSaveTelephonyValidatesPersistsAndRefreshes(t *testing.T) {
	fs := &fakeStore{}
	svc := testService(fs)
	ctx := context.Background()
	_ = svc.Load(ctx)

	// The template is checked at save (Task 4's review); nothing reaches the store.
	bad := domain.TelephonySettings{Enabled: true, Provider: domain.TelephonyProviderGenericHTTP,
		GenericHTTP: domain.TelephonyGenericHTTP{Method: "POST", URL: "https://sms.example/send",
			ContentType: "application/json", BodyTemplate: `{"to":"{{to}}"}`}}
	if err := svc.SaveTelephony(ctx, bad, domain.TelephonySaveOptions{}); !errors.Is(err, telephony.ErrTemplate) {
		t.Fatalf("bad template: err = %v", err)
	}
	if err := svc.SaveTelephony(ctx, domain.TelephonySettings{SMSMaxSegments: 7}, domain.TelephonySaveOptions{}); !errors.Is(err, domain.ErrInvalidTelephonySettings) {
		t.Fatalf("bad segments: err = %v", err)
	}
	if len(fs.telephonySave) != 0 {
		t.Fatal("an invalid group reached the store")
	}

	// Normalized, persisted, and the snapshot refreshed.
	good := bad
	good.GenericHTTP.BodyTemplate = `{"to":"{{to}}","text":"{{text}}"}`
	good.GenericHTTP.Method = "post"
	opts := domain.TelephonySaveOptions{ConfirmUnroutable: true}
	if err := svc.SaveTelephony(ctx, good, opts); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := svc.Telephony(); !got.Configured || !got.Enabled || got.GenericHTTP.Method != "POST" || got.VoiceLanguage != "en-US" {
		t.Fatalf("snapshot after save = %+v", got)
	}
	if len(fs.telephonySave) != 1 || fs.telephonySave[0] != opts {
		t.Fatalf("options passed to the store = %+v", fs.telephonySave)
	}

	// A store refusal (the 409) comes back unchanged and leaves the snapshot as it was.
	fs.telephonyErr = &domain.ServicesWithoutRouteError{Total: 1}
	off := good
	off.Enabled = false
	var sw *domain.ServicesWithoutRouteError
	if err := svc.SaveTelephony(ctx, off, domain.TelephonySaveOptions{}); !errors.As(err, &sw) {
		t.Fatalf("store refusal: err = %v", err)
	}
	if !svc.Telephony().Enabled {
		t.Fatal("a refused save changed the snapshot")
	}

	if !svc.EncryptionKeyConfigured() {
		t.Fatal("key reported missing")
	}
	fs.noKey = true
	if svc.EncryptionKeyConfigured() {
		t.Fatal("key reported configured")
	}
}

// FR-041a owner answer 2 (review M-6): a disabled generic_http group with a draft template — here
// one without a content type — is saved as typed; the template is judged only while generic_http is
// enabled and selected.
func TestSaveTelephonySavesADisabledGenericHTTPDraftTemplate(t *testing.T) {
	fs := &fakeStore{}
	svc := testService(fs)
	ctx := context.Background()
	_ = svc.Load(ctx)

	draft := domain.TelephonySettings{Enabled: false, Provider: domain.TelephonyProviderGenericHTTP,
		GenericHTTP: domain.TelephonyGenericHTTP{Method: "POST", URL: "https://sms.example/send",
			BodyTemplate: `{"to":"{{to}}","text":"{{text}}"}`}}
	if err := svc.SaveTelephony(ctx, draft, domain.TelephonySaveOptions{}); err != nil {
		t.Fatalf("disabled generic_http with a draft template: %v; want saved", err)
	}
	if got := svc.Telephony(); got.Enabled || got.GenericHTTP.ContentType != "" ||
		got.GenericHTTP.BodyTemplate != draft.GenericHTTP.BodyTemplate {
		t.Fatalf("snapshot after the draft save = %+v", got)
	}
}

// FR-041a review P2-8: a code send and the test SMS build their provider from the CURRENT settings,
// not from this replica's snapshot, which can be up to refreshEvery old. Two replicas over one store:
// the first saves, the second's snapshot still says telephony is off, and the provider it builds for
// a send is the saved one all the same.
func TestCurrentTelephonyProviderReadsTheStoreNotTheSnapshot(t *testing.T) {
	fs := &fakeStore{}
	ctx := context.Background()
	replica1, replica2 := testService(fs), testService(fs)
	for _, r := range []*Service{replica1, replica2} {
		if err := r.Load(ctx); err != nil {
			t.Fatal(err)
		}
	}
	saved := domain.TelephonySettings{Enabled: true, Provider: domain.TelephonyProviderGenericHTTP,
		GenericHTTP: domain.TelephonyGenericHTTP{Method: "POST", URL: "https://sms.example/send",
			ContentType: telephony.ContentTypeJSON, BodyTemplate: `{"to":"{{to}}","text":"{{text}}"}`}}
	if err := replica1.SaveTelephony(ctx, saved, domain.TelephonySaveOptions{}); err != nil {
		t.Fatalf("save on replica 1: %v", err)
	}

	tel := telephony.NewService(replica2, nil)
	if _, err := tel.Provider(); !errors.Is(err, telephony.ErrTelephonyDisabled) {
		t.Fatalf("replica 2's snapshot provider: %v; want the stale ErrTelephonyDisabled this test relies on", err)
	}
	p, err := tel.CurrentProvider(ctx)
	if err != nil {
		t.Fatalf("replica 2's current provider: %v; want the provider replica 1 saved", err)
	}
	if p.Name() != domain.TelephonyProviderGenericHTTP {
		t.Fatalf("current provider = %s, want generic_http", p.Name())
	}

	// A failed read is an error, not the snapshot's answer.
	fs.getErr = errors.New("database down")
	if _, err := tel.CurrentProvider(ctx); err == nil || errors.Is(err, telephony.ErrTelephonyDisabled) {
		t.Fatalf("current provider over a failed read: %v; want the read's error", err)
	}
}

// Group-A review M-5: CurrentTelephony answers the group it has just read from the store, not the
// snapshot after its reload — a periodic refresh that read the row earlier can store its older
// snapshot between the two, and the request would send with it after all. Another goroutine keeps
// storing a stale snapshot here; the answer must still be the stored group, every time.
func TestCurrentTelephonyAnswersWhatItRead(t *testing.T) {
	fs := &fakeStore{}
	fs.s.Telephony = domain.TelephonySettings{Configured: true, Enabled: true, Provider: domain.TelephonyProviderTwilio}
	svc := testService(fs)
	stale := svc.resolve(domain.InstanceSettings{})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				svc.cur.Store(&stale)
			}
		}
	}()
	defer func() { close(stop); <-done }()
	for i := 0; i < 500; i++ {
		ts, err := svc.CurrentTelephony(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ts.Enabled || ts.Provider != domain.TelephonyProviderTwilio {
			t.Fatalf("try %d: CurrentTelephony = %+v; want the stored group, not the stale snapshot", i, ts)
		}
	}
}
