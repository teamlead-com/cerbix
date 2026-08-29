package api

import (
	"testing"
	"time"
)

// The §5a limiter, on a fake clock (func-reliability-gate §7 "Bounds" and "Limiter boundaries").
// Every number below is the spec's: capacity = the per-minute value, refill = value/60 per
// second, Retry-After = ceil(seconds to the next token) and never below 1 for a rate refusal,
// exactly 1 for an in-flight refusal.

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newFakeClock() *fakeClock               { return &fakeClock{t: time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)} }
func testLimits(inflightP, inflightPr, rateP, ratePr int) GateLimits {
	return GateLimits{InflightProcess: inflightP, InflightPrincipal: inflightPr,
		RatePrincipalPerMinute: rateP, RateProcessPerMinute: ratePr, TxBudget: 5 * time.Second}
}

// admit is one admitted-and-finished decision at concurrency 1; it fails the test on a refusal.
func admit(t *testing.T, l *gateLimiter, principal string) {
	t.Helper()
	release, ref := l.acquire(principal, true)
	if ref != nil {
		t.Fatalf("acquire(%s) refused %s retry=%d, want admitted", principal, ref.reason, ref.retryAfter)
	}
	release()
}

func wantRefusal(t *testing.T, l *gateLimiter, principal string, rate bool, reason string, retryAfter int) {
	t.Helper()
	release, ref := l.acquire(principal, rate)
	if ref == nil {
		release()
		t.Fatalf("acquire(%s) admitted, want refusal %s", principal, reason)
	}
	if ref.reason != reason || ref.retryAfter != retryAfter {
		t.Fatalf("acquire(%s) = %s retry=%d, want %s retry=%d", principal, ref.reason, ref.retryAfter, reason, retryAfter)
	}
}

// The (n+1)th concurrent request from one principal is principal_inflight with Retry-After 1,
// and the refusal leaves the principal's tokens exactly where the n admitted requests put them.
func TestGateLimiterPrincipalInflightCap(t *testing.T) {
	clock := newFakeClock()
	l := newGateLimiter(testLimits(8, 2, 10, 60), clock.now)
	r1, ref := l.acquire("alice", true)
	if ref != nil {
		t.Fatal(ref)
	}
	r2, ref := l.acquire("alice", true)
	if ref != nil {
		t.Fatal(ref)
	}
	if got := l.principalTokens("alice"); got != 8 {
		t.Fatalf("tokens after two admitted = %v, want 8", got)
	}
	wantRefusal(t, l, "alice", true, gateRefusePrincipalInflight, 1)
	if got := l.principalTokens("alice"); got != 8 {
		t.Fatalf("an in-flight refusal changed the principal's tokens: %v, want 8", got)
	}
	// Another principal is not affected by alice's in-flight count.
	admit(t, l, "bob")
	r1()
	admit(t, l, "alice") // a slot freed → admitted again
	r2()
}

// The process cap refuses across TWO principals, and is checked before the principal cap.
func TestGateLimiterProcessInflightCapAcrossPrincipals(t *testing.T) {
	clock := newFakeClock()
	l := newGateLimiter(testLimits(2, 2, 10, 60), clock.now)
	ra, ref := l.acquire("alice", true)
	if ref != nil {
		t.Fatal(ref)
	}
	rb, ref := l.acquire("bob", true)
	if ref != nil {
		t.Fatal(ref)
	}
	wantRefusal(t, l, "carol", true, gateRefuseProcessInflight, 1)
	wantRefusal(t, l, "alice", true, gateRefuseProcessInflight, 1) // process first, then principal
	// A process-level refusal happens before the principal is even looked up: no per-principal
	// state is created for carol, so a flood refused at the process cap grows no map.
	if _, seen := l.principals["carol"]; seen {
		t.Fatal("a process_inflight refusal created (and possibly debited) carol's state")
	}
	ra()
	admit(t, l, "carol")
	rb()
}

