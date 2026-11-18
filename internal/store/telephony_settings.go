package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

// Telephony settings (FR-041a §4.1, R1, R10). The group is one jsonb document on the
// instance_settings singleton plus the typed columns the route predicates read (telephony_enabled,
// telephony_provider, and telephony_can_place_calls — D3's "the provider can place calls", generated
// by the database from the provider and Twilio's `from` in the document); every write sets them in one
// statement. The two
// secrets live inside the document as AAD-bound ciphertexts, each bound to its own field, so the
// path is fail-closed without a key (D29).

// maxServicesWithoutRoute bounds the 409 list (§4.1); the total is reported beside it.
const maxServicesWithoutRoute = 50

// The secret fields of the group and the AAD each is bound to.
const (
	telephonyFieldAuthToken = "twilio.auth_token"
	telephonyFieldHeaders   = "generic_http.headers"
)

func telephonyAAD(field string) []byte {
	return secret.CanonicalAAD("instance_settings", "telephony", field)
}

// telephonyRecord is the stored document: the group's non-secret fields (the domain type never
// serializes its secrets) and the two ciphertexts.
type telephonyRecord struct {
	domain.TelephonySettings
	AuthTokenEnc string `json:"twilio_auth_token_enc,omitempty"`
	HeadersEnc   string `json:"generic_http_headers_enc,omitempty"`
}

// EncryptionKeyConfigured reports whether the at-rest key is configured (D29).
func (s *Store) EncryptionKeyConfigured() bool { return s.cipher != nil }

