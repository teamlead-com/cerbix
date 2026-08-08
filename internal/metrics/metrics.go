// Package metrics exposes cerbix runtime metrics in Prometheus text format.
//
// Metrics use the cerbix_ prefix and low-cardinality labels. The registry also
// tracks process readiness surfaced by the HTTP /readyz endpoint.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
)

// Registry holds process-level metrics and readiness state.
type Registry struct {
	mu              sync.RWMutex
	info            buildinfo.Info
	role            string
	startTime       time.Time
	ready           bool
	lastError       string
	dbEnabled       bool
	dbUp            bool
	checksUp        uint64
	checksDown      uint64
	incidentsOpened uint64
	outboxDelivered uint64
	outboxDead      uint64
	pullStats       []PullStat
	leaderTracked   bool
	leader          bool
	brokerTracked   bool
	brokerUp        bool
	// Result-ingest outcome counters (spec func-result-protocol). Low-cardinality: keyed by
	// a fixed reason/origin set, never by monitor_id/job_id (those go to logs).
	resultQuarantined map[string]uint64            // reason → count (future_timestamp)
	resultIgnored     map[string]uint64            // reason → count (out_of_order, outside_retention)
	resultRejected    map[string]uint64            // reason → count (missing_timestamp, stale_revision, missing_revision)
	resultClockSkew   map[string]map[string]uint64 // origin → reason → count (push future|past)
	resultMissingRev  uint64                       // observe-mode: scheduled result with no revision
	// File-provider metrics (spec func-monitoring-as-code §16). Keyed by the bounded provider
	// name only — never by file/project/monitor id.
	fileProviders map[string]*fileProviderStat
	now           func() time.Time
}

// fileProviderStat holds one file provider's exported gauges/counters.
type fileProviderStat struct {
	leader          bool
	reconciles      map[string]uint64 // outcome → count (applied|noop|rejected|error)
	lastDuration    float64
	lastSuccessUnix int64
	managed         int
	orphaned        int
	bundleErrors    int
}

// PullStat is one region's pull-queue depth and lag, exported as gauges.
type PullStat struct {
	Region     string
	Pending    int
	LagSeconds float64
}

// New creates a Registry for the given build info and role.
func New(info buildinfo.Info, role string) *Registry {
	return &Registry{
		info:      info,
		role:      role,
		startTime: time.Now(),
		now:       time.Now,
	}
}

// fileProvider returns (creating) the stat block for a provider.
func (r *Registry) fileProvider(name string) *fileProviderStat {
	if r.fileProviders == nil {
		r.fileProviders = map[string]*fileProviderStat{}
	}
	s := r.fileProviders[name]
	if s == nil {
		s = &fileProviderStat{reconciles: map[string]uint64{}}
		r.fileProviders[name] = s
	}
	return s
}

// SetFileProviderLeader records whether this process holds a provider's reconcile leadership.
func (r *Registry) SetFileProviderLeader(name string, leader bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fileProvider(name).leader = leader
}

// RecordFileProviderReconcile counts one reconcile by bounded outcome (applied|noop|rejected|error).
func (r *Registry) RecordFileProviderReconcile(name, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fileProvider(name).reconciles[outcome]++
}

// SetFileProviderStatus records the post-reconcile gauges. lastSuccessUnix is advanced only on
// a successful reconcile (0 leaves the previous value untouched).
func (r *Registry) SetFileProviderStatus(name string, durationSeconds float64, lastSuccessUnix int64, managed, orphaned, bundleErrors int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.fileProvider(name)
	s.lastDuration = durationSeconds
	if lastSuccessUnix > 0 {
		s.lastSuccessUnix = lastSuccessUnix
	}
	s.managed = managed
	s.orphaned = orphaned
	s.bundleErrors = bundleErrors
}

// SetReady marks the service ready or not-ready, recording an optional reason.
func (r *Registry) SetReady(ready bool, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ready = ready
	r.lastError = reason
}

// SetDatabaseUp records database connectivity. Calling it marks the database as
// enabled, so cerbix_database_up is only exported when a database is configured.
func (r *Registry) SetDatabaseUp(up bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dbEnabled = true
	r.dbUp = up
}

// RecordCheck counts a completed monitor check by outcome.
func (r *Registry) RecordCheck(up bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if up {
		r.checksUp++
	} else {
		r.checksDown++
	}
}

// RecordResultQuarantined counts a result set aside without touching state (e.g. a
// future-timestamp beyond skew). reason is a fixed low-cardinality label.
func (r *Registry) RecordResultQuarantined(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resultQuarantined == nil {
		r.resultQuarantined = map[string]uint64{}
	}
	r.resultQuarantined[reason]++
}

