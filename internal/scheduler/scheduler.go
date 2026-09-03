// Package scheduler runs the periodic check schedule. Exactly one instance is
// active at a time, elected via a Postgres advisory lock; the leader scans
// enabled monitors on a tick and publishes a CheckJob whenever one is due.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/ingest"
	"github.com/teamlead-com/cerbix/internal/metrics"
	"github.com/teamlead-com/cerbix/internal/store"
)

// advisoryLockKey is the well-known key for scheduler leadership.
const advisoryLockKey int64 = 0x6365726269780001 // "cerbix" + slot 1

// Store is the persistence surface the scheduler needs.
// LeaderSession is a held leadership whose fenced work runs on the lock-owning connection,
// so losing the lock aborts an in-flight batch instead of letting it commit behind the new
// leader. Implemented by *store.LeaderSession.
type LeaderSession interface {
	Check(ctx context.Context) (bool, error)
	Release()
	// RunServiceSlice works the service-reliability queue until the deadline, on the
	// lock-owning connection. It reports whether it found anything to do.
	RunServiceSlice(ctx context.Context, deadline time.Time) (bool, error)
	// EvaluateServiceAlerts and EvaluateServiceBurnAlerts run the two FR-021 §16 alert slices on
	// that same connection. They belong here rather than on Store because an evaluation writes
	// the arming state that silences OTHER monitors' alerts and publishes pages: a deposed leader
	// committing one behind its successor could tell people an alert ended while the real leader
	// keeps it firing.
	EvaluateServiceAlerts(ctx context.Context, cadence time.Duration) (store.ServiceAlertEvaluation, error)
	EvaluateServiceBurnAlerts(ctx context.Context, cadence time.Duration) (store.ServiceBurnEvaluation, error)
}

// StoreAdapter widens *store.Store to the Store interface.
//
// It exists for one narrow reason: TryBecomeLeaderSession returns the CONCRETE
// *store.LeaderSession, and Go has no return-type covariance, so the concrete type does not
// satisfy an interface method returning the LeaderSession interface. Embedding forwards
// every other method untouched and the explicit one below performs the widening — the same
// shape the file provider already uses for the same reason.
type StoreAdapter struct{ *store.Store }

// NewStoreAdapter wraps a *store.Store for the scheduler.
func NewStoreAdapter(st *store.Store) Store { return StoreAdapter{Store: st} }

// TryBecomeLeaderSession widens the concrete session to the interface.
func (a StoreAdapter) TryBecomeLeaderSession(ctx context.Context, key int64) (LeaderSession, bool, error) {
	ls, ok, err := a.Store.TryBecomeLeaderSession(ctx, key)
	if err != nil || !ok {
		return nil, ok, err
	}
	return ls, true, nil
}

type Store interface {
	ListEnabledMonitors(ctx context.Context) ([]domain.Monitor, error)
	ListEnabledMonitorSnapshots(ctx context.Context) ([]domain.Monitor, error)
	MaterializeExecutionConfigs(ctx context.Context, monitorIDs []string, carrierByRegion map[string]int) ([]store.MaterializedExecution, error)
	StalePushMonitors(ctx context.Context) ([]domain.Monitor, error)
	RecordDeadmanResult(ctx context.Context, monitorID string, revision int64, cutoff time.Time) (store.ResultOutcome, error)
	// TryBecomeLeaderSession elects on a PINNED connection and hands back a session whose
	// fenced work runs on that same connection. It replaced TryBecomeLeader, which let
	// writes use the pool: a deposed leader could then still commit a pooled transaction
	// after a successor existed, and the 5s watchdog only narrowed that window rather than
	// closing it (FR-021 §10.7).
	TryBecomeLeaderSession(ctx context.Context, key int64) (LeaderSession, bool, error)
	RollupDailyAvailability(ctx context.Context, from, to time.Time) error
	EnsureHeartbeatPartitions(ctx context.Context, ahead int) error
	// FR-029 D9 / D9a: the canary in-flight lease. One execution per monitor and at most
	// `store.CanaryRegionLimit` per region, both decided HERE — the scheduler is leader-elected and
	// is the only writer, so the check is a count inside the inserting transaction rather than a
	// distributed semaphore.
	ClaimCanaryInflight(ctx context.Context, monitorID, region, runKey string, ttl time.Duration) error
	// InsertHeartbeat is how a shortage becomes an ORDINARY monitor outcome rather than an
	// indefinite pending: a run that could not be dispatched writes one DOWN heartbeat with a
	// bounded reason, and the monitor's own failure_threshold decides whether that flips its status.
	InsertHeartbeat(ctx context.Context, hb domain.Heartbeat) error
	EnsureServiceFactPartitions(ctx context.Context, aheadMonths int) error
	PurgeOldHeartbeats(ctx context.Context, cutoff time.Time) (int, error)
	EnqueueRenotifyReminders(ctx context.Context) (int, error)
	EvaluateBurnAlerts(ctx context.Context) (fired, resolved int, err error)
	EnqueueDueSLAReports(ctx context.Context) (int, error)
	EvaluateRegionWorkerAlerts(ctx context.Context, live map[string]bool, graceSeconds int) (fired, resolved int, err error)
	AdvanceEscalations(ctx context.Context) (store.EscalationPass, error)
	// leaseSeconds is the per-JOB claim lease: a probe that outlives the endpoint's default would
	// otherwise be re-claimed while it still runs — a duplicate probe for an ordinary type and a
	// duplicate external TRANSACTION for a canary (FR-029 §4.2). Zero keeps today's default.
	EnqueuePullJob(ctx context.Context, region string, payload []byte, ttlSeconds, leaseSeconds int) error
	EnqueuePullJobV2(ctx context.Context, region string, payload []byte, ttlSeconds, leaseSeconds int) error
	EnqueuePullJobV3(ctx context.Context, region string, payload []byte, ttlSeconds, leaseSeconds int) error
	LiveCredentialReadyAgentRegions(ctx context.Context, within time.Duration, minCapability int) (map[string]bool, error)
	PurgeExpiredPullJobs(ctx context.Context) (int, error)
	PurgeExpiredPullTests(ctx context.Context) (int, error)
	PurgeStaleAgentHeartbeats(ctx context.Context, olderThan time.Duration) (int, error)
	PurgeDeliveredOutbox(ctx context.Context, olderThan time.Duration) (int, error)
	DeleteExpiredSessions(ctx context.Context) (int64, error)
	DeleteExpiredAuthFlows(ctx context.Context) (int64, error)
	PullQueueStats(ctx context.Context) ([]metrics.PullStat, error)
	ServiceReliabilityStats(ctx context.Context) (metrics.ServiceReliabilityStat, error)
	// ServiceAlertStats samples what an evaluation SLICE cannot see: the alerts that are open
	// right now and the work waiting to be evaluated (FR-021 §16.6b).
	ServiceAlertStats(ctx context.Context) (metrics.ServiceAlertStat, error)
	// RunGateLedgerMaintenancePass is one whole decision-ledger maintenance lifecycle on the
	// gate's OWN fenced session (FR-024 D10): acquire the gate lock on a pinned connection,
	// work under passStart + 27 s, release as a proof by passStart + 30 s. It lives on the
	// Store rather than on LeaderSession because its authority is NOT scheduler leadership —
	// the scheduler's lock connection may already be dead while its pool still answers.
	// acquired=false: the lock is held elsewhere and the pass was skipped.
	RunGateLedgerMaintenancePass(ctx context.Context, passStart time.Time, cfg store.GateMaintenanceConfig,
		clock func() time.Time, metrics store.GateMaintenanceMetrics) (store.GateMaintenanceReport, bool, error)
	// PurgeChangeGroups removes at most groupsPerBatch change GROUPS whose latest phase is older
	// than cutoff, every phase row of a selected group in one statement (FR-025 D9). The leader
	// repeats it until a batch selects fewer than the bound.
	PurgeChangeGroups(ctx context.Context, cutoff time.Time, groupsPerBatch int) (groups, rows int, err error)
	// CountServiceChanges is the retention pass's sample for `cerbix_changes_retained` (D15):
	// the rows of service_changes kept after the pass.
	CountServiceChanges(ctx context.Context) (int64, error)
}

// ChangeRetentionSink receives the retention pass's sample of retained change rows and forgets
// it on leadership loss (FR-025 D15, `cerbix_changes_retained`). Implemented by
// *metrics.Registry. Optional.
type ChangeRetentionSink interface {
	SetChangesRetained(n int64)
	ClearChangesRetained()
}

// PullStatsSink receives the leader's per-region pull-queue gauge snapshot.
// Implemented by *metrics.Registry.
type PullStatsSink interface {
	SetPullStats(stats []metrics.PullStat)
}

// ServiceStatsSink receives the leader's service-side telemetry: the reliability snapshot and
// slice outcomes, and the FR-021 §16.6b alerting families. Implemented by *metrics.Registry.
// Optional: an installation with no services pays one cheap sample per cadence and exports
// honest zeros.
//
// ONE sink for both subsystems on purpose. They share a lifecycle — leader-only, published from
// the same loop, forgotten by the same step-down clear — and a second injected sink would be a
// second wiring to keep in step with it. It is also what keeps the alerting stall OFF the API's
// readiness: nothing but the scheduler's leader loop ever holds this interface.
type ServiceStatsSink interface {
	SetServiceReliabilityStats(st metrics.ServiceReliabilityStat)
	SetServiceWedged(wedged bool, reason string)
	SetServiceFactMaintenance(ok bool, nowUnix int64)
	ClearServiceReliabilityStats()
	RecordServiceSlice(outcome string)
	// The §16.6b evaluator families. Counters come from what a slice DID, the pass gauges from
	// each successful pass, the stats from the out-of-band sample.
	RecordServiceAlertEvaluations(signal, outcome string, n int)
	RecordServiceAlertWithheld(signal, reason string, n int)
	RecordServiceAlertEmitted(signal, edge string, n int)
	RecordServiceIncidents(action string, n int)
	RecordEscalationSteps(subject string, n int)
	SetServiceAlertPass(signal string, lastSuccessUnix int64, lagSeconds float64)
	SetServiceAlertStats(st metrics.ServiceAlertStat)
	SetServiceAlertStalled(signal string, stalled bool, reason string)
}

