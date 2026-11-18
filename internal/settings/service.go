// Package settings resolves the effective instance-wide settings (from the DB
// override if a group is configured, otherwise the config-file bootstrap or code
// defaults) and serves them from a lock-free atomic snapshot. Saves validate the
// group, persist it, and refresh the snapshot; a periodic refresh picks up writes
// made by other replicas.
package settings

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// refreshEvery is how often the snapshot is reloaded so cross-replica writes land.
const refreshEvery = 30 * time.Second

// Store is the persistence surface the service needs.
type Store interface {
	GetInstanceSettings(ctx context.Context) (domain.InstanceSettings, error)
	UpsertBranding(ctx context.Context, b domain.Branding) error
	UpsertAuthPolicy(ctx context.Context, p domain.AuthPolicy) error
	UpsertAlerting(ctx context.Context, a domain.Alerting) error
	UpsertMonitorDefaults(ctx context.Context, d domain.MonitorDefaults) error
	UpsertMail(ctx context.Context, m domain.MailSettings) error
	// SaveTelephonySettings writes the telephony group (FR-041a §4.1): blank secrets keep, the 409
	// list is computed in its transaction, and it is refused without the at-rest key.
	SaveTelephonySettings(ctx context.Context, ts domain.TelephonySettings, opts domain.TelephonySaveOptions) error
	EncryptionKeyConfigured() bool
}

// Bootstrap carries the config-file values used as the seed for groups that have a
// config-file counterpart and haven't been overridden in the UI yet. Telephony has none (D36).
type Bootstrap struct {
	MinPasswordLen    int
	SessionTTLSeconds int
	Mail              domain.MailSettings
}

// Service resolves and caches the effective instance settings.
type Service struct {
	store  Store
	boot   Bootstrap
	logger *slog.Logger
	cur    atomic.Pointer[domain.InstanceSettings]
}

// New builds a Service. Call Load and then run Run under the process lifecycle.
func New(store Store, boot Bootstrap, logger *slog.Logger) *Service {
	if boot.MinPasswordLen <= 0 {
		boot.MinPasswordLen = 8
	}
	if boot.SessionTTLSeconds <= 0 {
		boot.SessionTTLSeconds = int((24 * time.Hour).Seconds())
	}
	return &Service{store: store, boot: boot, logger: logger}
}

func defaultBranding() domain.Branding {
	return domain.Branding{ProductName: "cerbix", Announcement: domain.Announcement{Level: "info"}}
}

func defaultMonitorDefaults() domain.MonitorDefaults {
	return domain.MonitorDefaults{
		IntervalSeconds: 60, TimeoutSeconds: 10, Retries: 0,
		FailureThreshold: 1, RenotifySeconds: 0, AutoIncident: true,
	}
}

func (s *Service) bootstrapAuthPolicy() domain.AuthPolicy {
	return domain.AuthPolicy{
		MinPasswordLen:    s.boot.MinPasswordLen,
		SessionTTLSeconds: s.boot.SessionTTLSeconds,
		RequireTOTP:       domain.TOTPNone,
	}
}

// resolve fills unconfigured groups from the bootstrap/defaults.
func (s *Service) resolve(raw domain.InstanceSettings) domain.InstanceSettings {
	eff := raw
	if !raw.Branding.Configured {
		eff.Branding = defaultBranding()
	}
	if !raw.AuthPolicy.Configured {
		eff.AuthPolicy = s.bootstrapAuthPolicy()
	}
	if !raw.Alerting.Configured {
		eff.Alerting = domain.Alerting{}
	}
	if !raw.MonitorDefaults.Configured {
		eff.MonitorDefaults = defaultMonitorDefaults()
	}
	if !raw.Mail.Configured {
		eff.Mail = s.boot.Mail
	}
	// Database only (D36): absent means disabled, with no YAML seed to fall back to.
	if !raw.Telephony.Configured {
		eff.Telephony = domain.DefaultTelephonySettings()
	}
	return eff
}

// Load reads the row, resolves the effective settings, and swaps the snapshot.
func (s *Service) Load(ctx context.Context) error {
	_, err := s.load(ctx)
	return err
}

// load is Load, answering the settings it read and stored — not the snapshot afterwards, which a
// concurrent refresh may already have replaced with an older read.
func (s *Service) load(ctx context.Context) (domain.InstanceSettings, error) {
	raw, err := s.store.GetInstanceSettings(ctx)
	if err != nil {
		return domain.InstanceSettings{}, err
	}
	eff := s.resolve(raw)
	s.cur.Store(&eff)
	return eff, nil
}

