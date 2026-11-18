package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-041a — migration 00120: the part-(a) phone schema and `incidents.ref` (spec §8, D21, D27).

// incidentRefRe is the shape of a reference: eight characters of the Crockford base32 alphabet.
var incidentRefRe = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{8}$`)

// seedProbeProject writes an organization and a project naming only columns every release has.
func seedProbeProject(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	var orgID, projID string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO organizations (slug, name) VALUES ('acme','Acme') RETURNING id::text`).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`INSERT INTO projects (org_id, slug, name) VALUES ($1,'api','API') RETURNING id::text`, orgID).Scan(&projID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projID
}

// assertDistinctRefs reads every incident's ref and asserts each is set, well formed and unique.
func assertDistinctRefs(t *testing.T, ctx context.Context, db *sql.DB, want int) {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT ref FROM incidents`)
	if err != nil {
		t.Fatalf("read refs: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var ref sql.NullString
		if err := rows.Scan(&ref); err != nil {
			t.Fatal(err)
		}
		if !ref.Valid || !incidentRefRe.MatchString(ref.String) {
			t.Fatalf("incident ref %+v is not eight Crockford base32 characters", ref)
		}
		if seen[ref.String] {
			t.Fatalf("ref %s handed out twice", ref.String)
		}
		seen[ref.String] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != want {
		t.Fatalf("%d refs, want %d", len(seen), want)
	}
}

// The upgrade over a database an older release wrote: every existing incident gets a distinct ref,
// an INSERT that does not name the column (every insert of an older binary) still succeeds, and the
// Down returns the 00119 shape.
func TestPhoneAlertsMigrationBackfillsRefsAndGoesDown(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	db, _, cleanup := probeDatabaseAt(t, st, ctx, "phoneref", 119)
	defer cleanup()

	projID := seedProbeProject(t, ctx, db)
	const existing = 300
	if _, err := db.ExecContext(ctx, `
		INSERT INTO incidents (project_id, title, status, impact, source)
		SELECT $1, 'old ' || g, 'investigating', 'major', 'manual' FROM generate_series(1, $2) g`,
		projID, existing); err != nil {
		t.Fatalf("seed old incidents: %v", err)
	}

	if err := goose.UpToContext(ctx, db, "migrations", 120); err != nil {
		t.Fatalf("up to 120: %v", err)
	}
	assertDistinctRefs(t, ctx, db, existing)

	// The older binary's insert, which does not know the column.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO incidents (project_id, title, status, impact, source) VALUES ($1,'new','investigating','minor','manual')`,
		projID); err != nil {
		t.Fatalf("an insert that does not name ref failed after the upgrade: %v", err)
	}
	assertDistinctRefs(t, ctx, db, existing+1)

	var nullable, def string
	if err := db.QueryRowContext(ctx, `
		SELECT is_nullable, COALESCE(column_default, '') FROM information_schema.columns
		 WHERE table_name = 'incidents' AND column_name = 'ref'`).Scan(&nullable, &def); err != nil {
		t.Fatal(err)
	}
	if nullable != "NO" || !strings.Contains(def, "cerbix_new_incident_ref()") {
		t.Fatalf("incidents.ref nullable=%s default=%q, want NOT NULL DEFAULT cerbix_new_incident_ref()", nullable, def)
	}
	if _, err := db.ExecContext(ctx, `UPDATE incidents SET ref = (SELECT ref FROM incidents LIMIT 1)`); err == nil {
		t.Fatal("two incidents were given the same ref: the unique index is missing")
	}

	// The Down refuses while a phone channel exists, rather than deleting it.
	var chID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO notification_channels (project_id, type, name, recipient, number_enc, number_hash, number_masked)
		VALUES ($1, 'sms', 'pager', 'number', 'enc', 'hash', '+1•••••0147') RETURNING id::text`, projID).Scan(&chID); err != nil {
		t.Fatalf("sms channel: %v", err)
	}
	if err := goose.DownToContext(ctx, db, "migrations", 119); err == nil {
		t.Fatal("the Down ran while an sms channel exists")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM notification_channels WHERE id = $1`, chID); err != nil {
		t.Fatal(err)
	}

	if err := goose.DownToContext(ctx, db, "migrations", 119); err != nil {
		t.Fatalf("down to 119: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM information_schema.columns
		         WHERE (table_name = 'incidents' AND column_name = 'ref')
		            OR (table_name = 'instance_settings' AND column_name LIKE 'telephony%')
		            OR (table_name = 'notification_channels' AND column_name IN
		                ('recipient','recipient_user_id','number_enc','number_hash','number_masked',
		                 'number_verified_at','disabled_reason')))
		     + (SELECT count(*) FROM information_schema.tables
		         WHERE table_name IN ('user_phones','phone_verifications','phone_deliveries'))
		     + (SELECT count(*) FROM pg_proc WHERE proname LIKE 'cerbix_%incident_ref%')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d part-(a) objects survived the Down", n)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO notification_channels (project_id, type, name) VALUES ($1, 'voice', 'v')`, projID); err == nil {
		t.Fatal("the Down left 'voice' in the channel type check")
	}
	if err := goose.UpToContext(ctx, db, "migrations", 120); err != nil {
		t.Fatalf("up again: %v", err)
	}
	assertDistinctRefs(t, ctx, db, existing+1)
}