// A sequential flood from one principal at concurrency 1 is rate-limited: with 10/min the 11th
// request is principal_rate with Retry-After 6 (one token every 6 s), and the refusal runs no
// debit — the same header comes back until the clock moves.
func TestGateLimiterSequentialFloodIsRateLimited(t *testing.T) {
	clock := newFakeClock()
	l := newGateLimiter(testLimits(8, 1, 10, 60), clock.now)
	for i := 0; i < 10; i++ {
		admit(t, l, "ci")
	}
	wantRefusal(t, l, "ci", true, gateRefusePrincipalRate, 6)
	wantRefusal(t, l, "ci", true, gateRefusePrincipalRate, 6)
	if got := l.principalTokens("ci"); got != 0 {
		t.Fatalf("tokens after a refused flood = %v, want 0", got)
	}
	// ceil: one second later 1/6 of a token has arrived, so 5/6 remains → 5 s.
	clock.advance(time.Second)
	wantRefusal(t, l, "ci", true, gateRefusePrincipalRate, 5)
	clock.advance(5 * time.Second)
	admit(t, l, "ci")
	wantRefusal(t, l, "ci", true, gateRefusePrincipalRate, 6)
}

// Two identical bursts see identical Retry-After sequences (§5a).
func TestGateLimiterIdenticalBurstsIdenticalRetryAfter(t *testing.T) {
	burst := func(principal string) []int {
		clock := newFakeClock()
		l := newGateLimiter(testLimits(8, 1, 4, 600), clock.now)
		var out []int
		for i := 0; i < 8; i++ {
			release, ref := l.acquire(principal, true)
			if ref == nil {
				release()
				out = append(out, 0)
				continue
			}
			out = append(out, ref.retryAfter)
			clock.advance(7 * time.Second)
		}
		return out
	}
	a, b := burst("alice"), burst("bob")
	if len(a) != len(b) {
		t.Fatal("bursts differ in length")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("burst headers differ at %d: %v vs %v", i, a, b)
		}
	}
	// 4/min = one token per 15 s: four admitted, then 15 s → after 7 s, ceil(8) = 8, then 1.
	want := []int{0, 0, 0, 0, 15, 8, 1, 0}
	for i := range want {
		if a[i] != want[i] {
			t.Fatalf("burst = %v, want %v", a, want)
		}
	}
}

// A rate refusal releases the in-flight permit: the next request is not refused for concurrency.
func TestGateLimiterRateRefusalReleasesPermit(t *testing.T) {
	clock := newFakeClock()
	l := newGateLimiter(testLimits(1, 1, 1, 60), clock.now)
	admit(t, l, "ci")
	wantRefusal(t, l, "ci", true, gateRefusePrincipalRate, 60)
	if l.processInflight != 0 {
		t.Fatalf("process in-flight after a rate refusal = %d, want 0", l.processInflight)
	}
	// A ledger read (permits only) from the same principal is admitted — the permit is free.
	release, ref := l.acquire("ci", false)
	if ref != nil {
		t.Fatalf("ledger read refused %s after a rate refusal; the permit was not released", ref.reason)
	}
	release()
	// And a ledger read consumes no rate token: still exactly 0 tokens, still principal_rate.
	if l.principalTokens("ci") != 0 {
		t.Fatal("a permit-only acquire debited the bucket")
	}
	wantRefusal(t, l, "ci", true, gateRefusePrincipalRate, 60)
}

// Process bucket empty, principal bucket available: the refusal is process_rate and the
// principal's count is UNCHANGED — the two buckets are debited as a unit or not at all
// (§5a, review round 7 P1-6). The mutation that debits the principal before checking the
// process bucket fails here.
func TestGateLimiterProcessEmptyLeavesPrincipalUntouched(t *testing.T) {
	clock := newFakeClock()
	l := newGateLimiter(testLimits(8, 2, 10, 2), clock.now)
	admit(t, l, "alice")
	admit(t, l, "alice") // the process bucket (2/min) is now empty; alice has 8 left
	wantRefusal(t, l, "bob", true, gateRefuseProcessRate, 30)
	if got := l.principalTokens("bob"); got != 10 {
		t.Fatalf("bob's tokens after a process_rate refusal = %v, want 10 (untouched)", got)
	}
	if got := l.principalTokens("alice"); got != 8 {
		t.Fatalf("alice's tokens = %v, want 8", got)
	}
	// The process bucket refills at 1 token per 30 s.
	clock.advance(30 * time.Second)
	admit(t, l, "bob")
	if got := l.principalTokens("bob"); got != 9 {
		t.Fatalf("bob's tokens after one admitted = %v, want 9", got)
	}
}