// Run refreshes the snapshot periodically until ctx is cancelled. The caller owns
// the goroutine so process shutdown can wait for every database user before closing
// the shared pool. Call Load once before Run when the initial snapshot is required
// synchronously.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(refreshEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Load(ctx); err != nil {
				s.logger.Warn("instance_settings_refresh_failed", "error", err.Error())
			}
		}
	}
}

// current returns the snapshot, resolving empty defaults if Start/Load never ran.
func (s *Service) current() domain.InstanceSettings {
	if p := s.cur.Load(); p != nil {
		return *p
	}
	return s.resolve(domain.InstanceSettings{})
}

// Effective group accessors (lock-free).
func (s *Service) Branding() domain.Branding               { return s.current().Branding }
func (s *Service) AuthPolicy() domain.AuthPolicy           { return s.current().AuthPolicy }
func (s *Service) Alerting() domain.Alerting               { return s.current().Alerting }
func (s *Service) MonitorDefaults() domain.MonitorDefaults { return s.current().MonitorDefaults }
func (s *Service) Mail() domain.MailSettings               { return s.current().Mail }
func (s *Service) Telephony() domain.TelephonySettings     { return s.current().Telephony }

// The telephony service reads the snapshot and CurrentTelephony through this interface; production
// passes a *Service.
var _ telephony.SettingsSource = (*Service)(nil)

// CurrentTelephony reads the telephony group as it is stored now — reloading this replica's snapshot
// on the way — for the sends that must not use a snapshot up to refreshEvery old (FR-041a review
// P2-8: verification codes and the test SMS). It answers the group it read, never the snapshot after
// the reload: a periodic refresh that read the row earlier can store its older snapshot in between
// (Group-A review M-5). telephony.Service reads it through telephony.SettingsSource.
func (s *Service) CurrentTelephony(ctx context.Context) (domain.TelephonySettings, error) {
	eff, err := s.load(ctx)
	if err != nil {
		return domain.TelephonySettings{}, err
	}
	return eff.Telephony, nil
}

// EncryptionKeyConfigured reports whether the at-rest key is configured, without which telephony
// cannot be saved (D29). The settings form reads it before anything is typed.
func (s *Service) EncryptionKeyConfigured() bool { return s.store.EncryptionKeyConfigured() }

// SaveBranding validates, persists, and refreshes the branding group.
func (s *Service) SaveBranding(ctx context.Context, b domain.Branding) error {
	b.Configured = true
	if err := b.Validate(); err != nil {
		return err
	}
	if err := s.store.UpsertBranding(ctx, b); err != nil {
		return err
	}
	return s.Load(ctx)
}

// SaveAuthPolicy validates, normalizes, persists, and refreshes the auth policy.
func (s *Service) SaveAuthPolicy(ctx context.Context, p domain.AuthPolicy) error {
	p.Configured = true
	p.Normalize()
	if err := p.Validate(); err != nil {
		return err
	}
	if err := s.store.UpsertAuthPolicy(ctx, p); err != nil {
		return err
	}
	return s.Load(ctx)
}

// SaveAlerting validates, persists, and refreshes the alerting group.
func (s *Service) SaveAlerting(ctx context.Context, a domain.Alerting) error {
	a.Configured = true
	if err := a.Validate(); err != nil {
		return err
	}
	if err := s.store.UpsertAlerting(ctx, a); err != nil {
		return err
	}
	return s.Load(ctx)
}

// SaveMonitorDefaults validates, persists, and refreshes the monitor defaults.
func (s *Service) SaveMonitorDefaults(ctx context.Context, d domain.MonitorDefaults) error {
	d.Configured = true
	if err := d.Validate(); err != nil {
		return err
	}
	if err := s.store.UpsertMonitorDefaults(ctx, d); err != nil {
		return err
	}
	return s.Load(ctx)
}

// SaveMail validates, persists, and refreshes the mail group.
func (s *Service) SaveMail(ctx context.Context, m domain.MailSettings) error {
	m.Configured = true
	if err := m.Validate(); err != nil {
		return err
	}
	if err := s.store.UpsertMail(ctx, m); err != nil {
		return err
	}
	return s.Load(ctx)
}

// SaveTelephony normalizes and validates the telephony group — the template included — persists it,
// and refreshes the snapshot. A store refusal (no key, headers conflict, the 409 list) is returned as
// it came and leaves the snapshot unchanged.
func (s *Service) SaveTelephony(ctx context.Context, ts domain.TelephonySettings, opts domain.TelephonySaveOptions) error {
	ts.Configured = true
	ts.Normalize()
	if err := telephony.ValidateSettings(ts); err != nil {
		return err
	}
	if err := s.store.SaveTelephonySettings(ctx, ts, opts); err != nil {
		return err
	}
	return s.Load(ctx)
}