// GetTelephonySettings reads the group with its secrets opened. An absent group is the disabled
// defaults (D36).
func (s *Store) GetTelephonySettings(ctx context.Context) (domain.TelephonySettings, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT telephony FROM instance_settings WHERE id = true`).Scan(&raw)
	if noRows(err) {
		return domain.DefaultTelephonySettings(), nil
	}
	if err != nil {
		return domain.TelephonySettings{}, fmt.Errorf("store: get telephony settings: %w", err)
	}
	return s.decodeTelephony(raw)
}

// TelephonyEnabled reads the typed column the route predicates read.
func (s *Store) TelephonyEnabled(ctx context.Context) (bool, error) {
	var on bool
	err := s.pool.QueryRow(ctx, `SELECT telephony_enabled FROM instance_settings WHERE id = true`).Scan(&on)
	if noRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: telephony enabled: %w", err)
	}
	return on, nil
}

// TelephonyCanCall is GET /api/v1/me's telephony_can_call (FR-041a revision 9, §4.2): the instance's
// "can place calls" (D3) as a person can use it — telephony on AND telephony_can_place_calls, the
// typed columns the route predicates read. False when the group was never saved. No secret is opened.
func (s *Store) TelephonyCanCall(ctx context.Context) (bool, error) {
	var can bool
	err := s.pool.QueryRow(ctx, `SELECT telephony_enabled AND telephony_can_place_calls
		  FROM instance_settings WHERE id = true`).Scan(&can)
	if noRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: telephony can call: %w", err)
	}
	return can, nil
}

// TelephonyVoiceLanguage is the instance's voice_language (§4.1) as GET /api/v1/me names it (§4.2):
// §4.1's default when the group was never saved or names none. It reads the document's plain fields
// only — no secret is opened, so a key that cannot open one does not fail it.
func (s *Store) TelephonyVoiceLanguage(ctx context.Context) (string, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT telephony FROM instance_settings WHERE id = true`).Scan(&raw)
	if err != nil && !noRows(err) {
		return "", fmt.Errorf("store: telephony voice language: %w", err)
	}
	ts := domain.DefaultTelephonySettings()
	if len(raw) > 0 {
		var rec struct {
			Configured    bool   `json:"configured"`
			VoiceLanguage string `json:"voice_language"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			return "", fmt.Errorf("store: decode telephony voice language: %w", err)
		}
		if rec.Configured {
			stored := domain.TelephonySettings{VoiceLanguage: rec.VoiceLanguage}
			stored.Normalize()
			ts.VoiceLanguage = stored.VoiceLanguage
		}
	}
	return ts.VoiceLanguage, nil
}

// decodeTelephony turns the stored document into the group. Each secret is opened on its own: one the
// configured keys cannot open is left empty with its own *Unreadable flag set, and nothing else is
// touched — not the other secret, not the rest of the settings snapshot (branding, silence, …).
func (s *Store) decodeTelephony(raw []byte) (domain.TelephonySettings, error) {
	var rec telephonyRecord
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &rec); err != nil {
			return domain.TelephonySettings{}, fmt.Errorf("store: decode telephony settings: %w", err)
		}
	}
	if !rec.Configured {
		return domain.DefaultTelephonySettings(), nil
	}
	ts := rec.TelephonySettings
	if rec.AuthTokenEnc != "" {
		if pt, err := s.cipher.DecryptBytes(rec.AuthTokenEnc, telephonyAAD(telephonyFieldAuthToken)); err != nil {
			ts.AuthTokenUnreadable = true
		} else {
			ts.Twilio.AuthToken = string(pt)
			wipe(pt)
		}
	}
	if rec.HeadersEnc != "" {
		var headers map[string]string
		pt, err := s.cipher.DecryptBytes(rec.HeadersEnc, telephonyAAD(telephonyFieldHeaders))
		if err == nil {
			err = json.Unmarshal(pt, &headers)
			wipe(pt)
		}
		if err != nil {
			ts.HeadersUnreadable = true
		} else {
			ts.GenericHTTP.Headers = headers
		}
	}
	return ts, nil
}

// SaveTelephonySettings writes the group (§4.1, R1, R10). The caller has normalized and validated ts.
//
//   - Refused without the at-rest key (D29: domain.ErrEncryptionKeyMissing), and for headers_clear
//     together with new headers (domain.ErrTelephonyHeadersConflict).
//   - A blank auth token or an empty headers map keeps what is stored; HeadersClear removes the map.
//   - An enabled Twilio group must end up with a token (domain.ErrInvalidTelephonySettings).
//   - The paging services that are routable before the write and are not after it are computed in
//     the write's own transaction; unless opts.ConfirmUnroutable, a non-empty list rolls the write
//     back and is returned as *domain.ServicesWithoutRouteError.
func (s *Store) SaveTelephonySettings(ctx context.Context, ts domain.TelephonySettings, opts domain.TelephonySaveOptions) error {
	if s.cipher == nil {
		return domain.ErrEncryptionKeyMissing
	}
	if opts.HeadersClear && len(ts.GenericHTTP.Headers) > 0 {
		return domain.ErrTelephonyHeadersConflict
	}
	ts.Configured = true
	ts.AuthTokenUnreadable, ts.HeadersUnreadable = false, false

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin telephony save: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the singleton row (creating it if needed) so concurrent saves serialize and the list below
	// is computed against what this write replaces.
	if _, err := tx.Exec(ctx, `INSERT INTO instance_settings (id) VALUES (true) ON CONFLICT (id) DO NOTHING`); err != nil {
		return fmt.Errorf("store: ensure settings row: %w", err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT telephony FROM instance_settings WHERE id = true FOR UPDATE`).Scan(&raw); err != nil {
		return fmt.Errorf("store: lock telephony settings: %w", err)
	}
	var stored telephonyRecord
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &stored); err != nil {
			return fmt.Errorf("store: decode stored telephony settings: %w", err)
		}
	}

	// A secret the write keeps is re-encrypted under the primary key (FR-041a review P2-2): the save
	// then never writes back a ciphertext only a previous key opens, whatever `cerbix reencrypt` is
	// doing meanwhile. One no configured key opens is kept as it is — it cannot be re-encrypted, and
	// dropping it would lose it; the view reports it unreadable.
	rec := telephonyRecord{TelephonySettings: ts}
	if rec.AuthTokenEnc, err = s.reencryptKeptSecret(stored.AuthTokenEnc, telephonyFieldAuthToken); err != nil {
		return err
	}
	if rec.HeadersEnc, err = s.reencryptKeptSecret(stored.HeadersEnc, telephonyFieldHeaders); err != nil {
		return err
	}
	if token := strings.TrimSpace(ts.Twilio.AuthToken); token != "" {
		if rec.AuthTokenEnc, err = s.cipher.EncryptBytes([]byte(token), telephonyAAD(telephonyFieldAuthToken)); err != nil {
			return fmt.Errorf("store: encrypt telephony auth token: %w", err)
		}
	}
	switch {
	case opts.HeadersClear:
		rec.HeadersEnc = ""
	case len(ts.GenericHTTP.Headers) > 0:
		pt, merr := json.Marshal(ts.GenericHTTP.Headers)
		if merr != nil {
			return fmt.Errorf("store: encode telephony headers: %w", merr)
		}
		rec.HeadersEnc, err = s.cipher.EncryptBytes(pt, telephonyAAD(telephonyFieldHeaders))
		wipe(pt)
		if err != nil {
			return fmt.Errorf("store: encrypt telephony headers: %w", err)
		}
	}
	if err := ts.ValidateCredentials(rec.AuthTokenEnc != ""); err != nil {
		return err
	}

	before, err := ownsPagingRoutableTx(ctx, tx)
	if err != nil {
		return err
	}
	doc, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("store: encode telephony settings: %w", err)
	}
	var provider *string
	if ts.Provider != "" {
		p := ts.Provider
		provider = &p
	}
	if _, err := tx.Exec(ctx, `
		UPDATE instance_settings
		   SET telephony = $1, telephony_enabled = $2, telephony_provider = $3, updated_at = now()
		 WHERE id = true`, doc, ts.Enabled, provider); err != nil {
		return fmt.Errorf("store: write telephony settings: %w", err)
	}
	after, err := ownsPagingRoutableTx(ctx, tx)
	if err != nil {
		return err
	}
	if list, total := servicesLosingRoute(before, after); total > 0 && !opts.ConfirmUnroutable {
		return &domain.ServicesWithoutRouteError{Services: list, Total: total}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit telephony settings: %w", err)
	}
	return nil
}

