// Package metrics exposes cerbix runtime metrics in Prometheus text format.
//
// Metrics use the cerbix_ prefix and low-cardinality labels. The registry also
// tracks process readiness surfaced by the HTTP /readyz endpoint.
package metrics

import (
	"fmt"
	"io"
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
	now             func() time.Time
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

	fmt.Fprintln(w, "# HELP cerbix_incidents_opened_total Total incidents opened through the API.")
	fmt.Fprintln(w, "# TYPE cerbix_incidents_opened_total counter")
	fmt.Fprintf(w, "cerbix_incidents_opened_total %d\n", incidentsOpened)

	fmt.Fprintln(w, "# HELP cerbix_outbox_delivered_total Total outbox events delivered.")
	fmt.Fprintln(w, "# TYPE cerbix_outbox_delivered_total counter")
	fmt.Fprintf(w, "cerbix_outbox_delivered_total %d\n", outboxDelivered)

	fmt.Fprintln(w, "# HELP cerbix_outbox_dead_total Total outbox events parked as dead after exhausting retries.")
	fmt.Fprintln(w, "# TYPE cerbix_outbox_dead_total counter")
	fmt.Fprintf(w, "cerbix_outbox_dead_total %d\n", outboxDead)

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
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
