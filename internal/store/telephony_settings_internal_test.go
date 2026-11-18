package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

const (
	tsAccountSID = "AC0123456789abcdef0123456789abcdef"
	tsAuthToken  = "twilio-auth-token-plaintext"
	tsHeaderVal  = "gateway-header-plaintext"
)

func telephonyStore(t *testing.T) (*Store, context.Context, *secret.Cipher) {
	t.Helper()
	st, ctx := outboxTestStore(t)
	c, err := secret.New(randKey(t))
	if err != nil {
		t.Fatal(err)
	}
	st.WithCipher(c)
	return st, ctx, c
}

func twilioTS(enabled bool, token string) domain.TelephonySettings {
	ts := domain.TelephonySettings{
		Enabled: enabled, Provider: domain.TelephonyProviderTwilio,
		Twilio: domain.TelephonyTwilio{AccountSID: tsAccountSID, AuthToken: token, From: "+15005550006"},
	}
	ts.Normalize()
	return ts
}

func genericTS(enabled bool, headers map[string]string) domain.TelephonySettings {
	ts := domain.TelephonySettings{
		Enabled: enabled, Provider: domain.TelephonyProviderGenericHTTP,
		GenericHTTP: domain.TelephonyGenericHTTP{
			Method: "POST", URL: "https://sms.example/send", ContentType: "application/json",
			BodyTemplate: `{"to":"{{to}}","text":"{{text}}"}`, Headers: headers,
		},
	}
	ts.Normalize()
	return ts
}

// telephonyRow is the stored group exactly as the database holds it.
type telephonyRow struct {
	doc      string
	enabled  bool
	provider *string
}

func readTelephonyRow(t *testing.T, st *Store, ctx context.Context) (telephonyRow, bool) {
	t.Helper()
	var r telephonyRow
	err := st.pool.QueryRow(ctx,
		`SELECT telephony::text, telephony_enabled, telephony_provider FROM instance_settings WHERE id = true`).
		Scan(&r.doc, &r.enabled, &r.provider)
	if noRows(err) {
		return r, false
	}
	if err != nil {
		t.Fatalf("read telephony row: %v", err)
	}
	return r, true
}

func TestTelephonySettingsAbsentMeansDisabled(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	ts, err := st.GetTelephonySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ts.Configured || ts.Enabled || ts.Provider != "" || ts.VoiceLanguage != "en-US" || ts.SMSMaxSegments != 2 {
		t.Fatalf("absent group = %+v, want the disabled defaults", ts)
	}
	on, err := st.TelephonyEnabled(ctx)
	if err != nil || on {
		t.Fatalf("TelephonyEnabled = %v, %v", on, err)
	}
	all, err := st.GetInstanceSettings(ctx)
	if err != nil || all.Telephony.Configured || all.Telephony.Enabled {
		t.Fatalf("instance settings telephony = %+v, %v", all.Telephony, err)
	}
}

// D29: no at-rest key, no save — whatever the write carries.
func TestTelephonySaveRefusedWithoutTheKey(t *testing.T) {
	st, ctx := outboxTestStore(t)
	if st.EncryptionKeyConfigured() {
		t.Fatal("a store without a cipher reports a key")
	}
	for _, ts := range []domain.TelephonySettings{twilioTS(true, tsAuthToken), genericTS(false, nil)} {
		if err := st.SaveTelephonySettings(ctx, ts, domain.TelephonySaveOptions{}); !errors.Is(err, domain.ErrEncryptionKeyMissing) {
			t.Fatalf("save without a key: err = %v", err)
		}
	}
	if _, ok := readTelephonyRow(t, st, ctx); ok {
		t.Fatal("a refused save wrote the settings row")
	}
	c, _ := secret.New(randKey(t))
	st.WithCipher(c)
	if !st.EncryptionKeyConfigured() {
		t.Fatal("a store with a cipher reports no key")
	}
}

