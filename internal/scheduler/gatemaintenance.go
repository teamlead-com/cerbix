package scheduler

import (
	"context"
	"time"

	"github.com/teamlead-com/cerbix/internal/store"
)

// Decision-ledger partition maintenance, the leader's side (func-reliability-gate.md D10,
// invariant 18; iter-0163 changeset 3).
//
// The loop lives here; the AUTHORITY does not. Scheduler leadership is an advisory lock on one
// pinned connection while the leader's work uses the pool, so a node whose lock connection has
// died can still issue statements for up to a watchdog interval — and "cancelled and joined
// before lead returns" does not close that window. Every pass therefore takes the gate's OWN
// store.LeaderSession on GateMaintenanceLockKey and runs every statement on that connection:
// the store owns acquire → work → release-as-a-proof as one 30 s lifecycle; this loop owns the
// cadence, the gauges and the step-down clear. It runs in its own goroutine, off the dispatch
// tick, exactly like the fact-maintenance loop, so a DETACH or ATTACH waiting behind a lock
// never holds dispatch.

// GateMaintenanceSink is the metrics surface of the gate maintenance loop: the bounded error
// family the store counts through, and the four D10 ledger gauges the loop publishes from each
// pass's report and forgets on step-down. Implemented by *metrics.Registry.
type GateMaintenanceSink interface {
	RecordGateMaintenanceError(kind string) error
	SetGateLedgerGauges(pendingDrop int, oldestAgeSeconds, writableHorizonSeconds float64, bytes int64)
	ClearGateLedgerGauges()
}

// WithGateMaintenance enables the decision-ledger maintenance loop with the five gate.decision_*
// values (already range-validated by config.Load) and the metrics sink. Without it the loop is
// not started — a scheduler built for a deployment with no gate does nothing here.
func (s *Scheduler) WithGateMaintenance(cfg store.GateMaintenanceConfig, sink GateMaintenanceSink) *Scheduler {
	s.gateCfg = cfg
	s.gateMetrics = sink
	return s
}

// gateMaintenanceEnabled reports whether lead() starts the loop: a cadence is the one value the
// loop cannot run without, and config.Load never produces a zero one.
func (s *Scheduler) gateMaintenanceEnabled() bool {
	return s.gateCfg.PurgeEvery > 0
}

// gateLedgerMaintenanceLoop runs one pass at start and then every decision_purge_every, for this
// leadership's lifetime. On cancellation (step-down or shutdown) the ledger gauges are cleared —
// a deposed pass exporting the previous holder's horizon would make two scrapes disagree about
// one ledger — and, because lead() joins this goroutine before it returns, the clear precedes
// the step-down's leader=false.
func (s *Scheduler) gateLedgerMaintenanceLoop(ctx context.Context) {
	defer func() {
		if s.gateMetrics != nil {
			s.gateMetrics.ClearGateLedgerGauges()
		}
	}()
	run := func() {
		passStart := s.clock()
		var metrics store.GateMaintenanceMetrics
		if s.gateMetrics != nil {
			metrics = s.gateMetrics
		}
		// The store bounds the whole lifecycle from passStart (work ≤ 27 s, release proof
		// ≤ 3 s, min(now + 3 s, passStart + 30 s) absolute); the loop's context is only the
		// step-down signal, never a timer of its own on top of that timeline.
		report, acquired, err := s.store.RunGateLedgerMaintenancePass(ctx, passStart, s.gateCfg, s.clock, metrics)
		switch {
		case err != nil && ctx.Err() != nil:
			s.logger.Info("gate_ledger_maintenance_interrupted", "error", err.Error())
		case err != nil:
			s.logger.Warn("gate_ledger_maintenance_failed", "acquired", acquired, "error", err.Error())
		case !acquired:
			s.logger.Debug("gate_ledger_maintenance_skipped_not_acquired")
		}
		if !acquired {
			return
		}
		for _, r := range report.Refusals {
			s.logger.Error("gate_ledger_partition_identity_refused", "detail", r)
		}
		for _, f := range report.Failures {
			s.logger.Warn("gate_ledger_maintenance_statement_failed", "detail", f)
		}
		s.logger.Info("gate_ledger_maintenance_pass",
			"created", report.Created, "attached", report.Attached, "detached", report.Detached,
			"finalized", report.Finalized, "dropped", report.Dropped,
			"creation_skipped", report.CreationSkipped, "removal_skipped", report.RemovalSkipped,
			"elapsed_ms", s.clock().Sub(passStart).Milliseconds())
		// Gauges are set only from a pass that ran to its gauge query and only while this
		// leadership is still live: a sample completing mid-cancellation must not resurrect a
		// deposed leader's numbers after the clear.
		if report.GaugesValid && ctx.Err() == nil && s.gateMetrics != nil {
			g := report.Gauges
			s.gateMetrics.SetGateLedgerGauges(g.PendingDrop, g.OldestAgeSeconds, g.WritableHorizonSeconds, g.Bytes)
		}
	}
	run()
	ticker := time.NewTicker(s.gateCfg.PurgeEvery)
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
