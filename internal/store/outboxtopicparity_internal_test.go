package store

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

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