// LeaderStateSink records whether this process currently holds scheduler
// leadership, for the cerbix_scheduler_leader gauge. Implemented by
// *metrics.Registry. Optional.
type LeaderStateSink interface {
	SetSchedulerLeader(leader bool)
}

type SecretResolutionSink interface {
	RecordSecretResolutionFailure(reason string)
	// RecordDispatchTransportFailure is a separate family: a transport that will not take
	// a publish is a different operator problem from a credential that will not resolve.
	RecordDispatchTransportFailure(reason string)
	// FR-029: a run cerbix could not dispatch, by bounded reason. The metric is the attribution
	// half of counting those samples as unavailable — the number stays honest, the cause stays
	// visible, and no monitor id, URL or correlation id appears in a label.
	RecordCanaryDispatchRefused(reason string)
}

// LiveRegionSource reports which worker-pool regions currently have a live worker
// (a consumer on checks.jobs.<region>). Implemented by *mqadmin.Client. Optional:
// without it the scheduler skips region-worker alerting (e.g. the inproc build).
type LiveRegionSource interface {
	LiveJobRegions(ctx context.Context) (map[string]bool, error)
}

// CredentialLiveRegionSource reports only consumers on the physically isolated v2 job
// queues. Legacy worker liveness is intentionally insufficient for envelope dispatch.
type CredentialLiveRegionSource interface {
	LiveCredentialJobRegions(ctx context.Context) (map[string]bool, error)
	LiveCredentialV3JobRegions(ctx context.Context) (map[string]bool, error)
}

const (
	// rollupEvery is how often the leader refreshes the daily-availability rollup.
	rollupEvery = time.Minute
	// maintainEvery is how often the leader maintains heartbeat partitions
	// (pre-create upcoming, drop expired) — DDL, so far less often than the rollup.
	maintainEvery = time.Hour
	// changePurgeEvery is the cadence of FR-025 D9's change-group retention pass: daily, on the
	// leader, whole groups by age.
	changePurgeEvery = 24 * time.Hour
	// partitionAheadDays is how many future daily partitions to keep pre-created.
	partitionAheadDays = 2
	// factPartitionAheadMonths is how many future MONTHLY service-fact partitions to keep
	// pre-created (facts partition by month; migration 00064 only seeded around its run date).
	factPartitionAheadMonths = 2
	// factMaintenanceEvery / factMaintenanceTimeout pace the off-loop fact-partition pass: a
	// DEFAULT-month adoption moves data in bounded batches and may legitimately take a while,
	// which is exactly why it does not run inside a dispatch tick.
	factMaintenanceEvery   = 10 * time.Minute
	factMaintenanceTimeout = 5 * time.Minute

	// defaultRetentionDays is used when the caller doesn't set one.
	defaultRetentionDays = 30
	// refreshEvery is how often the leader reloads the enabled-monitor snapshot
	// from the database; scheduling between refreshes runs off the in-memory
	// snapshot, so the per-tick full scan is gone. A new/edited monitor is picked
	// up within this window.
	refreshEvery = 15 * time.Second
	// credentialFailureLogEvery bounds names-only logs for a persistently broken
	// credential reference/key without hiding the low-cardinality counter.
	credentialFailureLogEvery = 5 * time.Minute
	// pushCheckEvery is how often the leader runs the single batched query that
	// finds push monitors whose dead-man's switch has tripped.
	pushCheckEvery = 5 * time.Second
	// renotifyEvery is how often the leader re-sends alerts for monitors that have
	// stayed down past their renotify_seconds (a coarse tick; the per-monitor
	// interval gates the actual re-send).
	renotifyEvery = 15 * time.Second
	// burnEvery is how often the leader evaluates SLO burn-rate alerts.
	burnEvery = time.Minute
	// serviceAlertEvery is the LIVE service signal's fixed cadence (FR-021 §16.3). It is passed
	// to the evaluator rather than assumed by it, because the freshness lease is a multiple of
	// this number: a leader that evaluated on one cadence and armed on another would either
	// dis-arm a healthy evaluator or keep coverage armed after it stopped.
	serviceAlertEvery = 30 * time.Second
	// serviceBurnEvery is the SEALED service signal's cadence. It trails the seal watermark by
	// construction (§16.4), so evaluating it faster than facts are sealed buys nothing; it shares
	// the minute the monitor burn path already uses.
	serviceBurnEvery = time.Minute
	// reportEvery is how often the leader checks for due weekly SLA reports (the
	// 7-day watermark gates the actual send).
	reportEvery = time.Hour
	// regionWorkerEvery is how often the leader checks that every region with enabled
	// monitors still has a live worker (via the RabbitMQ management API).
	regionWorkerEvery = 30 * time.Second
	// regionWorkerGraceSeconds is how long a region must be observed without a worker
	// before alerting, so a brief worker reconnect (e.g. at startup or a rolling
	// restart) does not fire a false alarm.
	regionWorkerGraceSeconds = 90
	// escalationEvery is how often the leader advances on-call escalation ladders for
	// open, unacknowledged auto-incidents (a coarse tick; per-step offsets gate firing).
	escalationEvery = 15 * time.Second
	// pullStatsEvery is how often the leader samples per-region pull-queue depth/lag.
	pullStatsEvery = 15 * time.Second
	// leaderCheckEvery is how often the leader re-verifies it still holds the advisory
	// lock on its held connection. If the connection (and the session lock) was lost the
	// leader steps down immediately rather than keep dispatching against a lock another
	// node may now hold — the anti-split-brain watchdog.
	leaderCheckEvery = 5 * time.Second

	// Service-reliability work runs as a sub-tick of the leader loop, and the three numbers
	// below derive from ONE mechanism rather than being independent knobs (FR-021 §10.10).
	//
	// serviceSliceEvery is how often a slice is attempted; serviceCycleBudget is the TOTAL
	// wall clock a cycle may spend on it, spent across slices; and maxDispatchDelay bounds
	// any SINGLE slice, so dispatch is serviced between them. A slice never exceeds the
	// dispatch bound, which is what makes "the tick is not delayed beyond it" true by
	// construction instead of by hope.
	serviceSliceEvery  = time.Second
	serviceCycleBudget = 2 * time.Second
	maxDispatchDelay   = 250 * time.Millisecond
	// subCadenceTimeout bounds a single periodic leader task (rollup, renotify, burn,
	// SLA reports, region/escalation, stale-push) so one hung query — a lock wait, a slow
	// aggregate — can't stall the whole leader tick and freeze dispatch. maintainTimeout
	// is the larger budget for the hourly partition/purge sweep (drop_chunks + several
	// purges).
	subCadenceTimeout = 30 * time.Second
	maintainTimeout   = 2 * time.Minute
	// pullLagWarnSeconds logs a warning when a region's oldest unclaimed pull job is
	// older than this — a live agent that stopped draining (paging is a Prometheus rule
	// on cerbix_pull_agent_lag_seconds; this is the in-app operational breadcrumb).
	pullLagWarnSeconds = 120
	// staleAgentHeartbeatAge is how old an agent heartbeat row must be before the leader
	// prunes it (far beyond the liveness window; only long-dead agent_ids are removed).
	staleAgentHeartbeatAge = time.Hour
	// deliveredOutboxRetention is how long a delivered outbox row is kept for audit before
	// the leader purges it (dead-lettered rows are never auto-purged).
	deliveredOutboxRetention = 7 * 24 * time.Hour
	// confirmCapPerRegion bounds how many monitors may probe at their accelerated
	// confirm interval simultaneously per region: during a mass outage the herd
	// falls back to the normal rhythm instead of multiplying load (anti
	// thundering-herd). Beyond the cap the monitor still confirms — just at its
	// regular interval.
	confirmCapPerRegion = 50
)

// Scheduler publishes due check jobs while it holds leadership.
// canaryInflightSlack is the margin over a canary's own timeout that its in-flight lease carries
// (FR-029 §4b). It is what bounds recovery after an executor crash: the slot returns on its own after
// timeout + this, and the runbook states that number rather than leaving it to arithmetic.
const canaryInflightSlack = 60 * time.Second

// pullLeaseFor is the per-job claim lease a monitor needs: its own probe budget plus slack, or zero
// for a probe short enough that the endpoint's default already covers it. Written against the
// TIMEOUT rather than against the type, because the defect it closes predates the canary — any pull
// monitor with a timeout past the default is re-claimable mid-probe today.
func pullLeaseFor(m domain.Monitor) int {
	timeout := int(m.Timeout() / time.Second)
	if timeout <= pullLeaseDefaultSeconds {
		return 0
	}
	return timeout + int(canaryInflightSlack/time.Second)
}

// pullLeaseDefaultSeconds mirrors the agent endpoint's own constant. It is duplicated deliberately
// rather than imported: the scheduler must not depend on the API package, and a test asserts the two
// agree so the copy cannot drift silently.
const pullLeaseDefaultSeconds = 30

