// Package scheduler runs the periodic check schedule. Exactly one instance is
// active at a time, elected via a Postgres advisory lock; the leader scans
// enabled monitors on a tick and publishes a CheckJob whenever one is due.
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
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
	// RunServiceRepairSlice works one durable repair range until the deadline, on the
	// lock-owning connection. It reports whether it found anything to do.
	RunServiceRepairSlice(ctx context.Context, deadline time.Time) (bool, error)
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
	PurgeOldHeartbeats(ctx context.Context, cutoff time.Time) (int, error)
	EnqueueRenotifyReminders(ctx context.Context) (int, error)
	EvaluateBurnAlerts(ctx context.Context) (fired, resolved int, err error)
	EnqueueDueSLAReports(ctx context.Context) (int, error)
	EvaluateRegionWorkerAlerts(ctx context.Context, live map[string]bool, graceSeconds int) (fired, resolved int, err error)
	AdvanceEscalations(ctx context.Context) (fired int, err error)
	EnqueuePullJob(ctx context.Context, region string, payload []byte, ttlSeconds int) error
	EnqueuePullJobV2(ctx context.Context, region string, payload []byte, ttlSeconds int) error
	EnqueuePullJobV3(ctx context.Context, region string, payload []byte, ttlSeconds int) error
	LiveCredentialReadyAgentRegions(ctx context.Context, within time.Duration, minCapability int) (map[string]bool, error)
	PurgeExpiredPullJobs(ctx context.Context) (int, error)
	PurgeExpiredPullTests(ctx context.Context) (int, error)
	PurgeStaleAgentHeartbeats(ctx context.Context, olderThan time.Duration) (int, error)
	PurgeDeliveredOutbox(ctx context.Context, olderThan time.Duration) (int, error)
	DeleteExpiredSessions(ctx context.Context) (int64, error)
	DeleteExpiredAuthFlows(ctx context.Context) (int64, error)
	PullQueueStats(ctx context.Context) ([]metrics.PullStat, error)
}

// PullStatsSink receives the leader's per-region pull-queue gauge snapshot.
// Implemented by *metrics.Registry.
type PullStatsSink interface {
	SetPullStats(stats []metrics.PullStat)
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
	// partitionAheadDays is how many future daily partitions to keep pre-created.
	partitionAheadDays = 2
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
	leaderState            LeaderStateSink
	confirmCh              <-chan string      // monitor ids entering the confirmation phase (LISTEN monitor_confirm)
	configCh               <-chan struct{}    // execution-config changes (LISTEN monitor_config_changed) → force a snapshot reload
	reconciler             *ingest.Reconciler // shared post-commit flow for dead-man transitions (SSE + incident)
	credentialEnvelopes    bool
	secretResolution       SecretResolutionSink
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
func (s *Scheduler) setLeaderState(leader bool) {
	if s.leaderState != nil {
		s.leaderState.SetSchedulerLeader(leader)
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
	var lastRollup, lastMaintain, lastRefresh, lastPushCheck, lastRenotify, lastBurn, lastReport, lastRegionChk, lastEscalation, lastPullStats, lastPublishWarn, lastLeaderChk, lastServiceSlice time.Time
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
			// Service reliability: work durable repair ranges on the LOCK-OWNING
			// connection, in slices short enough that dispatch is serviced between them.
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
					worked, err := session.RunServiceRepairSlice(ctx, started.Add(slice))
					spent += time.Since(started)
					if err != nil {
						s.logger.Error("service_repair_slice_failed", "error", err.Error())
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
					if n, err := s.store.AdvanceEscalations(c); err != nil {
						s.logger.Error("advance_escalations_failed", "error", err.Error())
					} else if n > 0 {
						s.logger.Info("escalations_advanced", "fired", n)
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
				if s.pullRegions[m.Region] {
					// Pull-served region: enqueue for HTTP claim with a TTL (~interval) so
					// a job for a region with no live agent expires rather than piling up.
					// Confirm-phase jobs carry the short TTL so stale fast probes don't stack.
					payload, err := json.Marshal(dispatch.CheckJob{Monitor: m})
					if err != nil {
						s.logger.Error("marshal_pull_job_failed", "monitor_id", m.ID, "error", err.Error())
						continue // never enqueued → don't advance the cadence
					}
					if err := s.store.EnqueuePullJob(ctx, m.Region, payload, int(iv/time.Second)); err != nil {
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
			return s.store.EnqueuePullJobV3(ctx, region, payload, int(interval/time.Second))
		case job.ProtocolVersion == dispatch.ProtocolV2:
			return s.store.EnqueuePullJobV2(ctx, region, payload, int(interval/time.Second))
		default:
			return s.store.EnqueuePullJob(ctx, region, payload, int(interval/time.Second))
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
