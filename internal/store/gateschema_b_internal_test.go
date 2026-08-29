package store

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
)

// Agent B's pass over migration 00093 (func-reliability-gate §5, §7 Partitions / Ownership /
// Identity and reads). Every test here REACHES the mechanism it names: where a CHECK or an index
// is said to refuse a row, the same row is written again with that CHECK or index dropped inside
// a rolled-back transaction and must then go through — so the refusal is attributable to the one
// constraint and not to something else that happened to fail. The bootstrap assertions are
// anchored on the UTC day the bootstrap RAN (the registry's created_at), not on now(): the
// migration is a one-shot, and the shared cerbix_test database outlives the day it was migrated
// on. Mode-independent; run in both storage modes.

// gateBootstrapDayUTC is the UTC day the 00093 bootstrap ran on this database — the "today" of
// its [today, today + 7 d] window. Reading it from created_at (the bootstrap's now()) rather
// than from now() makes every assertion below hold on any later day.
func gateBootstrapDayUTC(t *testing.T, st *Store, ctx context.Context) time.Time {
	t.Helper()
	var d time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT (min(created_at) AT TIME ZONE 'UTC')::date FROM service_gate_decision_partitions`).Scan(&d); err != nil {
		t.Fatalf("bootstrap day: %v", err)
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
}

func gatePartitionName(day time.Time) string {
	return "service_gate_decisions_p" + day.UTC().Format("20060102")
}

// partitionOf returns the child relation a decision row landed in.
func partitionOf(t *testing.T, st *Store, ctx context.Context, id string) string {
	t.Helper()
	var rel string
	if err := st.pool.QueryRow(ctx, `SELECT tableoid::regclass::text FROM service_gate_decisions WHERE id = $1`, id).Scan(&rel); err != nil {
		t.Fatalf("locate %s: %v", id, err)
	}
	return rel
}

// §7 Partitions: "the migration leaves [today, today + 7 d] attached AND registered" — with
// today the day the bootstrap ran. Eight days, each attached, each with relid equal to the
// catalog's OID for its name, each marked 'cerbix:gate-ledger:<owner_token>' on the child (and
// nothing on the parent), and no DEFAULT partition anywhere under the parent. Then the routing
// itself: the first and last millisecond of the first and last bootstrapped day land in their
// day's child, and a row one millisecond outside the window on either side is refused with
// SQLSTATE 23514 "no partition" — the missing DEFAULT partition as a behaviour, not a catalog
// row.
func TestGateBootstrapWindowAnchoredOnTheDayItRan(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	proj, _, svc := seedService(t, st, ctx)
	first := gateBootstrapDayUTC(t, st, ctx)
	last := first.AddDate(0, 0, 7)

	for i := 0; i <= 7; i++ {
		day := first.AddDate(0, 0, i)
		var (
			state    string
			attached bool
			relidOK  bool
			markerOK bool
			isChild  bool
		)
		err := st.pool.QueryRow(ctx, `
			SELECT p.state, p.attached_at IS NOT NULL,
			       p.relid = to_regclass(p.relname)::oid,
			       obj_description(p.relid, 'pg_class') = 'cerbix:gate-ledger:' || p.owner_token::text,
			       EXISTS (SELECT 1 FROM pg_inherits WHERE inhrelid = p.relid AND inhparent = 'service_gate_decisions'::regclass)
			  FROM service_gate_decision_partitions p WHERE p.day = $1 AND p.relname = $2`,
			day, gatePartitionName(day)).Scan(&state, &attached, &relidOK, &markerOK, &isChild)
		if err != nil {
			t.Fatalf("day %s (bootstrap + %d) is not registered under %s: %v", day.Format("2006-01-02"), i, gatePartitionName(day), err)
		}
		if state != "attached" || !attached || !relidOK || !markerOK || !isChild {
			t.Errorf("day %s: state=%s attached_at=%v relid=catalog:%v marker:%v child:%v", day.Format("2006-01-02"), state, attached, relidOK, markerOK, isChild)
		}
	}
	var parentComment *string
	if err := st.pool.QueryRow(ctx, `SELECT obj_description('service_gate_decisions'::regclass, 'pg_class')`).Scan(&parentComment); err != nil {
		t.Fatal(err)
	}
	if parentComment != nil {
		t.Errorf("the parent carries a marker %q; ownership marks partitions, never the parent", *parentComment)
	}
	var defaults int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		 WHERE i.inhparent = 'service_gate_decisions'::regclass AND pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT'`).Scan(&defaults); err != nil {
		t.Fatal(err)
	}
	if defaults != 0 {
		t.Fatalf("%d DEFAULT partition(s) under service_gate_decisions; §5 says NO DEFAULT partition", defaults)
	}

	allow := "ALLOW"
	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{first, gatePartitionName(first)},
		{first.Add(24*time.Hour - time.Millisecond), gatePartitionName(first)},
		{last, gatePartitionName(last)},
		{last.Add(24*time.Hour - time.Millisecond), gatePartitionName(last)},
	} {
		id := gateUUIDv7(t, gateMs(tc.at))
		if err := insertDecision(st, ctx, id, proj, &svc, "ALLOW", &allow, tc.at, `{}`); err != nil {
			t.Fatalf("insert at %s: %v", tc.at.Format(time.RFC3339Nano), err)
		}
		if got := partitionOf(t, st, ctx, id); got != tc.want {
			t.Errorf("a row at %s landed in %s, want %s", tc.at.Format(time.RFC3339Nano), got, tc.want)
		}
	}
	for _, at := range []time.Time{first.Add(-time.Millisecond), last.Add(24 * time.Hour)} {
		err := insertDecision(st, ctx, gateUUIDv7(t, gateMs(at)), proj, &svc, "ALLOW", &allow, at, `{}`)
		if code, _ := pgCode(err); code != "23514" || !strings.Contains(err.Error(), "no partition") {
			t.Fatalf("a row at %s (outside [first, last + 1 d)) was accepted or refused otherwise: %v", at.Format(time.RFC3339Nano), err)
		}
	}
}