func TestTelephonySecretsEncryptedBlankKeepsAndHeadersClear(t *testing.T) {
	st, ctx, c := telephonyStore(t)

	ts := twilioTS(true, tsAuthToken)
	ts.GenericHTTP = genericTS(false, map[string]string{"X-Api-Key": tsHeaderVal}).GenericHTTP
	if err := st.SaveTelephonySettings(ctx, ts, domain.TelephonySaveOptions{}); err != nil {
		t.Fatalf("save: %v", err)
	}
	row, _ := readTelephonyRow(t, st, ctx)
	if strings.Contains(row.doc, tsAuthToken) || strings.Contains(row.doc, tsHeaderVal) || strings.Contains(row.doc, "X-Api-Key") {
		t.Fatalf("a secret is stored in clear: %s", row.doc)
	}
	// Each ciphertext is bound to its own field (R1).
	var doc map[string]any
	_ = json.Unmarshal([]byte(row.doc), &doc)
	tokEnc, _ := doc["twilio_auth_token_enc"].(string)
	if pt, err := c.DecryptBytes(tokEnc, secret.CanonicalAAD("instance_settings", "telephony", "twilio.auth_token")); err != nil || string(pt) != tsAuthToken {
		t.Fatalf("auth token ciphertext not bound to its field: %v", err)
	}
	if _, err := c.DecryptBytes(tokEnc, secret.CanonicalAAD("instance_settings", "telephony", "generic_http.headers")); err == nil {
		t.Fatal("the auth token opens under the headers' binding")
	}

	got, err := st.GetTelephonySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Configured || got.Twilio.AuthToken != tsAuthToken || got.GenericHTTP.Headers["X-Api-Key"] != tsHeaderVal {
		t.Fatalf("read back = %+v", got)
	}

	// A blank write keeps both secrets (§4.1).
	blank := twilioTS(true, "")
	blank.GenericHTTP = genericTS(false, nil).GenericHTTP
	blank.Twilio.From = "+15005550007"
	if err := st.SaveTelephonySettings(ctx, blank, domain.TelephonySaveOptions{}); err != nil {
		t.Fatalf("blank save: %v", err)
	}
	got, _ = st.GetTelephonySettings(ctx)
	if got.Twilio.AuthToken != tsAuthToken || got.GenericHTTP.Headers["X-Api-Key"] != tsHeaderVal || got.Twilio.From != "+15005550007" {
		t.Fatalf("blank write did not keep the secrets: %+v", got)
	}

	// A new token replaces; new headers replace the whole map.
	repl := twilioTS(true, "new-token")
	repl.GenericHTTP = genericTS(false, map[string]string{"Authorization": "Bearer x"}).GenericHTTP
	if err := st.SaveTelephonySettings(ctx, repl, domain.TelephonySaveOptions{}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, _ = st.GetTelephonySettings(ctx)
	if got.Twilio.AuthToken != "new-token" || !reflect.DeepEqual(got.GenericHTTP.Headers, map[string]string{"Authorization": "Bearer x"}) {
		t.Fatalf("replace = %+v", got)
	}

	// headers_clear with new headers is refused and changes nothing.
	before, _ := readTelephonyRow(t, st, ctx)
	conflict := twilioTS(true, "")
	conflict.GenericHTTP = genericTS(false, map[string]string{"X-New": "v"}).GenericHTTP
	if err := st.SaveTelephonySettings(ctx, conflict, domain.TelephonySaveOptions{HeadersClear: true}); !errors.Is(err, domain.ErrTelephonyHeadersConflict) {
		t.Fatalf("clear+headers: err = %v", err)
	}
	if after, _ := readTelephonyRow(t, st, ctx); after.doc != before.doc {
		t.Fatal("a refused write changed the stored group")
	}

	// headers_clear removes the stored map and keeps the token.
	clr := twilioTS(true, "")
	clr.GenericHTTP = genericTS(false, nil).GenericHTTP
	if err := st.SaveTelephonySettings(ctx, clr, domain.TelephonySaveOptions{HeadersClear: true}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ = st.GetTelephonySettings(ctx)
	if len(got.GenericHTTP.Headers) != 0 || got.Twilio.AuthToken != "new-token" {
		t.Fatalf("after clear = %+v", got)
	}
	row, _ = readTelephonyRow(t, st, ctx)
	if strings.Contains(row.doc, "generic_http_headers_enc") {
		t.Fatalf("cleared headers left a ciphertext: %s", row.doc)
	}
}

// An enabled Twilio group with no token anywhere — neither sent nor stored — cannot send.
func TestTelephonySaveNeedsTheTwilioToken(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	if err := st.SaveTelephonySettings(ctx, twilioTS(true, ""), domain.TelephonySaveOptions{}); !errors.Is(err, domain.ErrInvalidTelephonySettings) {
		t.Fatalf("enabled without a token: err = %v", err)
	}
	if _, ok := readTelephonyRow(t, st, ctx); ok {
		t.Fatal("the refused save left a settings row")
	}
	// Disabled, it may be saved without one.
	if err := st.SaveTelephonySettings(ctx, twilioTS(false, ""), domain.TelephonySaveOptions{}); err != nil {
		t.Fatalf("disabled without a token: %v", err)
	}
}

// R1: the typed columns the predicates read are written with the JSON, in the same statement.
func TestTelephonyTypedColumnsFollowTheJSON(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	check := func(label string, wantEnabled bool, wantProvider string) {
		t.Helper()
		row, ok := readTelephonyRow(t, st, ctx)
		if !ok {
			t.Fatalf("%s: no row", label)
		}
		var doc struct {
			Enabled  bool   `json:"enabled"`
			Provider string `json:"provider"`
		}
		if err := json.Unmarshal([]byte(row.doc), &doc); err != nil {
			t.Fatal(err)
		}
		gotProvider := ""
		if row.provider != nil {
			gotProvider = *row.provider
		}
		if row.enabled != wantEnabled || doc.Enabled != wantEnabled || gotProvider != wantProvider || doc.Provider != wantProvider {
			t.Fatalf("%s: columns (%v,%q) json (%v,%q), want (%v,%q)",
				label, row.enabled, gotProvider, doc.Enabled, doc.Provider, wantEnabled, wantProvider)
		}
		on, err := st.TelephonyEnabled(ctx)
		if err != nil || on != wantEnabled {
			t.Fatalf("%s: TelephonyEnabled = %v, %v", label, on, err)
		}
	}
	steps := []struct {
		label    string
		ts       domain.TelephonySettings
		enabled  bool
		provider string
	}{
		{"twilio on", twilioTS(true, tsAuthToken), true, "twilio"},
		{"generic on", genericTS(true, nil), true, "generic_http"},
		{"generic off", genericTS(false, nil), false, "generic_http"},
		{"no provider", func() domain.TelephonySettings { ts := domain.DefaultTelephonySettings(); return ts }(), false, ""},
	}
	for _, s := range steps {
		if err := st.SaveTelephonySettings(ctx, s.ts, domain.TelephonySaveOptions{ConfirmUnroutable: true}); err != nil {
			t.Fatalf("%s: %v", s.label, err)
		}
		check(s.label, s.enabled, s.provider)
	}
}

// Removing the key a secret was sealed with makes the secret unreadable, not the whole settings row:
// branding, silence and the rest must keep loading.
func TestTelephonyUnreadableSecretsDoNotBreakOtherSettings(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	if err := st.UpsertBranding(ctx, domain.Branding{Configured: true, ProductName: "Status"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveTelephonySettings(ctx, twilioTS(true, tsAuthToken), domain.TelephonySaveOptions{}); err != nil {
		t.Fatal(err)
	}
	other, _ := secret.New(randKey(t))
	st.WithCipher(other)
	all, err := st.GetInstanceSettings(ctx)
	if err != nil {
		t.Fatalf("instance settings with a foreign key: %v", err)
	}
	if all.Branding.ProductName != "Status" {
		t.Fatalf("branding lost: %+v", all.Branding)
	}
	if !all.Telephony.AuthTokenUnreadable || all.Telephony.Twilio.AuthToken != "" || !all.Telephony.Enabled ||
		!all.Telephony.AuthTokenStored() {
		t.Fatalf("telephony = %+v, want enabled with an unreadable, stored token", all.Telephony)
	}
}

// Unreadability is per secret: the key was rotated without reencrypt and the admin re-entered only the
// token. The token is readable, the headers are not — and the headers' failure must not blank the token
// (review of Task 6, finding 1c). The unreadable headers stay stored: a blank write keeps them.
func TestTelephonyUnreadableSecretIsPerSecret(t *testing.T) {
	st, ctx := outboxTestStore(t)
	cA, _ := secret.New(randKey(t))
	st.WithCipher(cA)
	ts := twilioTS(true, tsAuthToken)
	ts.GenericHTTP = genericTS(false, map[string]string{"X-Api-Key": tsHeaderVal}).GenericHTTP
	if err := st.SaveTelephonySettings(ctx, ts, domain.TelephonySaveOptions{}); err != nil {
		t.Fatal(err)
	}

	cB, _ := secret.New(randKey(t))
	st.WithCipher(cB)
	retoken := twilioTS(true, "re-entered-token")
	retoken.GenericHTTP = genericTS(false, nil).GenericHTTP
	if err := st.SaveTelephonySettings(ctx, retoken, domain.TelephonySaveOptions{}); err != nil {
		t.Fatalf("re-enter the token: %v", err)
	}
	got, err := st.GetTelephonySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthTokenUnreadable || got.Twilio.AuthToken != "re-entered-token" {
		t.Fatalf("the readable token was lost: unreadable=%v token=%q", got.AuthTokenUnreadable, got.Twilio.AuthToken)
	}
	if !got.HeadersUnreadable || got.GenericHTTP.Headers != nil || !got.HeadersStored() {
		t.Fatalf("headers: unreadable=%v headers=%v stored=%v", got.HeadersUnreadable, got.GenericHTTP.Headers, got.HeadersStored())
	}
	row, _ := readTelephonyRow(t, st, ctx)
	if !strings.Contains(row.doc, "generic_http_headers_enc") {
		t.Fatal("the unreadable headers were wiped instead of kept")
	}

	// The mirror case: readable headers, unreadable token.
	cC, _ := secret.New(randKey(t))
	st.WithCipher(cC)
	reheader := twilioTS(false, "")
	reheader.GenericHTTP = genericTS(false, map[string]string{"X-New": "v"}).GenericHTTP
	if err := st.SaveTelephonySettings(ctx, reheader, domain.TelephonySaveOptions{}); err != nil {
		t.Fatalf("re-enter the headers: %v", err)
	}
	got, _ = st.GetTelephonySettings(ctx)
	if !got.AuthTokenUnreadable || got.HeadersUnreadable || got.GenericHTTP.Headers["X-New"] != "v" || !got.AuthTokenStored() {
		t.Fatalf("mirror: %+v", got)
	}
}

func TestReencryptCoversTelephonySecrets(t *testing.T) {
	st, ctx := outboxTestStore(t)
	keyA, keyB := randKey(t), randKey(t)
	cA, _ := secret.New(keyA)
	st.WithCipher(cA)
	ts := twilioTS(true, tsAuthToken)
	ts.GenericHTTP = genericTS(false, map[string]string{"X-Api-Key": tsHeaderVal}).GenericHTTP
	if err := st.SaveTelephonySettings(ctx, ts, domain.TelephonySaveOptions{}); err != nil {
		t.Fatal(err)
	}

	cB, _ := secret.New(keyB)
	st.WithCipher(cB)
	if got, _ := st.GetTelephonySettings(ctx); !got.AuthTokenUnreadable || !got.HeadersUnreadable {
		t.Fatal("key B alone opened secrets sealed under key A")
	}

	rotated, _ := secret.New(keyB, keyA)
	st.WithCipher(rotated)
	if _, err := st.ReencryptSecrets(ctx); err != nil {
		t.Fatalf("reencrypt: %v", err)
	}

	st.WithCipher(cB)
	got, err := st.GetTelephonySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthTokenUnreadable || got.HeadersUnreadable || got.Twilio.AuthToken != tsAuthToken || got.GenericHTTP.Headers["X-Api-Key"] != tsHeaderVal {
		t.Fatalf("after reencrypt under key B = %+v", got)
	}
	st.WithCipher(cA)
	if got, _ := st.GetTelephonySettings(ctx); !got.AuthTokenUnreadable || !got.HeadersUnreadable {
		t.Fatal("the old key still opens the rewritten secrets")
	}
}

// FR-041a review P2-2: a save that keeps a stored secret (a blank token, no headers) re-encrypts it
// under the PRIMARY key, so it can never write back a ciphertext only the previous key opens — the
// save `cerbix reencrypt` races with leaves nothing for the old key.
func TestTelephonySaveWithBlankSecretsReencryptsThemUnderThePrimary(t *testing.T) {
	st, ctx := outboxTestStore(t)
	keyA, keyB := randKey(t), randKey(t)
	cA, _ := secret.New(keyA)
	st.WithCipher(cA)
	ts := twilioTS(true, tsAuthToken)
	ts.GenericHTTP = genericTS(false, map[string]string{"X-Api-Key": tsHeaderVal}).GenericHTTP
	if err := st.SaveTelephonySettings(ctx, ts, domain.TelephonySaveOptions{}); err != nil {
		t.Fatal(err)
	}

	rotated, _ := secret.New(keyB, keyA)
	st.WithCipher(rotated)
	blank := twilioTS(true, "")
	blank.SMSMaxSegments = 3
	blank.GenericHTTP = genericTS(false, nil).GenericHTTP
	if err := st.SaveTelephonySettings(ctx, blank, domain.TelephonySaveOptions{}); err != nil {
		t.Fatalf("blank save under the rotated keyring: %v", err)
	}

	cB, _ := secret.New(keyB)
	st.WithCipher(cB)
	got, err := st.GetTelephonySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthTokenUnreadable || got.HeadersUnreadable || got.Twilio.AuthToken != tsAuthToken ||
		got.GenericHTTP.Headers["X-Api-Key"] != tsHeaderVal || got.SMSMaxSegments != 3 {
		t.Fatalf("after a blank save under [B, A], read with B alone: token unreadable %v, headers unreadable %v, segments %d",
			got.AuthTokenUnreadable, got.HeadersUnreadable, got.SMSMaxSegments)
	}
}

// FR-041a review P2-2: a write that lands between `cerbix reencrypt`'s read and its compare-and-set
// makes the CAS change nothing; reencrypt reads again and retries rather than reporting success over
// a lost re-encryption. The write's own change survives.
func TestReencryptTelephonyRetriesWhenTheDocumentChangedMeanwhile(t *testing.T) {
	st, ctx := outboxTestStore(t)
	keyA, keyB := randKey(t), randKey(t)
	cA, _ := secret.New(keyA)
	st.WithCipher(cA)
	ts := twilioTS(true, tsAuthToken)
	ts.GenericHTTP = genericTS(false, map[string]string{"X-Api-Key": tsHeaderVal}).GenericHTTP
	if err := st.SaveTelephonySettings(ctx, ts, domain.TelephonySaveOptions{}); err != nil {
		t.Fatal(err)
	}
	rotated, _ := secret.New(keyB, keyA)
	st.WithCipher(rotated)

	interleaved := 0
	reencryptTelephonyBeforeWrite = func(ctx context.Context) {
		if interleaved > 0 {
			return
		}
		interleaved++
		// A concurrent write of a non-secret field, the stored ciphertexts left as they were.
		if _, err := st.pool.Exec(ctx, `UPDATE instance_settings
			   SET telephony = jsonb_set(telephony, '{sms_max_segments}', '3') WHERE id = true`); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { reencryptTelephonyBeforeWrite = nil })
	if err := st.reencryptTelephonySecrets(ctx); err != nil {
		t.Fatalf("reencrypt: %v", err)
	}
	if interleaved != 1 {
		t.Fatalf("the interleaved write ran %d time(s)", interleaved)
	}

	cB, _ := secret.New(keyB)
	st.WithCipher(cB)
	got, err := st.GetTelephonySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthTokenUnreadable || got.HeadersUnreadable || got.SMSMaxSegments != 3 {
		t.Fatalf("after reencrypt raced by a write, read with B alone: token unreadable %v, headers unreadable %v, segments %d",
			got.AuthTokenUnreadable, got.HeadersUnreadable, got.SMSMaxSegments)
	}
}

// Group-A review M-1: when every one of reencrypt's attempts loses its compare-and-set to a concurrent
// write, ReencryptSecrets — what `cerbix reencrypt` calls, and exits 1 on — fails naming the attempts,
// and never reports success over the re-encryption it could not write.
func TestReencryptFailsWhenEveryAttemptLosesTheRace(t *testing.T) {
	st, ctx := outboxTestStore(t)
	keyA, keyB := randKey(t), randKey(t)
	cA, _ := secret.New(keyA)
	st.WithCipher(cA)
	if err := st.SaveTelephonySettings(ctx, twilioTS(true, tsAuthToken), domain.TelephonySaveOptions{}); err != nil {
		t.Fatal(err)
	}
	rotated, _ := secret.New(keyB, keyA)
	st.WithCipher(rotated)

	writes := 0
	reencryptTelephonyBeforeWrite = func(ctx context.Context) {
		writes++
		if _, err := st.pool.Exec(ctx, `UPDATE instance_settings
			   SET telephony = jsonb_set(telephony, '{sms_max_segments}', to_jsonb($1::int)) WHERE id = true`, writes+2); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { reencryptTelephonyBeforeWrite = nil })
	_, err := st.ReencryptSecrets(ctx)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("each of %d attempts", reencryptTelephonyAttempts)) {
		t.Fatalf("reencrypt over %d lost races: %v; want the error naming the attempts", writes, err)
	}
	if writes != reencryptTelephonyAttempts {
		t.Fatalf("%d attempts; want %d", writes, reencryptTelephonyAttempts)
	}
	// Nothing was written under B: the token is still A's.
	cB, _ := secret.New(keyB)
	st.WithCipher(cB)
	if got, err := st.GetTelephonySettings(ctx); err != nil || !got.AuthTokenUnreadable {
		t.Fatalf("read with B alone: unreadable %v, %v; want the token still under A", got.AuthTokenUnreadable, err)
	}
}

// FR-041a review P2-2: a stored secret field reencrypt cannot handle — not a string, or a ciphertext
// no configured key opens — is reported as an error naming the field, never skipped.
func TestReencryptTelephonyReportsACorruptedSecret(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"not a string", `12345`},
		{"undecryptable", `"v1:not-a-ciphertext"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, ctx, _ := telephonyStore(t)
			if err := st.SaveTelephonySettings(ctx, twilioTS(true, tsAuthToken), domain.TelephonySaveOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.pool.Exec(ctx, `UPDATE instance_settings
				   SET telephony = jsonb_set(telephony, '{twilio_auth_token_enc}', $1::jsonb) WHERE id = true`, tc.value); err != nil {
				t.Fatal(err)
			}
			err := st.reencryptTelephonySecrets(ctx)
			if err == nil || !strings.Contains(err.Error(), telephonyFieldAuthToken) {
				t.Fatalf("reencrypt over a corrupted auth token: %v; want an error naming %s", err, telephonyFieldAuthToken)
			}
			if _, err := st.ReencryptSecrets(ctx); err == nil {
				t.Fatal("ReencryptSecrets reported success over a corrupted telephony secret")
			}
		})
	}
}

// ── R10: the 409 answer ──────────────────────────────────────────────────────

func TestServicesLosingRouteDiff(t *testing.T) {
	ref := func(p, s string) domain.ServiceWithoutRoute {
		return domain.ServiceWithoutRoute{Project: p, Service: strings.ToUpper(s), Slug: s}
	}
	before := map[string]domain.ServiceWithoutRoute{
		"1": ref("Beta", "b"), "2": ref("Alpha", "z"), "3": ref("Alpha", "a"), "4": ref("Gamma", "g"),
	}
	after := map[string]domain.ServiceWithoutRoute{"4": ref("Gamma", "g"), "9": ref("New", "n")}
	list, total := servicesLosingRoute(before, after)
	want := []domain.ServiceWithoutRoute{ref("Alpha", "a"), ref("Alpha", "z"), ref("Beta", "b")}
	if total != 3 || !reflect.DeepEqual(list, want) {
		t.Fatalf("diff = %v (%d), want %v", list, total, want)
	}
	if list, total := servicesLosingRoute(before, before); total != 0 || len(list) != 0 {
		t.Fatalf("no change = %v (%d)", list, total)
	}

	many := map[string]domain.ServiceWithoutRoute{}
	for i := 0; i < 60; i++ {
		many[fmt.Sprint(i)] = ref(fmt.Sprintf("P%02d", 59-i), "s")
	}
	list, total = servicesLosingRoute(many, nil)
	if total != 60 || len(list) != maxServicesWithoutRoute || list[0].Project != "P00" || list[49].Project != "P49" {
		t.Fatalf("truncation = %d entries, total %d, first %v, last %v", len(list), total, list[0], list[len(list)-1])
	}
}

type telephonySaveFunc func(ctx context.Context, ts domain.TelephonySettings, opts domain.TelephonySaveOptions) error

// routeFixture creates a project with one service and the given enabled channel types.
func routeFixture(t *testing.T, st *Store, ctx context.Context, orgID, project string, ownsPaging bool, slugs []string, channels ...string) {
	t.Helper()
	proj, err := st.CreateProject(ctx, orgID, strings.ToLower(project), project)
	if err != nil {
		t.Fatalf("project %s: %v", project, err)
	}
	for _, slug := range slugs {
		svc, err := st.CreateService(ctx, domain.Service{ProjectID: proj.ID, Slug: slug, Name: strings.ToUpper(slug)})
		if err != nil {
			t.Fatalf("service %s: %v", slug, err)
		}
		exec(t, st, ctx, `UPDATE services SET owns_paging = $2 WHERE id = $1`, svc.ID, ownsPaging)
	}
	for i, typ := range channels {
		switch typ {
		case "sms", "voice":
			exec(t, st, ctx, `
				INSERT INTO notification_channels
				  (project_id, type, name, enabled, recipient, number_enc, number_hash, number_masked, number_verified_at)
				VALUES ($1, $2, $3, true, 'number', 'enc', $4, '+1•••••0147', now())`,
				proj.ID, typ, fmt.Sprintf("%s-%d", typ, i), fmt.Sprintf("hash-%s-%s-%d", project, typ, i))
		default:
			exec(t, st, ctx, `
				INSERT INTO notification_channels (project_id, type, name, config, enabled)
				VALUES ($1, $2, $3, '{"url":"https://hooks.example/x"}', true)`, proj.ID, typ, fmt.Sprintf("%s-%d", typ, i))
		}
	}
}

func runTelephonyRouteScenario(t *testing.T, st *Store, ctx context.Context, save telephonySaveFunc) {
	org, err := st.CreateOrganization(ctx, "routes", "Routes")
	if err != nil {
		t.Fatal(err)
	}
	routeFixture(t, st, ctx, org.ID, "Alpha", true, []string{"alpha-pager"}, "sms")
	routeFixture(t, st, ctx, org.ID, "Beta", true, []string{"beta-pager"}, "sms", "slack")
	routeFixture(t, st, ctx, org.ID, "Gamma", false, []string{"gamma-quiet"}, "sms")
	routeFixture(t, st, ctx, org.ID, "Delta", true, []string{"delta-voice"}, "voice")

	// Switching telephony on gains routes and loses none.
	if err := save(ctx, twilioTS(true, tsAuthToken), domain.TelephonySaveOptions{}); err != nil {
		t.Fatalf("enable: %v", err)
	}

	expect409 := func(label string, ts domain.TelephonySettings, want []domain.ServiceWithoutRoute) {
		t.Helper()
		before, _ := readTelephonyRow(t, st, ctx)
		err := save(ctx, ts, domain.TelephonySaveOptions{})
		var sw *domain.ServicesWithoutRouteError
		if !errors.As(err, &sw) {
			t.Fatalf("%s: err = %v, want ServicesWithoutRouteError", label, err)
		}
		if sw.Total != len(want) || !reflect.DeepEqual(sw.Services, want) {
			t.Fatalf("%s: services = %v (total %d), want %v", label, sw.Services, sw.Total, want)
		}
		after, _ := readTelephonyRow(t, st, ctx)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("%s: the 409 changed the stored settings", label)
		}
	}
	// generic_http while a voice channel exists: the voice-only service loses its route.
	expect409("to generic_http", genericTS(true, nil), []domain.ServiceWithoutRoute{
		{Project: "Delta", Service: "DELTA-VOICE", Slug: "delta-voice"},
	})
	// Off: every service whose only route is a phone, and not Beta (Slack) nor Gamma (does not page).
	off := twilioTS(false, "")
	expect409("off", off, []domain.ServiceWithoutRoute{
		{Project: "Alpha", Service: "ALPHA-PAGER", Slug: "alpha-pager"},
		{Project: "Delta", Service: "DELTA-VOICE", Slug: "delta-voice"},
	})

	// The same request, confirmed, is saved.
	if err := save(ctx, off, domain.TelephonySaveOptions{ConfirmUnroutable: true}); err != nil {
		t.Fatalf("confirmed off: %v", err)
	}
	if on, _ := st.TelephonyEnabled(ctx); on {
		t.Fatal("the confirmed save did not switch telephony off")
	}
	// The kept token survived the confirmed blank write.
	if got, _ := st.GetTelephonySettings(ctx); got.Twilio.AuthToken != tsAuthToken {
		t.Fatal("the confirmed save lost the stored token")
	}
}

func runTelephonyRouteTruncation(t *testing.T, st *Store, ctx context.Context, save telephonySaveFunc) {
	org, err := st.CreateOrganization(ctx, "many", "Many")
	if err != nil {
		t.Fatal(err)
	}
	routeFixture(t, st, ctx, org.ID, "P00", true, []string{"b-svc", "a-svc"}, "sms")
	for i := 1; i < 55; i++ {
		routeFixture(t, st, ctx, org.ID, fmt.Sprintf("P%02d", i), true, []string{"svc"}, "sms")
	}
	if err := save(ctx, twilioTS(true, tsAuthToken), domain.TelephonySaveOptions{}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	err = save(ctx, twilioTS(false, ""), domain.TelephonySaveOptions{})
	var sw *domain.ServicesWithoutRouteError
	if !errors.As(err, &sw) {
		t.Fatalf("err = %v", err)
	}
	if sw.Total != 56 || len(sw.Services) != 50 {
		t.Fatalf("got %d entries of %d, want 50 of 56", len(sw.Services), sw.Total)
	}
	if sw.Services[0].Slug != "a-svc" || sw.Services[1].Slug != "b-svc" || sw.Services[2].Project != "P01" ||
		sw.Services[49].Project != "P48" {
		t.Fatalf("order: first %v %v %v, last %v", sw.Services[0], sw.Services[1], sw.Services[2], sw.Services[49])
	}
}

// D31 rule 1 is in routablePredicate (Task 9), so the save's own transaction sees a phone channel's
// routability change with the settings it is writing: telephony off, or generic_http under a voice
// channel, answers 409 with exactly the paging services that lose their last route.
func TestTelephonySaveAnswersServicesWithoutRoute(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	runTelephonyRouteScenario(t, st, ctx, st.SaveTelephonySettings)
}

func TestTelephonySaveTruncatesServicesWithoutRoute(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	runTelephonyRouteTruncation(t, st, ctx, st.SaveTelephonySettings)
}