// claimCanarySlot takes the in-flight slot for a canary and reports true when the job may be
// dispatched. A monitor of any other type always may. Both dispatch paths call it, because a canary
// without bindings never reaches the credential path and would otherwise keep no guarantee at all.
func (s *Scheduler) claimCanarySlot(ctx context.Context, m domain.Monitor) bool {
	if m.Type != domain.MonitorAsyncCanary {
		return true
	}
	err := s.store.ClaimCanaryInflight(ctx, m.ID, m.Region, m.Config[domain.CanaryRunKey], m.Timeout()+canaryInflightSlack)
	if err == nil {
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	// Not a queue that grows: the run is refused and reported as ONE ordinary DOWN with a bounded
	// reason, and the monitor's own failure_threshold decides whether that becomes a status flip.
	reason := "region_saturated"
	switch {
	case errors.Is(err, store.ErrCanaryMonitorInFlight):
		reason = "already_in_flight"
	case errors.Is(err, store.ErrCanaryRegionSaturated):
	default:
		reason = "in_flight_claim_failed"
	}
	s.reportCanaryShortage(ctx, m, reason)
	return false
}

// reportCanaryShortage turns a run cerbix could not dispatch into an ORDINARY monitor outcome: one
// DOWN heartbeat with a bounded reason. Not an indefinite pending, not a readiness flip, and not a
// silent exclusion from the service's number — the owner's brief settled that, and the attribution
// an operator needs lives in the reason, the region alert and the metrics instead.
func (s *Scheduler) reportCanaryShortage(ctx context.Context, m domain.Monitor, reason string) {
	hb := domain.Heartbeat{
		MonitorID:         m.ID,
		Ts:                time.Now().UTC(),
		ExecutionRevision: m.ExecutionRevision,
		Up:                false,
		Msg:               "dispatch: " + reason,
	}
	if s.secretResolution != nil {
		// Attribution where it belongs: the metric carries the bounded REASON and no monitor id,
		// which is the half of the "these samples count as unavailable" decision that keeps the
		// cause visible without making the number lie.
		s.secretResolution.RecordCanaryDispatchRefused(reason)
	}
	if err := s.store.InsertHeartbeat(ctx, hb); err != nil && ctx.Err() == nil {
		s.logger.Warn("canary_shortage_heartbeat_failed", "monitor_id", m.ID, "reason", reason, "error", err.Error())
		return
	}
	s.logger.Info("canary_dispatch_refused", "monitor_id", m.ID, "region", m.Region, "reason", reason)
}

type Scheduler struct {
	store                  Store
	dispatcher             dispatch.Dispatcher
	logger                 *slog.Logger
	tick                   time.Duration
	retry                  time.Duration
	leaderKey              int64
	retentionDays          int
	liveRegions            LiveRegionSource
	credentialLiveRegions  CredentialLiveRegionSource
	localCredentialRegions map[string]bool
	pullRegions            map[string]bool // regions served over HTTP-pull (jobs go to pull_jobs, not AMQP)
	pullMetrics            PullStatsSink
	serviceMetrics         ServiceStatsSink
	statsEvery             time.Duration // test override for the stats cadence
	// alertSuccess is when each alerting arm last SUCCEEDED, and it is what makes a persistently
	// failing evaluator visible. Readiness cannot be derived from lag alone: a pass that rolled
	// back reports no lag at all, so an arm erroring every cadence would keep the last successful
	// pass's lag forever and read as healthy while every lease it should refresh expired.
	alertSuccess   map[string]time.Time
	alertSuccessMu sync.Mutex
	// now is the clock, injectable so a readiness test can age a stall deterministically instead
	// of sleeping through three cadences.
	now                 func() time.Time
	leaderState         LeaderStateSink
	confirmCh           <-chan string      // monitor ids entering the confirmation phase (LISTEN monitor_confirm)
	configCh            <-chan struct{}    // execution-config changes (LISTEN monitor_config_changed) → force a snapshot reload
	reconciler          *ingest.Reconciler // shared post-commit flow for dead-man transitions (SSE + incident)
	credentialEnvelopes bool
	secretResolution    SecretResolutionSink
	// gateCfg / gateMetrics drive the decision-ledger maintenance loop (gatemaintenance.go);
	// a zero PurgeEvery means the loop is not started.
	gateCfg     store.GateMaintenanceConfig
	gateMetrics GateMaintenanceSink
	// changeRetentionDays / changeGroupsPerBatch drive the daily change-group retention pass
	// (FR-025 D9); a zero retention means the pass is not run.
	changeRetentionDays  int
	changeGroupsPerBatch int
	changeMetrics        ChangeRetentionSink
}

// WithChangeRetention wires FR-025 D9's retention: once a day the leader removes change groups
// whose latest phase is older than `days`, `groupsPerBatch` group keys per statement, repeating
// until a batch selects fewer than the bound. Zero days disables the pass.
func (s *Scheduler) WithChangeRetention(days, groupsPerBatch int) *Scheduler {
	s.changeRetentionDays = days
	s.changeGroupsPerBatch = groupsPerBatch
	return s
}

// WithChangeRetentionMetrics wires the `cerbix_changes_retained` gauge sink (FR-025 D15): the
// retention pass samples the rows kept after it runs; a deposed leader clears the gauge.
func (s *Scheduler) WithChangeRetentionMetrics(sink ChangeRetentionSink) *Scheduler {
	s.changeMetrics = sink
	return s
}

// WithCredentialEnvelopes switches the scheduler to the decrypt-free snapshot plus
// authoritative materialization path. Config validation guarantees this is enabled before
// any *_ref write surface is exposed.
func (s *Scheduler) WithCredentialEnvelopes(enabled bool) *Scheduler {
	s.credentialEnvelopes = enabled
	return s
}

func (s *Scheduler) WithSecretResolutionMetrics(sink SecretResolutionSink) *Scheduler {
	s.secretResolution = sink
	return s
}

// WithConfirmSignals wires the stream of monitor ids that just entered their
// failure-confirmation phase (store.ConfirmNotifier), so the leader reschedules
// their next probe at the confirm interval immediately instead of waiting for
// the snapshot refresh. Optional: without it, the refresh fallback still
// accelerates within refreshEvery.
func (s *Scheduler) WithConfirmSignals(ch <-chan string) *Scheduler {
	s.confirmCh = ch
	return s
}

// WithConfigSignals wires the execution-config change stream (store.ConfigNotifier on
// monitor_config_changed), so a committed file-provider apply forces the leader to reload its
// enabled-monitor snapshot on the next tick instead of waiting out refreshEvery (spec §12).
// Optional: without it, the periodic snapshot refresh is the fallback (up to refreshEvery late).
func (s *Scheduler) WithConfigSignals(ch <-chan struct{}) *Scheduler {
	s.configCh = ch
	return s
}

// New builds a scheduler.
func New(store Store, dispatcher dispatch.Dispatcher, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		store:         store,
		dispatcher:    dispatcher,
		logger:        logger,
		tick:          time.Second,
		retry:         5 * time.Second,
		leaderKey:     advisoryLockKey,
		retentionDays: defaultRetentionDays,
		alertSuccess:  map[string]time.Time{},
		now:           time.Now,
	}
}

// WithReconciler wires the shared post-commit flow (SSE event + auto-incident) run after a
// dead-man DOWN is applied. Optional and nil-safe: without it the status/outbox still land
// (via RecordDeadmanResult), only the live SSE event + incident reconciliation are skipped.
func (s *Scheduler) WithReconciler(rc *ingest.Reconciler) *Scheduler {
	s.reconciler = rc
	return s
}

// WithRetentionDays sets the raw-heartbeat retention window (in days), which also
// bounds the rollup recompute window. Values below 2 are clamped up. Returns the
// scheduler for chaining.
func (s *Scheduler) WithRetentionDays(days int) *Scheduler {
	if days < 2 {
		days = 2
	}
	s.retentionDays = days
	return s
}

// WithLiveRegions wires the RabbitMQ management lookup so the leader can alert when a
// region with enabled monitors loses its worker. Without it, region-worker alerting is
// skipped (the inproc build has no broker and always co-locates its worker).
func (s *Scheduler) WithLiveRegions(src LiveRegionSource) *Scheduler {
	s.liveRegions = src
	return s
}

func (s *Scheduler) WithCredentialLiveRegions(src CredentialLiveRegionSource) *Scheduler {
	s.credentialLiveRegions = src
	return s
}

// WithLocalCredentialRegions authorizes in-process v2 execution for --role=all, where
// there is no RabbitMQ consumer to discover but the validated local keyring is present.
func (s *Scheduler) WithLocalCredentialRegions(regions ...string) *Scheduler {
	if s.localCredentialRegions == nil {
		s.localCredentialRegions = map[string]bool{}
	}
	for _, region := range regions {
		s.localCredentialRegions[region] = true
	}
	return s
}

// WithPullRegions marks regions served over the HTTP-pull transport: their check jobs
// are enqueued to the pull queue (claimed by an agent over HTTP) instead of published
// to RabbitMQ. Regions not listed keep using the AMQP dispatcher.
func (s *Scheduler) WithPullRegions(regions []string) *Scheduler {
	set := make(map[string]bool, len(regions))
	for _, r := range regions {
		if r != "" {
			set[r] = true
		}
	}
	s.pullRegions = set
	return s
}

// WithPullMetrics wires the per-region pull-queue gauge sink so the leader publishes
// depth/lag for observability. Optional; without it, sampling is skipped.
// WithServiceMetrics wires the service-reliability gauges/counters sink.
func (s *Scheduler) WithServiceMetrics(sink ServiceStatsSink) *Scheduler {
	s.serviceMetrics = sink
	return s
}

func (s *Scheduler) WithPullMetrics(sink PullStatsSink) *Scheduler {
	s.pullMetrics = sink
	return s
}

// WithLeaderState wires the sink for the cerbix_scheduler_leader gauge, set to 1
// while this process is the leader and 0 on standby / after stepping down. Optional.
func (s *Scheduler) WithLeaderState(sink LeaderStateSink) *Scheduler {
	s.leaderState = sink
	return s
}

// setLeaderState is a nil-safe gauge update.
// serviceWedgeReason decides whether service-reliability work is WEDGED (§21: readiness
// must not report healthy while it is). Wedged means an operator is REQUIRED, so the rule is
// deliberately narrow: a repair range terminally parked in `error` — nothing will ever retry
// it by itself. Watermark LAG is explicitly NOT a wedge: a service adopting ninety days of
// history reports an enormous, shrinking lag while every slice advances normally, and a
// single absolute sample cannot tell that healthy backlog from a stuck materializer. Telling
// them apart needs progress ACROSS successive samples; until such a tracker exists, lag is a
// gauge with an alerting suggestion in the runbook, never a readiness verdict.
func serviceWedgeReason(st metrics.ServiceReliabilityStat) (bool, string) {
	if st.RepairErrored > 0 {
		return true, "service repair ranges parked in error"
	}
	return false, ""
}

