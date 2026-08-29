package api

import (
	"math"
	"sync"
	"time"
)

// The §5a bounds of the reliability gate (func-reliability-gate.md; invariant 14), process-local
// by contract: every api/all replica enforces its own copies, so the cluster allowance scales
// with the replica count and no shared state sits in the database the bound protects.
//
// Two kinds of bound, taken in a fixed order so a refusal costs nothing it should not:
//
//  1. IN-FLIGHT PERMITS — the process cap, then the principal cap. A refusal here consumes no
//     rate token, carries `Retry-After: 1` and is `process_inflight` / `principal_inflight`.
//  2. RATE TOKENS — the principal bucket and the process bucket, checked AND debited under ONE
//     lock as a unit: if either is empty, neither is debited, the permit just taken is released,
//     `Retry-After` is ceil(seconds until the next token) and the reason names the empty bucket.
//
// Decisions take both; ledger reads take only the permits (§5 "Ledger reads take the in-flight
// permits of §5a, not the rate tokens"). Buckets refill lazily on access, so a bucket costs two
// numbers and no goroutine; idle principals are swept so the map is bounded by the number of
// principals seen in the last refill window, not by every principal ever seen.

// GateLimits are the five request-time bounds of §5a as the config loader validated them
// (gate.evaluate_*). The handler package does not import config: the CLI maps the loaded
// snapshot onto this struct, so a zero value here is a wiring error, never a default.
type GateLimits struct {
	// InflightProcess caps decisions in flight per process (evaluate_inflight_process).
	InflightProcess int
	// InflightPrincipal caps decisions in flight per principal (evaluate_inflight_principal).
	InflightPrincipal int
	// RatePrincipalPerMinute is the per-principal bucket's capacity and refill per minute.
	RatePrincipalPerMinute int
	// RateProcessPerMinute is the process bucket's capacity and refill per minute.
	RateProcessPerMinute int
	// TxBudget is the begin-through-commit budget of the decision transaction
	// (evaluate_tx_budget_ms), handed to the store unchanged.
	TxBudget time.Duration
}

// The four refusal reasons of §5a — the closed label set of
// cerbix_gate_evaluate_rejected_total{reason}.
const (
	gateRefuseProcessInflight   = "process_inflight"
	gateRefusePrincipalInflight = "principal_inflight"
	gateRefusePrincipalRate     = "principal_rate"
	gateRefuseProcessRate       = "process_rate"
)

// gateSweepEvery bounds how often acquire walks the principal map to drop idle entries.
const gateSweepEvery = time.Minute

// gateRefusal is one 429: the reason (a metric label) and the Retry-After in whole seconds,
// never below 1.
type gateRefusal struct {
	reason     string
	retryAfter int
}

// gateBucket is a token bucket refilled lazily: capacity and refill are both `perMinute`, so
// a full bucket holds one minute of allowance and refills at perMinute/60 tokens per second.
type gateBucket struct {
	tokens    float64
	perMinute float64
	last      time.Time
}

func newGateBucket(perMinute int, now time.Time) gateBucket {
	return gateBucket{tokens: float64(perMinute), perMinute: float64(perMinute), last: now}
}

// refill credits the tokens earned since the last access, capped at capacity. A clock that
// stepped backwards credits nothing rather than debiting.
func (b *gateBucket) refill(now time.Time) {
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = math.Min(b.perMinute, b.tokens+elapsed.Seconds()*b.perMinute/60)
	}
	b.last = now
}

// retryAfterTolerance absorbs float64 rounding in the deficit/rate quotient, so a wait that
// is exactly 5 s in arithmetic (and 5.000000000000001 in binary) is 5, not 6. One nanosecond
// of tolerance on a whole-second header changes no honest answer.
const retryAfterTolerance = 1e-9

// retryAfter is the whole seconds until the bucket next holds one token: ceil of the deficit
// over the refill rate, never below 1 (§5a), so two identical bursts see identical headers.
func (b gateBucket) retryAfter() int {
	deficit := 1 - b.tokens
	if deficit <= 0 {
		return 1
	}
	secs := math.Ceil(deficit/(b.perMinute/60) - retryAfterTolerance)
	if secs < 1 {
		return 1
	}
	return int(secs)
}

