package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// GetInstanceSettings reads the singleton settings row and decodes every group.
// A missing row yields all-zero (unconfigured) groups — the caller then applies the
// config bootstrap / defaults.
func (s *Store) GetInstanceSettings(ctx context.Context) (domain.InstanceSettings, error) {
	var branding, authPolicy, alerting, monDefaults, mail []byte
	err := s.pool.QueryRow(ctx,
		`SELECT branding, auth_policy, alerting, monitor_defaults, mail FROM instance_settings WHERE id = true`).
		Scan(&branding, &authPolicy, &alerting, &monDefaults, &mail)
	if noRows(err) {
		return domain.InstanceSettings{}, nil
	}
	if err != nil {
		return domain.InstanceSettings{}, fmt.Errorf("store: get instance settings: %w", err)
	}
	var out domain.InstanceSettings
	for _, d := range []struct {
		raw []byte
		dst any
	}{
		{branding, &out.Branding}, {authPolicy, &out.AuthPolicy},
		{alerting, &out.Alerting}, {monDefaults, &out.MonitorDefaults}, {mail, &out.Mail},
	} {
		if len(d.raw) > 0 {
			if err := json.Unmarshal(d.raw, d.dst); err != nil {
				return domain.InstanceSettings{}, fmt.Errorf("store: decode instance settings: %w", err)
			}
		}
	}
	// The SMTP password is stored encrypted inside the mail JSON.
	plain, derr := s.cipher.Decrypt(out.Mail.SMTPPassword)
	if derr != nil {
		return domain.InstanceSettings{}, fmt.Errorf("store: decrypt mail password: %w", derr)
	}
	out.Mail.SMTPPassword = plain
	return out, nil
}

// upsertSettingsGroup writes one group's JSONB column on the singleton row.
func (s *Store) upsertSettingsGroup(ctx context.Context, column string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store: encode %s: %w", column, err)
	}
	// column is a compile-time constant from the callers below, never user input.
	q := fmt.Sprintf(
		`INSERT INTO instance_settings (id, %[1]s) VALUES (true, $1)
		 ON CONFLICT (id) DO UPDATE SET %[1]s = $1, updated_at = now()`, column)
	if _, err := s.pool.Exec(ctx, q, raw); err != nil {
		return fmt.Errorf("store: upsert %s: %w", column, err)
	}
	return nil
}

// UpsertBranding persists the branding group.
func (s *Store) UpsertBranding(ctx context.Context, b domain.Branding) error {
	return s.upsertSettingsGroup(ctx, "branding", b)
}

// UpsertAuthPolicy persists the auth-policy group.
func (s *Store) UpsertAuthPolicy(ctx context.Context, p domain.AuthPolicy) error {
	return s.upsertSettingsGroup(ctx, "auth_policy", p)
}

// UpsertAlerting persists the alerting group.
func (s *Store) UpsertAlerting(ctx context.Context, a domain.Alerting) error {
	return s.upsertSettingsGroup(ctx, "alerting", a)
}

// UpsertMonitorDefaults persists the monitor-defaults group.
func (s *Store) UpsertMonitorDefaults(ctx context.Context, d domain.MonitorDefaults) error {
	return s.upsertSettingsGroup(ctx, "monitor_defaults", d)
}

// UpsertMail persists the mail group, encrypting the SMTP password at rest.
func (s *Store) UpsertMail(ctx context.Context, m domain.MailSettings) error {
	enc, err := s.cipher.Encrypt(m.SMTPPassword)
	if err != nil {
		return fmt.Errorf("store: encrypt mail password: %w", err)
	}
	m.SMTPPassword = enc
	return s.upsertSettingsGroup(ctx, "mail", m)
}