// serviceAlertStallThreshold is §16.6b's readiness bound: an evaluation whose lag exceeds
// `lease_multiplier × cadence` marks the SCHEDULER not-ready, because a stalled evaluator is
// exactly the state in which delegation dis-arms and members resume paging — the system is
// degraded, and reporting ready would hide it.
//
// The multiplier is the store's, not a second number: the lease the evaluator writes and the lag
// readiness judges are the same claim about freshness read from two sides, and a bound that drifted
// from the lease would either wedge a healthy leader or stay ready straight through a stall.
func serviceAlertStallThreshold(cadence time.Duration) time.Duration {
	return store.ServiceAlertLeaseMultiplier * cadence
}

// serviceAlertPass is one arm's result reduced to the §16.6b vocabulary, so both arms publish
// through ONE function and cannot drift into two dialects of "ok".
type serviceAlertPass struct {
	signal  string
	cadence time.Duration
	lag     time.Duration
	// ok, skipped and errors PARTITION the units the pass touched — a service for the live arm, a
	// burn rule for the sealed one. `skipped` is where the burn arm's HOLDs land: a hold is a
	// successful evaluation that cannot speak, which is neither an error nor an answer.
	ok, skipped, errors int
	// withheld counts ONSETS a successful evaluation refused to announce, BY REASON (D-0176). The
	// reasons are a fixed pair — a broken route and a declaration that has not taken effect — and
	// they are different problems with different owners, so reporting one number would have named
	// the second as the first.
	withheld       map[string]int
	onsets, closes int
	// FR-022: incidents this pass OPENED and RESOLVED by machine. Counted apart from the edges
	// because they are not the same event — an onset for a service whose incident is already open
	// announces and opens nothing, and that difference is the interesting one.
	incidentsOpened, incidentsResolved int
}

// observeServiceAlertPass publishes ONE successful evaluation pass: the per-outcome counters, the
// edges it enqueued, the freshness gauges, and the readiness verdict.
//
// All three outcomes are recorded every pass, zeros included, so the series exist from the first
// healthy pass and an alert on `outcome="error"` does not have to survive a missing series first.
func (s *Scheduler) observeServiceAlertPass(p serviceAlertPass) {
	if s.serviceMetrics == nil {
		return
	}
	s.serviceMetrics.RecordServiceAlertEvaluations(p.signal, "ok", p.ok)
	s.serviceMetrics.RecordServiceAlertEvaluations(p.signal, "error", p.errors)
	s.serviceMetrics.RecordServiceAlertEvaluations(p.signal, "skipped", p.skipped)
	// Its OWN family, not a fourth `outcome`: that label partitions the units of work a pass did,
	// and a withheld onset is not a fourth kind of unit — it is something that did not happen to
	// one, so it would have overlapped `ok`.
	for reason, n := range p.withheld {
		s.serviceMetrics.RecordServiceAlertWithheld(p.signal, reason, n)
	}
	s.serviceMetrics.RecordServiceAlertEmitted(p.signal, "onset", p.onsets)
	s.serviceMetrics.RecordServiceAlertEmitted(p.signal, "close", p.closes)
	s.serviceMetrics.RecordServiceIncidents("opened", p.incidentsOpened)
	s.serviceMetrics.RecordServiceIncidents("resolved", p.incidentsResolved)
	now := s.clock()
	s.markAlertSuccess(p.signal, now)
	s.serviceMetrics.SetServiceAlertPass(p.signal, now.Unix(), p.lag.Seconds())

	threshold := serviceAlertStallThreshold(p.cadence)
	if p.lag > threshold {
		reason := fmt.Sprintf("service %s alert evaluation lagging %ds (bound %ds)",
			p.signal, int(p.lag.Seconds()), int(threshold.Seconds()))
		s.serviceMetrics.SetServiceAlertStalled(p.signal, true, reason)
		s.logger.Warn("service_alert_evaluator_stalled", "signal", p.signal,
			"lag_seconds", int(p.lag.Seconds()), "bound_seconds", int(threshold.Seconds()))
		return
	}
	s.serviceMetrics.SetServiceAlertStalled(p.signal, false, "")
}

// recordServiceAlertArmError counts a whole pass that rolled back. The unit counters above measure
// items, but a slice that failed evaluated NONE of them, and an arm erroring every cadence with no
// series moving at all would be invisible in a family that only counts successes.
//
// It also decides readiness, which an earlier revision left to the success path alone. That was
// wrong: lag is only measurable from a pass that READ the state, so an arm erroring every cadence
// kept the last successful pass's lag forever and read as healthy — while every lease it should
// have refreshed expired and every service it covers dis-armed. Invariant 91 asks for the opposite:
// a stalled evaluator marks the scheduler not-ready.
//
// The measure is therefore the AGE of the last success, not the last reported lag. With no success
// yet, it ages from when this leadership began — a process that has just acquired the lock is not
// stalled, it is starting.
func (s *Scheduler) recordServiceAlertArmError(signal string, cadence time.Duration) {
	if s.serviceMetrics == nil {
		return
	}
	s.serviceMetrics.RecordServiceAlertEvaluations(signal, "error", 1)

	age := s.sinceAlertSuccess(signal, s.clock())
	threshold := serviceAlertStallThreshold(cadence)
	if age <= threshold {
		return
	}
	reason := fmt.Sprintf("service %s alert evaluation failing for %ds (bound %ds)",
		signal, int(age.Seconds()), int(threshold.Seconds()))
	s.serviceMetrics.SetServiceAlertStalled(signal, true, reason)
	s.logger.Warn("service_alert_evaluator_failing", "signal", signal,
		"age_seconds", int(age.Seconds()), "bound_seconds", int(threshold.Seconds()))
}

// clock is the injectable now, so a readiness test can age a stall instead of sleeping.
func (s *Scheduler) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// markAlertSuccess records that an arm completed a pass, and startAlertBaseline records that this
// leadership began — the point failures age from before any pass has succeeded.
func (s *Scheduler) markAlertSuccess(signal string, at time.Time) {
	s.alertSuccessMu.Lock()
	defer s.alertSuccessMu.Unlock()
	if s.alertSuccess == nil {
		s.alertSuccess = map[string]time.Time{}
	}
	s.alertSuccess[signal] = at
}

func (s *Scheduler) startAlertBaseline(at time.Time) {
	for _, signal := range []string{"health", "burn"} {
		s.markAlertSuccess(signal, at)
	}
}

func (s *Scheduler) sinceAlertSuccess(signal string, now time.Time) time.Duration {
	s.alertSuccessMu.Lock()
	defer s.alertSuccessMu.Unlock()
	last, ok := s.alertSuccess[signal]
	if !ok {
		// No baseline at all: treat it as starting now rather than as infinitely stale, so a
		// process that has not yet run a cycle cannot declare itself wedged.
		s.alertSuccess[signal] = now
		return 0
	}
	return now.Sub(last)
}

// serviceStatsLoop samples the bounded service-reliability snapshot on its own cadence and
// derives the wedge verdict, isolated from dispatch.
//
// FAIL-CLOSED from the first instant: until a sample SUCCEEDS the subsystem's state is
// UNKNOWN, and unknown must not read as healthy — a terminal error range persisted before
// this leadership began would otherwise hide behind an empty registry until the first query
// landed. A failed sample re-enters the same unavailable state for the same reason.
func (s *Scheduler) serviceStatsLoop(ctx context.Context) {
	s.serviceMetrics.SetServiceWedged(true, "service reliability state unknown (no sample yet)")
	sample := func() {
		withTimeout(ctx, subCadenceTimeout, func(c context.Context) {
			st, err := s.store.ServiceReliabilityStats(c)
			if err != nil {
				if ctx.Err() != nil {
					return // shutting down: the join + step-down clear own what follows
				}
				s.logger.Warn("service_reliability_stats_failed", "error", err.Error())
				s.serviceMetrics.SetServiceWedged(true, "service reliability stats unavailable")
				return
			}
			s.serviceMetrics.SetServiceReliabilityStats(st)
			wedged, reason := serviceWedgeReason(st)
			s.serviceMetrics.SetServiceWedged(wedged, reason)
		})
		// The alerting sample (FR-021 §16.6b) rides the same cadence but its OWN timeout and its
		// own verdict: it answers what a slice cannot see (open episodes, evaluation backlog), and
		// failing to read it is not evidence that reliability work is wedged. It leaves the
		// previous gauges standing rather than publishing zeros — "we could not look" and "there
		// is nothing open" are the two answers this feature must never confuse.
		withTimeout(ctx, subCadenceTimeout, func(c context.Context) {
			st, err := s.store.ServiceAlertStats(c)
			if err != nil {
				if ctx.Err() != nil {
					return // shutting down: the join + step-down clear own what follows
				}
				s.logger.Warn("service_alert_stats_failed", "error", err.Error())
				return
			}
			s.serviceMetrics.SetServiceAlertStats(st)
		})
	}
	sample()
	ticker := time.NewTicker(s.statsCadence())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sample()
		}
	}
}

// statsCadence is pullStatsEvery unless a test shortens it to drive multi-sample lifecycles.
func (s *Scheduler) statsCadence() time.Duration {
	if s.statsEvery > 0 {
		return s.statsEvery
	}
	return pullStatsEvery
}