// A fresh database (no incidents to backfill) takes the migration too.
func TestPhoneAlertsMigrationOnAnEmptyDatabase(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	db, _, cleanup := probeDatabaseAt(t, st, ctx, "phonefresh", 120)
	defer cleanup()
	projID := seedProbeProject(t, ctx, db)
	for i := 0; i < 3; i++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO incidents (project_id, title, status, impact, source) VALUES ($1,'x','investigating','minor','manual')`,
			projID); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	assertDistinctRefs(t, ctx, db, 3)
}

// The default draws until it finds a value not in use: with the candidate stream forced to hand out
// two refs already taken, the third candidate is returned, and an insert gets it.
func TestTheRefDefaultSkipsRefsInUse(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	db, _, cleanup := probeDatabaseAt(t, st, ctx, "phonerefloop", 120)
	defer cleanup()
	projID := seedProbeProject(t, ctx, db)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // the forced stream never commits
	for _, ref := range []string{"AAAAAAAA", "BBBBBBBB"} {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO incidents (project_id, title, status, impact, source, ref)
			VALUES ($1, $2, 'investigating', 'minor', 'manual', $2)`, projID, ref); err != nil {
			t.Fatalf("seed %s: %v", ref, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE SEQUENCE forced_ref_seq;
		CREATE OR REPLACE FUNCTION cerbix_incident_ref_candidate() RETURNS text LANGUAGE sql AS
		$$ SELECT (ARRAY['AAAAAAAA','BBBBBBBB','CCCCCCCC','DDDDDDDD'])[nextval('forced_ref_seq')] $$`); err != nil {
		t.Fatalf("force the candidate stream: %v", err)
	}
	var got string
	if err := tx.QueryRowContext(ctx, `SELECT cerbix_new_incident_ref()`).Scan(&got); err != nil {
		t.Fatalf("draw: %v", err)
	}
	if got != "CCCCCCCC" {
		t.Fatalf("drew %q, want CCCCCCCC (the first candidate not in use)", got)
	}
	// The stream now stands at DDDDDDDD; an insert that does not name ref takes it.
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO incidents (project_id, title, status, impact, source)
		VALUES ($1, 'x', 'investigating', 'minor', 'manual') RETURNING ref`, projID).Scan(&got); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got != "DDDDDDDD" {
		t.Fatalf("insert got ref %q, want DDDDDDDD", got)
	}
}

// Every incident writer that does not name the column gets a ref: both creating doors, the service
// open inside the evaluator's transaction, and many raw inserts, all distinct and well formed.
func TestEveryIncidentWriterGetsARef(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	manual, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
		ProjectID: f.projectID, Title: "manual", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMinor, Source: domain.SourceManual,
	}, domain.ImpactBasisManual, "opened", "alice", fixtureActor)
	if err != nil {
		t.Fatalf("principal door: %v", err)
	}
	auto, err := st.CreateIncidentBySystem(ctx, domain.Incident{
		ProjectID: f.projectID, MonitorID: f.monitorID, Title: "checkout-http is down",
		Status: domain.IncidentInvestigating, Source: domain.SourceAuto,
	}, "monitor reported down", "system")
	if err != nil {
		t.Fatalf("system door: %v", err)
	}
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	svc, created, err := st.OpenServiceIncidentTx(ctx, tx, f.serviceID, f.projectID, "checkout down",
		domain.ServiceAlertDown, domain.DefaultImpactMapping(), 2, governingRevision(t, st, ctx, f.serviceID))
	if err != nil || !created {
		_ = tx.Rollback(ctx)
		t.Fatalf("service open: created=%v err=%v", created, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO incidents (project_id, title, status, impact, source)
		SELECT $1, 'bulk', 'investigating', 'minor', 'manual' FROM generate_series(1, 500)`, f.projectID); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}

	refs, err := st.IncidentRefs(ctx, []string{manual.ID, auto.ID, svc.ID})
	if err != nil {
		t.Fatalf("IncidentRefs: %v", err)
	}
	if len(refs) != 3 {
		t.Fatalf("IncidentRefs = %v, want three", refs)
	}
	for id, ref := range refs {
		one, err := st.IncidentRef(ctx, id)
		if err != nil || one != ref {
			t.Fatalf("IncidentRef(%s) = %q, %v; IncidentRefs said %q", id, one, err, ref)
		}
	}
	var total, distinct, malformed int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*), count(DISTINCT ref), count(*) FILTER (WHERE ref !~ '^[0-9A-HJKMNP-TV-Z]{8}$')
		  FROM incidents`).Scan(&total, &distinct, &malformed); err != nil {
		t.Fatal(err)
	}
	if total != 503 || distinct != total || malformed != 0 {
		t.Fatalf("incidents %d, distinct refs %d, malformed %d", total, distinct, malformed)
	}
}

