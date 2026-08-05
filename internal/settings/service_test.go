package settings

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

type fakeStore struct{ s domain.InstanceSettings }

func (f *fakeStore) GetInstanceSettings(context.Context) (domain.InstanceSettings, error) {
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