// RecordResultIgnored counts a result not applied to live state (out_of_order kept for
// SLA, or outside_retention dropped).
func (r *Registry) RecordResultIgnored(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resultIgnored == nil {
		r.resultIgnored = map[string]uint64{}
	}
	r.resultIgnored[reason]++
}

// RecordResultRejected counts a fail-closed reject (missing_timestamp, stale_revision,
// missing_revision) — no heartbeat inserted, no state change.
func (r *Registry) RecordResultRejected(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resultRejected == nil {
		r.resultRejected = map[string]uint64{}
	}
	r.resultRejected[reason]++
}

// RecordResultClockSkew counts an accepted-but-anomalous client clock (push only today):
// the result is still applied, this is adoption/diagnostic telemetry. reason is future|past.
func (r *Registry) RecordResultClockSkew(origin, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resultClockSkew == nil {
		r.resultClockSkew = map[string]map[string]uint64{}
	}
	if r.resultClockSkew[origin] == nil {
		r.resultClockSkew[origin] = map[string]uint64{}
	}
	r.resultClockSkew[origin][reason]++
}

// RecordResultMissingRevision counts a scheduled result with no revision that was applied
// under observe mode (the migration counter watched before switching to enforce).
func (r *Registry) RecordResultMissingRevision() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resultMissingRev++
}

// RecordResultOutcome routes a store result-outcome reason to its metric family (the
// single entry point callers use, so the reason→family mapping lives in one place). An
// empty reason (applied) or an unmapped one (e.g. "duplicate") is a no-op.
func (r *Registry) RecordResultOutcome(reason string) {
	switch reason {
	case "future_timestamp":
		r.RecordResultQuarantined(reason)
	case "out_of_order", "outside_retention":
		r.RecordResultIgnored(reason)
	case "missing_timestamp", "stale_revision", "missing_revision":
		r.RecordResultRejected(reason)
	}
}

// RecordIncidentOpened counts an incident opened through the API.
func (r *Registry) RecordIncidentOpened() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.incidentsOpened++
}

// RecordOutboxDelivered counts an outbox event delivered successfully.
func (r *Registry) RecordOutboxDelivered() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outboxDelivered++
}

// RecordOutboxDead counts an outbox event parked as dead after exhausting retries.
func (r *Registry) RecordOutboxDead() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outboxDead++
}

// SetPullStats replaces the per-region pull-queue gauge snapshot (leader-sampled).
func (r *Registry) SetPullStats(stats []PullStat) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pullStats = stats
}

// SetSchedulerLeader records whether this process currently holds scheduler
// leadership. Calling it marks leadership as tracked, so cerbix_scheduler_leader
// is only exported by a process that runs a scheduler (exactly one should read 1
// across the fleet — a persistent 0-everywhere or 2×1 is an alertable anomaly).
func (r *Registry) SetSchedulerLeader(leader bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaderTracked = true
	r.leader = leader
}

// SetBrokerUp records AMQP broker reachability. Calling it marks the broker as
// tracked, so cerbix_broker_up is only exported when the AMQP transport is in use
// (the inproc dev transport never calls it).
func (r *Registry) SetBrokerUp(up bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.brokerTracked = true
	r.brokerUp = up
}

// Ready reports whether the service is ready to serve.
func (r *Registry) Ready() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ready
}

// LastError returns the last recorded not-ready reason.
func (r *Registry) LastError() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastError
}