func TestIncidentByRef(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	inc, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
		ProjectID: f.projectID, Title: "manual", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMinor, Source: domain.SourceManual,
	}, domain.ImpactBasisManual, "opened", "alice", fixtureActor)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := st.IncidentRef(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.IncidentByRef(ctx, ref)
	if err != nil || got.ID != inc.ID || got.ProjectID != f.projectID {
		t.Fatalf("IncidentByRef(%s) = %+v, %v; want %s", ref, got, err, inc.ID)
	}
	// A link typed by hand: lower case and the Crockford aliases resolve to the same incident.
	typed := strings.NewReplacer("0", "o", "1", "l").Replace(strings.ToLower(ref))
	if got, err := st.IncidentByRef(ctx, typed); err != nil || got.ID != inc.ID {
		t.Fatalf("IncidentByRef(%q) = %+v, %v; want %s", typed, got, err, inc.ID)
	}
	for _, miss := range []string{"ZZZZZZZZ", "", "short", "UUUUUUUU", "AAAAAAAAA"} {
		if miss == ref {
			continue
		}
		if _, err := st.IncidentByRef(ctx, miss); !errors.Is(err, ErrNotFound) {
			t.Fatalf("IncidentByRef(%q) err = %v, want ErrNotFound", miss, err)
		}
	}
	if _, err := st.IncidentRef(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("IncidentRef(unknown) err = %v, want ErrNotFound", err)
	}
	if m, err := st.IncidentRefs(ctx, nil); err != nil || len(m) != 0 {
		t.Fatalf("IncidentRefs(nil) = %v, %v", m, err)
	}
	if m, err := st.IncidentRefs(ctx, []string{inc.ID, "00000000-0000-0000-0000-000000000000"}); err != nil ||
		len(m) != 1 || m[inc.ID] != ref {
		t.Fatalf("IncidentRefs with an unknown id = %v, %v", m, err)
	}
}

// pgCheck asserts that err is a CHECK violation (23514) of the named constraint, or of any CHECK
// when name is empty.
func pgCheck(t *testing.T, what string, err error, code, name string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code || (name != "" && pgErr.ConstraintName != name) {
		t.Fatalf("%s: err = %v, want %s on %q", what, err, code, name)
	}
}

