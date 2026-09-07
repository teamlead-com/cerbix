package mqadmin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/dispatch"
)

func TestFromAMQPDerivesManagement(t *testing.T) {
	c, err := FromAMQP("amqp://u:p@broker:5672/")
	if err != nil {
		t.Fatalf("from amqp: %v", err)
	}
	if c.base != "http://broker:15672" || c.user != "u" || c.pass != "p" {
		t.Fatalf("derived = base=%q user=%q pass=%q", c.base, c.user, c.pass)
	}
	tls, _ := FromAMQP("amqps://x@broker/")
	if tls.base != "https://broker:15671" {
		t.Fatalf("amqps base = %q, want https:15671", tls.base)
	}
}

func TestLiveJobRegions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, _, _ := r.BasicAuth(); u != "u" {
			t.Errorf("missing basic auth")
		}
		_, _ = w.Write([]byte(`[
			{"name":"checks.jobs.core","consumers":2},
			{"name":"checks.jobs.geo1","consumers":0},
			{"name":"checks.jobs.geo2","consumers":1},
			{"name":"checks.jobs.v2.core","consumers":0},
			{"name":"checks.jobs.v2.secure","consumers":1},
			{"name":"checks.results","consumers":1}
		]`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL)
	c.user, c.pass = "u", "p"
	live, err := c.LiveJobRegions(context.Background())
	if err != nil {
		t.Fatalf("live regions: %v", err)
	}
	if !live["core"] || live["geo1"] || !live["geo2"] || len(live) != 2 {
		t.Fatalf("live = %#v, want {core,geo2}", live)
	}
	credentialLive, err := c.LiveCredentialJobRegions(context.Background())
	if err != nil {
		t.Fatalf("credential live regions: %v", err)
	}
	if credentialLive["core"] || !credentialLive["secure"] || len(credentialLive) != 1 {
		t.Fatalf("credential live = %#v, want {secure}", credentialLive)
	}
}

// FR-029 invariant 6, AMQP half: the queue IS the announcement, so what this asserts is that a
// region's capability is read from the queues that actually have a consumer — and that the token
// survives the trip, because a region announcing a version core is not emitting must be
// distinguishable from a region announcing nothing at all.
func TestLiveCanaryJobRegionsReadsTheAnnouncedTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"name":"checks.jobs.core","consumers":3},
			{"name":"checks.canary.async_transaction_v1@1.core","consumers":1},
			{"name":"checks.canary.v3.async_transaction_v1@1.core","consumers":1},
			{"name":"checks.canary.async_transaction_v1@2.skewed","consumers":1},
			{"name":"checks.canary.async_transaction_v1@1.drained","consumers":0},
			{"name":"checks.canary.malformed","consumers":1},
			{"name":"checks.results","consumers":1}
		]`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL)
	live, err := c.LiveCanaryJobRegions(context.Background())
	if err != nil {
		t.Fatalf("live canary regions: %v", err)
	}
	// core: both carriers announce the same capability, and it is announced once.
	if got := live["core"]; len(got) != 1 || got[0] != "async_transaction_v1@1" {
		t.Fatalf("core = %#v, want the token exactly once", got)
	}
	// skewed: a real region with a real runner, announcing a version core is not emitting. The
	// caller must be able to tell this from "nothing there", which is why tokens are returned.
	if got := live["skewed"]; len(got) != 1 || got[0] != "async_transaction_v1@2" {
		t.Fatalf("skewed = %#v, want the v2 token", got)
	}
	if _, ok := live["drained"]; ok {
		t.Fatal("a queue with no consumer announces nothing")
	}
	if len(live) != 2 {
		t.Fatalf("live = %#v, want exactly core and skewed", live)
	}
}

// The prefix is spelled in this package and in internal/dispatch, which deliberately do not import
// each other. A drift in either spelling makes every region look incapable forever — silently — so
// the agreement is asserted against the other file's source rather than assumed.
func TestTheCanaryQueuePrefixMatchesTheDispatcher(t *testing.T) {
	src, err := os.ReadFile("../dispatch/amqp.go")
	if err != nil {
		t.Fatalf("read the dispatcher: %v", err)
	}
	for _, want := range []string{
		`canaryQueuePrefix = "` + canaryQueuePrefix + `"`,
		`canaryV3Infix = "` + canaryV3Infix + `"`,
	} {
		if !strings.Contains(string(src), want) {
			t.Fatalf("the dispatcher no longer declares %s — the two spellings have drifted", want)
		}
	}
}

// A6 — a consumer on a GENERATIONAL jobs queue never registers as a region.
//
// `LiveJobRegions` feeds the region-worker alert, and the generational queues share the legacy
// prefix: `checks.jobs.v4.geo1` starts with `checks.jobs.`. The exclusion list was hand-written and
// one generation behind, so a v4 consumer registered the phantom region `v4.geo1` — a region no
// monitor belongs to, alerting about a worker that is running perfectly well.
//
// The subtraction is now DERIVED from the dispatcher's own generation table, so a generation added
// tomorrow is excluded without anyone editing this package. The loop below iterates that table for
// the same reason.
//
// The mutation that must kill this: restate the exclusion by hand, or drop one generation from it.
func TestAGenerationalJobsQueueIsNotAPhantomRegion(t *testing.T) {
	body := `[{"name":"checks.jobs.core","consumers":2}`
	for _, prefix := range dispatch.JobsQueuePrefixes() {
		if prefix == dispatch.LegacyJobsQueuePrefix() {
			continue
		}
		body += `,{"name":"` + prefix + `geo1","consumers":1}`
	}
	body += `]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c, _ := New(srv.URL)

	live, err := c.LiveJobRegions(context.Background())
	if err != nil {
		t.Fatalf("live regions: %v", err)
	}
	if len(live) != 1 || !live["core"] {
		t.Fatalf("live = %#v, want {core} only — every other entry is a queue NAME read as a "+
			"region, and the region-worker alert would page about a region no monitor belongs to", live)
	}
	// The generational readers still see their own queues: subtracting them from the legacy set
	// must not make them invisible to the checks that exist to find them.
	ledger, err := c.LiveLedgerJobRegions(context.Background())
	if err != nil {
		t.Fatalf("ledger regions: %v", err)
	}
	if !ledger["geo1"] {
		t.Errorf("the generation-4 reader lost geo1: %#v", ledger)
	}
}