// §7 Identity: "an INSERT whose id millisecond differs from evaluated_at (planted) is refused by
// the CHECK". The refusal is read from the constraint NAME on the SQLSTATE 23514 error; and to
// prove that name is the mechanism rather than a coincidence, the CHECK is dropped inside a
// transaction, the same planted row goes through, the transaction rolls back, and the row is
// refused again. A vacuous assertion — one that would pass whatever refused the row — cannot
// survive the middle step.
func TestGateIdBindingCheckIsTheMechanism(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	proj, _, svc := seedService(t, st, ctx)
	at := gateBootstrapDayUTC(t, st, ctx).Add(9 * time.Hour)
	allow := "ALLOW"
	const constraint = "service_gate_decisions_id_binds_evaluated_at_chk"

	planted := gateUUIDv7(t, gateMs(at)+1)
	err := insertDecision(st, ctx, planted, proj, &svc, "ALLOW", &allow, at, `{}`)
	if code, name := pgCode(err); code != "23514" || name != constraint {
		t.Fatalf("planted id: want SQLSTATE 23514 from %s, got code=%q constraint=%q err=%v", constraint, code, name, err)
	}
	// One millisecond the other way, and one full day off, are the same CHECK.
	for _, ms := range []int64{gateMs(at) - 1, gateMs(at.AddDate(0, 0, 1))} {
		err := insertDecision(st, ctx, gateUUIDv7(t, ms), proj, &svc, "ALLOW", &allow, at, `{}`)
		if code, name := pgCode(err); code != "23514" || name != constraint {
			t.Fatalf("id ms=%d for evaluated_at ms=%d: want %s, got code=%q constraint=%q err=%v", ms, gateMs(at), constraint, code, name, err)
		}
	}

	tx, err := st.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `ALTER TABLE service_gate_decisions DROP CONSTRAINT `+constraint); err != nil {
		t.Fatalf("drop the CHECK (in a transaction that will roll back): %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO service_gate_decisions (id, project_id, service_id, service_slug, service_name, state, action, reasons, evidence,
		     policy_revision, window_name, policy_snapshot, evaluated_at)
		VALUES ($1, $2, $3, 's', 'n', 'ALLOW', 'ALLOW', '[]', '{}', 1, '30d', '{}', $4)`, planted, proj, svc, at); err != nil {
		t.Fatalf("with the CHECK gone the planted row is still refused, so something else refuses it and the assertion above names the wrong mechanism: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	err = insertDecision(st, ctx, planted, proj, &svc, "ALLOW", &allow, at, `{}`)
	if code, name := pgCode(err); code != "23514" || name != constraint {
		t.Fatalf("after the rollback the CHECK is not back: code=%q constraint=%q err=%v", code, name, err)
	}
	// The row that agrees with its id is accepted — the CHECK refuses the mismatch, not the shape.
	if err := insertDecision(st, ctx, gateUUIDv7(t, gateMs(at)), proj, &svc, "ALLOW", &allow, at, `{}`); err != nil {
		t.Fatalf("a matching id was refused: %v", err)
	}
}

// §7 Identity: "two INSERTs with one id into one day are refused by the local unique index" —
// and by the LOCAL unique, not by the primary key (evaluated_at, id), which the second row
// escapes with a different microsecond. Dropping that one child index inside a transaction lets
// the duplicate through; rolling back brings the refusal back. The constraint name on the 23505
// is the child's `<relname>_id_uniq`.
func TestGateLocalUniqueIdIndexIsTheMechanism(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	proj, _, svc := seedService(t, st, ctx)
	day := gateBootstrapDayUTC(t, st, ctx).AddDate(0, 0, 2)
	at := day.Add(15 * time.Hour)
	allow := "ALLOW"
	idx := gatePartitionName(day) + "_id_uniq"

	id := gateUUIDv7(t, gateMs(at))
	if err := insertDecision(st, ctx, id, proj, &svc, "ALLOW", &allow, at, `{"n":1}`); err != nil {
		t.Fatal(err)
	}
	twin := at.Add(time.Microsecond) // same millisecond → the binding CHECK passes; different PK
	err := insertDecision(st, ctx, id, proj, &svc, "ALLOW", &allow, twin, `{"n":2}`)
	if code, name := pgCode(err); code != "23505" || name != idx {
		t.Fatalf("duplicate id within a day: want 23505 from %s, got code=%q constraint=%q err=%v", idx, code, name, err)
	}

	tx, err := st.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DROP INDEX `+idx); err != nil {
		t.Fatalf("drop the local unique (in a transaction that will roll back): %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO service_gate_decisions (id, project_id, service_id, service_slug, service_name, state, action, reasons, evidence,
		     policy_revision, window_name, policy_snapshot, evaluated_at)
		VALUES ($1, $2, $3, 's', 'n', 'ALLOW', 'ALLOW', '[]', '{}', 1, '30d', '{}', $4)`, id, proj, svc, twin); err != nil {
		t.Fatalf("with the local unique gone the twin is still refused; the assertion above names the wrong mechanism: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	err = insertDecision(st, ctx, id, proj, &svc, "ALLOW", &allow, twin, `{"n":2}`)
	if code, name := pgCode(err); code != "23505" || name != idx {
		t.Fatalf("after the rollback the local unique is not back: code=%q constraint=%q err=%v", code, name, err)
	}
	// The primary key is the other guard: the identical (evaluated_at, id) pair is refused by IT.
	err = insertDecision(st, ctx, id, proj, &svc, "ALLOW", &allow, at, `{"n":3}`)
	if code, name := pgCode(err); code != "23505" || !strings.HasSuffix(name, "_pkey") {
		t.Fatalf("an identical (evaluated_at, id) pair: want 23505 from the primary key, got code=%q constraint=%q err=%v", code, name, err)
	}
	// The parent's key is (evaluated_at, id), in that order (§5).
	var pk string
	if err := st.pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conrelid = 'service_gate_decisions'::regclass AND contype = 'p'`).Scan(&pk); err != nil {
		t.Fatal(err)
	}
	if pk != "PRIMARY KEY (evaluated_at, id)" {
		t.Fatalf("parent primary key is %q, want PRIMARY KEY (evaluated_at, id)", pk)
	}
}