func TestPhoneChannelColumnsAreGuarded(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	projectID, _, _ := seedService(t, st, ctx)
	var userID string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO users (email, display_name) VALUES ('ops@example.com','Ops') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	insert := func(typ string, cols map[string]any) error {
		names := []string{"project_id", "type", "name"}
		args := []any{projectID, typ, "ch"}
		for k, v := range cols {
			names = append(names, k)
			args = append(args, v)
		}
		ph := make([]string, len(args))
		for i := range ph {
			ph[i] = fmt.Sprintf("$%d", i+1)
		}
		_, err := st.pool.Exec(ctx, `INSERT INTO notification_channels (`+strings.Join(names, ",")+`) VALUES (`+
			strings.Join(ph, ",")+`)`, args...)
		return err
	}
	number := map[string]any{"recipient": "number", "number_enc": "enc", "number_hash": "h1", "number_masked": "+1•••••0147"}
	user := map[string]any{"recipient": "user", "recipient_user_id": userID}

	// The type check accepts the two new types, and still refuses an unknown one.
	for _, typ := range []string{"sms", "voice"} {
		if err := insert(typ, user); err != nil {
			t.Fatalf("%s user-mode channel refused: %v", typ, err)
		}
		if err := insert(typ, number); err != nil {
			t.Fatalf("%s number-mode channel refused: %v", typ, err)
		}
	}
	pgCheck(t, "type pager", insert("pager", nil), "23514", "notification_channels_type_chk")

	// The four existing types carry no phone column.
	for col, v := range map[string]any{
		"recipient": "user", "recipient_user_id": userID, "number_enc": "e", "number_hash": "h",
		"number_masked": "m", "number_verified_at": "2026-11-17T00:00:00Z", "disabled_reason": "recipient_deleted",
	} {
		pgCheck(t, "slack with "+col, insert("slack", map[string]any{col: v}), "23514", "")
	}
	if err := insert("slack", nil); err != nil {
		t.Fatalf("a plain slack channel is refused: %v", err)
	}

	// A phone channel states its recipient, and the mode decides which columns may be set.
	pgCheck(t, "sms without recipient", insert("sms", nil), "23514", "")
	pgCheck(t, "sms with an unknown recipient", insert("sms", map[string]any{"recipient": "team"}), "23514", "")
	pgCheck(t, "number mode without a number", insert("voice", map[string]any{"recipient": "number"}), "23514", "")
	pgCheck(t, "number mode without a mask", insert("sms", map[string]any{
		"recipient": "number", "number_enc": "e", "number_hash": "h"}), "23514", "")
	pgCheck(t, "number mode with a user", insert("sms", map[string]any{
		"recipient": "number", "number_enc": "e", "number_hash": "h", "number_masked": "m", "recipient_user_id": userID}), "23514", "")
	pgCheck(t, "user mode with a number", insert("sms", map[string]any{
		"recipient": "user", "recipient_user_id": userID, "number_hash": "h"}), "23514", "")
	pgCheck(t, "user mode with a verification time", insert("sms", map[string]any{
		"recipient": "user", "recipient_user_id": userID, "number_verified_at": "2026-11-17T00:00:00Z"}), "23514", "")
	pgCheck(t, "unknown disabled_reason", insert("sms", map[string]any{
		"recipient": "user", "recipient_user_id": userID, "disabled_reason": "bored"}), "23514", "")
	if err := insert("voice", map[string]any{"recipient": "number", "number_enc": "e", "number_hash": "h2",
		"number_masked": "m", "disabled_reason": "number_unverified"}); err != nil {
		t.Fatalf("a number channel disabled as unverified is refused: %v", err)
	}

	// Deleting the user keeps the user-mode channel, with no recipient (ON DELETE SET NULL).
	if _, err := st.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatalf("delete a recipient user: %v", err)
	}
	var orphaned int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM notification_channels
		WHERE recipient = 'user' AND recipient_user_id IS NULL`).Scan(&orphaned); err != nil {
		t.Fatal(err)
	}
	if orphaned != 2 {
		t.Fatalf("%d user-mode channels outlived their user, want 2", orphaned)
	}
}

func TestPhoneTablesAreGuarded(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	projectID, monitorID, _ := seedService(t, st, ctx)
	var userID, chID string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO users (email, display_name) VALUES ('ops@example.com','Ops') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `INSERT INTO notification_channels (project_id, type, name, recipient, recipient_user_id)
		VALUES ($1,'sms','ops','user',$2) RETURNING id`, projectID, userID).Scan(&chID); err != nil {
		t.Fatal(err)
	}

	// instance_settings: the typed provider column is a closed set.
	if _, err := st.pool.Exec(ctx, `INSERT INTO instance_settings (id) VALUES (true) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var enabled bool
	var provider *string
	var tel string
	if err := st.pool.QueryRow(ctx,
		`SELECT telephony_enabled, telephony_provider, telephony::text FROM instance_settings`).Scan(&enabled, &provider, &tel); err != nil {
		t.Fatal(err)
	}
	if enabled || provider != nil || tel != "{}" {
		t.Fatalf("telephony defaults = %v %v %s", enabled, provider, tel)
	}
	for _, p := range []string{"twilio", "generic_http"} {
		if _, err := st.pool.Exec(ctx, `UPDATE instance_settings SET telephony_provider = $1`, p); err != nil {
			t.Fatalf("provider %s refused: %v", p, err)
		}
	}
	_, err := st.pool.Exec(ctx, `UPDATE instance_settings SET telephony_provider = 'pigeon'`)
	pgCheck(t, "provider pigeon", err, "23514", "")

	// user_phones: one number belongs to one user; the row goes with its user.
	// §8 lists quiet_hours on the (a) row: it exists, nullable jsonb, unused until FR-041c.
	var qhType, qhNullable string
	if err := st.pool.QueryRow(ctx, `SELECT data_type, is_nullable FROM information_schema.columns
		WHERE table_name = 'user_phones' AND column_name = 'quiet_hours'`).Scan(&qhType, &qhNullable); err != nil {
		t.Fatalf("user_phones.quiet_hours missing: %v", err)
	}
	if qhType != "jsonb" || qhNullable != "YES" {
		t.Fatalf("user_phones.quiet_hours = %s nullable=%s, want jsonb YES", qhType, qhNullable)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO user_phones (user_id, number_enc, number_hash, number_masked)
		VALUES ($1,'e','h1','m')`, userID); err != nil {
		t.Fatalf("user phone: %v", err)
	}
	var otherID string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO users (email, display_name) VALUES ('dev@example.com','Dev') RETURNING id`).Scan(&otherID); err != nil {
		t.Fatal(err)
	}
	_, err = st.pool.Exec(ctx, `INSERT INTO user_phones (user_id, number_enc, number_hash, number_masked)
		VALUES ($1,'e','h1','m')`, otherID)
	pgCheck(t, "a second user with the same number", err, "23505", "")

	// phone_verifications: the subject kind is a closed set.
	if _, err := st.pool.Exec(ctx, `INSERT INTO phone_verifications (subject_kind, subject_id, requested_by, number_hash, code_hash, expires_at)
		VALUES ('user',$1,'requester','h1','c', now() + interval '10 minutes'), ('channel',$2,'requester','h1','c', now())`, userID, chID); err != nil {
		t.Fatalf("verifications: %v", err)
	}
	_, err = st.pool.Exec(ctx, `INSERT INTO phone_verifications (subject_kind, subject_id, requested_by, number_hash, code_hash, expires_at)
		VALUES ('team',$1,'requester','h1','c', now())`, userID)
	pgCheck(t, "subject kind team", err, "23514", "")

	// phone_deliveries: every part-(a) reason is accepted, nothing else is.
	reasons := []string{
		domain.PhoneReasonSending, domain.PhoneReasonSent, domain.PhoneReasonProviderUnreachable,
		domain.PhoneReasonProviderRejected, domain.PhoneReasonOutcomeUnknown, domain.PhoneReasonNotDeliverable,
		domain.PhoneReasonVoiceUnsupportedByProvider, domain.PhoneReasonTelephonyDisabled, domain.PhoneReasonStormLimited,
	}
	deliver := func(kind, reason string, eventID any, code any) error {
		_, err := st.pool.Exec(ctx, `INSERT INTO phone_deliveries
			(project_id, channel_id, outbox_event_id, monitor_id, kind, alert_kind, reason, provider_code)
			VALUES ($1,$2,$3,$4,$5,'monitor_down',$6,$7)`, projectID, chID, eventID, monitorID, kind, reason, code)
		return err
	}
	for _, r := range reasons {
		if !domain.ValidPhoneReason(r) {
			t.Fatalf("%s is not a domain reason", r)
		}
		if err := deliver("sms", r, nil, nil); err != nil {
			t.Fatalf("reason %s refused: %v", r, err)
		}
	}
	pgCheck(t, "reason acked (part b)", deliver("sms", "acked", nil, nil), "23514", "")
	pgCheck(t, "kind email", deliver("email", "sent", nil, nil), "23514", "")
	pgCheck(t, "a provider code that is response text", deliver("sms", "provider_rejected", nil, "The number is blocked."), "23514", "")
	if err := deliver("voice", "provider_rejected", nil, "21610"); err != nil {
		t.Fatalf("a provider code: %v", err)
	}
	var notes []string
	var segments, attempts int
	if err := st.pool.QueryRow(ctx, `SELECT notes, segments, attempts FROM phone_deliveries LIMIT 1`).Scan(&notes, &segments, &attempts); err != nil {
		t.Fatal(err)
	}
	if notes == nil || len(notes) != 0 || segments != 0 || attempts != 1 {
		t.Fatalf("delivery defaults notes=%v segments=%d attempts=%d", notes, segments, attempts)
	}

	// One row per (event, channel): a redelivered event cannot phone twice (D33).
	const ev = "11111111-1111-1111-1111-111111111111"
	if err := deliver("sms", "sending", ev, nil); err != nil {
		t.Fatal(err)
	}
	pgCheck(t, "the same event and channel twice", deliver("sms", "sent", ev, nil), "23505", "phone_deliveries_event_channel_key")

	// The history goes with its channel; the user row goes with its user.
	if _, err := st.pool.Exec(ctx, `DELETE FROM notification_channels WHERE id = $1`, chID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := st.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM phone_deliveries) + (SELECT count(*) FROM user_phones)`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d delivery or phone rows outlived their channel or user", left)
	}
}
