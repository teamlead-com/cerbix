package metrics

import (
	"errors"
	"fmt"
	"maps"
	"sort"
)

// The FR-025 change-intelligence metric surface (docs/specs/func-change-intelligence.md D15,
// invariant 20; iter-0165 changeset 3).
//
// Every family has a CLOSED label set and NO per-service, per-source or per-identity label — the
// same rule that keeps `job_id` out of labels: the record route is reachable from an authenticated
// pipeline on every deploy, and a label that followed its input would let one tenant grow /metrics
// without bound. A recorder handed a value outside its set returns ErrChangeMetricLabel and records
// nothing; the outbox-facing pair (no error return by its interface) drops the observation instead.

// ErrChangeMetricLabel is returned when a change recorder is handed a label value outside its
// closed set. The observation is dropped, never recorded under the offending value.
var ErrChangeMetricLabel = errors.New("metrics: change label value outside its closed set")

var (
	changeKinds  = map[string]bool{"deploy": true, "rollback": true, "flag": true}
	changePhases = map[string]bool{"started": true, "succeeded": true, "failed": true, "cancelled": true}
	// D15: the 400/409 codes a record can be refused with, plus the four §5a bounds (the change
	// surface has no separate rejected family for them, and an uncounted 429 is the invisible
	// load the gate's counter exists to show) and `body_invalid` for a refusal that has no closed
	// code — an unknown field, a missing field, a wrong type, an unparseable `occurred_at`.
	changeRejectReasons = map[string]bool{
		"phase_order": true, "phase_exists": true, "kind_mismatch": true, "decision_unknown": true,
		"occurred_at_before_start": true, "occurred_at_out_of_bounds": true,
		"source_invalid": true, "external_id_invalid": true, "ref_invalid": true, "url_invalid": true,
		"kind_invalid": true, "phase_invalid": true, "body_invalid": true,
		"process_inflight": true, "principal_inflight": true, "process_rate": true, "principal_rate": true,
	}
	// D7: the two roles a link can carry.
	changeLinkRoles = map[string]bool{"own_service": true, "upstream": true}
	// D8/D15: what one comparison response amounted to — `pending` when `after` is not yet
	// sealed, `withheld` when either side is withheld for any other reason, `figure` when both
	// sides are figures (the only case with a `delta`).
	changeCompareOutcomes = map[string]bool{"figure": true, "withheld": true, "pending": true}
)

// changeRecordedKey is one series of cerbix_changes_recorded_total.
type changeRecordedKey struct {
	kind, phase, outcome string
}

// changeMetrics is the change surface as held inside Registry, guarded by Registry.mu.
type changeMetrics struct {
	recorded          map[changeRecordedKey]uint64
	rejected          map[string]uint64 // reason → count
	correlations      map[string]uint64 // role → links inserted
	correlationErrors uint64
	correlationSeen   bool              // any correlation outcome observed: the error counter exports its zero
	compare           map[string]uint64 // outcome → count
	// retained is the retention pass's sampled row count; nil means "not speaking" — a process
	// that has never run the pass exports no gauge.
	retained *int64
}