// The CHECK's arithmetic at the edges the writer will hit: floor(extract(epoch) * 1000) on the
// DATABASE side equals Go's UnixMilli for the last microsecond of a day, the first of the next,
// a .9995 instant and the epoch — exact numeric arithmetic, no float rounding — and the function
// is STRICT (NULL in, NULL out) so a NULL id could never satisfy the CHECK by accident.
func TestGateUUIDMsArithmeticAgreesAtTheEdges(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	day := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{
		day.Add(24*time.Hour - time.Microsecond),
		day.Add(24 * time.Hour),
		day.Add(12*time.Hour + 999*time.Millisecond + 500*time.Microsecond),
		time.Unix(0, 0).UTC(),
		time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		var dbMs int64
		var equal bool
		if err := st.pool.QueryRow(ctx,
			`SELECT floor(extract(epoch FROM $1::timestamptz) * 1000)::bigint, gate_uuid_ms($2::uuid) = floor(extract(epoch FROM $1::timestamptz) * 1000)`,
			at, gateUUIDv7(t, gateMs(at))).Scan(&dbMs, &equal); err != nil {
			t.Fatal(err)
		}
		if dbMs != gateMs(at) || !equal {
			t.Errorf("%s: database floor(epoch*1000)=%d, Go UnixMilli=%d, CHECK expression holds=%v", at.Format(time.RFC3339Nano), dbMs, gateMs(at), equal)
		}
	}
	var isNull bool
	if err := st.pool.QueryRow(ctx, `SELECT gate_uuid_ms(NULL::uuid) IS NULL`).Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Fatal("gate_uuid_ms(NULL) is not NULL; the function must be STRICT")
	}
}

