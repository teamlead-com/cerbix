package store

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Migration 00093 (func-reliability-gate §5, D10). These tests assert the constraints the
// migration exists FOR — the id/row binding, the missing default partition, the per-child
// unique, the presence table, the payload bound, the policy and override CHECKs, the
// bootstrap's registry — not that four tables exist. They are mode-independent: the ledger is
// a plain RANGE-partitioned table whether or not timescaledb is installed, and the suite runs
// against both.

// gateUUIDv7 builds a UUIDv7 whose 48-bit timestamp is ms, with random tail bits, the way the
// decision writer will (74 random bits after version and variant).
func gateUUIDv7(t *testing.T, ms int64) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(ms))
	copy(b[0:6], ts[2:8])
	b[6] = 0x70 | (b[6] & 0x0f) // version 7
	b[8] = 0x80 | (b[8] & 0x3f) // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func gateMs(ts time.Time) int64 { return ts.UnixMilli() }

// gateTodayUTC returns the UTC day the ledger bootstrap ran on — anchored on the registry, not
// on the wall clock. The bootstrap is a one-shot migration: a test that anchored on now() passed
// until UTC midnight after the test database was migrated and failed from then on (Agent B,
// iter-0163), because "today + 7" had moved past the eight days the migration registered.
func gateTodayUTC(t *testing.T, st *Store, ctx context.Context) time.Time {
	t.Helper()
	var d time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT (min(created_at) AT TIME ZONE 'UTC')::date FROM service_gate_decision_partitions`).Scan(&d); err != nil {
		t.Fatalf("bootstrap day: %v", err)
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
}

func pgCode(err error) (string, string) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code, pe.ConstraintName
	}
	return "", ""
}

// insertDecision writes a minimal, valid decision row; the caller varies one thing.
func insertDecision(st *Store, ctx context.Context, id, projectID string, serviceID *string, state string, action *string, at time.Time, evidence string) error {
	var policyRev *int64
	var window, snapshot *string
	if state != "NOT_CONFIGURED" {
		rev := int64(1)
		w, s := "30d", `{"clauses":{}}`
		policyRev, window, snapshot = &rev, &w, &s
	}
	_, err := st.pool.Exec(ctx, `
		INSERT INTO service_gate_decisions
		    (id, project_id, service_id, service_slug, service_name, state, action, reasons, evidence,
		     policy_revision, window_name, policy_snapshot, evaluated_at)
		VALUES ($1, $2, $3, 'checkout', 'Checkout', $4, $5, '[]', $6, $7, $8, $9, $10)`,
		id, projectID, serviceID, state, action, evidence, policyRev, window, snapshot, at)
	return err
}

func TestGateUUIDMsReadsTheFirst48BitsUnsigned(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	for _, ms := range []int64{0, 1, 1713997392845, 1<<48 - 1} {
		u := gateUUIDv7(t, ms)
		var got int64
		if err := st.pool.QueryRow(ctx, `SELECT gate_uuid_ms($1::uuid)`, u).Scan(&got); err != nil {
			t.Fatalf("gate_uuid_ms(%s): %v", u, err)
		}
		if got != ms {
			t.Errorf("gate_uuid_ms(%s) = %d, want %d", u, got, ms)
		}
	}
	// IMMUTABLE, so it may sit in a CHECK and be pruned on.
	var volatility string
	if err := st.pool.QueryRow(ctx, `SELECT provolatile FROM pg_proc WHERE proname = 'gate_uuid_ms'`).Scan(&volatility); err != nil {
		t.Fatalf("pg_proc: %v", err)
	}
	if volatility != "i" {
		t.Fatalf("gate_uuid_ms volatility = %q, want i (IMMUTABLE)", volatility)
	}
}

// §7 Partitions: the migration leaves [today, today + 7 d] attached AND registered — eight
// UTC days, each marked, each with exactly four indexes, and no DEFAULT partition anywhere.
func TestGateLedgerBootstrapRegistersAndAttachesEightDays(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	today := gateTodayUTC(t, st, ctx)

	rows, err := st.pool.Query(ctx, `
		SELECT day, relname, owner_token::text, relid, state, attached_at IS NOT NULL,
		       obj_description(relid, 'pg_class'),
		       to_regclass(relname)::oid,
		       (SELECT count(*) FROM pg_index WHERE indrelid = relid),
		       (SELECT count(*) FROM pg_index WHERE indrelid = relid AND indisunique AND NOT indisprimary
		           AND indnatts = 1 AND pg_get_indexdef(indexrelid) LIKE '%(id)'),
		       EXISTS (SELECT 1 FROM pg_inherits WHERE inhrelid = relid AND inhparent = 'service_gate_decisions'::regclass)
		  FROM service_gate_decision_partitions ORDER BY day`)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var (
			day                          time.Time
			relname, token, state        string
			relid, catalogOID            uint32
			attached, isChild            bool
			comment                      *string
			indexCount, localUniqueCount int
		)
		if err := rows.Scan(&day, &relname, &token, &relid, &state, &attached, &comment, &catalogOID, &indexCount, &localUniqueCount, &isChild); err != nil {
			t.Fatalf("scan: %v", err)
		}
		dayKey := day.Format("2006-01-02")
		seen[dayKey] = true
		if want := "service_gate_decisions_p" + day.Format("20060102"); relname != want {
			t.Errorf("%s: relname %q, want %q", dayKey, relname, want)
		}
		if state != "attached" || !attached {
			t.Errorf("%s: state %q attached_at set=%v, want attached with a timestamp", dayKey, state, attached)
		}
		if relid != catalogOID {
			t.Errorf("%s: registry relid %d, catalog says %d", dayKey, relid, catalogOID)
		}
		if comment == nil || *comment != "cerbix:gate-ledger:"+token {
			t.Errorf("%s: marker %v, want cerbix:gate-ledger:%s", dayKey, comment, token)
		}
		if !isChild {
			t.Errorf("%s: %s is not a partition of service_gate_decisions", dayKey, relname)
		}
		if indexCount != 4 {
			t.Errorf("%s: %d indexes on the child, want exactly four (PK, local unique id, two listing paths)", dayKey, indexCount)
		}
		if localUniqueCount != 1 {
			t.Errorf("%s: %d local UNIQUE (id) indexes, want 1", dayKey, localUniqueCount)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= 7; i++ {
		d := today.AddDate(0, 0, i).Format("2006-01-02")
		if !seen[d] {
			t.Errorf("day %s (today + %d) is not registered", d, i)
		}
	}
	if len(seen) < 8 {
		t.Fatalf("%d days registered, want at least the eight of [today, today+7]", len(seen))
	}

	// No DEFAULT partition, and the parent carries only its three partitioned indexes — no
	// (id) index at the parent, which day pruning plus the local unique make redundant.
	var defaults, parentIdx int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		 WHERE i.inhparent = 'service_gate_decisions'::regclass
		   AND pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT'`).Scan(&defaults); err != nil {
		t.Fatal(err)
	}
	if defaults != 0 {
		t.Fatalf("the ledger has a DEFAULT partition; a decision the ledger cannot hold must FAIL, not land somewhere")
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM pg_index WHERE indrelid = 'service_gate_decisions'::regclass`).Scan(&parentIdx); err != nil {
		t.Fatal(err)
	}
	if parentIdx != 3 {
		t.Fatalf("parent has %d indexes, want 3 (PK + two listing paths, no parent (id) index)", parentIdx)
	}
	// The bootstrap did not attach via CREATE … PARTITION OF (which takes ACCESS EXCLUSIVE):
	// the day CHECK it added standalone is still on every child.
	var dayChecks int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_constraint con JOIN service_gate_decision_partitions p ON p.relid = con.conrelid
		 WHERE con.contype = 'c' AND con.conname = p.relname || '_day_chk'`).Scan(&dayChecks); err != nil {
		t.Fatal(err)
	}
	if dayChecks != len(seen) {
		t.Fatalf("%d children carry their standalone day CHECK, want %d", dayChecks, len(seen))
	}
}

