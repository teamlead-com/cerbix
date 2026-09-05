package cli

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// FR-032: the generation-4 announcement follows the ROLE, never the envelope capability.
//
// Tying it to secrets left an envelope-disabled deployment — the default, since
// `secrets.enabled: false` is meant to change nothing else — with no ledger-capable worker, so the
// region never became eligible and the ordinary secretless monitor FR-032 exists for could not be
// scheduled onto the carrier at all.
func TestTheLedgerAnnouncementFollowsTheRoleAndNotTheEnvelopeCapability(t *testing.T) {
	for _, tc := range []struct {
		role string
		want int
	}{
		{role: "worker", want: 1},
		{role: "all", want: 0},
		{role: "api", want: 0},
		{role: "scheduler", want: 0},
		{role: "agent", want: 0},
	} {
		t.Run(tc.role, func(t *testing.T) {
			if got := executorLedgerCapability(tc.role); got != tc.want {
				t.Fatalf("executorLedgerCapability(%q) = %d, want %d", tc.role, got, tc.want)
			}
		})
	}
}

// The point of the previous test stated as the deployment it protects: envelope enforcement OFF
// must still produce an announcing worker. The function takes no envelope argument precisely so
// that this cannot regress by someone threading one back in.
func TestAnEnvelopeDisabledDeploymentStillAnnouncesTheLedgerCarrier(t *testing.T) {
	if got := executorLedgerCapability("worker"); got != 1 {
		t.Fatalf("a worker announces %d with no envelope capability in play; "+
			"LiveLedgerJobRegions would stay false and FR-032 could schedule nothing", got)
	}
}

// FR-032 §11 and §12.3 — the two calls that make the ledger's maintenance pass do anything.
//
// A SOURCE SCAN and not a behavioural test, for the reason phase A's timeline guard is one: the
// compiler cannot see a missing builder call. `WithLedgerMetrics` and `WithExpectedRunRetention`
// shipped defined, documented in the runbook, described in `config.example.yaml` — and called from
// nowhere. The gauge `cerbix_expected_runs_hot_update_ratio` was therefore exported by no binary at
// all, which is §11's entire measurement gate, and the purge cut at the built-in default whatever
// `ledger.expected_run_retention_days` said, so one setting produced two different retentions.
//
// Every `scheduler.New(` in this file is a leader construction and every one must carry both. The
// SET is asserted per site rather than by counting calls, because a same-count edit — moving one
// call from one site to the other — walks straight through a count.
func TestEveryLeaderConstructionWiresTheLedgerPass(t *testing.T) {
	src, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatalf("read cli.go: %v", err)
	}
	// Each construction is a `scheduler.New(...)` and everything up to the statement's end — the
	// builder chain is one expression, so the next `scheduler.New(` (or EOF) bounds it.
	sites := regexp.MustCompile(`scheduler\.New\(`).FindAllStringIndex(string(src), -1)
	if len(sites) == 0 {
		t.Fatal("no scheduler.New( construction found in cli.go — this guard is now blind")
	}
	for i, at := range sites {
		end := len(src)
		if i+1 < len(sites) {
			end = sites[i+1][0]
		}
		chain := string(src[at[0]:end])
		for _, call := range []string{"WithLedgerMetrics(", "WithExpectedRunRetention("} {
			if !strings.Contains(chain, call) {
				t.Errorf("scheduler construction #%d does not call %s — FR-032's HOT gauge is then "+
					"exported by nothing and the ledger purge ignores its configured retention", i+1, call)
			}
		}
	}
	// The containment check above has one hole, and it is the LAST site's: its chunk runs to
	// end-of-file, so a construction missing the call would pass on any later occurrence of the
	// same text anywhere in the file. The count equality closes it — with one call per site and no
	// strays, the totals must match — and it is asserted BESIDE the per-site check rather than
	// instead of it, because a count alone lets both calls sit on one construction while the other
	// has none.
	for _, call := range []string{"WithLedgerMetrics(", "WithExpectedRunRetention("} {
		if got := strings.Count(string(src), call); got != len(sites) {
			t.Errorf("%s appears %d time(s) against %d scheduler construction(s): one per "+
				"construction and no strays, or the per-site check above can be satisfied by text "+
				"that belongs to a different one", call, got, len(sites))
		}
	}
}