// §5 registry and the parent's shape: the ownership registry refuses a second row per day, per
// relname and per owner_token (each a unique violation naming its constraint), refuses a state
// outside the five, and the two listing indexes of the parent are exactly the §5 paths.
func TestGateRegistryAndParentShapeConstraints(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	first := gateBootstrapDayUTC(t, st, ctx)
	var relname, token string
	var relid uint32
	if err := st.pool.QueryRow(ctx,
		`SELECT relname, owner_token::text, relid FROM service_gate_decision_partitions WHERE day = $1`, first).Scan(&relname, &token, &relid); err != nil {
		t.Fatal(err)
	}
	insert := func(day time.Time, rel, tok, state string) error {
		tx, err := st.pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		_, err = tx.Exec(ctx, `
			INSERT INTO service_gate_decision_partitions (day, relname, owner_token, relid, state)
			VALUES ($1, $2, $3::uuid, $4, $5)`, day, rel, tok, relid, state)
		return err
	}
	far := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		day        time.Time
		rel, tok   string
		state      string
		code, cons string
	}{
		{"second row for a day", first, "service_gate_decisions_p_other", "00000000-0000-7000-8000-000000000001", "created", "23505", "service_gate_decision_partitions_pkey"},
		{"second row for a relname", far, relname, "00000000-0000-7000-8000-000000000002", "created", "23505", "service_gate_decision_partitions_relname_key"},
		{"second row for an owner_token", far, "service_gate_decisions_p_other", token, "created", "23505", "service_gate_decision_partitions_owner_token_key"},
		{"unknown state", far, "service_gate_decisions_p_other", "00000000-0000-7000-8000-000000000003", "attaching", "23514", "service_gate_decision_partitions_state_chk"},
		{"a well-formed row is accepted (then rolled back)", far, "service_gate_decisions_p_other", "00000000-0000-7000-8000-000000000004", "created", "", ""},
	} {
		err := insert(tc.day, tc.rel, tc.tok, tc.state)
		if tc.code == "" {
			if err != nil {
				t.Errorf("%s: refused: %v", tc.name, err)
			}
			continue
		}
		if code, name := pgCode(err); code != tc.code || name != tc.cons {
			t.Errorf("%s: want %s from %s, got code=%q constraint=%q err=%v", tc.name, tc.code, tc.cons, code, name, err)
		}
	}
	// The markers are one-to-one with the registry: no two children share one, and every
	// registered child's marker is in the registry.
	var strayMarkers int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_inherits i JOIN pg_description d ON d.objoid = i.inhrelid AND d.classoid = 'pg_class'::regclass AND d.objsubid = 0
		 WHERE i.inhparent = 'service_gate_decisions'::regclass
		   AND NOT EXISTS (SELECT 1 FROM service_gate_decision_partitions p WHERE 'cerbix:gate-ledger:' || p.owner_token::text = d.description AND p.relid = i.inhrelid)`).Scan(&strayMarkers); err != nil {
		t.Fatal(err)
	}
	if strayMarkers != 0 {
		t.Fatalf("%d attached children carry a marker the registry does not own", strayMarkers)
	}
	// The parent's two listing indexes are the §5 keyset paths, verbatim.
	rows, err := st.pool.Query(ctx, `SELECT indexname, indexdef FROM pg_indexes WHERE tablename = 'service_gate_decisions' ORDER BY indexname`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var n, d string
		if err := rows.Scan(&n, &d); err != nil {
			t.Fatal(err)
		}
		got[n] = d
	}
	for name, cols := range map[string]string{
		"service_gate_decisions_project_idx":         "(project_id, evaluated_at DESC, id DESC)",
		"service_gate_decisions_project_service_idx": "(project_id, service_id, evaluated_at DESC, id DESC)",
		"service_gate_decisions_pkey":                "(evaluated_at, id)",
	} {
		if !strings.HasSuffix(got[name], cols) {
			t.Errorf("parent index %s is %q, want a definition ending in %s", name, got[name], cols)
		}
	}
	if len(got) != 3 {
		t.Errorf("parent has %d indexes (%v), want exactly the three of §5", len(got), got)
	}
}

// The remaining CHECKs of the policy and override tables that the first pass left un-reached:
// an empty window, schema_version 0 and a non-object clauses document on the policy; an empty
// actor label and a missing expiry on the override. Each is refused by ITS constraint.
func TestGatePolicyAndOverrideShapeChecks(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	proj, _, svc := seedService(t, st, ctx)

	policy := func(window string, version int, clauses string) error {
		_, err := st.pool.Exec(ctx, `
			INSERT INTO service_gate_policies (service_id, project_id, window_name, schema_version, clauses,
			    budget_consumed_percent, max_seal_lag_seconds, unknown_behavior, revision)
			VALUES ($1, $2, $3, $4, $5::jsonb, 90, 900, 'warn', 1)`, svc, proj, window, version, clauses)
		return err
	}
	for _, tc := range []struct {
		name    string
		window  string
		version int
		clauses string
		cons    string
	}{
		{"empty window", "", 1, `{}`, "service_gate_policies_window_chk"},
		{"schema_version 0", "30d", 0, `{}`, "service_gate_policies_schema_version_chk"},
		{"clauses as an array", "30d", 1, `[]`, "service_gate_policies_clauses_chk"},
		{"clauses as a string", "30d", 1, `"block"`, "service_gate_policies_clauses_chk"},
	} {
		err := policy(tc.window, tc.version, tc.clauses)
		if code, name := pgCode(err); code != "23514" || name != tc.cons {
			t.Errorf("%s: want 23514 from %s, got code=%q constraint=%q err=%v", tc.name, tc.cons, code, name, err)
		}
	}

	_, err := st.pool.Exec(ctx, `
		INSERT INTO service_gate_overrides (service_id, project_id, policy_revision, via_token, actor_label, reason, expires_at)
		VALUES ($1, $2, 1, true, '', 'deploying', now() + interval '1 hour')`, svc, proj)
	if code, name := pgCode(err); code != "23514" || name != "service_gate_overrides_actor_label_chk" {
		t.Errorf("empty actor_label: want 23514 from service_gate_overrides_actor_label_chk, got code=%q constraint=%q err=%v", code, name, err)
	}
	_, err = st.pool.Exec(ctx, `
		INSERT INTO service_gate_overrides (service_id, project_id, policy_revision, via_token, actor_label, reason)
		VALUES ($1, $2, 1, true, 'token:ci', 'deploying')`, svc, proj)
	if code, _ := pgCode(err); code != "23502" {
		t.Errorf("an override without expires_at: want 23502 (not null), got code=%q err=%v", code, err)
	}
	// A manual closure by a token: user null, via_token true, label set — the D9 triple as data.
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_gate_overrides (service_id, project_id, policy_revision, via_token, actor_label, reason, expires_at,
		    revoked_at, revoked_reason, revoked_by_user_id, revoked_via_token, revoked_by_label)
		VALUES ($1, $2, 1, true, 'token:ci', 'deploying', now() + interval '1 hour', now(), 'manual', NULL, true, 'token:release-bot')`, svc, proj); err != nil {
		t.Errorf("a manual token closure with the complete triple was refused: %v", err)
	}
}