// WritePrometheus emits the current metrics in Prometheus text exposition format.
func (r *Registry) WritePrometheus(w io.Writer) {
	r.mu.RLock()
	ready := r.ready
	dbEnabled := r.dbEnabled
	dbUp := r.dbUp
	checksUp := r.checksUp
	checksDown := r.checksDown
	incidentsOpened := r.incidentsOpened
	outboxDelivered := r.outboxDelivered
	outboxDead := r.outboxDead
	pullStats := r.pullStats
	leaderTracked := r.leaderTracked
	leader := r.leader
	brokerTracked := r.brokerTracked
	brokerUp := r.brokerUp
	quarantined := copyCounts(r.resultQuarantined)
	ignored := copyCounts(r.resultIgnored)
	rejected := copyCounts(r.resultRejected)
	clockSkew := map[string]map[string]uint64{}
	for origin, byReason := range r.resultClockSkew {
		clockSkew[origin] = copyCounts(byReason)
	}
	missingRev := r.resultMissingRev
	fileProviders := map[string]fileProviderStat{}
	for name, s := range r.fileProviders {
		cp := fileProviderStat{leader: s.leader, reconciles: copyCounts(s.reconciles), lastDuration: s.lastDuration, lastSuccessUnix: s.lastSuccessUnix, managed: s.managed, orphaned: s.orphaned, bundleErrors: s.bundleErrors}
		fileProviders[name] = cp
	}
	uptime := r.now().Sub(r.startTime).Seconds()
	r.mu.RUnlock()

	fmt.Fprintln(w, "# HELP cerbix_build_info Build metadata for the running binary.")
	fmt.Fprintln(w, "# TYPE cerbix_build_info gauge")
	fmt.Fprintf(w, "cerbix_build_info{version=%q,commit=%q,go_version=%q,role=%q} 1\n",
		r.info.Version, r.info.Commit, r.info.GoVersion, r.role)

	fmt.Fprintln(w, "# HELP cerbix_up Always 1 while the process is serving.")
	fmt.Fprintln(w, "# TYPE cerbix_up gauge")
	fmt.Fprintln(w, "cerbix_up 1")

	fmt.Fprintln(w, "# HELP cerbix_ready Whether the service reports ready (1) or not (0).")
	fmt.Fprintln(w, "# TYPE cerbix_ready gauge")
	fmt.Fprintf(w, "cerbix_ready %d\n", b2i(ready))

	fmt.Fprintln(w, "# HELP cerbix_uptime_seconds Seconds since process start.")
	fmt.Fprintln(w, "# TYPE cerbix_uptime_seconds gauge")
	fmt.Fprintf(w, "cerbix_uptime_seconds %.3f\n", uptime)

	if dbEnabled {
		fmt.Fprintln(w, "# HELP cerbix_database_up Whether the database is reachable (1) or not (0).")
		fmt.Fprintln(w, "# TYPE cerbix_database_up gauge")
		fmt.Fprintf(w, "cerbix_database_up %d\n", b2i(dbUp))
	}

	fmt.Fprintln(w, "# HELP cerbix_checks_total Total monitor checks recorded, by result.")
	fmt.Fprintln(w, "# TYPE cerbix_checks_total counter")
	fmt.Fprintf(w, "cerbix_checks_total{result=\"up\"} %d\n", checksUp)
	fmt.Fprintf(w, "cerbix_checks_total{result=\"down\"} %d\n", checksDown)

	// Result-ingest outcomes (spec func-result-protocol). Only non-empty series are emitted.
	writeReasonCounter(w, "cerbix_result_quarantined_total",
		"Results set aside without touching state (by reason).", quarantined)
	writeReasonCounter(w, "cerbix_result_ignored_total",
		"Results not applied to live state (by reason).", ignored)
	writeReasonCounter(w, "cerbix_result_rejected_total",
		"Results fail-closed rejected with no insert (by reason).", rejected)
	if len(clockSkew) > 0 {
		fmt.Fprintln(w, "# HELP cerbix_result_clock_skew_total Accepted results with an anomalous client clock (by origin, reason).")
		fmt.Fprintln(w, "# TYPE cerbix_result_clock_skew_total counter")
		for _, origin := range sortedKeys(clockSkew) {
			for _, reason := range sortedKeys(clockSkew[origin]) {
				fmt.Fprintf(w, "cerbix_result_clock_skew_total{origin=%q,reason=%q} %d\n", origin, reason, clockSkew[origin][reason])
			}
		}
	}
	if missingRev > 0 {
		fmt.Fprintln(w, "# HELP cerbix_result_missing_revision_total Scheduled results with no revision applied under observe mode.")
		fmt.Fprintln(w, "# TYPE cerbix_result_missing_revision_total counter")
		fmt.Fprintf(w, "cerbix_result_missing_revision_total %d\n", missingRev)
	}

	fmt.Fprintln(w, "# HELP cerbix_incidents_opened_total Total incidents opened through the API.")
	fmt.Fprintln(w, "# TYPE cerbix_incidents_opened_total counter")
	fmt.Fprintf(w, "cerbix_incidents_opened_total %d\n", incidentsOpened)

	fmt.Fprintln(w, "# HELP cerbix_outbox_delivered_total Total outbox events delivered.")
	fmt.Fprintln(w, "# TYPE cerbix_outbox_delivered_total counter")
	fmt.Fprintf(w, "cerbix_outbox_delivered_total %d\n", outboxDelivered)

	fmt.Fprintln(w, "# HELP cerbix_outbox_dead_total Total outbox events parked as dead after exhausting retries.")
	fmt.Fprintln(w, "# TYPE cerbix_outbox_dead_total counter")
	fmt.Fprintf(w, "cerbix_outbox_dead_total %d\n", outboxDead)

	if leaderTracked {
		fmt.Fprintln(w, "# HELP cerbix_scheduler_leader Whether this process currently holds scheduler leadership (1) or is on standby (0).")
		fmt.Fprintln(w, "# TYPE cerbix_scheduler_leader gauge")
		fmt.Fprintf(w, "cerbix_scheduler_leader %d\n", b2i(leader))
	}

	if brokerTracked {
		fmt.Fprintln(w, "# HELP cerbix_broker_up Whether the AMQP broker is currently reachable (1) or not (0).")
		fmt.Fprintln(w, "# TYPE cerbix_broker_up gauge")
		fmt.Fprintf(w, "cerbix_broker_up %d\n", b2i(brokerUp))
	}

	if pullStats != nil {
		fmt.Fprintln(w, "# HELP cerbix_pull_jobs_pending Unclaimed pull jobs per region (HTTP-pull transport).")
		fmt.Fprintln(w, "# TYPE cerbix_pull_jobs_pending gauge")
		fmt.Fprintln(w, "# HELP cerbix_pull_agent_lag_seconds Age of the oldest unclaimed pull job per region.")
		fmt.Fprintln(w, "# TYPE cerbix_pull_agent_lag_seconds gauge")
		for _, s := range pullStats {
			fmt.Fprintf(w, "cerbix_pull_jobs_pending{region=%q} %d\n", s.Region, s.Pending)
			fmt.Fprintf(w, "cerbix_pull_agent_lag_seconds{region=%q} %.3f\n", s.Region, s.LagSeconds)
		}
	}

	if len(fileProviders) > 0 {
		fmt.Fprintln(w, "# HELP cerbix_file_provider_leader Whether this process holds a file provider's reconcile leadership.")
		fmt.Fprintln(w, "# TYPE cerbix_file_provider_leader gauge")
		fmt.Fprintln(w, "# HELP cerbix_file_provider_reconcile_total File-provider reconciles by outcome.")
		fmt.Fprintln(w, "# TYPE cerbix_file_provider_reconcile_total counter")
		fmt.Fprintln(w, "# HELP cerbix_file_provider_reconcile_duration_seconds Duration of the last reconcile.")
		fmt.Fprintln(w, "# TYPE cerbix_file_provider_reconcile_duration_seconds gauge")
		fmt.Fprintln(w, "# HELP cerbix_file_provider_last_success_timestamp_seconds Unix time of the last successful reconcile.")
		fmt.Fprintln(w, "# TYPE cerbix_file_provider_last_success_timestamp_seconds gauge")
		fmt.Fprintln(w, "# HELP cerbix_file_provider_managed_monitors Monitors currently owned by the provider.")
		fmt.Fprintln(w, "# TYPE cerbix_file_provider_managed_monitors gauge")
		fmt.Fprintln(w, "# HELP cerbix_file_provider_orphaned_monitors Owned monitors currently orphaned.")
		fmt.Fprintln(w, "# TYPE cerbix_file_provider_orphaned_monitors gauge")
		fmt.Fprintln(w, "# HELP cerbix_file_provider_bundle_errors Bundles/files rejected in the last scan.")
		fmt.Fprintln(w, "# TYPE cerbix_file_provider_bundle_errors gauge")
		for _, name := range sortedKeys(fileProviders) {
			s := fileProviders[name]
			fmt.Fprintf(w, "cerbix_file_provider_leader{provider=%q} %d\n", name, b2i(s.leader))
			for _, outcome := range sortedKeys(s.reconciles) {
				fmt.Fprintf(w, "cerbix_file_provider_reconcile_total{provider=%q,outcome=%q} %d\n", name, outcome, s.reconciles[outcome])
			}
			fmt.Fprintf(w, "cerbix_file_provider_reconcile_duration_seconds{provider=%q} %.3f\n", name, s.lastDuration)
			fmt.Fprintf(w, "cerbix_file_provider_last_success_timestamp_seconds{provider=%q} %d\n", name, s.lastSuccessUnix)
			fmt.Fprintf(w, "cerbix_file_provider_managed_monitors{provider=%q} %d\n", name, s.managed)
			fmt.Fprintf(w, "cerbix_file_provider_orphaned_monitors{provider=%q} %d\n", name, s.orphaned)
			fmt.Fprintf(w, "cerbix_file_provider_bundle_errors{provider=%q} %d\n", name, s.bundleErrors)
		}
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// copyCounts snapshots a reason→count map under the caller's lock.
func copyCounts(m map[string]uint64) map[string]uint64 {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]uint64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// sortedKeys returns a map's keys in a stable order (deterministic /metrics output).
func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// writeReasonCounter emits a {reason="…"} counter family, skipping it entirely when empty.
func writeReasonCounter(w io.Writer, name, help string, counts map[string]uint64) {
	if len(counts) == 0 {
		return
	}
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s counter\n", name)
	for _, reason := range sortedKeys(counts) {
		fmt.Fprintf(w, "%s{reason=%q} %d\n", name, reason, counts[reason])
	}
}
