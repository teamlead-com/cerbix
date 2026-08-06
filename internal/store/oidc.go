package store

import (
	"context"
	"fmt"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// GetOIDCSettings returns the instance-wide OIDC override, or ErrNotFound if none
// has been saved (the caller then falls back to the config-file bootstrap). The
// client secret is decrypted with the keyring on the way out.
func (s *Store) GetOIDCSettings(ctx context.Context) (domain.OIDCSettings, error) {
	var (
		st     domain.OIDCSettings
		secret string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT enabled, issuer, client_id, client_secret, redirect_url, scopes,
		        post_logout_url, button_label, bootstrap_admins
		   FROM oidc_settings WHERE id = true`).
		Scan(&st.Enabled, &st.Issuer, &st.ClientID, &secret, &st.RedirectURL, &st.Scopes,
			&st.PostLogoutURL, &st.ButtonLabel, &st.BootstrapAdmins)
	if noRows(err) {
		return domain.OIDCSettings{}, ErrNotFound
	}
	if err != nil {
		return domain.OIDCSettings{}, fmt.Errorf("store: get oidc settings: %w", err)
	}
	plain, derr := s.cipher.Decrypt(secret)
	if derr != nil {
		return domain.OIDCSettings{}, fmt.Errorf("store: decrypt oidc client secret: %w", derr)
	}
	st.ClientSecret = plain
	if st.Scopes == nil {
		st.Scopes = []string{}
	}
	if st.BootstrapAdmins == nil {
		st.BootstrapAdmins = []string{}
	}
	return st, nil
}

// UpsertOIDCSettings writes the singleton OIDC override, encrypting the client
// secret at rest with the primary key.
func (s *Store) UpsertOIDCSettings(ctx context.Context, st domain.OIDCSettings) error {
	enc, eerr := s.cipher.Encrypt(st.ClientSecret)
	if eerr != nil {
		return fmt.Errorf("store: encrypt oidc client secret: %w", eerr)
	}
	scopes := st.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	admins := st.BootstrapAdmins
	if admins == nil {
		admins = []string{}
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO oidc_settings
		     (id, enabled, issuer, client_id, client_secret, redirect_url, scopes, post_logout_url, button_label, bootstrap_admins, updated_at)
		 VALUES (true, $1, $2, $3, $4, $5, $6, $7, $8, $9, now())
		 ON CONFLICT (id) DO UPDATE SET
		     enabled = EXCLUDED.enabled, issuer = EXCLUDED.issuer, client_id = EXCLUDED.client_id,
		     client_secret = EXCLUDED.client_secret, redirect_url = EXCLUDED.redirect_url, scopes = EXCLUDED.scopes,
		     post_logout_url = EXCLUDED.post_logout_url, button_label = EXCLUDED.button_label,
		     bootstrap_admins = EXCLUDED.bootstrap_admins, updated_at = now()`,
		st.Enabled, st.Issuer, st.ClientID, enc, st.RedirectURL, scopes, st.PostLogoutURL, st.ButtonLabel, admins)
	if err != nil {
		return fmt.Errorf("store: upsert oidc settings: %w", err)
	}
	return nil
}
