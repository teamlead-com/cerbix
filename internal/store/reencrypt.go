package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// ReencryptSecrets rewrites EVERY stored secret under the current primary key:
// webhook signing secrets, notification-channel config, the OIDC client secret,
// the instance SMTP password, monitor config secrets, and user TOTP secrets are
// each read (decrypted with the keyring, which includes previous keys) and written
// back (encrypted with the primary). Run it after adding a new primary key and
// moving the old one to previous_keys; afterwards the old key can be dropped. It is
// a no-op when encryption is disabled. Returns the number of webhooks and channels
// rewritten (the other secret-bearing columns are covered but not separately counted).
func (s *Store) ReencryptSecrets(ctx context.Context) (webhooks, channels int, err error) {
	if s.cipher == nil {
		return 0, 0, nil
	}

	// Webhooks: read (decrypt) + rewrite (encrypt under primary).
	whRows, err := s.pool.Query(ctx, `SELECT id, secret FROM webhooks`)
	if err != nil {
		return 0, 0, fmt.Errorf("store: scan webhooks for reencrypt: %w", err)
	}
	type kv struct{ id, val string }
	var whs []kv
	for whRows.Next() {
		var r kv
		if err := whRows.Scan(&r.id, &r.val); err != nil {
			whRows.Close()
			return 0, 0, fmt.Errorf("store: scan webhook: %w", err)
		}
		whs = append(whs, r)
	}
	whRows.Close()
	if err := whRows.Err(); err != nil {
		return 0, 0, fmt.Errorf("store: iterate webhooks: %w", err)
	}
	for _, r := range whs {
		plain, derr := s.cipher.Decrypt(r.val)
		if derr != nil {
			return webhooks, 0, fmt.Errorf("store: decrypt webhook %s: %w", r.id, derr)
		}
		enc, eerr := s.cipher.Encrypt(plain)
		if eerr != nil {
			return webhooks, 0, fmt.Errorf("store: encrypt webhook %s: %w", r.id, eerr)
		}
		if _, uerr := s.pool.Exec(ctx, `UPDATE webhooks SET secret = $2 WHERE id = $1`, r.id, enc); uerr != nil {
			return webhooks, 0, fmt.Errorf("store: rewrite webhook %s: %w", r.id, uerr)
		}
		webhooks++
	}

	// Channels: read (decrypt each config value) + rewrite (encrypt under primary).
	chRows, err := s.pool.Query(ctx, `SELECT id, config FROM notification_channels`)
	if err != nil {
		return webhooks, 0, fmt.Errorf("store: scan channels for reencrypt: %w", err)
	}
	type chrow struct {
		id  string
		cfg []byte
	}
	var chs []chrow
	for chRows.Next() {
		var r chrow
		if err := chRows.Scan(&r.id, &r.cfg); err != nil {
			chRows.Close()
			return webhooks, 0, fmt.Errorf("store: scan channel: %w", err)
		}
		chs = append(chs, r)
	}
	chRows.Close()
	if err := chRows.Err(); err != nil {
		return webhooks, 0, fmt.Errorf("store: iterate channels: %w", err)
	}
	for _, r := range chs {
		cfg := map[string]string{}
		if len(r.cfg) > 0 {
			if uerr := json.Unmarshal(r.cfg, &cfg); uerr != nil {
				return webhooks, channels, fmt.Errorf("store: decode channel %s config: %w", r.id, uerr)
			}
		}
		enc := make(map[string]string, len(cfg))
		for k, v := range cfg {
			plain, derr := s.cipher.Decrypt(v)
			if derr != nil {
				return webhooks, channels, fmt.Errorf("store: decrypt channel %s %q: %w", r.id, k, derr)
			}
			ev, eerr := s.cipher.Encrypt(plain)
			if eerr != nil {
				return webhooks, channels, fmt.Errorf("store: encrypt channel %s %q: %w", r.id, k, eerr)
			}
			enc[k] = ev
		}
		out, merr := json.Marshal(enc)
		if merr != nil {
			return webhooks, channels, fmt.Errorf("store: encode channel %s config: %w", r.id, merr)
		}
		if _, uerr := s.pool.Exec(ctx, `UPDATE notification_channels SET config = $2 WHERE id = $1`, r.id, string(out)); uerr != nil {
			return webhooks, channels, fmt.Errorf("store: rewrite channel %s: %w", r.id, uerr)
		}
		channels++
	}

	// OIDC client secret (singleton row): decrypt with the keyring, re-encrypt under
	// the primary. Folded into the channel count so key rotation covers it too.
	var oidcSecret string
	oerr := s.pool.QueryRow(ctx, `SELECT client_secret FROM oidc_settings WHERE id = true`).Scan(&oidcSecret)
	if oerr == nil {
		plain, derr := s.cipher.Decrypt(oidcSecret)
		if derr != nil {
			return webhooks, channels, fmt.Errorf("store: decrypt oidc client secret: %w", derr)
		}
		enc, eerr := s.cipher.Encrypt(plain)
		if eerr != nil {
			return webhooks, channels, fmt.Errorf("store: encrypt oidc client secret: %w", eerr)
		}
		if _, uerr := s.pool.Exec(ctx, `UPDATE oidc_settings SET client_secret = $1 WHERE id = true`, enc); uerr != nil {
			return webhooks, channels, fmt.Errorf("store: rewrite oidc client secret: %w", uerr)
		}
	} else if !noRows(oerr) {
		return webhooks, channels, fmt.Errorf("store: scan oidc settings for reencrypt: %w", oerr)
	}

	// Instance mail SMTP password (nested in the mail JSONB group).
	var mailRaw []byte
	merr := s.pool.QueryRow(ctx, `SELECT mail FROM instance_settings WHERE id = true`).Scan(&mailRaw)
	if merr == nil && len(mailRaw) > 0 {
		var mail struct {
			P string `json:"smtp_password"`
		}
		if uerr := json.Unmarshal(mailRaw, &mail); uerr != nil {
			return webhooks, channels, fmt.Errorf("store: decode mail settings for reencrypt: %w", uerr)
		}
		if mail.P != "" {
			plain, derr := s.cipher.Decrypt(mail.P)
			if derr != nil {
				return webhooks, channels, fmt.Errorf("store: decrypt mail password: %w", derr)
			}
			enc, eerr := s.cipher.Encrypt(plain)
			if eerr != nil {
				return webhooks, channels, fmt.Errorf("store: encrypt mail password: %w", eerr)
			}
			if _, uerr := s.pool.Exec(ctx,
				`UPDATE instance_settings SET mail = jsonb_set(mail, '{smtp_password}', to_jsonb($1::text)) WHERE id = true`,
				enc); uerr != nil {
				return webhooks, channels, fmt.Errorf("store: rewrite mail password: %w", uerr)
			}
		}
	} else if merr != nil && !noRows(merr) {
		return webhooks, channels, fmt.Errorf("store: scan mail settings for reencrypt: %w", merr)
	}

	// Monitors: only the SecretMonitorConfigKeys values in config are encrypted
	// (non-secret keys are plaintext), so re-encrypt ONLY those. Skipping this left
	// monitor credentials unreadable once the old key was dropped.
	monRows, err := s.pool.Query(ctx, `SELECT id, config FROM monitors`)
	if err != nil {
		return webhooks, channels, fmt.Errorf("store: scan monitors for reencrypt: %w", err)
	}
	type monrow struct {
		id  string
		cfg []byte
	}
	var mons []monrow
	for monRows.Next() {
		var r monrow
		if err := monRows.Scan(&r.id, &r.cfg); err != nil {
			monRows.Close()
			return webhooks, channels, fmt.Errorf("store: scan monitor: %w", err)
		}
		mons = append(mons, r)
	}
	monRows.Close()
	if err := monRows.Err(); err != nil {
		return webhooks, channels, fmt.Errorf("store: iterate monitors: %w", err)
	}
	for _, r := range mons {
		if len(r.cfg) == 0 {
			continue
		}
		cfg := map[string]string{}
		if uerr := json.Unmarshal(r.cfg, &cfg); uerr != nil {
			return webhooks, channels, fmt.Errorf("store: decode monitor %s config: %w", r.id, uerr)
		}
		changed := false
		for k, v := range cfg {
			if !domain.SecretMonitorConfigKeys[k] || v == "" {
				continue
			}
			plain, derr := s.cipher.Decrypt(v)
			if derr != nil {
				return webhooks, channels, fmt.Errorf("store: decrypt monitor %s %q: %w", r.id, k, derr)
			}
			ev, eerr := s.cipher.Encrypt(plain)
			if eerr != nil {
				return webhooks, channels, fmt.Errorf("store: encrypt monitor %s %q: %w", r.id, k, eerr)
			}
			cfg[k] = ev
			changed = true
		}
		if !changed {
			continue
		}
		out, merr := json.Marshal(cfg)
		if merr != nil {
			return webhooks, channels, fmt.Errorf("store: encode monitor %s config: %w", r.id, merr)
		}
		if _, uerr := s.pool.Exec(ctx, `UPDATE monitors SET config = $2 WHERE id = $1`, r.id, string(out)); uerr != nil {
			return webhooks, channels, fmt.Errorf("store: rewrite monitor %s: %w", r.id, uerr)
		}
	}

	// TOTP secrets (users.totp_secret, encrypted at rest). Without this, rotating the
	// key would lock out every 2FA user once the old key is dropped.
	totpRows, err := s.pool.Query(ctx, `SELECT id, totp_secret FROM users WHERE totp_secret <> ''`)
	if err != nil {
		return webhooks, channels, fmt.Errorf("store: scan totp secrets for reencrypt: %w", err)
	}
	var totps []kv // reuses the {id,val} type declared for webhooks above
	for totpRows.Next() {
		var r kv
		if err := totpRows.Scan(&r.id, &r.val); err != nil {
			totpRows.Close()
			return webhooks, channels, fmt.Errorf("store: scan totp secret: %w", err)
		}
		totps = append(totps, r)
	}
	totpRows.Close()
	if err := totpRows.Err(); err != nil {
		return webhooks, channels, fmt.Errorf("store: iterate totp secrets: %w", err)
	}
	for _, r := range totps {
		plain, derr := s.cipher.Decrypt(r.val)
		if derr != nil {
			return webhooks, channels, fmt.Errorf("store: decrypt totp secret %s: %w", r.id, derr)
		}
		enc, eerr := s.cipher.Encrypt(plain)
		if eerr != nil {
			return webhooks, channels, fmt.Errorf("store: encrypt totp secret %s: %w", r.id, eerr)
		}
		if _, uerr := s.pool.Exec(ctx, `UPDATE users SET totp_secret = $2 WHERE id = $1`, r.id, enc); uerr != nil {
			return webhooks, channels, fmt.Errorf("store: rewrite totp secret %s: %w", r.id, uerr)
		}
	}
	return webhooks, channels, nil
}
