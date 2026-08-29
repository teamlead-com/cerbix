package metrics

import (
	"errors"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"time"
)

// The FR-024 reliability-gate metric surface (docs/specs/func-reliability-gate.md D9, D10, §5a).
//
// Every family here has a CLOSED label set: the enumerations below are the whole vocabulary, and a
// recorder handed a value outside them returns ErrGateMetricLabel and records nothing. This surface
// is reachable from an authenticated pipeline on every decision, so a label that follows a caller's
// input — a principal, a service, a token name — would let a bug or a tenant grow /metrics without
// bound. None exists; the spec forbids one and the tests hold the line.

// ErrGateMetricLabel is returned when a gate recorder is handed a label value outside its closed
// set. The observation is dropped, never recorded under the offending value.
var ErrGateMetricLabel = errors.New("metrics: gate label value outside its closed set")

// gateActionNone is the `action` label of a NOT_CONFIGURED decision, which has no action (D9). The
// text format needs a well-formed value, so the absence is spelled "none" — and ONLY there: the
// recorder refuses "none" with any configured state and any real action with NOT_CONFIGURED.
const gateActionNone = "none"

// gateStateNotConfigured is the one state whose action is gateActionNone and that is never overridden.
const gateStateNotConfigured = "NOT_CONFIGURED"

var (
	gateStates = map[string]bool{
		"ALLOW": true, "WARN": true, "BLOCK": true, "UNKNOWN": true, gateStateNotConfigured: true,
	}
	gateActions = map[string]bool{
		"ALLOW": true, "WARN": true, "BLOCK": true, gateActionNone: true,
	}
	// §5a: the four process-local bounds an evaluation can be refused by, before evaluation.
	gateRejectReasons = map[string]bool{
		"process_inflight": true, "principal_inflight": true, "process_rate": true, "principal_rate": true,
	}
	// §5a: evaluation errors only — a maintenance failure must never move this family.
	gateEvaluateErrorKinds = map[string]bool{
		"snapshot_conflict": true, "timeout": true, "ledger_unwritable": true, "error": true,
	}
	// D10: the maintenance pass's own bounded family.
	gateMaintenanceErrorKinds = map[string]bool{
		"lock_timeout": true, "statement_timeout": true, "partition_identity": true, "error": true,
	}
)

// gateDurationBuckets are the fixed upper bounds of cerbix_gate_decision_duration_seconds
// (§5a: "a histogram over fixed buckets (0.05 … 30 s)"); +Inf is implicit.
var gateDurationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// gateDecisionKey is one series of cerbix_gate_decisions_total.
type gateDecisionKey struct {
	state, action string
	overridden    bool
}

// gateMetrics is the gate surface as held inside Registry, guarded by Registry.mu.
type gateMetrics struct {
	decisions   map[gateDecisionKey]uint64
	rejected    map[string]uint64 // reason → count
	evalErrors  map[string]uint64 // kind → count
	maintErrors map[string]uint64 // kind → count
	duration    histogram
	// ledger is the maintenance pass's sampled snapshot; nil means "not speaking" — a process
	// that has never run a pass, or one that lost the gate session, exports NO ledger gauge.
	ledger *gateLedgerStat
}

// gateLedgerStat holds the four D10 gauges, none computed by counting rows.
type gateLedgerStat struct {
	pendingDrop            int
	oldestAgeSeconds       float64
	writableHorizonSeconds float64
	bytes                  int64
}

