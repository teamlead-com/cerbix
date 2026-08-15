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
}

// Bootstrap carries the config-file values used as the seed for groups that have a
// config-file counterpart and haven't been overridden in the UI yet.
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
	return eff
}

// Load reads the row, resolves the effective settings, and swaps the snapshot.
func (s *Service) Load(ctx context.Context) error {
	raw, err := s.store.GetInstanceSettings(ctx)
	if err != nil {
		return err
	}
	eff := s.resolve(raw)
	s.cur.Store(&eff)
	return nil
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