// serviceFactMaintenanceLoop keeps the monthly fact partitions pre-created and adopts
// stranded DEFAULT months, off the dispatch loop, for this leadership's lifetime.
func (s *Scheduler) serviceFactMaintenanceLoop(ctx context.Context) {
	run := func() {
		withTimeout(ctx, factMaintenanceTimeout, func(c context.Context) {
			err := s.store.EnsureServiceFactPartitions(c, factPartitionAheadMonths)
			if err != nil {
				s.logger.Warn("ensure_service_fact_partitions_failed", "error", err.Error())
			}
			if s.serviceMetrics != nil && ctx.Err() == nil {
				s.serviceMetrics.SetServiceFactMaintenance(err == nil, time.Now().Unix())
			}
		})
	}
	run()
	ticker := time.NewTicker(factMaintenanceEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func (s *Scheduler) setLeaderState(leader bool) {
	if s.leaderState != nil {
		s.leaderState.SetSchedulerLeader(leader)
	}
	// A deposed leader stops exporting the service gauges and its wedge verdict: two
	// schedulers describing one cluster from different moments is the HA lie the clear
	// exists to prevent — and a stale wedged=true would hold /readyz down on a standby.
	if !leader && s.serviceMetrics != nil {
		s.serviceMetrics.ClearServiceReliabilityStats()
	}
	// The retained-changes gauge is the leader's sample of one table: a standby exporting the
	// previous leader's count is the same two-answers lie (FR-025 D15).
	if !leader && s.changeMetrics != nil {
		s.changeMetrics.ClearChangesRetained()
	}
}

// Run blocks until ctx is cancelled, contending for leadership and scheduling
// checks while leader.
func (s *Scheduler) Run(ctx context.Context) {
	s.setLeaderState(false) // standby until we win the lock
	for {
		if ctx.Err() != nil {
			return
		}
		session, ok, err := s.store.TryBecomeLeaderSession(ctx, s.leaderKey)
		if err != nil {
			s.logger.Error("leader_election_failed", "error", err.Error())
			if !sleep(ctx, s.retry) {
				return
			}
			continue
		}
		if !ok {
			s.logger.Debug("scheduler_standby")
			if !sleep(ctx, s.retry) {
				return
			}
			continue
		}
		s.logger.Info("scheduler_leader_acquired")
		// UNKNOWN is published SYNCHRONOUSLY, before leadership is visible and before the
		// sampler goroutine is even spawned: the readiness endpoint can run between
		// setLeaderState(true) and the goroutine's first instruction, and in that window a
		// terminal error state persisted before this leadership must not read as healthy.
		if s.serviceMetrics != nil {
			s.serviceMetrics.SetServiceWedged(true, "service reliability state unknown (no sample yet)")
		}
		// The alerting arms age their failures from HERE. A process that has just acquired the
		// lock has not stalled, it has started, and a baseline taken at the epoch would make it
		// declare itself not-ready before its first cadence.
		s.startAlertBaseline(s.clock())
		s.setLeaderState(true)
		lost := s.lead(ctx, session)
		session.Release()
		s.setLeaderState(false)
		// A clean context cancellation is shutdown → stop. A lost lock (watchdog
		// stepped us down) means we must re-contend, not exit: another node may have
		// taken over, or the lock is free again after a Postgres blip.
		if lost && ctx.Err() == nil {
			s.logger.Warn("scheduler_leader_lost_recontending")
			if !sleep(ctx, s.retry) {
				return
			}
			continue
		}
		s.logger.Info("scheduler_leader_released")
		return
	}
}

// lead runs the scheduling loop until ctx is cancelled or leadership is lost. It
// reloads the enabled monitors on a slow cadence into an in-memory snapshot and, on
// each fast tick, publishes due active checks from that snapshot — so the hot path no
// longer scans the monitors table every second. Push-liveness runs as a single batched
// query on its own cadence. Returns true if it stepped down because the advisory lock
// was lost (the caller re-contends), false on a clean context cancellation.
func (s *Scheduler) lead(ctx context.Context, session LeaderSession) bool {
	// The service stats sampler runs OFF the dispatch loop, in its own goroutine for this
	// leadership's lifetime: its samples, however bounded, are still queries, and a
	// synchronous sample inside the loop could hold dispatch against §10.10's 250ms cadence.
	// The lifecycle is CALLER-OWNED: cancel AND JOIN before lead returns, so the step-down
	// clear in setLeaderState(false) can never race a sample completing mid-cancellation and
	// resurrecting a deposed leader's gauges.
	if s.serviceMetrics != nil {
		statsCtx, stopStats := context.WithCancel(ctx)
		statsDone := make(chan struct{})
		go func() {
			defer close(statsDone)
			s.serviceStatsLoop(statsCtx)
		}()
		defer func() {
			stopStats()
			<-statsDone
		}()
	}
	// Fact-partition maintenance likewise runs off the loop: its DEFAULT-month recovery
	// moves data in batches, and even bounded batches do not belong inside a dispatch tick.
	{
		maintCtx, stopMaint := context.WithCancel(ctx)
		maintDone := make(chan struct{})
		go func() {
			defer close(maintDone)
			s.serviceFactMaintenanceLoop(maintCtx)
		}()
		defer func() {
			stopMaint()
			<-maintDone
		}()
	}
	// Decision-ledger maintenance (FR-024 D10) runs off the loop too, on its OWN fenced session:
	// a DETACH or ATTACH waiting behind a lock must leave the dispatch tick firing on cadence.
	// Same caller-owned lifecycle — cancelled AND joined before lead returns — so its step-down
	// clear cannot race a pass completing mid-cancellation.
	if s.gateMaintenanceEnabled() {
		gateCtx, stopGate := context.WithCancel(ctx)
		gateDone := make(chan struct{})
		go func() {
			defer close(gateDone)
			s.gateLedgerMaintenanceLoop(gateCtx)
		}()
		defer func() {
			stopGate()
			<-gateDone
		}()
	}
	nextRun := map[string]time.Time{}
	credentialFailures := map[string]int{}
	// Consecutive publish/enqueue failures per monitor, kept apart from credential
	// failures so the two causes back off independently (§4.4.5).
	publishFailures := map[string]int{}
	credentialLastLog := map[string]time.Time{}
	var monitors []domain.Monitor
	byID := map[string]domain.Monitor{}
	// confirmFast holds monitors currently probed at their confirm interval,
	// mapped to an acceleration expiry (one main interval past the last failure
	// signal — a recovery stops the signals, so acceleration decays on its own;
	// the snapshot refresh prunes it authoritatively).
	confirmFast := map[string]time.Time{}
	var lastRollup, lastMaintain, lastRefresh, lastPushCheck, lastRenotify, lastBurn, lastReport, lastRegionChk, lastEscalation, lastPullStats, lastPublishWarn, lastLeaderChk, lastServiceSlice, lastServiceAlert, lastServiceBurn, lastChangePurge time.Time
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case id := <-s.confirmCh:
			// A failure was just counted (no verdict yet): probe again at the
			// confirm interval, respecting the per-region cap.
			if m, ok := byID[id]; ok && m.ConfirmConfigured() {
				s.enterConfirm(confirmFast, byID, m, time.Now(), nextRun)
			}
		case <-s.configCh:
			// A file-provider apply committed an execution-config change: force a snapshot
			// reload on the next tick so it is scheduled promptly, not up to refreshEvery late
			// (spec §12). A zero lastRefresh makes the tick's refresh guard fire immediately.
			lastRefresh = time.Time{}
		case now := <-ticker.C:
			// Anti-split-brain watchdog: verify we still hold the lock before doing
			// any leader work this tick. On loss (connection died, Postgres blip)
			// step down immediately so we stop dispatching against a lock we no
			// longer own; the caller re-contends.
			if session != nil && (lastLeaderChk.IsZero() || now.Sub(lastLeaderChk) >= leaderCheckEvery) {
				lastLeaderChk = now
				held, err := session.Check(ctx)
				if err != nil {
					s.logger.Error("scheduler_leadership_check_failed", "error", err.Error())
					return true
				}
				if !held {
					s.logger.Warn("scheduler_leadership_lost")
					return true
				}
			}
			// Service reliability: durable repair ranges FIRST, then forward
			// materialization, on the LOCK-OWNING connection, in slices short enough
			// that dispatch is serviced between them.
			// An empty queue costs one claim query and backs off to the next sub-tick, so
			// an installation with no services pays effectively nothing.
			if session != nil && now.Sub(lastServiceSlice) >= serviceSliceEvery {
				lastServiceSlice = now
				spent := time.Duration(0)
				for spent < serviceCycleBudget {
					slice := maxDispatchDelay
					if left := serviceCycleBudget - spent; left < slice {
						slice = left
					}
					started := time.Now()
					worked, err := session.RunServiceSlice(ctx, started.Add(slice))
					spent += time.Since(started)
					if s.serviceMetrics != nil {
						switch {
						case err != nil:
							s.serviceMetrics.RecordServiceSlice("error")
						case worked:
							s.serviceMetrics.RecordServiceSlice("worked")
						default:
							s.serviceMetrics.RecordServiceSlice("empty")
						}
					}
					if err != nil {
						s.logger.Error("service_slice_failed", "error", err.Error())
						break
					}
					if !worked {
						break
					}
				}
			}
			if now.Sub(lastRollup) >= rollupEvery {
				lastRollup = now
				today := now.UTC().Truncate(24 * time.Hour)
				// Recompute only the retained range: raw older than retention is
				// dropped, so recomputing it would wipe the frozen rollup rows that
				// carry long-range availability.
				withTimeout(ctx, subCadenceTimeout, func(c context.Context) {
					if err := s.store.RollupDailyAvailability(c, today.AddDate(0, 0, -s.retentionDays), today); err != nil {
						s.logger.Error("rollup_daily_failed", "error", err.Error())
					}
				})
			}
			if lastMaintain.IsZero() || now.Sub(lastMaintain) >= maintainEvery {
				lastMaintain = now
				withTimeout(ctx, maintainTimeout, func(c context.Context) { s.maintainPartitions(c, now) })
			}
			// Change-group retention (FR-025 D9): the daily pass on the leader's existing
			// retention cadence, bounded like the partition sweep.
			if s.changeRetentionDays > 0 && (lastChangePurge.IsZero() || now.Sub(lastChangePurge) >= changePurgeEvery) {
				lastChangePurge = now
				withTimeout(ctx, maintainTimeout, func(c context.Context) { s.purgeChangeGroups(c, now) })
			}
			if lastRefresh.IsZero() || now.Sub(lastRefresh) >= refreshEvery {
				lastRefresh = now
				withTimeout(ctx, subCadenceTimeout, func(c context.Context) {
					var ms []domain.Monitor
					var err error
					if s.credentialEnvelopes {
						ms, err = s.store.ListEnabledMonitorSnapshots(c)
					} else {
						ms, err = s.store.ListEnabledMonitors(c)
					}
					if err != nil {
						s.logger.Error("list_enabled_monitors_failed", "error", err.Error())
						return
					}
					monitors = ms
					pruneNextRun(nextRun, monitors)
					byID = make(map[string]domain.Monitor, len(monitors))
					for _, m := range monitors {
						byID[m.ID] = m
					}
					for id := range credentialFailures {
						if _, ok := byID[id]; !ok {
							delete(credentialFailures, id)
							delete(credentialLastLog, id)
						}
					}
					// The fresh snapshot is authoritative: drop acceleration for
					// monitors no longer mid-confirmation, and pick up ones whose
					// notify signal was missed (fallback path).
					for id := range confirmFast {
						if m, ok := byID[id]; !ok || !m.InConfirmPhase() {
							delete(confirmFast, id)
						}
					}
					for _, m := range monitors {
						if m.InConfirmPhase() {
							s.enterConfirm(confirmFast, byID, m, now, nextRun)
						}
					}
				})
			}
			if lastPushCheck.IsZero() || now.Sub(lastPushCheck) >= pushCheckEvery {
				lastPushCheck = now
				withTimeout(ctx, subCadenceTimeout, func(c context.Context) { s.checkStalePush(c, now, nextRun) })
			}
			if lastRenotify.IsZero() || now.Sub(lastRenotify) >= renotifyEvery {
				lastRenotify = now
				withTimeout(ctx, subCadenceTimeout, func(c context.Context) {
					if n, err := s.store.EnqueueRenotifyReminders(c); err != nil {
						s.logger.Error("renotify_failed", "error", err.Error())
					} else if n > 0 {
						s.logger.Info("renotify_reminders_enqueued", "count", n)
					}
				})
			}
			if lastBurn.IsZero() || now.Sub(lastBurn) >= burnEvery {
				lastBurn = now
				withTimeout(ctx, subCadenceTimeout, func(c context.Context) {
					if fired, resolved, err := s.store.EvaluateBurnAlerts(c); err != nil {
						s.logger.Error("burn_eval_failed", "error", err.Error())
					} else if fired > 0 || resolved > 0 {
						s.logger.Info("burn_alerts_evaluated", "fired", fired, "resolved", resolved)
					}
				})
			}
			// FR-021 §16.3/§16.4. Both arms are LEADER-ONLY and session-fenced: `session != nil`
			// is the same guard the service slices use, because an evaluation writes the arming
			// state that silences other people's alerts, and a deposed leader must not.
			if session != nil && (lastServiceAlert.IsZero() || now.Sub(lastServiceAlert) >= serviceAlertEvery) {
				lastServiceAlert = now
				withTimeout(ctx, subCadenceTimeout, func(c context.Context) {
					ev, err := session.EvaluateServiceAlerts(c, serviceAlertEvery)
					if err != nil {
						s.recordServiceAlertArmError(string(domain.ServiceSignalHealth), serviceAlertEvery)
						s.logger.Error("service_alert_eval_failed", "error", err.Error())
						return
					}
					// The live arm's unit is the SERVICE: `Evaluated` are the ones that got a
					// verdict, `Errors` the ones whose evaluation dis-armed them, and `withheld` the
					// onsets it refused to announce for want of a recipient.
					s.observeServiceAlertPass(serviceAlertPass{
						signal: string(domain.ServiceSignalHealth), cadence: serviceAlertEvery,
						lag: ev.Lag, ok: ev.Evaluated, errors: ev.Errors, withheld: ev.Withheld,
						onsets: ev.Onsets, closes: ev.Closes,
						incidentsOpened: ev.IncidentsOpened, incidentsResolved: ev.IncidentsResolved,
					})
					// Logged whenever anything happened OR anything is behind: a stalled evaluator
					// has to read as lag rather than as an absence of alerts, which is
					// indistinguishable from "nothing is wrong" (§16.7).
					if ev.Onsets > 0 || ev.Closes > 0 || ev.Errors > 0 || len(ev.Withheld) > 0 ||
						ev.Lag > serviceAlertEvery {
						s.logger.Info("service_alerts_evaluated", "evaluated", ev.Evaluated,
							"onsets", ev.Onsets, "closes", ev.Closes, "errors", ev.Errors,
							"incidents_opened", ev.IncidentsOpened, "incidents_resolved", ev.IncidentsResolved,
							"lag_seconds", int(ev.Lag.Seconds()))
					}
				})
			}
			if session != nil && (lastServiceBurn.IsZero() || now.Sub(lastServiceBurn) >= serviceBurnEvery) {
				lastServiceBurn = now
				withTimeout(ctx, subCadenceTimeout, func(c context.Context) {
					ev, err := session.EvaluateServiceBurnAlerts(c, serviceBurnEvery)
					if err != nil {
						s.recordServiceAlertArmError(string(domain.ServiceSignalBurn), serviceBurnEvery)
						s.logger.Error("service_burn_eval_failed", "error", err.Error())
						return
					}
					// The burn arm's unit is the RULE (`Rules` is every rule that got a verdict,
					// `Holds` the subset that could not be quoted), except for `Errors`, which the
					// evaluator counts per TARGET because a target that cannot be read dis-arms all
					// of its rules at once and names none of them.
					s.observeServiceAlertPass(serviceAlertPass{
						signal: string(domain.ServiceSignalBurn), cadence: serviceBurnEvery,
						lag: ev.Lag, ok: ev.Rules - ev.Holds, skipped: ev.Holds, errors: ev.Errors,
						withheld: ev.Withheld,
						onsets:   ev.Onsets, closes: ev.Closes,
					})
					if ev.Onsets > 0 || ev.Closes > 0 || ev.Holds > 0 || ev.Errors > 0 || ev.Lag > serviceBurnEvery {
						s.logger.Info("service_burn_alerts_evaluated", "targets", ev.Targets,
							"rules", ev.Rules, "onsets", ev.Onsets, "closes", ev.Closes,
							"holds", ev.Holds, "errors", ev.Errors,
							"lag_seconds", int(ev.Lag.Seconds()))
					}
				})
			}
			if lastReport.IsZero() || now.Sub(lastReport) >= reportEvery {
				lastReport = now
				withTimeout(ctx, subCadenceTimeout, func(c context.Context) {
					if n, err := s.store.EnqueueDueSLAReports(c); err != nil {
						s.logger.Error("sla_report_enqueue_failed", "error", err.Error())
					} else if n > 0 {
						s.logger.Info("sla_reports_enqueued", "count", n)
					}
				})
			}
			if s.liveRegions != nil && (lastRegionChk.IsZero() || now.Sub(lastRegionChk) >= regionWorkerEvery) {
				lastRegionChk = now
				withTimeout(ctx, subCadenceTimeout, func(c context.Context) {
					// A lookup failure must NOT be treated as "no workers" (that would alert
					// every region), so evaluate only on a successful management response.
					if liveSet, err := s.liveRegions.LiveJobRegions(c); err != nil {
						s.logger.Warn("region_worker_live_lookup_failed", "error", err.Error())
					} else if fired, resolved, err := s.store.EvaluateRegionWorkerAlerts(c, liveSet, regionWorkerGraceSeconds); err != nil {
						s.logger.Error("region_worker_eval_failed", "error", err.Error())
					} else if fired > 0 || resolved > 0 {
						s.logger.Info("region_worker_alerts_evaluated", "fired", fired, "resolved", resolved)
					}
				})
			}
			if s.pullMetrics != nil && (lastPullStats.IsZero() || now.Sub(lastPullStats) >= pullStatsEvery) {
				lastPullStats = now
				withTimeout(ctx, subCadenceTimeout, func(c context.Context) {
					if stats, err := s.store.PullQueueStats(c); err != nil {
						s.logger.Warn("pull_queue_stats_failed", "error", err.Error())
					} else {
						s.pullMetrics.SetPullStats(stats)
						for _, st := range stats {
							if st.LagSeconds >= pullLagWarnSeconds {
								s.logger.Warn("pull_region_lagging", "region", st.Region, "pending", st.Pending, "lag_seconds", int(st.LagSeconds))
							}
						}
					}
				})
			}
			if lastEscalation.IsZero() || now.Sub(lastEscalation) >= escalationEvery {
				lastEscalation = now
				withTimeout(ctx, subCadenceTimeout, func(c context.Context) {
					if p, err := s.store.AdvanceEscalations(c); err != nil {
						s.logger.Error("advance_escalations_failed", "error", err.Error())
					} else if p.Total() > 0 {
						// Split by SUBJECT (FR-023): "12 steps fired" stops being an answer the moment
						// two kinds of thing can page, and the interesting question at 3am is which.
						s.logger.Info("escalations_advanced", "fired", p.Total(),
							"monitor_steps", p.MonitorSteps, "service_steps", p.ServiceSteps)
						if s.serviceMetrics != nil {
							s.serviceMetrics.RecordEscalationSteps("monitor", p.MonitorSteps)
							s.serviceMetrics.RecordEscalationSteps("service", p.ServiceSteps)
						}
					}
				})
			}
			// Publish due active checks. Non-credentialed monitors keep the snapshot path;
			// credentialed monitors are nominated by the snapshot then authorized/materialized
			// in one bounded DB batch immediately before dispatch (§4.4.3/4).
			publishFailed, publishErr := 0, ""
			credentialByRegion := map[string][]string{}
			for _, m := range monitors {
				if m.Type == domain.MonitorPush || !m.Type.Active() {
					continue
				}
				if due, ok := nextRun[m.ID]; ok && now.Before(due) {
					continue
				}
				if s.credentialEnvelopes && domain.CredentialedType(m.Type) {
					credentialByRegion[m.Region] = append(credentialByRegion[m.Region], m.ID)
					continue
				}
				// Confirmation phase: probe at the accelerated interval until the
				// acceleration expires (recovery/verdict stops the signals).
				iv := m.Interval()
				if exp, ok := confirmFast[m.ID]; ok {
					if now.After(exp) {
						delete(confirmFast, m.ID)
					} else {
						iv = m.ConfirmInterval()
					}
				}
				// FR-029 D9/D9a. BOTH dispatch paths take the lease: a canary with no binding never
				// reaches the credential branch below, and the first version of this change put the
				// claim only there — so a canary without secrets kept every guarantee off.
				if !s.claimCanarySlot(ctx, m) {
					nextRun[m.ID] = now.Add(iv)
					continue
				}
				if s.pullRegions[m.Region] {
					// Pull-served region: enqueue for HTTP claim with a TTL (~interval) so
					// a job for a region with no live agent expires rather than piling up.
					// Confirm-phase jobs carry the short TTL so stale fast probes don't stack.
					payload, err := json.Marshal(dispatch.CheckJob{Monitor: m})
					if err != nil {
						s.logger.Error("marshal_pull_job_failed", "monitor_id", m.ID, "error", err.Error())
						continue // never enqueued → don't advance the cadence
					}
					if err := s.store.EnqueuePullJob(ctx, m.Region, payload, int(iv/time.Second), pullLeaseFor(m)); err != nil {
						if ctx.Err() != nil {
							return false
						}
						// Leave nextRun unchanged so the next tick retries this monitor
						// promptly rather than skipping a whole interval after a transient
						// enqueue failure.
						s.logger.Error("enqueue_pull_job_failed", "monitor_id", m.ID, "error", err.Error())
						continue
					}
					nextRun[m.ID] = now.Add(iv)
					continue
				}
				if err := s.dispatcher.PublishJob(ctx, dispatch.CheckJob{Monitor: m}); err != nil {
					if ctx.Err() != nil {
						return false
					}
					// During a broker outage every due monitor fails — aggregate
					// into one line per tick instead of a line per monitor.
					publishFailed++
					publishErr = err.Error()
					continue
				}
				nextRun[m.ID] = now.Add(iv)
			}
			regions := make([]string, 0, len(credentialByRegion))
			for region := range credentialByRegion {
				regions = append(regions, region)
			}
			sort.Strings(regions)
			credentialReadyPull := map[string]bool{}
			credentialReadyAMQP := map[string]bool{}
			// carrierGeneration is the HIGHEST carrier a region may be emitted into: 3 once
			// something there can open envelope v2, otherwise 2. It is derived from the same
			// existential checks as readiness, never assumed, so core cannot emit a payload
			// the region's executors are unable to open (§4.7, D-0160).
			carrierGeneration := map[string]int{}
			if len(regions) > 0 {
				// Capability 1 is the floor for the generation-2 carrier; the floor rises with
				// the emitted generation, never independently of it.
				if ready, err := s.store.LiveCredentialReadyAgentRegions(ctx, 45*time.Second, dispatch.EnvelopeV1); err != nil {
					s.logger.Warn("credential_agent_capability_lookup_failed", "error", err.Error())
				} else {
					credentialReadyPull = ready
				}
				if ready, err := s.store.LiveCredentialReadyAgentRegions(ctx, 45*time.Second, dispatch.EnvelopeV2); err != nil {
					s.logger.Warn("credential_agent_capability_lookup_failed", "error", err.Error())
				} else {
					for region := range ready {
						if s.pullRegions[region] {
							carrierGeneration[region] = dispatch.ProtocolV3
						}
					}
				}
				if s.credentialLiveRegions != nil {
					if ready, err := s.credentialLiveRegions.LiveCredentialJobRegions(ctx); err != nil {
						s.logger.Warn("credential_worker_capability_lookup_failed", "error", err.Error())
					} else {
						credentialReadyAMQP = ready
					}
					if ready, err := s.credentialLiveRegions.LiveCredentialV3JobRegions(ctx); err != nil {
						s.logger.Warn("credential_worker_capability_lookup_failed", "error", err.Error())
					} else {
						for region := range ready {
							if !s.pullRegions[region] {
								carrierGeneration[region] = dispatch.ProtocolV3
							}
						}
					}
				}
				for region := range s.localCredentialRegions {
					credentialReadyAMQP[region] = true
					// A same-process executor IS this binary, so its capability is ours by
					// construction — there is no wire and no version skew to discover. Without
					// this the default single-binary role=all never moved past generation 2,
					// which meant the execution binding and field-set rules — the whole point
					// of the amendment — were inert in the most common deployment while the
					// worker inside the same process could open them perfectly well.
					carrierGeneration[region] = dispatch.ProtocolV3
				}
			}
			for _, snapshotRegion := range regions {
				ids := credentialByRegion[snapshotRegion]
				for start := 0; start < len(ids); start += 64 {
					end := start + 64
					if end > len(ids) {
						end = len(ids)
					}
					// The whole policy goes in: the batch is nominated by snapshot region,
					// but each job's carrier is picked from its AUTHORITATIVE region.
					items, err := s.store.MaterializeExecutionConfigs(ctx, ids[start:end], carrierGeneration)
					if err != nil {
						if ctx.Err() != nil {
							return false
						}
						s.logger.Error("credential_materialization_batch_failed", "region", snapshotRegion, "count", end-start, "error", err.Error())
						if s.secretResolution != nil {
							s.secretResolution.RecordSecretResolutionFailure("batch_error")
						}
						for _, id := range ids[start:end] {
							if snap, ok := byID[id]; ok {
								credentialFailures[id]++
								nextRun[id] = now.Add(credentialFailureRetry(snap.Interval(), credentialFailures[id]))
							}
						}
						continue
					}
					for _, item := range items {
						// A monitor the authoritative read found disabled, deleted or of a
						// non-dispatchable type is SKIPPED, not failed (§4.4.3). It leaves the
						// failure path entirely — no warning, no failure counter, no backoff —
						// because the snapshot nominating a row the row itself has since
						// disabled is ordinary reconcile churn, and dressing it as an
						// operational error is how a metric and a log both learn to cry wolf.
						// Cadence is not advanced either: the monitor simply is not due.
						if item.Reason == store.MaterializeSkippedCurrentState {
							delete(credentialFailures, item.MonitorID)
							delete(credentialLastLog, item.MonitorID)
							continue
						}
						if item.Reason != "" {
							if last := credentialLastLog[item.MonitorID]; last.IsZero() || now.Sub(last) >= credentialFailureLogEvery {
								credentialLastLog[item.MonitorID] = now
								s.logger.Warn("credential_materialization_rejected", "monitor_id", item.MonitorID, "reason", item.Reason)
							}
							if s.secretResolution != nil {
								s.secretResolution.RecordSecretResolutionFailure(item.Reason)
							}
							if snap, ok := byID[item.MonitorID]; ok {
								credentialFailures[item.MonitorID]++
								nextRun[item.MonitorID] = now.Add(credentialFailureRetry(snap.Interval(), credentialFailures[item.MonitorID]))
							}
							continue
						}
						m := item.Job.Monitor // authoritative row: region + cadence inputs
						if item.Job.ProtocolVersion >= dispatch.ProtocolV2 {
							// Re-checked on the AUTHORITATIVE region, and against the capability
							// this job's OWN carrier needs — not merely the base one. A job
							// materialized for generation 3 must not be published into a region
							// that only proved it can open generation 2, or it lands on a queue
							// nobody there consumes and expires by TTL.
							ready := credentialReadyAMQP[m.Region]
							if s.pullRegions[m.Region] {
								ready = credentialReadyPull[m.Region]
							}
							if ready && item.Job.ProtocolVersion >= dispatch.ProtocolV3 {
								ready = carrierGeneration[m.Region] >= item.Job.ProtocolVersion
							}
							if !ready {
								if last := credentialLastLog[m.ID]; last.IsZero() || now.Sub(last) >= credentialFailureLogEvery {
									credentialLastLog[m.ID] = now
									s.logger.Warn("credential_materialization_rejected", "monitor_id", m.ID, "reason", "no_capable_executor")
								}
								if s.secretResolution != nil {
									s.secretResolution.RecordSecretResolutionFailure("no_capable_executor")
								}
								credentialFailures[m.ID]++
								nextRun[m.ID] = now.Add(credentialFailureRetry(m.Interval(), credentialFailures[m.ID]))
								continue
							}
						}
						iv := m.Interval()
						if exp, ok := confirmFast[m.ID]; ok && now.Before(exp) && m.InConfirmPhase() {
							iv = m.ConfirmInterval()
						}
						// FR-029 D9/D9a. A canary holds its delivery for the whole journey, so a
						// second dispatch of the same monitor would submit a SECOND external
						// transaction, and a region with several executors has no worker-local way
						// to bound how many long journeys run at once. Both are decided here.
						if !s.claimCanarySlot(ctx, m) {
							nextRun[m.ID] = now.Add(iv)
							continue
						}
						if err := s.publishScheduledJob(ctx, item.Job, iv); err != nil {
							if ctx.Err() != nil {
								return false
							}
							publishFailed++
							publishErr = err.Error()
							// Retry eligibility is STATE, not a rate (§4.4.5, D-0160): the
							// failure counter grows, next-eligible moves to now+backoff, and
							// the probe is not marked sent. Leaving nextRun untouched — as
							// this path did — is not the same as "not sent": it makes the
							// monitor due again on the very next tick, so the one fault
							// guaranteed to hit every credentialed monitor at once, a broker
							// outage, turned each tick into a full authoritative-read +
							// decrypt + seal storm across the whole due set. That is the
							// failure mode the floor exists to prevent, reached through the
							// door it did not cover. The counter is separate from the
							// credential one so a transport fault and a secret-resolution
							// fault back off independently and neither masks the other.
							if s.secretResolution != nil {
								s.secretResolution.RecordDispatchTransportFailure("publish_failed")
							}
							publishFailures[m.ID]++
							nextRun[m.ID] = now.Add(credentialFailureRetry(iv, publishFailures[m.ID]))
							continue
						}
						delete(credentialFailures, m.ID)
						delete(credentialLastLog, m.ID)
						delete(publishFailures, m.ID)
						nextRun[m.ID] = now.Add(iv)
					}
				}
			}
			if publishFailed > 0 && now.Sub(lastPublishWarn) >= 10*time.Second {
				lastPublishWarn = now
				s.logger.Warn("jobs_publish_failed", "count", publishFailed, "error", publishErr)
			}
		}
	}
}

