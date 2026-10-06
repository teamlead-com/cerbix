package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Windows longer than raw retention (func-sla-sli.md, D-0268 / iter-0199).
//
// Raw heartbeats live for `heartbeats.retention_days`; `heartbeats_daily` keeps one frozen row per
// (monitor, UTC day) forever. The raw part of every window is simply every raw heartbeat in
// [since, now). The ROLLUP part is the whole UTC days of the window that hold no raw heartbeat of
// that monitor at all — the days before the UTC day of its earliest retained heartbeat. Purging is
// day-aligned in both storage modes (one-day hypertable chunks; daily partitions and a
// DEFAULT-partition delete at a midnight cutoff), so that day is whole whenever older data was
// purged, and the two parts never overlap. The earliest raw heartbeat is NOT a retention boundary:
// raw days the rollup has not reached yet are raw and count as raw.
//
// ONE STATEMENT, ONE SNAPSHOT. Each reader computes its raw aggregate, the earliest raw heartbeat
// and the rollup part in a single statement, by embedding windowRollupCTEs. Two statements would see
// two database states: a purge landing between them advances the earliest raw heartbeat after the
// raw aggregate already counted the purged day, and the rollup then adds that day again (iter-0199
// re-review RR1-1). A single statement reads one snapshot under any isolation level, including the
// READ COMMITTED transactions of the weekly report and of burn evaluation.

// windowQuerier is satisfied by *pgxpool.Pool and pgx.Tx, so readers that run on a transaction
// (the weekly report, burn evaluation) keep doing so.
type windowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// windowRollupCTEs returns the CTEs `rs`, `roll` and `cov`, over a caller-defined CTE `scope(id)`
// listing the monitors. The arguments are the positional parameter numbers of: the window start S
// (timestamptz), ceil_to_utc_day(S), utc_day(S) and the start of today (all timestamptz; see
// windowDays).
//
//	rs(id, raw_start)               earliest retained raw heartbeat per monitor
//	roll(id, total, up, first_day)  rollup part: whole days [ceil_day(S), utc_day(raw_start)), or up
//	                                to today when there is no raw at all; only when raw_start > S
//	cov(id, complete)               raw at or before S, or a rollup day strictly before utc_day(S)
func windowRollupCTEs(sinceArg, ceilArg, dayArg, todayArg int) string {
	return fmt.Sprintf(`rs AS (
		    SELECT sc.id, (SELECT min(h.ts) FROM heartbeats h WHERE h.monitor_id = sc.id) AS raw_start
		      FROM scope sc
		), roll AS (
		    SELECT rs.id, sum(hd.total)::bigint AS total, sum(hd.up)::bigint AS up, min(hd.day) AS first_day
		      FROM rs
		      JOIN heartbeats_daily hd
		        ON hd.monitor_id = rs.id AND hd.total > 0
		       AND hd.day >= ($%[2]d::timestamptz AT TIME ZONE 'UTC')::date
		       AND hd.day < COALESCE((rs.raw_start AT TIME ZONE 'UTC')::date,
		                             ($%[4]d::timestamptz AT TIME ZONE 'UTC')::date)
		     WHERE rs.raw_start IS NULL OR rs.raw_start > $%[1]d
		     GROUP BY rs.id
		), cov AS (
		    SELECT rs.id,
		           (rs.raw_start IS NOT NULL AND rs.raw_start <= $%[1]d)
		           OR EXISTS (SELECT 1 FROM heartbeats_daily o
		                       WHERE o.monitor_id = rs.id AND o.total > 0
		                         AND o.day < ($%[3]d::timestamptz AT TIME ZONE 'UTC')::date) AS complete
		      FROM rs
		)`, sinceArg, ceilArg, dayArg, todayArg)
}

// scopeRollupColumns aggregates the CTEs over the whole scope, for single-scope readers.
const scopeRollupColumns = `COALESCE((SELECT sum(total) FROM roll), 0)::bigint,
		       COALESCE((SELECT sum(up) FROM roll), 0)::bigint,
		       ((SELECT min(first_day) FROM roll)::timestamp AT TIME ZONE 'UTC'),
		       COALESCE((SELECT bool_or(complete) FROM cov), false)`

// windowDays returns the day arguments windowRollupCTEs needs for a window starting at `since`.
func windowDays(since time.Time) (ceil, day, today time.Time) {
	return ceilUTCDay(since), utcDay(since), utcDay(time.Now())
}

// rollupPart is one scope's (or one monitor's) rollup contribution to a window.
type rollupPart struct {
	total, up int64
	// complete: raw data at or before the window start, or a rollup day strictly before the window
	// start's own UTC day. A rollup row FOR the start day proves nothing: it may hold only data after
	// the start.
	complete bool
	// firstDay is the earliest rollup day counted, zero when the rollup contributed nothing.
	firstDay time.Time
}

func newRollupPart(total, up int64, first *time.Time, complete bool) rollupPart {
	p := rollupPart{total: total, up: up, complete: complete}
	if first != nil {
		p.firstDay = utcDay(*first)
	}
	return p
}

func utcDay(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

func ceilUTCDay(t time.Time) time.Time {
	d := utcDay(t)
	if d.Equal(t.UTC()) {
		return d
	}
	return d.Add(24 * time.Hour)
}

// windowCoverage folds a rollup part into the raw aggregate read in the same statement: summed
// counts, DataFrom when no monitor in the scope has data older than the window, and LatencyFrom when
// the rollup reaches back further than the raw latency aggregates. `firstRaw` is the earliest raw
// heartbeat counted (nil if none).
func windowCoverage(p rollupPart, rawTotal, rawUp int64, firstRaw *time.Time) (total, up int64, dataFrom, latencyFrom *time.Time) {
	total, up = rawTotal+p.total, rawUp+p.up
	first := p.firstDay
	if firstRaw != nil && (first.IsZero() || utcDay(*firstRaw).Before(first)) {
		first = utcDay(*firstRaw)
	}
	if !p.complete && !first.IsZero() {
		d := first
		dataFrom = &d
	}
	if firstRaw != nil && !p.firstDay.IsZero() && utcDay(*firstRaw).After(p.firstDay) {
		d := utcDay(*firstRaw)
		latencyFrom = &d
	}
	return total, up, dataFrom, latencyFrom
}
