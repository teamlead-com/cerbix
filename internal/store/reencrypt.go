package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

// ReencryptSecrets rewrites EVERY stored secret under the current primary key:
// webhook signing secrets, notification-channel config, the OIDC client secret,
// the instance SMTP password, monitor config secrets, and user TOTP secrets are
// each read (decrypted with the keyring, which includes previous keys) and written
// back (encrypted with the primary). Run it after adding a new primary key and
// moving the old one to previous_keys; afterwards the old key can be dropped. It is
// a no-op when encryption is disabled. Returns the number of webhooks and channels
// rewritten (the other secret-bearing columns are covered but not separately counted).
// BackfillPushTokenEnc encrypts any push_token_enc still stored as plaintext (seeded by
// migration 00053 from the old plaintext column). Unlike ReencryptSecrets — which only runs
// via the `cerbix reencrypt` command — this runs at startup (readiness-gated) so a normal
// migrate+serve never leaves a push bearer token in plaintext once a key is configured. A
// no-op without a cipher (plaintext is then the chosen posture). Idempotent (already-
// encrypted rows are skipped by the prefix check) and concurrency-safe (the UPDATE CAS on
// the old value means only one replica converts each row). Returns rows converted.
func (s *Store) BackfillPushTokenEnc(ctx context.Context) (int, error) {
	if s.cipher == nil {
		return 0, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, push_token_enc FROM monitors WHERE push_token_enc IS NOT NULL`)
	if err != nil {
		return 0, fmt.Errorf("store: scan push tokens for backfill: %w", err)
	}
	type kv struct{ id, val string }
	var pts []kv
	for rows.Next() {
		var r kv
		if err := rows.Scan(&r.id, &r.val); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: scan push token: %w", err)
		}
		pts = append(pts, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: iterate push tokens: %w", err)
	}
	converted := 0
	for _, r := range pts {
		if secret.IsEncrypted(r.val) {
			continue // already ciphertext
		}
		enc, err := s.cipher.Encrypt(r.val)
		if err != nil {
			return converted, fmt.Errorf("store: encrypt push token %s: %w", r.id, err)
		}
		tag, err := s.pool.Exec(ctx,
			`UPDATE monitors SET push_token_enc = $2 WHERE id = $1 AND push_token_enc = $3`,
			r.id, enc, r.val)
		if err != nil {
			return converted, fmt.Errorf("store: backfill push token %s: %w", r.id, err)
		}
		if tag.RowsAffected() > 0 {
			converted++
		}
	}
	return converted, nil
}

func (s *Store) ReencryptSecrets(ctx context.Context) (webhooks, channels int, err error) {
	if s.cipher == nil {
		return 0, 0, nil
	}
	if err := s.reencryptProjectSecrets(ctx); err != nil {
		return 0, 0, err
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
		ct, uerr := s.pool.Exec(ctx, `UPDATE webhooks SET secret = $2 WHERE id = $1 AND secret = $3`, r.id, enc, r.val)
		if uerr != nil {
			return webhooks, 0, fmt.Errorf("store: rewrite webhook %s: %w", r.id, uerr)
		}
		if ct.RowsAffected() == 1 {
			webhooks++
		}
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
		ct, uerr := s.pool.Exec(ctx, `UPDATE notification_channels SET config = $2 WHERE id = $1 AND config = $3::jsonb`, r.id, string(out), string(r.cfg))
		if uerr != nil {
			return webhooks, channels, fmt.Errorf("store: rewrite channel %s: %w", r.id, uerr)
		}
		if ct.RowsAffected() == 1 {
			channels++
		}
	}

	// OIDC client secret (singleton row): decrypt with the keyring, re-encrypt under
	// the primary. Folded into the channel count so key rotation covers it too.
	var oidcSecret string
	oerr := s.pool.QueryRow(ctx, `SELECT client_secret FROM oidc_settings WHERE id = true`).Scan(&oidcSecret) //nolint:forbidigo // encryption-boundary key rotation; value is never logged
	if oerr == nil {
		plain, derr := s.cipher.Decrypt(oidcSecret)
		if derr != nil {
			return webhooks, channels, fmt.Errorf("store: decrypt oidc client secret: %w", derr)
		}
		enc, eerr := s.cipher.Encrypt(plain)
		if eerr != nil {
			return webhooks, channels, fmt.Errorf("store: encrypt oidc client secret: %w", eerr)
		}
		if _, uerr := s.pool.Exec(ctx, `UPDATE oidc_settings SET client_secret = $1 WHERE id = true AND client_secret = $2`, enc, oidcSecret); uerr != nil {
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
				`UPDATE instance_settings SET mail = jsonb_set(mail, '{smtp_password}', to_jsonb($1::text))
				  WHERE id = true AND mail = $2::jsonb`, enc, string(mailRaw)); uerr != nil {
				return webhooks, channels, fmt.Errorf("store: rewrite mail password: %w", uerr)
			}
		}
	} else if merr != nil && !noRows(merr) {
		return webhooks, channels, fmt.Errorf("store: scan mail settings for reencrypt: %w", merr)
	}

	// Monitors: only the values in the encrypted-at-rest classification are ciphertext
	// (everything else in config is plaintext), so re-encrypt ONLY those. Skipping this left
	// monitor credentials unreadable once the old key was dropped. Since FR-028 the set also
	// carries the synthetic scenario — `func-oncall-synthetic-pull.md` §217 promised exactly
	// that from the start and the code did not do it.
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
			if !domain.EncryptedMonitorConfigKeys[k] || v == "" {
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
		if _, uerr := s.pool.Exec(ctx, `UPDATE monitors SET config = $2 WHERE id = $1 AND config = $3::jsonb`, r.id, string(out), string(r.cfg)); uerr != nil {
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
		if _, uerr := s.pool.Exec(ctx, `UPDATE users SET totp_secret = $2 WHERE id = $1 AND totp_secret = $3`, r.id, enc, r.val); uerr != nil {
			return webhooks, channels, fmt.Errorf("store: rewrite totp secret %s: %w", r.id, uerr)
		}
	}

	// Push tokens (monitors.push_token_enc). Also upgrades the plaintext values the
	// 00053 migration seeds (unprefixed → Decrypt passes them through, Encrypt writes
	// ciphertext) so nothing stays plaintext once a key is configured, and covers key
	// rotation for push endpoints.
	ptRows, err := s.pool.Query(ctx, `SELECT id, push_token_enc FROM monitors WHERE push_token_enc IS NOT NULL`)
	if err != nil {
		return webhooks, channels, fmt.Errorf("store: scan push tokens for reencrypt: %w", err)
	}
	var pts []kv // reuses the {id,val} type declared for webhooks above
	for ptRows.Next() {
		var r kv
		if err := ptRows.Scan(&r.id, &r.val); err != nil {
			ptRows.Close()
			return webhooks, channels, fmt.Errorf("store: scan push token: %w", err)
		}
		pts = append(pts, r)
	}
	ptRows.Close()
	if err := ptRows.Err(); err != nil {
		return webhooks, channels, fmt.Errorf("store: iterate push tokens: %w", err)
	}
	for _, r := range pts {
		plain, derr := s.cipher.Decrypt(r.val)
		if derr != nil {
			return webhooks, channels, fmt.Errorf("store: decrypt push token %s: %w", r.id, derr)
		}
		enc, eerr := s.cipher.Encrypt(plain)
		if eerr != nil {
			return webhooks, channels, fmt.Errorf("store: encrypt push token %s: %w", r.id, eerr)
		}
		if _, uerr := s.pool.Exec(ctx, `UPDATE monitors SET push_token_enc = $2 WHERE id = $1 AND push_token_enc = $3`, r.id, enc, r.val); uerr != nil {
			return webhooks, channels, fmt.Errorf("store: rewrite push token %s: %w", r.id, uerr)
		}
	}
	return webhooks, channels, nil
}

const (
	reencryptInventoryBatch    = 100
	reencryptInventoryAttempts = 8
)

// reencryptProjectSecrets converges the AAD-bound inventory to the current primary key.
// Every rewrite is an exact-ciphertext CAS, so a concurrent rotate wins rather than being
// overwritten. Success is returned only after a full bounded scan proves zero old-key rows.
func (s *Store) reencryptProjectSecrets(ctx context.Context) error {
	for attempt := 1; attempt <= reencryptInventoryAttempts; attempt++ {
		remaining, err := s.sweepProjectSecrets(ctx, true)
		if err != nil {
			return err
		}
		if remaining == 0 {
			return nil
		}
	}
	remaining, err := s.sweepProjectSecrets(ctx, false)
	if err != nil {
		return err
	}
	if remaining != 0 {
		return fmt.Errorf("store: reencrypt project secrets did not converge: %d old-key row(s) remain", remaining)
	}
	return nil
}

func (s *Store) sweepProjectSecrets(ctx context.Context, rewrite bool) (int, error) {
	var after *string
	remaining := 0
	for {
		rows, err := s.pool.Query(ctx,
			`SELECT id::text, project_id::text, value_encrypted
			   FROM project_secrets
			  WHERE ($1::uuid IS NULL OR id > $1::uuid)
			  ORDER BY id
			  LIMIT $2`, after, reencryptInventoryBatch)
		if err != nil {
			return 0, fmt.Errorf("store: scan project secrets for reencrypt: %w", err)
		}
		type inventoryRow struct{ id, projectID, ciphertext string }
		batch := make([]inventoryRow, 0, reencryptInventoryBatch)
		for rows.Next() {
			var row inventoryRow
			if err := rows.Scan(&row.id, &row.projectID, &row.ciphertext); err != nil {
				rows.Close()
				return 0, fmt.Errorf("store: scan project secret: %w", err)
			}
			batch = append(batch, row)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("store: iterate project secrets: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		last := batch[len(batch)-1].id
		after = &last
		for _, row := range batch {
			aad := secret.CanonicalAAD(row.projectID, row.id)
			needs, err := s.cipher.NeedsReencryptBytes(row.ciphertext, aad)
			if err != nil {
				return 0, fmt.Errorf("store: inspect project secret %s for reencrypt: %w", row.id, err)
			}
			if !needs {
				continue
			}
			remaining++
			if !rewrite {
				continue
			}
			plain, err := s.cipher.DecryptBytes(row.ciphertext, aad)
			if err != nil {
				return 0, fmt.Errorf("store: decrypt project secret %s: %w", row.id, err)
			}
			next, encErr := s.cipher.EncryptBytes(plain, aad)
			for i := range plain {
				plain[i] = 0
			}
			if encErr != nil {
				return 0, fmt.Errorf("store: encrypt project secret %s: %w", row.id, encErr)
			}
			applied, err := s.reencryptProjectSecretCAS(ctx, row.id, row.projectID, row.ciphertext, next)
			if err != nil {
				return 0, fmt.Errorf("store: rewrite project secret %s: %w", row.id, err)
			}
			if applied {
				remaining--
			}
		}
		if len(batch) < reencryptInventoryBatch {
			break
		}
	}
	return remaining, nil
}

// reencryptProjectSecretCAS is the rotation linearization point for an inventory row.
// A concurrent UpdateProjectSecret that already replaced oldCiphertext wins; reencrypt
// never resurrects the stale plaintext it read before that update.
func (s *Store) reencryptProjectSecretCAS(ctx context.Context, id, projectID, oldCiphertext, nextCiphertext string) (bool, error) {
	ct, err := s.pool.Exec(ctx,
		`UPDATE project_secrets SET value_encrypted=$4
		  WHERE id=$1 AND project_id=$2 AND value_encrypted=$3`,
		id, projectID, oldCiphertext, nextCiphertext)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() == 1, nil
}

// BackfillMonitorConfigEnc encrypts any monitor config value in the encrypted-at-rest
// classification that is still stored as plaintext — today the synthetic scenario, which
// FR-SYN-1 promised would be encrypted "like the other types" and which never was (FR-028,
// D-0216). It runs at startup beside BackfillPushTokenEnc and shares its properties:
// idempotent (an already-encrypted value is skipped by the cipher's own prefix check),
// concurrency-safe (the UPDATE carries the old config as a CAS so only one replica converts
// a row), and a no-op without a cipher.
//
// Unlike the push-token backfill it is NOT readiness-gated and its failure NEVER fails
// startup. That is the owner's ruling of §10: a service must not go down because of an unset
// variable or one unconvertible row. A row that is already ciphertext is protected; a row
// that is not keeps working exactly as it does today and is retried on the next start. The
// count and the failure are reported to the caller, which logs them.
func (s *Store) BackfillMonitorConfigEnc(ctx context.Context) (int, error) {
	if s.cipher == nil {
		return 0, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, config::text FROM monitors WHERE config IS NOT NULL AND config::text <> '{}'`)
	if err != nil {
		return 0, fmt.Errorf("store: scan monitor configs for backfill: %w", err)
	}
	type row struct{ id, raw string }
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.raw); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: scan monitor config: %w", err)
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: iterate monitor configs: %w", err)
	}

	converted := 0
	for _, r := range pending {
		cfg := map[string]string{}
		if err := json.Unmarshal([]byte(r.raw), &cfg); err != nil {
			return converted, fmt.Errorf("store: decode monitor config %s: %w", r.id, err)
		}
		changed := false
		for k, v := range cfg {
			if !domain.EncryptedMonitorConfigKeys[k] || v == "" || secret.IsEncrypted(v) {
				continue
			}
			enc, err := s.cipher.Encrypt(v)
			if err != nil {
				return converted, fmt.Errorf("store: encrypt monitor config %q of %s: %w", k, r.id, err)
			}
			cfg[k] = enc
			changed = true
		}
		if !changed {
			continue
		}
		next, err := json.Marshal(cfg)
		if err != nil {
			return converted, fmt.Errorf("store: encode monitor config %s: %w", r.id, err)
		}
		// CAS on the old document: a concurrent writer that already converted this row, or
		// changed it, leaves the UPDATE matching nothing and this pass simply moves on.
		ct, err := s.pool.Exec(ctx,
			`UPDATE monitors SET config = $1 WHERE id = $2 AND config::text = $3`, next, r.id, r.raw)
		if err != nil {
			return converted, fmt.Errorf("store: backfill monitor config %s: %w", r.id, err)
		}
		if ct.RowsAffected() == 1 {
			converted++
		}
	}
	return converted, nil
}
