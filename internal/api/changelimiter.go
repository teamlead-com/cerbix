package api

import "time"

// The §5a bounds of change intelligence (func-change-intelligence.md §5a; invariant 21), built
// on the gate's limiter so the order and the arithmetic are the same code: permits first, then
// the atomic bucket debit, `Retry-After = ceil(seconds to one token) ≥ 1`, the four codes
// `process_inflight | principal_inflight | process_rate | principal_rate`. Process-local by
// contract, as the gate's.
//
// Two pools, because §5a names two: `change.record_*` bounds `POST …/changes` (in-flight permits
// AND the two rate buckets), `change.read_inflight_process` bounds the timeline, the comparison
// and the incident-changes reads (permits only, no rate token — the ledger's rule). Neither pool
// has a per-principal in-flight key in §5a, so each is built with the principal cap equal to the
// process cap: the process check runs first and a principal never holds more permits than the
// process does, which makes `principal_inflight` unreachable here by construction rather than by
// a special case in the shared code.

// ChangeLimits are the four request-time bounds of §5a as the config loader validated them
// (change.record_* and change.read_inflight_process). The handler package does not import
// config: the CLI maps the loaded snapshot onto this struct, so a zero value here is a wiring
// error, never a default.
type ChangeLimits struct {
	// RecordInflightProcess caps records in flight per process (record_inflight_process).
	RecordInflightProcess int
	// RecordRatePrincipalPerMinute is the per-principal record bucket (record_rate_principal_per_minute).
	RecordRatePrincipalPerMinute int
	// RecordRateProcessPerMinute is the process record bucket (record_rate_process_per_minute).
	RecordRateProcessPerMinute int
	// ReadInflightProcess caps timeline/compare/incident-changes reads in flight per process
	// (read_inflight_process).
	ReadInflightProcess int
	// MaxPast / MaxFuture bound `occurred_at` against the DATABASE clock (change.max_past,
	// change.max_future); handed to the store unchanged, as the gate's TxBudget is.
	MaxPast   time.Duration
	MaxFuture time.Duration
}

// changeLimiter is the process's one instance of the change bounds: the record pool and the read
// pool, each a gate limiter.
type changeLimiter struct {
	limits ChangeLimits
	record *gateLimiter
	read   *gateLimiter
}

func newChangeLimiter(limits ChangeLimits, now func() time.Time) *changeLimiter {
	return &changeLimiter{
		limits: limits,
		record: newGateLimiter(GateLimits{
			InflightProcess:        limits.RecordInflightProcess,
			InflightPrincipal:      limits.RecordInflightProcess,
			RatePrincipalPerMinute: limits.RecordRatePrincipalPerMinute,
			RateProcessPerMinute:   limits.RecordRateProcessPerMinute,
		}, now),
		read: newGateLimiter(GateLimits{
			InflightProcess:   limits.ReadInflightProcess,
			InflightPrincipal: limits.ReadInflightProcess,
			// Reads take no rate token; the buckets exist only because the shared limiter has
			// them, and they are never consulted (acquire is always called with rate=false).
			RatePrincipalPerMinute: 1,
			RateProcessPerMinute:   1,
		}, now),
	}
}

// acquireRecord takes a record permit and one token from each record bucket, as a unit.
func (l *changeLimiter) acquireRecord(principal string) (func(), *gateRefusal) {
	return l.record.acquire(principal, true)
}

// acquireRead takes a read permit and nothing else.
func (l *changeLimiter) acquireRead(principal string) (func(), *gateRefusal) {
	return l.read.acquire(principal, false)
}