// reencryptKeptSecret is a stored ciphertext of field re-encrypted under the primary key, or the
// ciphertext as it is when no configured key opens it (or it is empty).
func (s *Store) reencryptKeptSecret(enc, field string) (string, error) {
	if enc == "" {
		return "", nil
	}
	aad := telephonyAAD(field)
	pt, err := s.cipher.DecryptBytes(enc, aad)
	if err != nil {
		return enc, nil
	}
	defer wipe(pt)
	next, err := s.cipher.EncryptBytes(pt, aad)
	if err != nil {
		return "", fmt.Errorf("store: re-encrypt kept telephony %s: %w", field, err)
	}
	return next, nil
}

// ownsPagingRoutableTx is every service that owns paging and is routable — routablePredicate, which
// reads the telephony columns through D31 rule 1 — as the transaction sees it, keyed by service id.
// Asked after the UPDATE in the same transaction, it judges the PROPOSED settings.
func ownsPagingRoutableTx(ctx context.Context, tx pgx.Tx) (map[string]domain.ServiceWithoutRoute, error) {
	rows, err := tx.Query(ctx, `
		SELECT s.id::text, p.name, s.name, s.slug
		  FROM services s
		  JOIN projects p ON p.id = s.project_id
		 WHERE s.owns_paging AND `+routablePredicate)
	if err != nil {
		return nil, fmt.Errorf("store: routable paging services: %w", err)
	}
	defer rows.Close()
	out := map[string]domain.ServiceWithoutRoute{}
	for rows.Next() {
		var id string
		var r domain.ServiceWithoutRoute
		if err := rows.Scan(&id, &r.Project, &r.Service, &r.Slug); err != nil {
			return nil, fmt.Errorf("store: scan routable paging service: %w", err)
		}
		out[id] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate routable paging services: %w", err)
	}
	return out, nil
}

