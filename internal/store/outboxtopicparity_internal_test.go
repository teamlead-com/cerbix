package store

import (
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// The topic whitelist lives in TWO places — `domain.AllTopics` and the `outbox_events_topic_check`
// constraint — and a migration that REPLACES the constraint restates the whole list by hand. That is
// how phase 5 added `service_alert` while silently dropping `region_worker_alert`,
// `escalation_step` and `subscriber_confirm`: nothing failed to build, nothing failed to migrate, and
// every escalation step, region-worker alert and subscriber confirmation would have failed its INSERT
// in production instead.
//
// This is the guard that makes that class of change impossible to land quietly. It asserts PARITY in
// both directions against the live constraint:
//   - every topic the binary can enqueue is accepted (a narrowed whitelist breaks alerting);
//   - every topic the constraint accepts is one the binary knows (a stale name nobody dispatches is
//     a row that can be written and never delivered).
func TestOutboxTopicWhitelistMatchesTheBinary(t *testing.T) {
	st, ctx := outboxTestStore(t)

	// Every quoted literal in the constraint definition IS the accepted set; matching the literals
	// is robust to how Postgres chooses to print the expression (ANY(ARRAY[...]) vs IN (...), casts,
	// line breaks), which a comma-split of the whole text is not.
	var accepted []string
	if err := st.pool.QueryRow(ctx, `
		SELECT COALESCE(ARRAY(
		    SELECT (regexp_matches(pg_get_constraintdef(c.oid), '''([a-z_]+)''', 'g'))[1]
		      FROM pg_constraint c
		     WHERE c.conname = 'outbox_events_topic_check'
		), '{}')`).Scan(&accepted); err != nil {
		t.Fatalf("read constraint: %v", err)
	}
	sort.Strings(accepted)

	known := domain.AllTopics()
	sort.Strings(known)

	inList := func(list []string, want string) bool {
		for _, v := range list {
			if v == want {
				return true
			}
		}
		return false
	}
	for _, topic := range known {
		if !inList(accepted, topic) {
			t.Fatalf("topic %q is enqueued by this binary but REJECTED by the database: every "+
				"insert of it fails at runtime. Constraint accepts %v", topic, accepted)
		}
	}
	for _, topic := range accepted {
		if !inList(known, topic) {
			t.Fatalf("the database accepts topic %q that this binary never enqueues or dispatches: "+
				"such a row could be written and never delivered. Binary knows %v", topic, known)
		}
	}
}

// TestEveryKnownTopicInserts proves the parity claim by BEHAVIOUR rather than by string comparison:
// one row per topic, actually inserted. A constraint that parses correctly but rejects a real insert
// (a cast, a trigger, a typo in a literal) fails here.
func TestEveryKnownTopicInserts(t *testing.T) {
	st, ctx := outboxTestStore(t)
	for _, topic := range domain.AllTopics() {
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO outbox_events (topic, payload, status) VALUES ($1, '{}'::jsonb, $2)`,
			topic, outboxStatusFor(topic)); err != nil {
			t.Fatalf("topic %q cannot be enqueued: %v", topic, err)
		}
	}
}

// outboxStatusFor mirrors the enqueue class the fence assigns, so this test inserts rows the way the
// product does rather than inventing a shape the status constraint would reject.
func outboxStatusFor(topic string) string {
	if domain.FencedTopic(topic) {
		return "pending_fenced"
	}
	return "pending"
}

// The claim CLASS has one owner too, and it is easier to bypass than the whitelist: a raw
// `INSERT INTO outbox_events` compiles, migrates and passes every functional test, while quietly
// taking the column default and filing a FENCED topic's row in the LEGACY class. That is what
// happened to the first version of the lifecycle close (b38a2ab): the onset was invisible to a
// pre-fence binary while its close was claimable by one, so in a rolling fleet the ENDING of an
// announcement was the row that got attempt-burned by a worker unable to dispatch it.
//
// `enqueueOutboxTx` / `EnqueueOutbox` derive the class from `domain.FencedTopic`, and this test is
// what keeps them the only writers. It reads source rather than behaviour on purpose: the defect is
// not that some path is wrong today, it is that the NEXT path can be wrong in a way no delivery test
// looks at.
func TestOnlyTheOutboxOwnerInsertsOutboxRows(t *testing.T) {
	root := ".."
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if filepath.Base(path) == "outbox.go" && filepath.Base(filepath.Dir(path)) == "store" {
			return nil // the owner itself
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(src), "INSERT INTO outbox_events") {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk sources: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("outbox rows inserted outside the enqueue owner in %v — a raw INSERT takes the "+
			"legacy 'pending' default and files a fenced topic in the wrong claim class; call "+
			"enqueueOutboxTx/EnqueueOutbox instead", offenders)
	}
}

// The trigger in 00088 duplicates `domain.FencedTopics()` in SQL, because the class has to be the
// DATABASE's rule: a producer of any version — including an old one still running through a rolling
// upgrade — must not be able to write a fenced topic into the legacy class. Two copies are fine; two
// copies that can drift are not, which is what this gate is for.
func TestOutboxFencedClassMatchesTheBinary(t *testing.T) {
	st, ctx := outboxTestStore(t)

	var body string
	if err := st.pool.QueryRow(ctx,
		`SELECT prosrc FROM pg_proc WHERE proname = 'outbox_enforce_fenced_class'`).Scan(&body); err != nil {
		t.Fatalf("read the trigger function: %v", err)
	}
	for _, topic := range domain.FencedTopics() {
		if !strings.Contains(body, "'"+topic+"'") {
			t.Fatalf("fenced topic %q is not in the trigger's list: a producer running an older "+
				"binary would write it into the legacy class, where the ordering fence does not "+
				"apply", topic)
		}
	}
	// And the other direction: the trigger must not fence a topic the binary dispatches unfenced,
	// which would strand it for every worker.
	for _, quoted := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(body, -1) {
		topic := quoted[1]
		if topic == "pending" || topic == "pending_fenced" {
			continue
		}
		if !domain.FencedTopic(topic) {
			t.Fatalf("the trigger fences %q, which this binary dispatches as legacy: those rows would "+
				"wait for a worker that never claims them", topic)
		}
	}
}

// A producer that predates the fence writes the legacy class; the DATABASE corrects it. This is the
// rolling-upgrade window the consumer-side fence could not cover, because the class was chosen by
// whichever binary happened to be inserting.
func TestAnOldProducersRowIsFencedByTheDatabase(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	// Exactly what an old binary's INSERT looks like: the legacy class, no sequence in the payload.
	var status string
	var fenced bool
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO outbox_events (topic, payload, status, fenced)
		VALUES ($1, $2, 'pending', false)
		RETURNING status, fenced`,
		domain.TopicIncidentEvent,
		`{"event":"incident.opened","incident":{"id":"`+proj.ID+`","project_id":"`+proj.ID+`"}}`).
		Scan(&status, &fenced); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	if status != "pending_fenced" || !fenced {
		t.Fatalf("an old producer's row landed as %q/fenced=%v: an old worker would claim it and "+
			"deliver an incident's events in any order", status, fenced)
	}
}

// A DEAD row from before the barrier keeps its class in the `fenced` column, and `ReplayDeadOutbox`
// restores from exactly that. Left at false, a replay months later would put an incident's event back
// into the legacy class — into the hands of the worker the barrier exists to keep away from it.
//
// This runs the REAL migration rather than a copy of its statement. A test that re-types the SQL it
// checks passes just as happily after somebody deletes the original, and this arc has already caught
// itself doing that twice.
func TestAPreBarrierDeadRowIsFencedByTheMigration(t *testing.T) {
	st, ctx := outboxTestStore(t)

	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("CERBIX_TEST_DATABASE_DSN is not a URL (%q): %v", dsn, err)
	}
	name := fmt.Sprintf("cerbix_test_deadfence_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := st.pool.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create probe database: %v", err)
	}
	defer func() {
		if _, err := st.pool.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("drop probe database: %v", err)
		}
	}()
	u.Path = "/" + name
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open probe: %v", err)
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	// Before the barrier there is no trigger, so a legacy insert stays legacy.
	if err := goose.UpToContext(ctx, db, "migrations", 87); err != nil {
		t.Fatalf("migrate to 87: %v", err)
	}
	var id string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO outbox_events (topic, payload, status, attempts, last_error, fenced)
		VALUES ($1, '{"event":"incident.opened","incident":{"id":"i1"}}', 'dead', 10, 'boom', false)
		RETURNING id::text`, domain.TopicIncidentEvent).Scan(&id); err != nil {
		t.Fatalf("seed a pre-barrier dead row: %v", err)
	}

	if err := goose.UpToContext(ctx, db, "migrations", 88); err != nil {
		t.Fatalf("migrate to 88: %v", err)
	}

	var status string
	var fenced bool
	if err := db.QueryRowContext(ctx,
		`SELECT status, fenced FROM outbox_events WHERE id = $1::uuid`, id).Scan(&status, &fenced); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !fenced {
		t.Fatal("the barrier left a DEAD pre-barrier row unfenced: a replay restores the claimable " +
			"class from this column, so an old worker would claim an incident's event and deliver it " +
			"in any order")
	}
	if status != "dead" {
		t.Fatalf("the backfill changed a dead row's status to %q: promoting a class is not "+
			"resurrecting the row", status)
	}
}

// A row an old worker had already CLAIMED when the barrier ran keeps that worker's token. Replacing it
// during the promotion is what stops a pre-barrier claim from settling the row and releasing the
// successor behind it. It cannot recall an external call that worker may already have made — nothing
// on this side can, which is what D-0177 now says out loud.
func TestTheMigrationInvalidatesAPreBarrierClaim(t *testing.T) {
	st, ctx := outboxTestStore(t)

	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("CERBIX_TEST_DATABASE_DSN is not a URL (%q): %v", dsn, err)
	}
	name := fmt.Sprintf("cerbix_test_claimfence_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := st.pool.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create probe database: %v", err)
	}
	defer func() {
		if _, err := st.pool.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("drop probe database: %v", err)
		}
	}()
	u.Path = "/" + name
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open probe: %v", err)
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 87); err != nil {
		t.Fatalf("migrate to 87: %v", err)
	}
	// A legacy row an old worker has already claimed: it holds this token.
	var id, oldToken string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO outbox_events (topic, payload, status, fenced, claim_token)
		VALUES ($1, '{"event":"incident.opened","incident":{"id":"i1"}}', 'pending', false,
		        gen_random_uuid())
		RETURNING id::text, claim_token::text`, domain.TopicIncidentEvent).Scan(&id, &oldToken); err != nil {
		t.Fatalf("seed a claimed pre-barrier row: %v", err)
	}

	if err := goose.UpToContext(ctx, db, "migrations", 88); err != nil {
		t.Fatalf("migrate to 88: %v", err)
	}

	var token string
	if err := db.QueryRowContext(ctx,
		`SELECT claim_token::text FROM outbox_events WHERE id = $1::uuid`, id).Scan(&token); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if token == oldToken {
		t.Fatal("the promotion left the pre-barrier claim token valid: that worker's " +
			"MarkOutboxDelivered would still win the CAS, mark the event delivered and release the " +
			"successor behind it")
	}
}