// Identity on a partitioned ledger (§5): a row lands in its day by evaluated_at (the last
// second of a UTC day included), the id's millisecond must equal evaluated_at's, one id
// cannot appear twice within a day, and a day with no partition refuses the row.
func TestGateDecisionIdentityAndPartitioning(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	proj, _, svc := seedService(t, st, ctx)
	today := gateTodayUTC(t, st, ctx)
	allow := "ALLOW"

	// Last millisecond of today lands in today's partition; first of tomorrow in tomorrow's.
	lastOfToday := today.Add(24*time.Hour - time.Millisecond)
	id := gateUUIDv7(t, gateMs(lastOfToday))
	if err := insertDecision(st, ctx, id, proj, &svc, "ALLOW", &allow, lastOfToday, `{"k":1}`); err != nil {
		t.Fatalf("insert at the last ms of today: %v", err)
	}
	var partition string
	if err := st.pool.QueryRow(ctx, `SELECT tableoid::regclass::text FROM service_gate_decisions WHERE id = $1`, id).Scan(&partition); err != nil {
		t.Fatal(err)
	}
	if want := "service_gate_decisions_p" + today.Format("20060102"); partition != want {
		t.Fatalf("a row at 23:59:59.999 landed in %s, want %s", partition, want)
	}
	firstOfTomorrow := today.Add(24 * time.Hour)
	id2 := gateUUIDv7(t, gateMs(firstOfTomorrow))
	if err := insertDecision(st, ctx, id2, proj, &svc, "ALLOW", &allow, firstOfTomorrow, `{"k":1}`); err != nil {
		t.Fatalf("insert at 00:00 tomorrow: %v", err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT tableoid::regclass::text FROM service_gate_decisions WHERE id = $1`, id2).Scan(&partition); err != nil {
		t.Fatal(err)
	}
	if want := "service_gate_decisions_p" + firstOfTomorrow.Format("20060102"); partition != want {
		t.Fatalf("a row at 00:00:00 landed in %s, want %s", partition, want)
	}

	// The id is bound to the row: a planted id one millisecond off is refused by the CHECK.
	at := today.Add(12 * time.Hour)
	planted := gateUUIDv7(t, gateMs(at)+1)
	err := insertDecision(st, ctx, planted, proj, &svc, "ALLOW", &allow, at, `{"k":1}`)
	if code, name := pgCode(err); code != "23514" || !strings.Contains(name, "id_binds_evaluated_at") {
		t.Fatalf("an id whose millisecond differs from evaluated_at was accepted or refused otherwise: code=%s constraint=%s err=%v", code, name, err)
	}
	// And an id whose day differs from its row's — the cross-day duplicate — is the same CHECK.
	err = insertDecision(st, ctx, id, proj, &svc, "ALLOW", &allow, firstOfTomorrow.Add(time.Hour), `{"k":1}`)
	if code, _ := pgCode(err); code != "23514" {
		t.Fatalf("a row carrying yesterday's id into tomorrow's partition was accepted: %v", err)
	}
	// Two rows with one id within a day: same millisecond (so the binding CHECK passes), a
	// different microsecond (so the PK (evaluated_at, id) differs) — only the LOCAL UNIQUE (id)
	// can refuse this, and it does.
	err = insertDecision(st, ctx, id, proj, &svc, "ALLOW", &allow, lastOfToday.Add(time.Microsecond), `{"k":2}`)
	if code, name := pgCode(err); code != "23505" || !strings.HasSuffix(name, "_id_uniq") {
		t.Fatalf("a duplicate id in one day was accepted or refused by something other than the local unique: code=%s constraint=%s err=%v", code, name, err)
	}

	// No DEFAULT partition: a day nobody created refuses the row with 23514 "no partition".
	for _, at := range []time.Time{today.Add(-time.Second), today.AddDate(0, 0, 8)} {
		err := insertDecision(st, ctx, gateUUIDv7(t, gateMs(at)), proj, &svc, "ALLOW", &allow, at, `{"k":1}`)
		if code, _ := pgCode(err); code != "23514" || !strings.Contains(err.Error(), "no partition") {
			t.Fatalf("a row for %s (no partition) was accepted or failed otherwise: %v", at, err)
		}
	}
}

// The D7 presence table and the §5a byte bound as facts of the DATA.
func TestGateDecisionPresenceAndPayloadChecks(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	proj, _, svc := seedService(t, st, ctx)
	at := gateTodayUTC(t, st, ctx).Add(6 * time.Hour)
	allow := "ALLOW"

	// NOT_CONFIGURED: no action, no policy fields — accepted.
	if err := insertDecision(st, ctx, gateUUIDv7(t, gateMs(at)), proj, &svc, "NOT_CONFIGURED", nil, at, `{}`); err != nil {
		t.Fatalf("a NOT_CONFIGURED row without action was refused: %v", err)
	}
	// NOT_CONFIGURED with an action, and a policy state without one: both refused.
	err := insertDecision(st, ctx, gateUUIDv7(t, gateMs(at)), proj, &svc, "NOT_CONFIGURED", &allow, at, `{}`)
	if code, name := pgCode(err); code != "23514" || !strings.Contains(name, "policy_presence") {
		t.Fatalf("NOT_CONFIGURED with an action accepted: %v", err)
	}
	err = insertDecision(st, ctx, gateUUIDv7(t, gateMs(at)), proj, &svc, "ALLOW", nil, at, `{}`)
	if code, name := pgCode(err); code != "23514" || !strings.Contains(name, "policy_presence") {
		t.Fatalf("ALLOW without an action accepted: %v", err)
	}
	// A state or action outside the enum.
	pass := "PASS"
	if err := insertDecision(st, ctx, gateUUIDv7(t, gateMs(at)), proj, &svc, "ALLOW", &pass, at, `{}`); err == nil {
		t.Fatal("action PASS accepted")
	}
	// A synthetic 5 KiB evidence fails at the CHECK; 4 KiB exactly passes.
	big := `{"pad":"` + strings.Repeat("x", 5*1024) + `"}`
	err = insertDecision(st, ctx, gateUUIDv7(t, gateMs(at)), proj, &svc, "ALLOW", &allow, at, big)
	if code, name := pgCode(err); code != "23514" || !strings.Contains(name, "payload") {
		t.Fatalf("5 KiB evidence accepted or refused otherwise: code=%s constraint=%s err=%v", code, name, err)
	}
	exact := `{"pad":"` + strings.Repeat("x", 4096-len(`{"pad": ""}`)) + `"}`
	if err := insertDecision(st, ctx, gateUUIDv7(t, gateMs(at)), proj, &svc, "ALLOW", &allow, at, exact); err != nil {
		t.Fatalf("evidence at exactly the bound refused: %v", err)
	}
	// reasons must be an array, evidence an object.
	_, err = st.pool.Exec(ctx, `
		INSERT INTO service_gate_decisions (id, project_id, service_id, service_slug, service_name, state, action, reasons, evidence,
		     policy_revision, window_name, policy_snapshot, evaluated_at)
		VALUES ($1, $2, $3, 's', 'n', 'ALLOW', 'ALLOW', '{}', '{}', 1, '30d', '{}', $4)`,
		gateUUIDv7(t, gateMs(at)), proj, svc, at)
	if code, name := pgCode(err); code != "23514" || !strings.Contains(name, "reasons") {
		t.Fatalf("an object in reasons accepted: %v", err)
	}
}

// D10/§7 Ledger: the row survives service deletion with service_id NULL and the tenant key
// intact (column-list SET NULL), and the tenant-composite FK refuses a cross-project pair.
func TestGateDecisionSurvivesItsServiceAndRefusesACrossTenantPair(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	proj, _, svc := seedService(t, st, ctx)
	at := gateTodayUTC(t, st, ctx).Add(3 * time.Hour)
	allow := "ALLOW"

	// Another project of the same org: (svc, otherProj) is not a real service.
	var otherProj string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO projects (org_id, slug, name) SELECT org_id, 'other', 'Other' FROM projects WHERE id = $1 RETURNING id`,
		proj).Scan(&otherProj); err != nil {
		t.Fatalf("other project: %v", err)
	}
	err := insertDecision(st, ctx, gateUUIDv7(t, gateMs(at)), otherProj, &svc, "ALLOW", &allow, at, `{}`)
	if code, _ := pgCode(err); code != "23503" {
		t.Fatalf("a decision naming project B's id with project A's service was accepted: %v", err)
	}

	id := gateUUIDv7(t, gateMs(at))
	if err := insertDecision(st, ctx, id, proj, &svc, "ALLOW", &allow, at, `{}`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM services WHERE id = $1`, svc); err != nil {
		t.Fatalf("delete service: %v", err)
	}
	var gotProj string
	var gotSvc *string
	var slug string
	if err := st.pool.QueryRow(ctx, `SELECT project_id, service_id, service_slug FROM service_gate_decisions WHERE id = $1`, id).Scan(&gotProj, &gotSvc, &slug); err != nil {
		t.Fatalf("the decision did not survive its service: %v", err)
	}
	if gotSvc != nil || gotProj != proj || slug != "checkout" {
		t.Fatalf("after service delete: service_id=%v project_id=%s slug=%s; want NULL, the same project, the snapshot slug", gotSvc, gotProj, slug)
	}
	// And a project delete cascades the ledger with it.
	if _, err := st.pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, proj); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_gate_decisions WHERE project_id = $1`, proj).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d rows survived their project (err=%v)", n, err)
	}
}