// credentialFailureRetry applies the §4.4.5 floor and bounded exponential backoff.
// The normal monitor interval is the minimum. refreshEvery is the materializer's
// authoritative resync cadence; intervals already slower than that remain their own cap.
func credentialFailureRetry(interval time.Duration, failures int) time.Duration {
	if interval <= 0 {
		interval = time.Second
	}
	capDelay := refreshEvery
	if interval > capDelay {
		capDelay = interval
	}
	delay := interval
	for i := 1; i < failures && delay < capDelay; i++ {
		if delay > capDelay/2 {
			return capDelay
		}
		delay *= 2
	}
	if delay > capDelay {
		return capDelay
	}
	return delay
}

func (s *Scheduler) publishScheduledJob(ctx context.Context, job dispatch.CheckJob, interval time.Duration) error {
	region := job.Monitor.Region
	if s.pullRegions[region] {
		payload, err := json.Marshal(job)
		if err != nil {
			return fmt.Errorf("marshal pull job: %w", err)
		}
		// The row's carrier generation IS the job's, so a claim predicate can never hand a
		// payload to an executor that did not declare the capability to open it.
		switch {
		case job.ProtocolVersion >= dispatch.ProtocolV3:
			return s.store.EnqueuePullJobV3(ctx, region, payload, int(interval/time.Second), pullLeaseFor(job.Monitor))
		case job.ProtocolVersion == dispatch.ProtocolV2:
			return s.store.EnqueuePullJobV2(ctx, region, payload, int(interval/time.Second), pullLeaseFor(job.Monitor))
		default:
			return s.store.EnqueuePullJob(ctx, region, payload, int(interval/time.Second), pullLeaseFor(job.Monitor))
		}
	}
	return s.dispatcher.PublishJob(ctx, job)
}