// §7 Partitions: "a CREATE … PARTITION OF (the mutation) is not in the code (asserted by grep
// over the store)". The heartbeat and service-fact partitioners legitimately use PARTITION OF,
// so the grep is scoped to the GATE LEDGER's code: every non-test .go file and every migration
// under internal/store whose name mentions the gate or whose text mentions
// service_gate_decisions must not contain PARTITION OF in any spelling. A partition of the
// ledger is built standalone and ATTACHed (D10).
func TestNoCreatePartitionOfInTheGateLedgerCode(t *testing.T) {
	// Any "PARTITION OF" in any spelling — except PostgreSQL's own error text "no partition of
	// relation … found for row", which the ledger's writer matches to name a missing day
	// (invariant 19) and which no DDL statement can contain: the DDL form is
	// `CREATE TABLE <name> PARTITION OF <parent>`, with an identifier, never the word "no",
	// in front.
	partitionOf := regexp.MustCompile(`(?i)(\bno\s+)?partition\s+of\b`)
	hasDDLPartitionOf := func(text string) bool {
		for _, m := range partitionOf.FindAllStringSubmatch(text, -1) {
			if m[1] == "" {
				return true
			}
		}
		return false
	}
	var scanned, offenders []string
	walk := func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			isGo := strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
			isSQL := strings.HasSuffix(name, ".sql")
			if !isGo && !isSQL {
				continue
			}
			path := filepath.Join(dir, name)
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			text := string(src)
			if !strings.Contains(strings.ToLower(name), "gate") && !strings.Contains(text, "service_gate_decisions") {
				continue
			}
			scanned = append(scanned, path)
			if hasDDLPartitionOf(text) {
				offenders = append(offenders, path)
			}
		}
	}
	walk(".")
	walk("migrations")
	if len(scanned) == 0 {
		t.Fatal("the grep scanned nothing; the ledger's migration must at least be in scope")
	}
	found := false
	for _, p := range scanned {
		if strings.HasSuffix(p, "00093_reliability_gate.sql") {
			found = true
		}
	}
	if !found {
		t.Fatalf("00093_reliability_gate.sql is not in scope; scanned %v", scanned)
	}
	if len(offenders) != 0 {
		t.Fatalf("PARTITION OF appears in gate-ledger code: %v — a ledger partition is built standalone and attached (D10); CREATE … PARTITION OF takes ACCESS EXCLUSIVE on the parent", offenders)
	}
}

