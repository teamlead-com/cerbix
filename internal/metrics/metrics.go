// Package metrics exposes cerbix runtime metrics in Prometheus text format.
//
// Metrics use the cerbix_ prefix and low-cardinality labels. The registry also
// tracks process readiness surfaced by the HTTP /readyz endpoint.
package metrics

import (
	"fmt"
	"io"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
)

// Registry holds process-level metrics and readiness state.
type Registry struct {
	mu                  sync.RWMutex
	info                buildinfo.Info
	role                string
	startTime           time.Time
	ready               bool
	lastError           string
	credentialTracked   bool
	credentialReady     bool
	dispatchSharedTrust bool
	dbEnabled           bool
	dbUp                bool
	checksUp            uint64
	checksDown          uint64
	incidentsOpened     uint64
	outboxDelivered     uint64
	outboxDead          uint64
	// Impact correlation (FR-021 §14.6): links counted on INSERTED rows only
	// (role → count, fixed two-role cardinality); failures are failed correlation
	// attempts (each also rides the outbox retry envelope).
	impactLinks           map[string]uint64
	impactFailures        uint64
	impactWitnessOverflow uint64
	// Alerting ownership (FR-021 §16.6b). Fixed, low-cardinality labels only: this surface is
	// reachable by anyone who can create a service, so per-tenant labels would let them grow the
	// metrics endpoint.
	alertSuppressed    map[string]uint64 // "topic|reason" → count
	delegationFailOpen map[string]uint64 // reason → count
	// The EVALUATOR half of the same §16.6b table. `signal` is the only dimension the alerting
	// surface ever grows along, and it has exactly two values (health, burn) — no service, no
	// target, no rule, no tenant.
	serviceAlertEvals   map[string]map[string]uint64 // signal → outcome → count (ok|error|skipped)
	serviceAlertEmitted map[string]map[string]uint64 // signal → edge → count (onset|close)
	// The DELIVERY side of §16.6b: what delegation concluded per signal, and the two ways an
	// announcement can fail to reach anybody without failing to deliver.
	serviceDelegation       map[string]map[string]uint64 // signal → state → count
	serviceAlertUndeliver   map[string]uint64            // signal → count
	serviceRecipientMissing uint64
	// Set on every SUCCESSFUL pass of an arm. A stalled evaluator has to read as lag rather than
	// as an absence of alerts, which is indistinguishable from "nothing is wrong" (§16.7).
	serviceAlertLastSuccess map[string]int64   // signal → unix seconds
	serviceAlertLag         map[string]float64 // signal → seconds
	serviceAlertStats       *ServiceAlertStat
	// serviceAlertStalls holds the signals whose evaluation lag exceeded lease_multiplier ×
	// cadence, keyed by signal so both arms can be stalled at once and each recovers on its own
	// pass. Presence IS the stall; the value is the readiness reason (§16.6b).
	serviceAlertStalls map[string]string
	// Status-page projection (FR-021 §15.0, invariant 71a): a component whose ACTIVE service the
	// projection could not read. With the RESTRICT FK it cannot legitimately be absent, so this is
	// a failed READ, and it is counted rather than only logged because the public page's rendering
	// of it is deliberately indistinguishable from a calm value to a customer's eye.
	statusPageUnreadable uint64
	pullStats            []PullStat
	serviceStats         *ServiceReliabilityStat
	serviceSlices        map[string]uint64 // outcome → count (worked|empty|error)
	serviceWedgedSet     bool
	serviceWedged        bool
	serviceWedgedReason  string
	factMaintTracked     bool
	factMaintFailing     bool
	factMaintLastOKUnix  int64
	// Service-reliability EVENT counters (§21): fan-out, terminal rejections, lifecycle
	// outcomes, late-excluded arrivals. Monotonic — unlike a gauge over a table sum, they
	// cannot decrease when a service (and its rows) is deleted.
	serviceRejections     uint64                       // §21: mutations refused as unrecomputable
	serviceRepairOutcomes map[string]map[string]uint64 // outcome → reason → count
	leaderTracked         bool
	leader                bool
	brokerTracked         bool
	brokerUp              bool
	// Result-ingest outcome counters (spec func-result-protocol). Low-cardinality: keyed by
	// a fixed reason/origin set, never by monitor_id/job_id (those go to logs).
	resultQuarantined         map[string]uint64            // reason → count (future_timestamp)
	resultIgnored             map[string]uint64            // reason → count (out_of_order, outside_retention)
	resultRejected            map[string]uint64            // reason → count (missing_timestamp, stale_revision, missing_revision)
	resultClockSkew           map[string]map[string]uint64 // origin → reason → count (push future|past)
	resultMissingRev          uint64                       // observe-mode: scheduled result with no revision
	executorProbeErrors       map[string]uint64            // bounded reason → count
	secretResolutionFailures  map[string]uint64            // bounded reason → count
	dispatchTransportFailures map[string]uint64            // bounded reason → count
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

// ServiceReliabilityStat is the service-reliability subsystem's operational snapshot,
// exported as gauges: durable repair queue depth by state, and the worst watermark lag. The
// lag counts from era_start for a service that has sealed nothing yet — that IS a lag — and
// its steady state sits around the late-arrival grace plus one tick, never zero.
type ServiceReliabilityStat struct {
	RepairPending       int
	RepairRunning       int
	RepairErrored       int
	WatermarkLagSeconds float64
	// Sampled from the persisted service_metric_events aggregate, which the OWNING
	// transactions maintain — monotonic by construction, exported as counters.
	EpochFanoutTotal  int64
	LateArrivalsTotal int64
	LateOverflowTotal int64
	// ObservedBeforeIssueTotal counts scheduled results ACCEPTED despite an observation instant
	// before their job's issue instant — inside `result.allowed_skew`. A region's clock trailing
	// the core's is ordinary one at a time and worth watching before it grows past the tolerance,
	// where results start being rejected (func-result-protocol §9, iter-0155).
	ObservedBeforeIssueTotal int64
}

// ServiceAlertStat is the alerting evaluator's sampled snapshot (FR-021 §16.6b), exported as
// gauges: open episodes and the evaluation backlog, PER SIGNAL and nothing finer.
//
// Deliberately four scalars rather than a map keyed by anything the caller controls: this surface
// is reachable by anyone who can create a service, and a per-tenant or per-service breakdown would
// let them grow the metrics endpoint without limit.
type ServiceAlertStat struct {
	// Open (unclosed) service_alert_episodes rows.
	ActiveHealth int
	ActiveBurn   int
	// Owning services / enabled burn targets DUE for evaluation — lease expired on the DB clock,
	// or never evaluated at all. Backlog that does not drain is the same wedge the lag gauge
	// reports from the other side.
	BacklogHealth int
	BacklogBurn   int
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

// SetFileProviderReconcileStats updates the duration / last-success / bundle-error gauges when
// the owned COUNTS are unknown (a degraded scan or a failed counts lookup), WITHOUT clobbering
// the last-known managed/orphaned gauges to a misleading zero (§16). lastSuccessUnix==0 leaves
// the previous success untouched.
func (r *Registry) SetFileProviderReconcileStats(name string, durationSeconds float64, lastSuccessUnix int64, bundleErrors int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.fileProvider(name)
	s.lastDuration = durationSeconds
	if lastSuccessUnix > 0 {
		s.lastSuccessUnix = lastSuccessUnix
	}
	s.bundleErrors = bundleErrors
}

// SetReady marks the service ready or not-ready, recording an optional reason.
func (r *Registry) SetReady(ready bool, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ready = ready
	r.lastError = reason
}

// SetCredentialReady tracks the executor's dispatch-key health independently from base
// process/database readiness. A persistent envelope authentication failure makes /readyz
// fail without letting a later generic SetReady(true) erase the component failure.
func (r *Registry) SetCredentialReady(ready bool, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.credentialTracked = true
	r.credentialReady = ready
	if !ready {
		r.lastError = reason
	}
}

// SetDispatchSharedTrust surfaces the explicitly acknowledged wider dispatch-key trust
// domain from static config (func-secret-inventory §4.7 / D-0155).
func (r *Registry) SetDispatchSharedTrust(shared bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dispatchSharedTrust = shared
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

// RecordExecutorProbeError counts a typed executor diagnostic. Reasons are validated at
// ingest/store before this boundary and never include monitor/job/key identifiers.
func (r *Registry) RecordExecutorProbeError(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.executorProbeErrors == nil {
		r.executorProbeErrors = map[string]uint64{}
	}
	r.executorProbeErrors[reason]++
}

// RecordDispatchTransportFailure counts a failure to hand a materialized job to its
// transport. It is a SEPARATE family from secret resolution on purpose (§4.4.5): a broker
// that will not take a publish and a credential that will not resolve need different
// responses from an operator, and folding them together hides whichever is rarer.
func (r *Registry) RecordDispatchTransportFailure(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dispatchTransportFailures == nil {
		r.dispatchTransportFailures = map[string]uint64{}
	}
	r.dispatchTransportFailures[reason]++
}

func (r *Registry) RecordSecretResolutionFailure(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.secretResolutionFailures == nil {
		r.secretResolutionFailures = map[string]uint64{}
	}
	r.secretResolutionFailures[reason]++
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

// RecordImpactLinks counts NEWLY inserted incident-service impact links by role
// (FR-021 §14.6) — redeliveries insert nothing and therefore count nothing.
func (r *Registry) RecordImpactLinks(role string, n int) {
	if n <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.impactLinks == nil {
		r.impactLinks = map[string]uint64{}
	}
	r.impactLinks[role] += uint64(n)
}

// RecordImpactFailure counts a failed correlation attempt (the WARN path made
// countable; the attempt itself retries through the outbox).
func (r *Registry) RecordImpactFailure() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.impactFailures++
}

// RecordImpactWitnessOverflow counts incidents beyond the per-service witness
// bound of a correlation attempt ([278]) — a deterministic durable-fact
// omission that must be visible. Plain counter, no tenant/project labels.
func (r *Registry) RecordImpactWitnessOverflow(n int) {
	if n <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.impactWitnessOverflow += uint64(n)
}

// RecordAlertSuppressed counts a monitor alert withheld because a service actively covers that
// signal. A suppressed delivery leaves NO other trace — the outbox row is marked delivered and no
// channel sees it — so this counter and the `alert_suppressions` rows are the only way to answer
// "did anyone get told?" after an incident.
func (r *Registry) RecordAlertSuppressed(topic, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.alertSuppressed == nil {
		r.alertSuppressed = map[string]uint64{}
	}
	r.alertSuppressed[topic+"|"+reason]++
}

// RecordDelegationFailOpen counts a delivery that PAGED because coverage could not be confirmed.
// It is not an error counter: "no_active_owner" is the overwhelmingly common value and means the
// system is behaving exactly as designed. What makes it worth collecting is the OTHER values —
// stale, error, unroutable — which say members are paging for themselves because a replacement
// went quiet.
// RecordServiceDelegation counts what delegation concluded for one signal: `armed` (a member's
// alert was suppressed because a replacement is active), `disarmed` (no active replacement, so the
// member paged for itself) or `degraded` (the lookup could not conclude, which also pages).
func (r *Registry) RecordServiceDelegation(signal, state string) {
	if signal == "" || state == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.serviceDelegation == nil {
		r.serviceDelegation = map[string]map[string]uint64{}
	}
	if r.serviceDelegation[signal] == nil {
		r.serviceDelegation[signal] = map[string]uint64{}
	}
	r.serviceDelegation[signal][state]++
}

// RecordServiceAlertUndeliverable counts an announcement with nobody left to tell — an empty
// recipient snapshot, or one whose every channel has since gone. It is not a delivery failure and
// there is nothing to retry, which is exactly why it needs a counter of its own: otherwise it is
// indistinguishable from a successful page.
func (r *Registry) RecordServiceAlertUndeliverable(signal string) {
	if signal == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.serviceAlertUndeliver == nil {
		r.serviceAlertUndeliver = map[string]uint64{}
	}
	r.serviceAlertUndeliver[signal]++
}

// RecordServiceAlertRecipientMissing counts snapshot recipients whose channel no longer exists.
// §16.4a: they are COUNTED, never silently replaced by whoever is on call now — replacing one would
// page a stranger and leave the person who heard the onset holding an alert nothing will close.
func (r *Registry) RecordServiceAlertRecipientMissing(n int) {
	if n <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.serviceRecipientMissing += uint64(n)
}

func (r *Registry) RecordDelegationFailOpen(reason string) {
	if reason == "" {
		reason = "unspecified"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.delegationFailOpen == nil {
		r.delegationFailOpen = map[string]uint64{}
	}
	r.delegationFailOpen[reason]++
}

// RecordServiceAlertEvaluations counts n units of alerting work reaching one outcome, for one
// signal (FR-021 §16.6b). The UNIT is the thing that gets a verdict: a service for the live arm, a
// burn RULE for the sealed arm — and `error` additionally counts the whole pass when the arm itself
// fails, because a slice that rolled back evaluated nothing at all.
//
// n == 0 is recorded rather than dropped: it materializes the series, so `outcome="error"` exists
// at 0 from the first healthy pass and an alerting rule on it does not have to survive a missing
// series before it can fire.
func (r *Registry) RecordServiceAlertEvaluations(signal, outcome string, n int) {
	if n < 0 {
		n = 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.serviceAlertEvals == nil {
		r.serviceAlertEvals = map[string]map[string]uint64{}
	}
	if r.serviceAlertEvals[signal] == nil {
		r.serviceAlertEvals[signal] = map[string]uint64{}
	}
	r.serviceAlertEvals[signal][outcome] += uint64(n)
}

// RecordServiceAlertEmitted counts n alert EDGES enqueued for one signal (onset|close). Edges, not
// deliveries: the outbox is at-least-once and owns its own counters, and conflating the two would
// make a retried delivery look like a second onset.
func (r *Registry) RecordServiceAlertEmitted(signal, edge string, n int) {
	if n < 0 {
		n = 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.serviceAlertEmitted == nil {
		r.serviceAlertEmitted = map[string]map[string]uint64{}
	}
	if r.serviceAlertEmitted[signal] == nil {
		r.serviceAlertEmitted[signal] = map[string]uint64{}
	}
	r.serviceAlertEmitted[signal][edge] += uint64(n)
}

// SetServiceAlertPass records one SUCCESSFUL evaluation pass of a signal: when it happened and how
// far behind the stalest verdict it found was. Both are gauges of the LAST success — a failed pass
// leaves them untouched on purpose, so an aging last-success next to a live process is exactly the
// stalled-evaluator symptom the §16.6b runbook names.
func (r *Registry) SetServiceAlertPass(signal string, lastSuccessUnix int64, lagSeconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.serviceAlertLastSuccess == nil {
		r.serviceAlertLastSuccess = map[string]int64{}
	}
	if r.serviceAlertLag == nil {
		r.serviceAlertLag = map[string]float64{}
	}
	r.serviceAlertLastSuccess[signal] = lastSuccessUnix
	r.serviceAlertLag[signal] = lagSeconds
}

// SetServiceAlertStats records the sampled active/backlog snapshot. Calling it marks the subsystem
// as tracked, so the gauges are exported only by a process that actually runs the leader loop.
func (r *Registry) SetServiceAlertStats(st ServiceAlertStat) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.serviceAlertStats = &st
}

// SetServiceAlertStalled marks one alerting signal's evaluator stalled or healthy — §16.6b: an
// evaluation whose lag exceeds lease_multiplier × cadence marks the SCHEDULER not-ready, because a
// stalled evaluator is exactly the state in which delegation dis-arms and members resume paging.
//
// Component readiness, mirroring SetCredentialReady and SetServiceWedged: a later generic
// SetReady(true) cannot erase it. It is set ONLY from the scheduler's leader loop, which is what
// keeps it off the API's readiness — taking the API out of rotation for an alerting stall would
// turn a degradation into an outage.
func (r *Registry) SetServiceAlertStalled(signal string, stalled bool, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !stalled {
		delete(r.serviceAlertStalls, signal)
		return
	}
	if r.serviceAlertStalls == nil {
		r.serviceAlertStalls = map[string]string{}
	}
	r.serviceAlertStalls[signal] = reason
}

// RecordStatusPageUnreadableComponent counts a status-page component whose active service could
// not be read. Plain counter: no page or tenant labels, because an unauthenticated surface must not
// grow per-tenant metric cardinality for anyone who can request a page.
func (r *Registry) RecordStatusPageUnreadableComponent() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statusPageUnreadable++
}

// SetPullStats replaces the per-region pull-queue gauge snapshot (leader-sampled).
// SetServiceFactMaintenance records the fact-partition maintenance pass outcome: the [142]
// stuck-month signal, low-cardinality by construction — one boolean and one timestamp, never
// a month label. "Failing" repeating while last-success ages IS the stuck month.
func (r *Registry) SetServiceFactMaintenance(ok bool, nowUnix int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.factMaintTracked {
		// The tracking start is the last-success FLOOR: without it, the very first
		// transient failure exported last_success=0 and the runbook's age-based alert
		// fired instantly on a startup blip instead of after 30 minutes of failure.
		r.factMaintLastOKUnix = nowUnix
	}
	r.factMaintTracked = true
	r.factMaintFailing = !ok
	if ok {
		r.factMaintLastOKUnix = nowUnix
	}
}

// SetServiceWedged marks the service-reliability subsystem wedged or healthy — §21: the
// scheduler's readiness must not report healthy while service work is wedged. Component
// readiness, mirroring SetCredentialReady: a later generic SetReady(true) cannot erase it.
func (r *Registry) SetServiceWedged(wedged bool, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.serviceWedgedSet = true
	r.serviceWedged = wedged
	r.serviceWedgedReason = reason
}

// ClearServiceReliabilityStats forgets the service gauges AND the wedged component — called
// on leadership loss, because a deposed scheduler exporting the old leader's queue depth and
// wedge verdict makes two scrapes disagree about one cluster.
func (r *Registry) ClearServiceReliabilityStats() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.serviceStats = nil
	r.serviceWedgedSet = false
	r.serviceWedged = false
	r.serviceWedgedReason = ""
	r.factMaintTracked = false
	r.factMaintFailing = false
	r.factMaintLastOKUnix = 0
	// The alerting evaluator's SAMPLED state goes with them, for both halves of the same
	// argument: a deposed leader exporting the old leader's backlog makes two scrapes disagree
	// about one cluster, and a stale stall verdict would hold a standby's /readyz down for an
	// evaluation it is no longer running. The monotonic counters stay — they are this PROCESS's
	// history, not a claim about the cluster's current state.
	r.serviceAlertStats = nil
	r.serviceAlertStalls = nil
	r.serviceAlertLastSuccess = nil
	r.serviceAlertLag = nil
}

// RecordServiceRepairOutcome counts one repair/recompute range reaching a lifecycle outcome,
// attributed by the range's REASON — declaration, epoch, late_data, maintenance, admin,
// backfill — which is what tells a repair from a recompute (§21). Both label sets are
// bounded enums. Recorded commit-independently: each lifecycle write commits its own
// transaction before this fires.
func (r *Registry) RecordServiceRepairOutcome(outcome, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.serviceRepairOutcomes == nil {
		r.serviceRepairOutcomes = make(map[string]map[string]uint64)
	}
	if r.serviceRepairOutcomes[outcome] == nil {
		r.serviceRepairOutcomes[outcome] = make(map[string]uint64)
	}
	r.serviceRepairOutcomes[outcome][reason]++
}

// RecordServiceUnrecomputableRejection counts one mutation REFUSED because its range is
// unrecomputable (ErrUnrecomputableRange at preview or confirm) — the §21 rejection counter,
// distinct from a repair range being parked on evidence loss.
func (r *Registry) RecordServiceUnrecomputableRejection() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.serviceRejections++
}

// RecordServiceSlice counts one leader service-reliability slice by outcome. Bounded label
// set: worked, empty, error.
func (r *Registry) RecordServiceSlice(outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.serviceSlices == nil {
		r.serviceSlices = make(map[string]uint64)
	}
	r.serviceSlices[outcome]++
}

// SetServiceReliabilityStats records the service subsystem's queue/watermark snapshot.
// Calling it marks the subsystem as tracked, so the gauges are exported only by a process
// that actually runs the leader loop.
func (r *Registry) SetServiceReliabilityStats(st ServiceReliabilityStat) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.serviceStats = &st
}

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
	return r.ready && (!r.credentialTracked || r.credentialReady) &&
		(!r.serviceWedgedSet || !r.serviceWedged) && len(r.serviceAlertStalls) == 0
}