// gatePrincipal is one principal's in-flight count and bucket; lastSeen drives the sweep.
type gatePrincipal struct {
	inflight int
	bucket   gateBucket
	lastSeen time.Time
}

// gateLimiter is the process's one instance of the §5a bounds.
type gateLimiter struct {
	mu     sync.Mutex
	now    func() time.Time
	limits GateLimits

	processInflight int
	process         gateBucket
	principals      map[string]*gatePrincipal
	lastSweep       time.Time
}

func newGateLimiter(limits GateLimits, now func() time.Time) *gateLimiter {
	if now == nil {
		now = time.Now
	}
	t := now()
	return &gateLimiter{
		now:        now,
		limits:     limits,
		process:    newGateBucket(limits.RateProcessPerMinute, t),
		principals: map[string]*gatePrincipal{},
		lastSweep:  t,
	}
}

// acquire takes the in-flight permits (process, then principal) and — when rate is true — one
// token from each rate bucket as a unit. It returns a release function to call when the request
// is done, or a refusal. A refused request holds nothing: an in-flight refusal never touched a
// bucket, and a rate refusal has already released the permit it briefly held.
func (l *gateLimiter) acquire(principal string, rate bool) (release func(), refusal *gateRefusal) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)

	// 1. Permits: process, then principal. No bucket is touched on the way out.
	if l.processInflight >= l.limits.InflightProcess {
		return nil, &gateRefusal{reason: gateRefuseProcessInflight, retryAfter: 1}
	}
	p := l.principalLocked(principal, now)
	if p.inflight >= l.limits.InflightPrincipal {
		return nil, &gateRefusal{reason: gateRefusePrincipalInflight, retryAfter: 1}
	}

	// 2. Rate: both buckets refilled, both checked, and only then both debited — one unit.
	// A refusal names the first empty bucket and waits for the LAST one to refill, since the
	// request needs a token from each; the permits taken in step 1 are not taken at all.
	if rate {
		p.bucket.refill(now)
		l.process.refill(now)
		var ref *gateRefusal
		if p.bucket.tokens < 1 {
			ref = &gateRefusal{reason: gateRefusePrincipalRate, retryAfter: p.bucket.retryAfter()}
		}
		if l.process.tokens < 1 {
			if ref == nil {
				ref = &gateRefusal{reason: gateRefuseProcessRate}
			}
			ref.retryAfter = max(ref.retryAfter, l.process.retryAfter())
		}
		if ref != nil {
			return nil, ref
		}
		p.bucket.tokens--
		l.process.tokens--
	}

	l.processInflight++
	p.inflight++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.processInflight--
			if p.inflight > 0 {
				p.inflight--
			}
			p.lastSeen = l.now()
		})
	}, nil
}

// principalLocked returns the principal's state, creating it with a full bucket on first
// sight. Caller holds the lock.
func (l *gateLimiter) principalLocked(key string, now time.Time) *gatePrincipal {
	p, ok := l.principals[key]
	if !ok {
		p = &gatePrincipal{bucket: newGateBucket(l.limits.RatePrincipalPerMinute, now)}
		l.principals[key] = p
	}
	p.lastSeen = now
	return p
}

// sweepLocked drops principals with nothing in flight whose bucket has had a full minute to
// refill — indistinguishable from a principal never seen — at most once per gateSweepEvery.
// Caller holds the lock.
func (l *gateLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < gateSweepEvery {
		return
	}
	l.lastSweep = now
	for k, p := range l.principals {
		if p.inflight == 0 && now.Sub(p.lastSeen) >= time.Minute {
			delete(l.principals, k)
		}
	}
}

// principalCount is the size of the principal map — the quantity the sweep bounds.
func (l *gateLimiter) principalCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.principals)
}

// principalTokens reports a principal's current token count without refilling it, so a test
// can see exactly what a refusal did (or did not) debit. Zero for a principal never seen.
func (l *gateLimiter) principalTokens(key string) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p, ok := l.principals[key]; ok {
		return p.bucket.tokens
	}
	return 0
}