// RecordGateDecision counts one gate decision by observed state, effective action, and whether an
// active override changed the action (D9). An override changes ONLY the action, so the state label
// stays the truth: {state="BLOCK",action="ALLOW",overridden="true"}. A NOT_CONFIGURED decision has
// no action; pass "" (or "none") and it is recorded as action="none". Any value outside the closed
// sets, "none" with a configured state, a real action with NOT_CONFIGURED, or an overridden
// NOT_CONFIGURED is refused with ErrGateMetricLabel and nothing is recorded.
func (r *Registry) RecordGateDecision(state, action string, overridden bool) error {
	if !gateStates[state] {
		return fmt.Errorf("%w: state %q", ErrGateMetricLabel, state)
	}
	if action == "" {
		action = gateActionNone
	}
	if !gateActions[action] {
		return fmt.Errorf("%w: action %q", ErrGateMetricLabel, action)
	}
	notConfigured := state == gateStateNotConfigured
	if (action == gateActionNone) != notConfigured {
		return fmt.Errorf("%w: action %q with state %q", ErrGateMetricLabel, action, state)
	}
	if notConfigured && overridden {
		return fmt.Errorf("%w: %s is never overridden", ErrGateMetricLabel, gateStateNotConfigured)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gate.decisions == nil {
		r.gate.decisions = map[gateDecisionKey]uint64{}
	}
	r.gate.decisions[gateDecisionKey{state: state, action: action, overridden: overridden}]++
	return nil
}

// RecordGateEvaluateRejected counts one evaluation refused by a process-local bound BEFORE
// evaluation (§5a): process_inflight | principal_inflight | process_rate | principal_rate.
func (r *Registry) RecordGateEvaluateRejected(reason string) error {
	if !gateRejectReasons[reason] {
		return fmt.Errorf("%w: rejected reason %q", ErrGateMetricLabel, reason)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gate.rejected = bumpCount(r.gate.rejected, reason)
	return nil
}

// RecordGateEvaluateError counts one admitted evaluation that failed, by kind (§5a):
// snapshot_conflict | timeout | ledger_unwritable | error. Evaluation errors ONLY — the maintenance
// pass has its own family, so an evaluation metric never moves without an evaluation.
func (r *Registry) RecordGateEvaluateError(kind string) error {
	if !gateEvaluateErrorKinds[kind] {
		return fmt.Errorf("%w: evaluate error kind %q", ErrGateMetricLabel, kind)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gate.evalErrors = bumpCount(r.gate.evalErrors, kind)
	return nil
}

// RecordGateMaintenanceError counts one decision-ledger maintenance statement refused or failed,
// by kind (D10): lock_timeout | statement_timeout | partition_identity | error. Each is retried on
// the next pass, never escalated to a longer wait; partition_identity pages.
func (r *Registry) RecordGateMaintenanceError(kind string) error {
	if !gateMaintenanceErrorKinds[kind] {
		return fmt.Errorf("%w: maintenance error kind %q", ErrGateMetricLabel, kind)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gate.maintErrors = bumpCount(r.gate.maintErrors, kind)
	return nil
}

// ObserveGateDecisionDuration records the wall time of one admitted evaluation, request to
// decision, into the fixed-bucket histogram of §5a.
func (r *Registry) ObserveGateDecisionDuration(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gate.duration.bounds == nil {
		r.gate.duration = newHistogram(gateDurationBuckets)
	}
	r.gate.duration.observe(d.Seconds())
}

// SetGateLedgerGauges publishes the maintenance pass's sampled snapshot of the decision ledger
// (D10): partitions pending drop, the oldest attached partition's age, the writable horizon, and
// the ledger's bytes — computed from the registry joined to the catalog, never by counting rows.
// Calling it marks the ledger as spoken for; until then, and after ClearGateLedgerGauges, the four
// gauges are absent.
func (r *Registry) SetGateLedgerGauges(pendingDrop int, oldestAgeSeconds, writableHorizonSeconds float64, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gate.ledger = &gateLedgerStat{
		pendingDrop:            pendingDrop,
		oldestAgeSeconds:       oldestAgeSeconds,
		writableHorizonSeconds: writableHorizonSeconds,
		bytes:                  bytes,
	}
}

// ClearGateLedgerGauges forgets the ledger gauges — called when the gate maintenance session is
// lost, because a deposed pass exporting the previous holder's horizon makes two scrapes disagree
// about one ledger. The counters and the histogram stay: they are this PROCESS's history, not a
// claim about the ledger's current state.
func (r *Registry) ClearGateLedgerGauges() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gate.ledger = nil
}

// bumpCount increments key in m, allocating m on first use, and returns the map.
func bumpCount(m map[string]uint64, key string) map[string]uint64 {
	if m == nil {
		m = map[string]uint64{}
	}
	m[key]++
	return m
}

// snapshot deep-copies the surface under the caller's lock so exposition runs unlocked.
func (g *gateMetrics) snapshot() gateMetrics {
	cp := gateMetrics{
		decisions:   maps.Clone(g.decisions),
		rejected:    copyCounts(g.rejected),
		evalErrors:  copyCounts(g.evalErrors),
		maintErrors: copyCounts(g.maintErrors),
		duration:    g.duration.clone(),
	}
	if g.ledger != nil {
		l := *g.ledger
		cp.ledger = &l
	}
	return cp
}

// write emits every gate family that has something to say, in a fixed order with sorted labels,
// so two scrapes with no new observation are byte-identical.
func (g gateMetrics) write(w *prometheusWriter) {
	if len(g.decisions) > 0 {
		w.println(`# HELP cerbix_gate_decisions_total Gate decisions by observed state, effective action and whether an active override changed the action (FR-024 D9); a NOT_CONFIGURED decision has no action and carries action="none".`)
		w.println("# TYPE cerbix_gate_decisions_total counter")
		keys := make([]gateDecisionKey, 0, len(g.decisions))
		for k := range g.decisions {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			a, b := keys[i], keys[j]
			if a.state != b.state {
				return a.state < b.state
			}
			if a.action != b.action {
				return a.action < b.action
			}
			return !a.overridden && b.overridden
		})
		for _, k := range keys {
			w.printf("cerbix_gate_decisions_total{state=%q,action=%q,overridden=%q} %d\n",
				k.state, k.action, strconv.FormatBool(k.overridden), g.decisions[k])
		}
	}
	writeLabelCounter(w, "cerbix_gate_evaluate_rejected_total", "reason",
		"Gate evaluations refused by a process-local bound before evaluation, by reason (FR-024 §5a).", g.rejected)
	writeLabelCounter(w, "cerbix_gate_evaluate_errors_total", "kind",
		"Admitted gate evaluations that failed, by kind — evaluation errors only, never maintenance (FR-024 §5a).", g.evalErrors)
	writeLabelCounter(w, "cerbix_gate_maintenance_errors_total", "kind",
		"Decision-ledger maintenance statements refused or failed, by kind; retried next pass, never escalated to a longer wait (FR-024 D10).", g.maintErrors)
	if g.duration.count > 0 {
		g.duration.write(w, "cerbix_gate_decision_duration_seconds",
			"Wall time of one admitted gate evaluation, request to decision, in seconds over fixed buckets 0.05 s to 30 s (FR-024 §5a).")
	}
	if l := g.ledger; l != nil {
		w.println("# HELP cerbix_gate_decisions_partitions_pending_drop Decision-ledger partitions attached past the retention cutoff plus detached but not yet dropped (FR-024 D10).")
		w.println("# TYPE cerbix_gate_decisions_partitions_pending_drop gauge")
		w.printf("cerbix_gate_decisions_partitions_pending_drop %d\n", l.pendingDrop)
		w.println("# HELP cerbix_gate_decisions_oldest_partition_age_seconds Age of the oldest attached decision partition's upper bound, in seconds; 0 when none is past the cutoff (FR-024 D10).")
		w.println("# TYPE cerbix_gate_decisions_oldest_partition_age_seconds gauge")
		w.printf("cerbix_gate_decisions_oldest_partition_age_seconds %.3f\n", l.oldestAgeSeconds)
		w.println("# HELP cerbix_gate_decisions_writable_horizon_seconds Seconds until the upper bound of the newest attached decision partition, from the registry and catalog; the ledger stops accepting decisions at 0 (FR-024 D10).")
		w.println("# TYPE cerbix_gate_decisions_writable_horizon_seconds gauge")
		w.printf("cerbix_gate_decisions_writable_horizon_seconds %.3f\n", l.writableHorizonSeconds)
		w.println("# HELP cerbix_gate_decisions_bytes Sum of pg_total_relation_size over decision-ledger partitions not yet dropped, in bytes (FR-024 D10).")
		w.println("# TYPE cerbix_gate_decisions_bytes gauge")
		w.printf("cerbix_gate_decisions_bytes %d\n", l.bytes)
	}
}

// writeLabelCounter emits a single-label counter family, skipping it entirely when empty.
func writeLabelCounter(w *prometheusWriter, name, label, help string, counts map[string]uint64) {
	if len(counts) == 0 {
		return
	}
	w.printf("# HELP %s %s\n", name, help)
	w.printf("# TYPE %s counter\n", name)
	for _, v := range sortedKeys(counts) {
		w.printf("%s{%s=%q} %d\n", name, label, v, counts[v])
	}
}

// histogram is a fixed-bucket Prometheus histogram. Counts are kept PER bucket and made cumulative
// at exposition, which is what the text format carries; the last slot of counts is the +Inf
// overflow. `le` is inclusive: an observation equal to a bound lands in that bound's bucket.
type histogram struct {
	bounds []float64 // ascending upper bounds, +Inf excluded
	counts []uint64  // len(bounds)+1
	sum    float64
	count  uint64
}

func newHistogram(bounds []float64) histogram {
	return histogram{bounds: bounds, counts: make([]uint64, len(bounds)+1)}
}

func (h *histogram) observe(v float64) {
	// SearchFloat64s returns the first index whose bound is >= v — exactly the `le` bucket; an
	// observation above every bound lands at len(bounds), the +Inf slot.
	h.counts[sort.SearchFloat64s(h.bounds, v)]++
	h.sum += v
	h.count++
}

func (h histogram) clone() histogram {
	if h.counts == nil {
		return h
	}
	cp := h
	cp.counts = make([]uint64, len(h.counts))
	copy(cp.counts, h.counts)
	return cp
}

// write renders the histogram: cumulative _bucket series ending in +Inf (which equals _count),
// then _sum and _count. Bounds are printed as the shortest float text that round-trips, so
// 0.05 stays "0.05" and 1 stays "1" — the spelling Prometheus itself uses for `le`.
func (h histogram) write(w *prometheusWriter, name, help string) {
	w.printf("# HELP %s %s\n", name, help)
	w.printf("# TYPE %s histogram\n", name)
	var cumulative uint64
	for i, bound := range h.bounds {
		cumulative += h.counts[i]
		w.printf("%s_bucket{le=%q} %d\n", name, strconv.FormatFloat(bound, 'g', -1, 64), cumulative)
	}
	cumulative += h.counts[len(h.bounds)]
	w.printf("%s_bucket{le=\"+Inf\"} %d\n", name, cumulative)
	w.printf("%s_sum %s\n", name, strconv.FormatFloat(h.sum, 'g', -1, 64))
	w.printf("%s_count %d\n", name, h.count)
}