// enterConfirm puts a monitor on the accelerated confirm rhythm: pulls its next
// probe closer (never pushes it out) and stamps the acceleration expiry. The
// per-region cap keeps a mass outage from multiplying probe load; beyond it the
// monitor keeps confirming at its normal interval.
func (s *Scheduler) enterConfirm(confirmFast map[string]time.Time, byID map[string]domain.Monitor, m domain.Monitor, now time.Time, nextRun map[string]time.Time) {
	if _, ok := confirmFast[m.ID]; !ok {
		region := 0
		for id := range confirmFast {
			if other, ok := byID[id]; ok && other.Region == m.Region {
				region++
			}
		}
		if region >= confirmCapPerRegion {
			s.logger.Warn("confirm_phase_capped", "region", m.Region, "monitor_id", m.ID, "cap", confirmCapPerRegion)
			return
		}
	}
	confirmFast[m.ID] = now.Add(m.Interval()) // refreshed on every failure signal
	fast := now.Add(m.ConfirmInterval())
	if due, ok := nextRun[m.ID]; !ok || due.After(fast) {
		nextRun[m.ID] = fast
	}
}

// pruneNextRun drops schedule entries for monitors no longer in the snapshot.
func pruneNextRun(nextRun map[string]time.Time, monitors []domain.Monitor) {
	seen := make(map[string]struct{}, len(monitors))
	for _, m := range monitors {
		seen[m.ID] = struct{}{}
	}
	for id := range nextRun {
		if _, ok := seen[id]; !ok {
			delete(nextRun, id)
		}
	}
}