// LastError returns the last recorded not-ready reason.
func (r *Registry) LastError() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.serviceWedgedSet && r.serviceWedged {
		return r.serviceWedgedReason
	}
	if len(r.serviceAlertStalls) > 0 {
		// Deterministic when both arms are stalled: /readyz must not alternate its reason
		// between scrapes for one unchanged state.
		return r.serviceAlertStalls[sortedKeys(r.serviceAlertStalls)[0]]
	}
	if r.credentialTracked && !r.credentialReady {
		return "credential envelope decrypt unavailable"
	}
	return r.lastError
}

// WritePrometheus emits the current metrics in Prometheus text exposition format.
func (r *Registry) WritePrometheus(w io.Writer) {
	r.mu.RLock()
	// The SAME composite Ready() reports: /readyz and cerbix_ready must never disagree —
	// a wedged scheduler failing its probe while exporting cerbix_ready 1 is two answers
	// to one question.
	ready := r.ready && (!r.credentialTracked || r.credentialReady) &&
		(!r.serviceWedgedSet || !r.serviceWedged) && len(r.serviceAlertStalls) == 0
	dbEnabled := r.dbEnabled
	dbUp := r.dbUp
	checksUp := r.checksUp
	checksDown := r.checksDown
	incidentsOpened := r.incidentsOpened
	outboxDelivered := r.outboxDelivered
	outboxDead := r.outboxDead
	impactLinks := copyCounts(r.impactLinks)
	impactFailures := r.impactFailures
	impactWitnessOverflow := r.impactWitnessOverflow
	statusPageUnreadable := r.statusPageUnreadable
	alertSuppressed := maps.Clone(r.alertSuppressed)
	delegationFailOpen := maps.Clone(r.delegationFailOpen)
	serviceDelegation := make(map[string]map[string]uint64, len(r.serviceDelegation))
	for signal, states := range r.serviceDelegation {
		serviceDelegation[signal] = maps.Clone(states)
	}
	serviceAlertUndeliver := maps.Clone(r.serviceAlertUndeliver)
	serviceRecipientMissing := r.serviceRecipientMissing
	serviceAlertEvals := copyCounts2(r.serviceAlertEvals)
	serviceAlertEmitted := copyCounts2(r.serviceAlertEmitted)
	serviceAlertLastSuccess := maps.Clone(r.serviceAlertLastSuccess)
	serviceAlertLag := maps.Clone(r.serviceAlertLag)
	serviceAlertStats := r.serviceAlertStats
	pullStats := r.pullStats
	serviceStats := r.serviceStats
	serviceWedgedSet, serviceWedged := r.serviceWedgedSet, r.serviceWedged
	factMaintTracked, factMaintFailing, factMaintLastOKUnix := r.factMaintTracked, r.factMaintFailing, r.factMaintLastOKUnix
	serviceRejections := r.serviceRejections
	serviceRepairOutcomes := copyCounts2(r.serviceRepairOutcomes)
	serviceSlices := make(map[string]uint64, len(r.serviceSlices))
	for k, v := range r.serviceSlices {
		serviceSlices[k] = v
	}
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
	executorProbeErrors := copyCounts(r.executorProbeErrors)
	secretResolutionFailures := copyCounts(r.secretResolutionFailures)
	dispatchTransportFailures := copyCounts(r.dispatchTransportFailures)
	dispatchSharedTrust := r.dispatchSharedTrust
	fileProviders := map[string]fileProviderStat{}
	for name, s := range r.fileProviders {
		cp := fileProviderStat{leader: s.leader, reconciles: copyCounts(s.reconciles), lastDuration: s.lastDuration, lastSuccessUnix: s.lastSuccessUnix, managed: s.managed, orphaned: s.orphaned, bundleErrors: s.bundleErrors}
		fileProviders[name] = cp
	}
	uptime := r.now().Sub(r.startTime).Seconds()
	r.mu.RUnlock()
	out := prometheusWriter{w: w}

	out.println("# HELP cerbix_build_info Build metadata for the running binary.")
	out.println("# TYPE cerbix_build_info gauge")
	out.printf("cerbix_build_info{version=%q,commit=%q,go_version=%q,role=%q} 1\n",
		r.info.Version, r.info.Commit, r.info.GoVersion, r.role)

	out.println("# HELP cerbix_up Always 1 while the process is serving.")
	out.println("# TYPE cerbix_up gauge")
	out.println("cerbix_up 1")

	out.println("# HELP cerbix_ready Whether the service reports ready (1) or not (0).")
	out.println("# TYPE cerbix_ready gauge")
	out.printf("cerbix_ready %d\n", b2i(ready))

	out.println("# HELP cerbix_dispatch_shared_trust Whether one acknowledged fallback dispatch key can open more than one region's retained credential payloads.")
	out.println("# TYPE cerbix_dispatch_shared_trust gauge")
	out.printf("cerbix_dispatch_shared_trust %d\n", b2i(dispatchSharedTrust))

	out.println("# HELP cerbix_uptime_seconds Seconds since process start.")
	out.println("# TYPE cerbix_uptime_seconds gauge")
	out.printf("cerbix_uptime_seconds %.3f\n", uptime)

	if dbEnabled {
		out.println("# HELP cerbix_database_up Whether the database is reachable (1) or not (0).")
		out.println("# TYPE cerbix_database_up gauge")
		out.printf("cerbix_database_up %d\n", b2i(dbUp))
	}

	out.println("# HELP cerbix_checks_total Total monitor checks recorded, by result.")
	out.println("# TYPE cerbix_checks_total counter")
	out.printf("cerbix_checks_total{result=\"up\"} %d\n", checksUp)
	out.printf("cerbix_checks_total{result=\"down\"} %d\n", checksDown)

	// Result-ingest outcomes (spec func-result-protocol). Only non-empty series are emitted.
	writeReasonCounter(&out, "cerbix_result_quarantined_total",
		"Results set aside without touching state (by reason).", quarantined)
	writeReasonCounter(&out, "cerbix_result_ignored_total",
		"Results not applied to live state (by reason).", ignored)
	writeReasonCounter(&out, "cerbix_result_rejected_total",
		"Results fail-closed rejected with no insert (by reason).", rejected)
	writeReasonCounter(&out, "cerbix_executor_probe_error_total",
		"Typed credential-envelope execution errors that did not mutate monitor liveness.", executorProbeErrors)
	writeReasonCounter(&out, "cerbix_secret_resolution_failed_total",
		"Credential materialization or capability rejections before dispatch.", secretResolutionFailures)
	writeReasonCounter(&out, "cerbix_dispatch_transport_failed_total",
		"Materialized jobs that could not be handed to their transport (by reason).", dispatchTransportFailures)
	if len(clockSkew) > 0 {
		out.println("# HELP cerbix_result_clock_skew_total Accepted results with an anomalous client clock (by origin, reason).")
		out.println("# TYPE cerbix_result_clock_skew_total counter")
		for _, origin := range sortedKeys(clockSkew) {
			for _, reason := range sortedKeys(clockSkew[origin]) {
				out.printf("cerbix_result_clock_skew_total{origin=%q,reason=%q} %d\n", origin, reason, clockSkew[origin][reason])
			}
		}
	}
	if missingRev > 0 {
		out.println("# HELP cerbix_result_missing_revision_total Scheduled results with no revision applied under observe mode.")
		out.println("# TYPE cerbix_result_missing_revision_total counter")
		out.printf("cerbix_result_missing_revision_total %d\n", missingRev)
	}

	out.println("# HELP cerbix_incidents_opened_total Total incidents opened through the API.")
	out.println("# TYPE cerbix_incidents_opened_total counter")
	out.printf("cerbix_incidents_opened_total %d\n", incidentsOpened)

	out.println("# HELP cerbix_outbox_delivered_total Total outbox events delivered.")
	out.println("# TYPE cerbix_outbox_delivered_total counter")
	out.printf("cerbix_outbox_delivered_total %d\n", outboxDelivered)

	out.println("# HELP cerbix_outbox_dead_total Total outbox events parked as dead after exhausting retries.")
	out.println("# TYPE cerbix_outbox_dead_total counter")
	out.printf("cerbix_outbox_dead_total %d\n", outboxDead)

	if len(impactLinks) > 0 || impactFailures > 0 || impactWitnessOverflow > 0 {
		out.println("# HELP cerbix_service_impact_links_total Newly inserted incident-service impact links, by role (FR-021 §14).")
		out.println("# TYPE cerbix_service_impact_links_total counter")
		roles := make([]string, 0, len(impactLinks))
		for role := range impactLinks {
			roles = append(roles, role)
		}
		sort.Strings(roles)
		for _, role := range roles {
			out.printf("cerbix_service_impact_links_total{role=%q} %d\n", role, impactLinks[role])
		}
		out.println("# HELP cerbix_service_impact_correlation_failures_total Failed impact-correlation attempts (each retries via the outbox).")
		out.println("# TYPE cerbix_service_impact_correlation_failures_total counter")
		out.printf("cerbix_service_impact_correlation_failures_total %d\n", impactFailures)
		out.println("# HELP cerbix_service_impact_witness_overflow_total Open incidents beyond the per-service correlation witness bound (deterministically not selected; never silent).")
		out.println("# TYPE cerbix_service_impact_witness_overflow_total counter")
		out.printf("cerbix_service_impact_witness_overflow_total %d\n", impactWitnessOverflow)
	}
	if len(alertSuppressed) > 0 {
		out.println("# HELP cerbix_alert_suppressed_total Monitor alerts withheld because a service actively covers that signal (FR-021 §16).")
		out.println("# TYPE cerbix_alert_suppressed_total counter")
		keys := make([]string, 0, len(alertSuppressed))
		for k := range alertSuppressed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			topic, reason, _ := strings.Cut(k, "|")
			out.printf("cerbix_alert_suppressed_total{topic=%q,reason=%q} %d\n", topic, reason, alertSuppressed[k])
		}
	}
	if len(delegationFailOpen) > 0 {
		out.println("# HELP cerbix_alert_delegation_fail_open_total Alert deliveries that PAGED because service coverage could not be confirmed.")
		out.println("# TYPE cerbix_alert_delegation_fail_open_total counter")
		keys := make([]string, 0, len(delegationFailOpen))
		for k := range delegationFailOpen {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out.printf("cerbix_alert_delegation_fail_open_total{reason=%q} %d\n", k, delegationFailOpen[k])
		}
	}
	// The DELIVERY half of §16.6b: what delegation concluded, and the two ways an announcement can
	// fail to reach anybody without failing to deliver.
	if len(serviceDelegation) > 0 {
		out.println("# HELP cerbix_service_alert_delegation_total Delegation outcomes per signal: armed (a member's alert was suppressed), disarmed (it paged for itself), degraded (the lookup could not conclude, which also pages).")
		out.println("# TYPE cerbix_service_alert_delegation_total counter")
		signals := make([]string, 0, len(serviceDelegation))
		for signal := range serviceDelegation {
			signals = append(signals, signal)
		}
		sort.Strings(signals)
		for _, signal := range signals {
			states := make([]string, 0, len(serviceDelegation[signal]))
			for state := range serviceDelegation[signal] {
				states = append(states, state)
			}
			sort.Strings(states)
			for _, state := range states {
				out.printf("cerbix_service_alert_delegation_total{signal=%q,state=%q} %d\n",
					signal, state, serviceDelegation[signal][state])
			}
		}
	}
	if len(serviceAlertUndeliver) > 0 {
		out.println("# HELP cerbix_service_alert_undeliverable_total Service alerts with nobody left to tell — an empty recipient snapshot, or one whose every channel has since gone. Not a delivery failure, which is why it needs its own counter.")
		out.println("# TYPE cerbix_service_alert_undeliverable_total counter")
		signals := make([]string, 0, len(serviceAlertUndeliver))
		for signal := range serviceAlertUndeliver {
			signals = append(signals, signal)
		}
		sort.Strings(signals)
		for _, signal := range signals {
			out.printf("cerbix_service_alert_undeliverable_total{signal=%q} %d\n",
				signal, serviceAlertUndeliver[signal])
		}
	}
	if serviceRecipientMissing > 0 {
		out.println("# HELP cerbix_service_alert_recipient_missing_total Snapshot recipients whose channel no longer exists. COUNTED rather than replaced by whoever is on call now (FR-021 §16.4a).")
		out.println("# TYPE cerbix_service_alert_recipient_missing_total counter")
		out.printf("cerbix_service_alert_recipient_missing_total %d\n", serviceRecipientMissing)
	}
	// The EVALUATOR half of §16.6b. Every family here is labelled by `signal` only (health|burn)
	// plus a fixed enum: no tenant, service, target or rule label anywhere.
	if len(serviceAlertEvals) > 0 {
		out.println("# HELP cerbix_service_alert_evaluations_total Service alerting evaluations by signal and outcome (FR-021 §16.6b).")
		out.println("# TYPE cerbix_service_alert_evaluations_total counter")
		for _, signal := range sortedKeys(serviceAlertEvals) {
			for _, outcome := range sortedKeys(serviceAlertEvals[signal]) {
				out.printf("cerbix_service_alert_evaluations_total{signal=%q,outcome=%q} %d\n",
					signal, outcome, serviceAlertEvals[signal][outcome])
			}
		}
	}
	if len(serviceAlertEmitted) > 0 {
		out.println("# HELP cerbix_service_alert_emitted_total Service alert edges enqueued, by signal and edge.")
		out.println("# TYPE cerbix_service_alert_emitted_total counter")
		for _, signal := range sortedKeys(serviceAlertEmitted) {
			for _, edge := range sortedKeys(serviceAlertEmitted[signal]) {
				out.printf("cerbix_service_alert_emitted_total{signal=%q,edge=%q} %d\n",
					signal, edge, serviceAlertEmitted[signal][edge])
			}
		}
	}
	if serviceAlertStats != nil {
		out.println("# HELP cerbix_service_alert_active Open service alert episodes, by signal.")
		out.println("# TYPE cerbix_service_alert_active gauge")
		out.printf("cerbix_service_alert_active{signal=\"burn\"} %d\n", serviceAlertStats.ActiveBurn)
		out.printf("cerbix_service_alert_active{signal=\"health\"} %d\n", serviceAlertStats.ActiveHealth)
		out.println("# HELP cerbix_service_alert_backlog Owning services (health) and enabled burn targets (burn) due for evaluation: lease expired on the DB clock, or never evaluated.")
		out.println("# TYPE cerbix_service_alert_backlog gauge")
		out.printf("cerbix_service_alert_backlog{signal=\"burn\"} %d\n", serviceAlertStats.BacklogBurn)
		out.printf("cerbix_service_alert_backlog{signal=\"health\"} %d\n", serviceAlertStats.BacklogHealth)
	}
	if len(serviceAlertLastSuccess) > 0 {
		out.println("# HELP cerbix_service_alert_last_success_seconds Unix time of the last SUCCESSFUL evaluation pass, by signal (a failed pass leaves it aging).")
		out.println("# TYPE cerbix_service_alert_last_success_seconds gauge")
		for _, signal := range sortedKeys(serviceAlertLastSuccess) {
			out.printf("cerbix_service_alert_last_success_seconds{signal=%q} %d\n", signal, serviceAlertLastSuccess[signal])
		}
		out.println("# HELP cerbix_service_alert_lag_seconds How far behind the stalest verdict of the last successful pass was, by signal.")
		out.println("# TYPE cerbix_service_alert_lag_seconds gauge")
		for _, signal := range sortedKeys(serviceAlertLag) {
			out.printf("cerbix_service_alert_lag_seconds{signal=%q} %.3f\n", signal, serviceAlertLag[signal])
		}
	}
	if statusPageUnreadable > 0 {
		out.println("# HELP cerbix_status_page_component_unreadable_total Status-page components whose active service could not be read (a FAILED read, not absent measurement).")
		out.println("# TYPE cerbix_status_page_component_unreadable_total counter")
		out.printf("cerbix_status_page_component_unreadable_total %d\n", statusPageUnreadable)
	}

	if leaderTracked {
		out.println("# HELP cerbix_scheduler_leader Whether this process currently holds scheduler leadership (1) or is on standby (0).")
		out.println("# TYPE cerbix_scheduler_leader gauge")
		out.printf("cerbix_scheduler_leader %d\n", b2i(leader))
	}

	if brokerTracked {
		out.println("# HELP cerbix_broker_up Whether the AMQP broker is currently reachable (1) or not (0).")
		out.println("# TYPE cerbix_broker_up gauge")
		out.printf("cerbix_broker_up %d\n", b2i(brokerUp))
	}

	if pullStats != nil {
		out.println("# HELP cerbix_pull_jobs_pending Unclaimed pull jobs per region (HTTP-pull transport).")
		out.println("# TYPE cerbix_pull_jobs_pending gauge")
		out.println("# HELP cerbix_pull_agent_lag_seconds Age of the oldest unclaimed pull job per region.")
		out.println("# TYPE cerbix_pull_agent_lag_seconds gauge")
		for _, s := range pullStats {
			out.printf("cerbix_pull_jobs_pending{region=%q} %d\n", s.Region, s.Pending)
			out.printf("cerbix_pull_agent_lag_seconds{region=%q} %.3f\n", s.Region, s.LagSeconds)
		}
	}

	if serviceStats != nil {
		out.println("# HELP cerbix_service_repair_ranges Durable service repair ranges by state.")
		out.println("# TYPE cerbix_service_repair_ranges gauge")
		out.printf("cerbix_service_repair_ranges{state=%q} %d\n", "pending", serviceStats.RepairPending)
		out.printf("cerbix_service_repair_ranges{state=%q} %d\n", "running", serviceStats.RepairRunning)
		out.printf("cerbix_service_repair_ranges{state=%q} %d\n", "error", serviceStats.RepairErrored)
		out.println("# HELP cerbix_service_watermark_lag_seconds Worst sealed-watermark lag across declared services (steady state ~ late-arrival grace).")
		out.println("# TYPE cerbix_service_watermark_lag_seconds gauge")
		out.printf("cerbix_service_watermark_lag_seconds %.3f\n", serviceStats.WatermarkLagSeconds)
	}
	if len(serviceRepairOutcomes) > 0 {
		out.println("# HELP cerbix_service_repair_outcomes_total Repair/recompute ranges by lifecycle outcome and range reason.")
		out.println("# TYPE cerbix_service_repair_outcomes_total counter")
		for _, outcome := range sortedKeys(serviceRepairOutcomes) {
			for _, reason := range sortedKeys(serviceRepairOutcomes[outcome]) {
				out.printf("cerbix_service_repair_outcomes_total{outcome=%q,reason=%q} %d\n",
					outcome, reason, serviceRepairOutcomes[outcome][reason])
			}
		}
	}
	if serviceRejections > 0 {
		out.println("# HELP cerbix_service_unrecomputable_rejections_total Mutations refused because their range is unrecomputable (ErrUnrecomputableRange at preview/confirm).")
		out.println("# TYPE cerbix_service_unrecomputable_rejections_total counter")
		out.printf("cerbix_service_unrecomputable_rejections_total %d\n", serviceRejections)
	}
	if serviceStats != nil {
		out.println("# HELP cerbix_service_epoch_fanout_total Evaluation epochs created (fan-out of execution-changing writes); persisted with the owning transaction.")
		out.println("# TYPE cerbix_service_epoch_fanout_total counter")
		out.printf("cerbix_service_epoch_fanout_total %d\n", serviceStats.EpochFanoutTotal)
		out.println("# HELP cerbix_service_late_arrivals_total Heartbeats excluded behind the seal; persisted with the owning transaction.")
		out.println("# TYPE cerbix_service_late_arrivals_total counter")
		out.printf("cerbix_service_late_arrivals_total %d\n", serviceStats.LateArrivalsTotal)
		out.println("# HELP cerbix_service_late_arrival_overflow_total Late-arrival example slots that overflowed their bound.")
		out.println("# TYPE cerbix_service_late_arrival_overflow_total counter")
		out.printf("cerbix_service_late_arrival_overflow_total %d\n", serviceStats.LateOverflowTotal)
		out.println("# HELP cerbix_result_observed_before_issue_total Scheduled results accepted with observed_at before job_issued_at, inside result.allowed_skew; a region clock trailing the core's.")
		out.println("# TYPE cerbix_result_observed_before_issue_total counter")
		out.printf("cerbix_result_observed_before_issue_total %d\n", serviceStats.ObservedBeforeIssueTotal)
	}
	if factMaintTracked {
		out.println("# HELP cerbix_service_fact_maintenance_failing Whether the last fact-partition maintenance pass failed (1) — repeated 1s with an aging last-success is a stuck month.")
		out.println("# TYPE cerbix_service_fact_maintenance_failing gauge")
		out.printf("cerbix_service_fact_maintenance_failing %d\n", b2i(factMaintFailing))
		out.println("# HELP cerbix_service_fact_maintenance_last_success_timestamp_seconds Unix time of the last fully successful fact-partition maintenance pass (or of tracking start, before any success).")
		out.println("# TYPE cerbix_service_fact_maintenance_last_success_timestamp_seconds gauge")
		out.printf("cerbix_service_fact_maintenance_last_success_timestamp_seconds %d\n", factMaintLastOKUnix)
	}
	if serviceWedgedSet {
		out.println("# HELP cerbix_service_wedged Whether service-reliability work is wedged (a repair range terminally parked in error, or the state is unknown/unavailable). Wedged fails /readyz, and cerbix_ready agrees.")
		out.println("# TYPE cerbix_service_wedged gauge")
		out.printf("cerbix_service_wedged %d\n", b2i(serviceWedged))
	}
	if len(serviceSlices) > 0 {
		out.println("# HELP cerbix_service_slices_total Leader service-reliability slices by outcome.")
		out.println("# TYPE cerbix_service_slices_total counter")
		for _, outcome := range sortedKeys(serviceSlices) {
			out.printf("cerbix_service_slices_total{outcome=%q} %d\n", outcome, serviceSlices[outcome])
		}
	}

	if len(fileProviders) > 0 {
		out.println("# HELP cerbix_file_provider_leader Whether this process holds a file provider's reconcile leadership.")
		out.println("# TYPE cerbix_file_provider_leader gauge")
		out.println("# HELP cerbix_file_provider_reconcile_total File-provider reconciles by outcome.")
		out.println("# TYPE cerbix_file_provider_reconcile_total counter")
		out.println("# HELP cerbix_file_provider_reconcile_duration_seconds Duration of the last reconcile.")
		out.println("# TYPE cerbix_file_provider_reconcile_duration_seconds gauge")
		out.println("# HELP cerbix_file_provider_last_success_timestamp_seconds Unix time of the last successful reconcile.")
		out.println("# TYPE cerbix_file_provider_last_success_timestamp_seconds gauge")
		out.println("# HELP cerbix_file_provider_managed_monitors Monitors currently owned by the provider.")
		out.println("# TYPE cerbix_file_provider_managed_monitors gauge")
		out.println("# HELP cerbix_file_provider_orphaned_monitors Owned monitors currently orphaned.")
		out.println("# TYPE cerbix_file_provider_orphaned_monitors gauge")
		out.println("# HELP cerbix_file_provider_bundle_errors Bundles/files rejected in the last scan.")
		out.println("# TYPE cerbix_file_provider_bundle_errors gauge")
		for _, name := range sortedKeys(fileProviders) {
			s := fileProviders[name]
			out.printf("cerbix_file_provider_leader{provider=%q} %d\n", name, b2i(s.leader))
			for _, outcome := range sortedKeys(s.reconciles) {
				out.printf("cerbix_file_provider_reconcile_total{provider=%q,outcome=%q} %d\n", name, outcome, s.reconciles[outcome])
			}
			out.printf("cerbix_file_provider_reconcile_duration_seconds{provider=%q} %.3f\n", name, s.lastDuration)
			out.printf("cerbix_file_provider_last_success_timestamp_seconds{provider=%q} %d\n", name, s.lastSuccessUnix)
			out.printf("cerbix_file_provider_managed_monitors{provider=%q} %d\n", name, s.managed)
			out.printf("cerbix_file_provider_orphaned_monitors{provider=%q} %d\n", name, s.orphaned)
			out.printf("cerbix_file_provider_bundle_errors{provider=%q} %d\n", name, s.bundleErrors)
		}
	}
}

// prometheusWriter stops emitting after the first client/write failure. Metrics
// collection is best-effort for a single scrape, but write errors are never
// accidentally discarded by individual format calls.
type prometheusWriter struct {
	w   io.Writer
	err error
}

func (w *prometheusWriter) println(args ...any) {
	if w.err == nil {
		_, w.err = fmt.Fprintln(w.w, args...)
	}
}

func (w *prometheusWriter) printf(format string, args ...any) {
	if w.err == nil {
		_, w.err = fmt.Fprintf(w.w, format, args...)
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

// copyCounts2 snapshots a two-label counter map (outer → inner → count) under the caller's lock.
func copyCounts2(m map[string]map[string]uint64) map[string]map[string]uint64 {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]map[string]uint64, len(m))
	for outer, inner := range m {
		out[outer] = copyCounts(inner)
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
func writeReasonCounter(w *prometheusWriter, name, help string, counts map[string]uint64) {
	if len(counts) == 0 {
		return
	}
	w.printf("# HELP %s %s\n", name, help)
	w.printf("# TYPE %s counter\n", name)
	for _, reason := range sortedKeys(counts) {
		w.printf("%s{reason=%q} %d\n", name, reason, counts[reason])
	}
}