// When both buckets are empty the reason names the principal's (the one the caller controls)
// and Retry-After waits for whichever refills LAST, since the request needs a token from each.
func TestGateLimiterBothEmptyWaitsForTheSlowerBucket(t *testing.T) {
	clock := newFakeClock()
	l := newGateLimiter(testLimits(8, 1, 6, 2), clock.now)
	admit(t, l, "ci")
	admit(t, l, "ci") // process (2/min) empty; principal (6/min) has 4 left
	wantRefusal(t, l, "ci", true, gateRefuseProcessRate, 30)
	l2 := newGateLimiter(testLimits(8, 1, 2, 2), clock.now)
	admit(t, l2, "ci")
	admit(t, l2, "ci") // both empty, both 30 s
	wantRefusal(t, l2, "ci", true, gateRefusePrincipalRate, 30)
	l3 := newGateLimiter(testLimits(8, 1, 2, 60), clock.now)
	admit(t, l3, "ci")
	admit(t, l3, "ci") // principal empty (30 s), process has 58
	wantRefusal(t, l3, "ci", true, gateRefusePrincipalRate, 30)
}

// Retry-After is never below 1, even when a fraction of a second would do.
func TestGateLimiterRetryAfterNeverBelowOne(t *testing.T) {
	clock := newFakeClock()
	l := newGateLimiter(testLimits(8, 1, 600, 600), clock.now) // 10 tokens per second
	for i := 0; i < 600; i++ {
		admit(t, l, "ci")
	}
	wantRefusal(t, l, "ci", true, gateRefusePrincipalRate, 1)
	clock.advance(50 * time.Millisecond) // half a token: deficit 0.5 → 0.05 s → ceil 1
	wantRefusal(t, l, "ci", true, gateRefusePrincipalRate, 1)
	clock.advance(50 * time.Millisecond)
	admit(t, l, "ci")
}

// Per-principal state is bounded: a principal with nothing in flight and a full minute of
// idleness is swept on the next acquire after the sweep interval; one still in flight is kept.
func TestGateLimiterSweepsIdlePrincipals(t *testing.T) {
	clock := newFakeClock()
	l := newGateLimiter(testLimits(8, 2, 10, 60), clock.now)
	for i := 0; i < 50; i++ {
		admit(t, l, "p"+string(rune('A'+i%26))+string(rune('a'+i/26)))
	}
	held, ref := l.acquire("held", true)
	if ref != nil {
		t.Fatal(ref)
	}
	if n := l.principalCount(); n != 51 {
		t.Fatalf("principals before sweep = %d, want 51", n)
	}
	clock.advance(gateSweepEvery + time.Minute)
	admit(t, l, "fresh")                 // triggers the sweep
	if n := l.principalCount(); n != 2 { // "held" (in flight) and "fresh"
		t.Fatalf("principals after sweep = %d, want 2", n)
	}
	if _, ok := l.principals["held"]; !ok {
		t.Fatal("a principal with a request in flight was swept")
	}
	held()
}

// The clock never debits: a backwards step credits nothing and refuses nothing extra.
func TestGateLimiterClockStepBackwardsIsHarmless(t *testing.T) {
	clock := newFakeClock()
	l := newGateLimiter(testLimits(8, 1, 2, 60), clock.now)
	admit(t, l, "ci")
	clock.advance(-time.Hour)
	admit(t, l, "ci")
	wantRefusal(t, l, "ci", true, gateRefusePrincipalRate, 30)
	if got := l.principalTokens("ci"); got != 0 {
		t.Fatalf("tokens = %v, want 0", got)
	}
}