// servicesLosingRoute is the services routable before and not after, ordered by project name then
// slug, at most maxServicesWithoutRoute of them, and how many there are in all.
func servicesLosingRoute(before, after map[string]domain.ServiceWithoutRoute) ([]domain.ServiceWithoutRoute, int) {
	type lost struct {
		id string
		domain.ServiceWithoutRoute
	}
	var all []lost
	for id, r := range before {
		if _, still := after[id]; !still {
			all = append(all, lost{id: id, ServiceWithoutRoute: r})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		if a.Slug != b.Slug {
			return a.Slug < b.Slug
		}
		return a.id < b.id
	})
	n := len(all)
	if n > maxServicesWithoutRoute {
		all = all[:maxServicesWithoutRoute]
	}
	out := make([]domain.ServiceWithoutRoute, len(all))
	for i, l := range all {
		out[i] = l.ServiceWithoutRoute
	}
	return out, n
}

// reencryptTelephonyBeforeWrite, when set by a test, runs between reencrypt's read of the document
// and its compare-and-set write.
var reencryptTelephonyBeforeWrite func(ctx context.Context)

// reencryptTelephonyAttempts bounds reencrypt's read-and-CAS loop over the telephony document.
const reencryptTelephonyAttempts = 5

// telephonySecretFields are the document's two ciphertext keys and the field each is bound to.
var telephonySecretFields = []struct{ key, field string }{
	{"twilio_auth_token_enc", telephonyFieldAuthToken},
	{"generic_http_headers_enc", telephonyFieldHeaders},
}

// reencryptTelephonySecrets rewrites the group's two ciphertexts under the primary key, with the
// document as the compare-and-set (FR-041a review P2-2): a write that lands between the read and the
// CAS makes the CAS change nothing, and the document is read again and the rewrite retried — a save
// re-encrypts what it keeps under the primary too, so the retry finds either that or what to do.
// After reencryptTelephonyAttempts lost races it fails rather than report success. A secret field
// that is not a string, or that no configured key opens, is an error naming the field, never
// skipped.
func (s *Store) reencryptTelephonySecrets(ctx context.Context) error {
	for attempt := 0; attempt < reencryptTelephonyAttempts; attempt++ {
		done, err := s.reencryptTelephonyOnce(ctx)
		if err != nil || done {
			return err
		}
	}
	return fmt.Errorf("store: rewrite telephony secrets: the settings changed under each of %d attempts; run cerbix reencrypt again",
		reencryptTelephonyAttempts)
}

// reencryptTelephonyOnce is one read-and-CAS. done is false only when the CAS changed nothing because
// the document changed after the read.
func (s *Store) reencryptTelephonyOnce(ctx context.Context) (done bool, err error) {
	var raw []byte
	err = s.pool.QueryRow(ctx, `SELECT telephony FROM instance_settings WHERE id = true`).Scan(&raw)
	if noRows(err) || (err == nil && len(raw) == 0) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: scan telephony settings for reencrypt: %w", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false, fmt.Errorf("store: decode telephony settings for reencrypt: %w", err)
	}
	changed := false
	for _, f := range telephonySecretFields {
		v, ok := doc[f.key]
		if !ok || string(v) == "null" {
			continue
		}
		var token string
		if err := json.Unmarshal(v, &token); err != nil {
			return false, fmt.Errorf("store: telephony %s is not a stored ciphertext", f.field)
		}
		if token == "" {
			continue
		}
		aad := telephonyAAD(f.field)
		pt, err := s.cipher.DecryptBytes(token, aad)
		if err != nil {
			return false, fmt.Errorf("store: decrypt telephony %s: %w", f.field, err)
		}
		next, err := s.cipher.EncryptBytes(pt, aad)
		wipe(pt)
		if err != nil {
			return false, fmt.Errorf("store: encrypt telephony %s: %w", f.field, err)
		}
		enc, _ := json.Marshal(next)
		doc[f.key] = enc
		changed = true
	}
	if !changed {
		return true, nil
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return false, fmt.Errorf("store: encode telephony settings for reencrypt: %w", err)
	}
	if reencryptTelephonyBeforeWrite != nil {
		reencryptTelephonyBeforeWrite(ctx)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE instance_settings SET telephony = $1::jsonb WHERE id = true AND telephony = $2::jsonb`,
		string(out), string(raw))
	if err != nil {
		return false, fmt.Errorf("store: rewrite telephony secrets: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