// checkStalePush marks tripped push (dead-man's-switch) monitors down. A batched query
// returns the stale monitors; the leader applies each DOWN directly via RecordDeadmanResult
// (atomic staleness re-check — no synthetic heartbeat through the dispatcher, closing the
// race where a real ping lands after the staleness snapshot), throttled by nextRun so the
// dead-man re-samples at roughly one interval while the outage persists. The transition, if
// any, runs the shared post-commit reconciler.
func (s *Scheduler) checkStalePush(ctx context.Context, now time.Time, nextRun map[string]time.Time) {
	stale, err := s.store.StalePushMonitors(ctx)
	if err != nil {
		s.logger.Error("stale_push_monitors_failed", "error", err.Error())
		return
	}
	for _, m := range stale {
		if due, ok := nextRun[m.ID]; ok && now.Before(due) {
			continue
		}
		// cutoff mirrors the StalePushMonitors selection: stale iff the last real result is
		// older than one interval+grace. Re-checked atomically inside RecordDeadmanResult.
		cutoff := now.Add(-(m.Interval() + time.Duration(m.GraceSeconds)*time.Second))
		o, err := s.store.RecordDeadmanResult(ctx, m.ID, m.ExecutionRevision, cutoff)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logger.Error("deadman_result_failed", "monitor_id", m.ID, "error", err.Error())
			continue
		}
		if o.Applied && o.Prev != o.Cur && s.reconciler != nil {
			hb := domain.Heartbeat{MonitorID: m.ID, Up: false, Ts: now, Msg: "push timeout: no heartbeat within interval"}
			s.reconciler.Reconcile(ctx, hb, o.Prev, o.Cur, o.Suppressed)
		}
		nextRun[m.ID] = now.Add(m.Interval())
	}
}

// purgeChangeGroups is FR-025 D9's daily pass: change GROUPS whose latest phase is older than
// the retention bound are removed whole, `changeGroupsPerBatch` group keys per statement,
// repeating until a batch selects fewer than the bound (or the pass's context ends). Best-effort:
// an error is logged and the pass resumes at the next cadence.
func (s *Scheduler) purgeChangeGroups(ctx context.Context, now time.Time) {
	cutoff := now.UTC().Add(-time.Duration(s.changeRetentionDays) * 24 * time.Hour)
	totalGroups, totalRows, batches := 0, 0, 0
	for {
		groups, rows, err := s.store.PurgeChangeGroups(ctx, cutoff, s.changeGroupsPerBatch)
		if err != nil {
			s.logger.Error("purge_change_groups_failed", "error", err.Error(), "cutoff", cutoff.Format(time.RFC3339), "batches", batches)
			break
		}
		batches++
		totalGroups += groups
		totalRows += rows
		if groups < s.changeGroupsPerBatch {
			break
		}
	}
	if totalGroups > 0 {
		s.logger.Info("change_groups_purged", "groups", totalGroups, "rows", totalRows, "batches", batches, "cutoff", cutoff.Format(time.RFC3339))
	}
	// D15: `cerbix_changes_retained` is what the pass left behind, sampled once per pass. A
	// failed sample leaves the previous value standing rather than exporting a zero that would
	// read as "nothing retained".
	if s.changeMetrics != nil && ctx.Err() == nil {
		n, err := s.store.CountServiceChanges(ctx)
		if err != nil {
			s.logger.Warn("count_service_changes_failed", "error", err.Error())
			return
		}
		s.changeMetrics.SetChangesRetained(n)
	}
}

// maintainPartitions keeps upcoming daily heartbeat partitions pre-created and
// drops those older than the retention window. Best-effort: failures are logged,
// never fatal to scheduling. The default partition keeps inserts safe if this
// falls behind.
func (s *Scheduler) maintainPartitions(ctx context.Context, now time.Time) {
	if err := s.store.EnsureHeartbeatPartitions(ctx, partitionAheadDays); err != nil {
		s.logger.Warn("ensure_heartbeat_partitions_failed", "error", err.Error())
	}
	cutoff := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -s.retentionDays)
	if dropped, err := s.store.PurgeOldHeartbeats(ctx, cutoff); err != nil {
		s.logger.Error("purge_old_heartbeats_failed", "error", err.Error())
	} else if dropped > 0 {
		s.logger.Info("heartbeat_partitions_dropped", "count", dropped, "cutoff", cutoff.Format("2006-01-02"))
	}
	if purged, err := s.store.PurgeExpiredPullJobs(ctx); err != nil {
		s.logger.Warn("purge_pull_jobs_failed", "error", err.Error())
	} else if purged > 0 {
		s.logger.Info("pull_jobs_purged", "count", purged)
	}
	if _, err := s.store.PurgeExpiredPullTests(ctx); err != nil {
		s.logger.Warn("purge_pull_tests_failed", "error", err.Error())
	}
	// Delivered outbox rows accumulate forever otherwise; reclaim old ones (dead-lettered
	// rows are kept for operator inspection/replay).
	if purged, err := s.store.PurgeDeliveredOutbox(ctx, deliveredOutboxRetention); err != nil {
		s.logger.Warn("purge_delivered_outbox_failed", "error", err.Error())
	} else if purged > 0 {
		s.logger.Info("delivered_outbox_purged", "count", purged)
	}
	// Drop heartbeat rows from long-dead agents (each restart leaves one under a new
	// agent_id); far beyond the liveness window, so live agents are untouched.
	if purged, err := s.store.PurgeStaleAgentHeartbeats(ctx, staleAgentHeartbeatAge); err != nil {
		s.logger.Warn("purge_agent_heartbeats_failed", "error", err.Error())
	} else if purged > 0 {
		s.logger.Info("agent_heartbeats_purged", "count", purged)
	}
	// Auth housekeeping: expired session rows and abandoned OIDC login flows
	// otherwise accumulate forever.
	if purged, err := s.store.DeleteExpiredSessions(ctx); err != nil {
		s.logger.Warn("purge_sessions_failed", "error", err.Error())
	} else if purged > 0 {
		s.logger.Info("expired_sessions_purged", "count", purged)
	}
	if purged, err := s.store.DeleteExpiredAuthFlows(ctx); err != nil {
		s.logger.Warn("purge_auth_flows_failed", "error", err.Error())
	} else if purged > 0 {
		s.logger.Info("expired_auth_flows_purged", "count", purged)
	}
}

// withTimeout runs a periodic leader task under a bounded child context so one
// slow/hung store call can't stall the whole leader tick. The child is always
// cancelled when fn returns.
func withTimeout(parent context.Context, d time.Duration, fn func(context.Context)) {
	ctx, cancel := context.WithTimeout(parent, d)
	defer cancel()
	fn(ctx)
}

// sleep waits for d or ctx cancellation; returns false if ctx was cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