// RecordChangeRecorded counts one accepted `POST …/changes` by kind, phase and outcome —
// `recorded` for a new row, `replayed` for an identical replay that wrote nothing (D3, D15).
func (r *Registry) RecordChangeRecorded(kind, phase string, replayed bool) error {
	if !changeKinds[kind] {
		return fmt.Errorf("%w: kind %q", ErrChangeMetricLabel, kind)
	}
	if !changePhases[phase] {
		return fmt.Errorf("%w: phase %q", ErrChangeMetricLabel, phase)
	}
	outcome := "recorded"
	if replayed {
		outcome = "replayed"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.change.recorded == nil {
		r.change.recorded = map[changeRecordedKey]uint64{}
	}
	r.change.recorded[changeRecordedKey{kind: kind, phase: phase, outcome: outcome}]++
	return nil
}

// RecordChangeRecordRejected counts one refused `POST …/changes` by the closed code it was
// refused with (D15): the domain's 400/409 codes, `body_invalid` for a shape refusal without a
// code, or one of the four §5a bounds.
func (r *Registry) RecordChangeRecordRejected(reason string) error {
	if !changeRejectReasons[reason] {
		return fmt.Errorf("%w: rejected reason %q", ErrChangeMetricLabel, reason)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.change.rejected = bumpCount(r.change.rejected, reason)
	return nil
}

// RecordChangeCorrelations counts n incident-change links INSERTED at one `opened` delivery, by
// role (D7, D15). Satisfies outbox.ChangeCorrelationMetrics, whose contract returns no error: a
// role outside {own_service, upstream} or a non-positive n is dropped.
func (r *Registry) RecordChangeCorrelations(role string, n int) {
	if n <= 0 || !changeLinkRoles[role] {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.change.correlations == nil {
		r.change.correlations = map[string]uint64{}
	}
	r.change.correlations[role] += uint64(n)
	r.change.correlationSeen = true
}

// RecordChangeCorrelationError counts one failed correlation attempt (D7 fail-open: the
// incident's delivery proceeded, the failure is counted here and logged).
func (r *Registry) RecordChangeCorrelationError() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.change.correlationErrors++
	r.change.correlationSeen = true
}

// RecordChangeCompare counts one served comparison by outcome (D8, D15): `figure` (both sides
// stated), `withheld` (a side withheld with a reason other than pending), `pending` (`after` not
// yet sealed).
func (r *Registry) RecordChangeCompare(outcome string) error {
	if !changeCompareOutcomes[outcome] {
		return fmt.Errorf("%w: compare outcome %q", ErrChangeMetricLabel, outcome)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.change.compare = bumpCount(r.change.compare, outcome)
	return nil
}

// SetChangesRetained publishes the retention pass's sample of `service_changes` rows kept (D9,
// D15). Calling it marks the gauge as spoken for; until then, and after ClearChangesRetained, it
// is absent.
func (r *Registry) SetChangesRetained(n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.change.retained = &n
}

// ClearChangesRetained forgets the gauge — called on scheduler leadership loss, because a
// deposed leader exporting the previous pass's count makes two scrapes disagree about one table.
// The counters stay: they are this PROCESS's history.
func (r *Registry) ClearChangesRetained() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.change.retained = nil
}

// snapshot deep-copies the surface under the caller's lock so exposition runs unlocked.
func (c *changeMetrics) snapshot() changeMetrics {
	cp := changeMetrics{
		recorded:          maps.Clone(c.recorded),
		rejected:          copyCounts(c.rejected),
		correlations:      copyCounts(c.correlations),
		correlationErrors: c.correlationErrors,
		correlationSeen:   c.correlationSeen,
		compare:           copyCounts(c.compare),
	}
	if c.retained != nil {
		n := *c.retained
		cp.retained = &n
	}
	return cp
}

// write emits every change family that has something to say, in a fixed order with sorted
// labels, so two scrapes with no new observation are byte-identical.
func (c changeMetrics) write(w *prometheusWriter) {
	if len(c.recorded) > 0 {
		w.println(`# HELP cerbix_changes_recorded_total Change phases accepted by POST …/changes, by kind, phase and outcome — recorded (a new row) or replayed (an identical replay that wrote nothing) (FR-025 D3, D15).`)
		w.println("# TYPE cerbix_changes_recorded_total counter")
		keys := make([]changeRecordedKey, 0, len(c.recorded))
		for k := range c.recorded {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			a, b := keys[i], keys[j]
			if a.kind != b.kind {
				return a.kind < b.kind
			}
			if a.phase != b.phase {
				return a.phase < b.phase
			}
			return a.outcome < b.outcome
		})
		for _, k := range keys {
			w.printf("cerbix_changes_recorded_total{kind=%q,phase=%q,outcome=%q} %d\n", k.kind, k.phase, k.outcome, c.recorded[k])
		}
	}
	writeLabelCounter(w, "cerbix_change_record_rejected_total", "reason",
		"Change records refused, by the closed code they were refused with — the 400/409 codes, body_invalid for a shape refusal, or one of the four §5a bounds (FR-025 D15).", c.rejected)
	if c.correlationSeen {
		writeLabelCounter(w, "cerbix_change_correlations_total", "role",
			"Incident-change links inserted at a service auto-incident's opened delivery, by role (FR-025 D7).", c.correlations)
		w.println("# HELP cerbix_change_correlation_errors_total Failed change-correlation attempts; the incident opened and resolves regardless (FR-025 D7 fail-open).")
		w.println("# TYPE cerbix_change_correlation_errors_total counter")
		w.printf("cerbix_change_correlation_errors_total %d\n", c.correlationErrors)
	}
	writeLabelCounter(w, "cerbix_change_compare_total", "outcome",
		"Before/after comparisons served, by outcome — figure (both sides stated), withheld (a side withheld), pending (after not yet sealed) (FR-025 D8).", c.compare)
	if c.retained != nil {
		w.println("# HELP cerbix_changes_retained Rows of service_changes kept after the last retention pass, sampled by the scheduler leader (FR-025 D9).")
		w.println("# TYPE cerbix_changes_retained gauge")
		w.printf("cerbix_changes_retained %d\n", *c.retained)
	}
}