// The policy's CHECKs mirror the domain's bounds: the seal-lag floor is domain.MinSealLag and
// the ceiling domain.MaxSealLag, in seconds, whole minutes only; the rest per §5.
func TestGatePolicyChecksAgreeWithTheDomain(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	proj, _, svc := seedService(t, st, ctx)
	minLag := int(domain.MinSealLag / time.Second)
	maxLag := int(domain.MaxSealLag / time.Second)

	insert := func(pct, lag int, behavior string, rev int64) error {
		_, err := st.pool.Exec(ctx, `
			INSERT INTO service_gate_policies
			    (service_id, project_id, window_name, schema_version, clauses, budget_consumed_percent,
			     max_seal_lag_seconds, unknown_behavior, revision, updated_by)
			VALUES ($1, $2, '30d', 1, '{"budget_exhausted":"block"}', $3, $4, $5, $6, 'tester')`,
			svc, proj, pct, lag, behavior, rev)
		if err == nil {
			_, _ = st.pool.Exec(ctx, `DELETE FROM service_gate_policies WHERE service_id = $1`, svc)
		}
		return err
	}
	for _, tc := range []struct {
		name       string
		pct, lag   int
		behavior   string
		rev        int64
		constraint string // "" = accepted
	}{
		{"floor accepted", 90, minLag, "warn", 1, ""},
		{"ceiling accepted", 90, maxLag, "block", 1, ""},
		{"240 refused (one minute under the floor)", 90, minLag - 60, "warn", 1, "max_seal_lag"},
		{"one over the ceiling", 90, maxLag + 60, "warn", 1, "max_seal_lag"},
		{"not whole minutes", 90, minLag + 30, "warn", 1, "max_seal_lag"},
		{"threshold 0", 0, 900, "warn", 1, "budget_consumed_percent"},
		{"threshold 101", 101, 900, "warn", 1, "budget_consumed_percent"},
		{"unknown_behavior allow", 90, 900, "allow", 1, "unknown_behavior"},
		{"revision 0", 90, 900, "warn", 0, "revision"},
	} {
		err := insert(tc.pct, tc.lag, tc.behavior, tc.rev)
		if tc.constraint == "" {
			if err != nil {
				t.Errorf("%s: refused: %v", tc.name, err)
			}
			continue
		}
		if code, name := pgCode(err); code != "23514" || !strings.Contains(name, tc.constraint) {
			t.Errorf("%s: want CHECK %s, got code=%s constraint=%s err=%v", tc.name, tc.constraint, code, name, err)
		}
	}
	// Tenant-composite: the policy cannot name a service of another project.
	var otherProj string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO projects (org_id, slug, name) SELECT org_id, 'other', 'Other' FROM projects WHERE id = $1 RETURNING id`,
		proj).Scan(&otherProj); err != nil {
		t.Fatal(err)
	}
	_, err := st.pool.Exec(ctx, `
		INSERT INTO service_gate_policies (service_id, project_id, window_name, schema_version, clauses,
		    budget_consumed_percent, max_seal_lag_seconds, unknown_behavior, revision)
		VALUES ($1, $2, '30d', 1, '{}', 90, 900, 'warn', 1)`, svc, otherProj)
	if code, _ := pgCode(err); code != "23503" {
		t.Fatalf("a cross-tenant policy pair was accepted: %v", err)
	}
	// The tombstone is allowed: a deleted policy keeps its row and its generation.
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_gate_policies (service_id, project_id, window_name, schema_version, clauses,
		    budget_consumed_percent, max_seal_lag_seconds, unknown_behavior, revision, deleted_at)
		VALUES ($1, $2, '30d', 1, '{}', 90, 900, 'warn', 2, now())`, svc, proj); err != nil {
		t.Fatalf("a tombstoned policy row was refused: %v", err)
	}
	// And it goes with its service.
	if _, err := st.pool.Exec(ctx, `DELETE FROM services WHERE id = $1`, svc); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_gate_policies WHERE service_id = $1`, svc).Scan(&n); err != nil || n != 0 {
		t.Fatalf("policy survived its service: n=%d err=%v", n, err)
	}
}

// The override's CHECKs (D9, D13a): a bounded reason, a closed reason set, the lifecycle pair
// set together, the revoker triple present exactly for `manual`; and the history index.
func TestGateOverrideChecksAndHistoryIndex(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	proj, _, svc := seedService(t, st, ctx)

	type row struct {
		reason        string
		revokedAt     *string // SQL expression or nil
		revokedReason *string
		byUser        *string
		viaToken      *bool
		byLabel       *string
	}
	insert := func(r row) error {
		_, err := st.pool.Exec(ctx, `
			INSERT INTO service_gate_overrides
			    (service_id, project_id, policy_revision, actor_user_id, via_token, actor_label, reason,
			     expires_at, revoked_at, revoked_reason, revoked_by_user_id, revoked_via_token, revoked_by_label)
			VALUES ($1, $2, 1, NULL, true, 'token:ci', $3, now() + interval '1 hour',
			        CASE WHEN $4::text IS NULL THEN NULL ELSE now() END, $4, $5::uuid, $6, $7)`,
			svc, proj, r.reason, r.revokedReason, r.byUser, r.viaToken, r.byLabel)
		return err
	}
	s := func(v string) *string { return &v }
	b := func(v bool) *bool { return &v }
	for _, tc := range []struct {
		name       string
		r          row
		constraint string
	}{
		{"active row", row{reason: "deploying the fix"}, ""},
		{"empty reason", row{reason: ""}, "reason_chk"},
		{"501-char reason", row{reason: strings.Repeat("r", 501)}, "reason_chk"},
		{"500-char reason accepted", row{reason: strings.Repeat("r", 500)}, ""},
		{"unknown closure reason", row{reason: "x", revokedReason: s("timeout")}, "revoked_reason_chk"},
		{"system closure: no attribution", row{reason: "x", revokedReason: s("expired")}, ""},
		{"system closure with a label", row{reason: "x", revokedReason: s("policy_changed"), byLabel: s("ops")}, "revoker_chk"},
		{"manual by a token", row{reason: "x", revokedReason: s("manual"), viaToken: b(true), byLabel: s("token:ci")}, ""},
		{"manual without a label", row{reason: "x", revokedReason: s("manual"), viaToken: b(false)}, "revoker_chk"},
		{"manual without via_token", row{reason: "x", revokedReason: s("manual"), byLabel: s("ops")}, "revoker_chk"},
	} {
		err := insert(tc.r)
		if tc.constraint == "" {
			if err != nil {
				t.Errorf("%s: refused: %v", tc.name, err)
			}
			continue
		}
		if code, name := pgCode(err); code != "23514" || !strings.Contains(name, tc.constraint) {
			t.Errorf("%s: want CHECK %s, got code=%s constraint=%s err=%v", tc.name, tc.constraint, code, name, err)
		}
	}
	// revoked_at without a reason, and a reason without revoked_at: the lifecycle pair.
	_, err := st.pool.Exec(ctx, `
		INSERT INTO service_gate_overrides (service_id, project_id, policy_revision, via_token, actor_label, reason, expires_at, revoked_at)
		VALUES ($1, $2, 1, true, 'token:ci', 'x', now() + interval '1 hour', now())`, svc, proj)
	if code, name := pgCode(err); code != "23514" || !strings.Contains(name, "close_chk") {
		t.Fatalf("revoked_at without a reason accepted: %v", err)
	}
	// The history index: (service_id, created_at DESC, id DESC).
	var def string
	if err := st.pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = 'service_gate_overrides_history_idx'`).Scan(&def); err != nil {
		t.Fatalf("history index: %v", err)
	}
	if !strings.Contains(def, "(service_id, created_at DESC, id DESC)") {
		t.Fatalf("history index is %q, want (service_id, created_at DESC, id DESC)", def)
	}
	// Overrides go with their service.
	if _, err := st.pool.Exec(ctx, `DELETE FROM services WHERE id = $1`, svc); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_gate_overrides WHERE service_id = $1`, svc).Scan(&n); err != nil || n != 0 {
		t.Fatalf("overrides survived their service: n=%d err=%v", n, err)
	}
}

// The bootstrap is idempotent and SAYS what it did: a fresh database migrated through 00093
// reports eight days created; the bootstrap block re-run over that database creates nothing
// and reports every day skipped. The notices are the evidence — sql.Open("pgx") throws them
// away, so the probe database's OnNotice is how they reach a test at all.
func TestGateLedgerBootstrapIsIdempotentAndReportsIt(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	db, notices, cleanup := probeDatabaseAt(t, st, ctx, "gate", 93)
	defer cleanup()

	created := func() []string {
		var out []string
		for _, n := range *notices {
			if strings.HasPrefix(n, "gate ledger: ") && strings.Contains(n, "created and attached") {
				out = append(out, n)
			}
		}
		return out
	}
	skipped := func() int {
		n := 0
		for _, m := range *notices {
			if strings.HasPrefix(m, "gate ledger: ") && strings.Contains(m, "already registered") {
				n++
			}
		}
		return n
	}
	if got := created(); len(got) != 8 {
		t.Fatalf("a fresh database reported %d created days, want 8: %v", len(got), got)
	}
	if skipped() != 0 {
		t.Fatalf("a fresh database reported skipped days")
	}
	var before int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM service_gate_decision_partitions`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	src, err := migrationsFS.ReadFile("migrations/00093_reliability_gate.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	begin := strings.Index(text, "-- gate-ledger-bootstrap:begin")
	end := strings.Index(text, "-- gate-ledger-bootstrap:end")
	if begin < 0 || end < begin {
		t.Fatal("the bootstrap block markers are gone from 00093")
	}
	block := text[begin:end]
	if !strings.Contains(block, "LIKE service_gate_decisions INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING INDEXES") ||
		strings.Contains(block, "PARTITION OF") {
		t.Fatal("the bootstrap must build each day STANDALONE and attach it; CREATE … PARTITION OF takes ACCESS EXCLUSIVE on the parent (D10)")
	}
	*notices = (*notices)[:0]
	if _, err := db.ExecContext(ctx, block); err != nil {
		t.Fatalf("re-running the bootstrap block: %v", err)
	}
	var after int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM service_gate_decision_partitions`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	// Midnight may pass between the two runs; then exactly one new day is legitimate.
	if after != before && after != before+1 {
		t.Fatalf("re-run changed the registry from %d to %d rows", before, after)
	}
	if got := len(created()); got != after-before {
		t.Fatalf("re-run reported %d created days, want %d", got, after-before)
	}
	if got := skipped(); got < 7 {
		t.Fatalf("re-run reported %d skipped days, want at least 7 (%v)", got, *notices)
	}
	// Every registry row still agrees with the catalog after the re-run.
	var disagreeing int
	if err := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT count(*) FROM service_gate_decision_partitions p
		 WHERE p.state <> 'attached'
		    OR to_regclass(p.relname)::oid IS DISTINCT FROM p.relid
		    OR obj_description(p.relid, 'pg_class') IS DISTINCT FROM 'cerbix:gate-ledger:' || p.owner_token::text
		    OR NOT EXISTS (SELECT 1 FROM pg_inherits WHERE inhrelid = p.relid AND inhparent = %s)`,
		"'service_gate_decisions'::regclass")).Scan(&disagreeing); err != nil {
		t.Fatal(err)
	}
	if disagreeing != 0 {
		t.Fatalf("%d registry rows disagree with the catalog after the re-run", disagreeing)
	}
}