// The migration's Down block is exercised too: on a probe database migrated through 00093 and
// rolled back one step, the four tables, the function and every bootstrapped partition (which
// the parent's DROP would not reach once detached) are gone, and no relation named for the
// ledger survives. A Down nobody runs is a Down nobody knows works.
func TestGateMigrationDownRemovesEverythingItCreated(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	db, _, cleanup := probeDatabaseAt(t, st, ctx, "gatedown", 93)
	defer cleanup()

	var before int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_class WHERE relname LIKE 'service_gate_%'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before < 8+4 {
		t.Fatalf("expected at least the four tables and eight partitions before Down, found %d relations", before)
	}
	if err := goose.DownToContext(ctx, db, "migrations", 92); err != nil {
		t.Fatalf("goose down 93 → 92: %v", err)
	}
	var after int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_class WHERE relname LIKE 'service_gate_%' OR relname LIKE 'gate_%'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("%d relations named for the ledger survive the Down", after)
	}
	var fn int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_proc WHERE proname = 'gate_uuid_ms'`).Scan(&fn); err != nil {
		t.Fatal(err)
	}
	if fn != 0 {
		t.Fatal("gate_uuid_ms survives the Down")
	}
	// And Up again converges: the bootstrap re-runs on the empty registry and registers eight days.
	if err := goose.UpToContext(ctx, db, "migrations", 93); err != nil {
		t.Fatalf("goose up 92 → 93 after the Down: %v", err)
	}
	var days int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM service_gate_decision_partitions WHERE state = 'attached'`).Scan(&days); err != nil {
		t.Fatal(err)
	}
	if days != 8 {
		t.Fatalf("after Down and Up, %d attached days registered, want 8", days)
	}
}
