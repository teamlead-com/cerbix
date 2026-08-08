// Package runtime hosts the Monitoring-as-Code file provider's live component: the
// per-provider leader election, the directory watch/reconcile loop, and the wiring that
// calls the pure fileprovider contract layer and the store's atomic apply. It is started
// only by cerbix serve --role api / --role all (spec §12); there is no controller role.
package runtime

import (
	"context"
	"hash/fnv"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/fileprovider"
	"github.com/teamlead-com/cerbix/internal/store"
)

// Applier is the store surface the reconcile loop needs (implemented by *store.Store). The
// mutating apply is NOT here — it lives on LeaderSession so it can be fenced by the leader's
// connection (spec §17). The read-only lookups may use the pool: a stale read only feeds a
// destructive decision whose apply is itself fenced.
type Applier interface {
	TryBecomeLeaderSession(ctx context.Context, key int64) (LeaderSession, bool, error)
	FileProviderProjects(ctx context.Context, providerID string) ([]store.TenantRef, error)
	FileProviderCounts(ctx context.Context, providerID string) (managed, orphaned int, err error)
	RecordBundleAttempt(ctx context.Context, providerID, orgSlug, projSlug, sourcePath, status, lastErr string) error
}

// LeaderSession is a held leadership whose apply transaction runs on the lock-owning
// connection, so a lost lock aborts the in-flight apply (fencing). Implemented by
// *store.LeaderSession.
type LeaderSession interface {
	Check(ctx context.Context) (bool, error)
	Release()
	ApplyFileManagedBundle(ctx context.Context, providerID string, desired *fileprovider.DesiredProject, sourcePath string, orphanGrace time.Duration, maxManaged int, allowAbsence bool) (store.ApplyResult, error)
}

// MetricsSink receives the provider's bounded observability signals (spec §16). Nil-safe:
// a Provider without a sink simply emits none. Implemented by *metrics.Registry.
type MetricsSink interface {
	SetFileProviderLeader(name string, leader bool)
	RecordFileProviderReconcile(name, outcome string)
	SetFileProviderStatus(name string, durationSeconds float64, lastSuccessUnix int64, managed, orphaned, bundleErrors int)
}

// fileProviderLeaderBaseKey namespaces per-provider advisory locks away from the scheduler
// (…0001) and migrate (…0002) keys. The low 32 bits are an FNV hash of the provider name, so
// each provider elects independently and a name change (a restart-only event) re-keys.
const fileProviderLeaderBaseKey int64 = 0x6365726269781000

// leaderKeyFor derives a stable, provider-specific advisory-lock key.
func leaderKeyFor(name string) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return fileProviderLeaderBaseKey | int64(h.Sum32())
}

// Provider is one configured file provider's live reconciler.
type Provider struct {
	name        string
	dir         string
	scope       config.ProviderScopeConfig
	limits      config.ProviderLimits
	debounce    time.Duration
	resync      time.Duration
	orphanGrace time.Duration
	leaderKey   int64
	applier     Applier
	logger      *slog.Logger
	metrics     MetricsSink
	status      *StatusRegistry
	sem         chan struct{} // shared across providers: global reconcile-concurrency bound (§17)

	leaderCheckEvery time.Duration
	pollEvery        time.Duration
	lastLog          map[string]time.Time // per-reason log throttle (single reconcile goroutine)
}

// errorLogEvery rate-limits repeated parse/apply/watcher error logs (spec §16).
const errorLogEvery = 30 * time.Second

// warnThrottled logs a warning at most once per errorLogEvery per (msg,key) so a persistently
// broken bundle doesn't flood the log. Runs on the single reconcile goroutine (no lock).
func (p *Provider) warnThrottled(key, msg string, kv ...any) {
	if p.lastLog == nil {
		p.lastLog = map[string]time.Time{}
	}
	id := msg + "|" + key
	now := time.Now()
	if last, ok := p.lastLog[id]; ok && now.Sub(last) < errorLogEvery {
		return
	}
	p.lastLog[id] = now
	p.logger.Warn(msg, kv...)
}

// persistAttempt records a bindable rejection on the bundle's diagnostics row (best-effort).
func (p *Provider) persistAttempt(ctx context.Context, org, proj, path, status, reason string) {
	if err := p.applier.RecordBundleAttempt(ctx, p.name, org, proj, path, status, reason); err != nil {
		p.warnThrottled(org+"/"+proj, "file_provider_attempt_persist_failed", "error", err.Error())
	}
}

// WithMetrics attaches an observability sink (nil-safe).
func (p *Provider) WithMetrics(m MetricsSink) *Provider {
	p.metrics = m
	return p
}

// WithReconcileLimiter attaches a semaphore shared by ALL providers in the process, bounding
// how many providers may reconcile at once (spec §17). Nil = unbounded.
func (p *Provider) WithReconcileLimiter(sem chan struct{}) *Provider {
	p.sem = sem
	return p
}

// WithStatus attaches the process-local diagnostics registry and seeds this provider's entry
// (so a configured-but-idle provider is already visible). Nil-safe.
func (p *Provider) WithStatus(reg *StatusRegistry) *Provider {
	p.status = reg
	if reg != nil {
		reg.register(p.name, p.scope.Type, p.scope.Organization, p.scope.Project)
	}
	return p
}

// New builds a Provider from its static config (already validated/defaulted by config.Load).
func New(name string, cfg config.FileProviderConfig, applier Applier, logger *slog.Logger) *Provider {
	return &Provider{
		name:             name,
		dir:              cfg.Directory,
		scope:            cfg.Scope,
		limits:           cfg.Limits,
		debounce:         cfg.Debounce.Std(),
		resync:           cfg.ResyncOrDefault(),
		orphanGrace:      cfg.OrphanGraceOrDefault(),
		leaderKey:        leaderKeyFor(name),
		applier:          applier,
		logger:           logger.With("provider", name),
		leaderCheckEvery: 5 * time.Second,
		pollEvery:        time.Second,
	}
}

// ReconcileReport is a bounded per-scan summary for logs/metrics (no per-file identifiers).
type ReconcileReport struct {
	Applied   int // valid bundles applied (any outcome incl. no-op)
	Rejected  int // per-file rejections (decode/scope/duplicate/size/apply)
	Orphaned  int // whole-project disappearances processed
	Degraded  bool
	LastError string // bounded reason of the last rejection this scan (for diagnostics)
}

// Run elects leadership for this provider and, while leader, runs the watch/reconcile loop.
// Loss of the advisory lock stops apply before re-contention (spec §12); a follower simply
// retries. Blocks until ctx is cancelled.
func (p *Provider) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		session, ok, err := p.applier.TryBecomeLeaderSession(ctx, p.leaderKey)
		if err != nil {
			p.logger.Error("file_provider_leader_error", "error", err.Error())
			if !sleepCtx(ctx, backoff) {
				return
			}
			continue
		}
		if !ok {
			// Another replica leads this provider; stay a ready follower and retry.
			if !sleepCtx(ctx, p.leaderCheckEvery) {
				return
			}
			continue
		}
		p.logger.Info("file_provider_leader_acquired")
		p.setLeader(true)
		p.leaderLoop(ctx, session)
		p.setLeader(false)
		session.Release()
		p.logger.Info("file_provider_leader_released")
	}
}

// leaderLoop runs while this process holds leadership: an initial reconcile, then reconciles
// on a debounced directory change, on the mandatory periodic resync (a lost/coalesced event
// must not stall progress), and steps down if the advisory lock is lost.
func (p *Provider) leaderLoop(ctx context.Context, session LeaderSession) {
	p.reconcile(ctx, session)
	lastFP := p.fingerprint()

	resyncT := time.NewTicker(p.resync)
	defer resyncT.Stop()
	pollT := time.NewTicker(p.pollEvery)
	defer pollT.Stop()
	checkT := time.NewTicker(p.leaderCheckEvery)
	defer checkT.Stop()

	var (
		dirty         bool
		debounceUntil time.Time
	)
	for {
		select {
		case <-ctx.Done():
			return
		case <-checkT.C:
			if held, err := session.Check(ctx); err != nil || !held {
				p.logger.Warn("file_provider_leadership_lost")
				return
			}
		case <-resyncT.C:
			p.reconcile(ctx, session) // mandatory resync (§11): the lost-notification recovery path
			lastFP = p.fingerprint()
			dirty = false
		case <-pollT.C:
			// Poll-based watch: fingerprint the directory (name+size+mtime, no content read);
			// a change arms the debounce, and the coalesced rescan fires once it settles. This
			// is a single dirty bit, not an unbounded event queue (§17 backpressure).
			if fp := p.fingerprint(); fp != lastFP {
				lastFP = fp
				dirty = true
				debounceUntil = time.Now().Add(p.debounce)
			}
			if dirty && !time.Now().Before(debounceUntil) {
				dirty = false
				p.reconcile(ctx, session)
				lastFP = p.fingerprint()
			}
		}
	}
}

// reconcile performs one full bounded scan → group → apply, honoring last-known-good and the
// unbound orphan-suspension rule. Best-effort: per-project failures are logged and isolated;
// a directory read error degrades without touching the committed runtime.
func (p *Provider) reconcile(ctx context.Context, session LeaderSession) ReconcileReport {
	// Global concurrency gate (§17): block until a slot is free, but never past ctx.
	if p.sem != nil {
		select {
		case p.sem <- struct{}{}:
			defer func() { <-p.sem }()
		case <-ctx.Done():
			return ReconcileReport{}
		}
	}
	start := time.Now()
	rep := p.reconcileInner(ctx, session)
	p.publishMetrics(ctx, rep, time.Since(start).Seconds())
	return rep
}

func (p *Provider) reconcileInner(ctx context.Context, session LeaderSession) ReconcileReport {
	var rep ReconcileReport
	cands, scanErrs, err := fileprovider.ScanDirectory(p.dir, p.limits)
	if err != nil {
		// Directory unreadable/replaced: keep last-known-good, mark degraded, never orphan.
		rep.Degraded = true
		rep.LastError = "scan_failed"
		p.logger.Warn("file_provider_scan_failed", "error", err.Error())
		return rep
	}
	for _, se := range scanErrs {
		rep.Rejected++
		rep.LastError = string(se.Err.Reason)
		p.warnThrottled(se.RelPath, "file_provider_file_rejected", "path", se.RelPath, "reason", string(se.Err.Reason))
	}
	grp := fileprovider.GroupBundles(cands, p.scope)
	for _, se := range grp.Errors {
		rep.Rejected++
		rep.LastError = string(se.Err.Reason)
		p.warnThrottled(se.RelPath, "file_provider_bundle_rejected", "path", se.RelPath, "reason", string(se.Err.Reason))
	}

	// Trust of absence is decided PROVIDER-WIDE, BEFORE any apply (§9.1). An unbindable file —
	// a decode/scope failure (grp.SuspendOrphan) or a scan-level rejection (over-size, symlink
	// escape, unreadable) — means the snapshot cannot be read as declaring anything absent.
	// While suspended, NO apply may orphan/disable a UID (not even one absent from a project
	// that IS present in the snapshot): the per-bundle apply runs with allowAbsence=false, so a
	// present-project bundle that dropped a UID keeps that UID's last-known-good. A duplicate
	// (grp.Frozen) is bindable-but-rejected → frozen per-project (kept out of orphaning) without
	// suspending the whole provider.
	suspendOrphan := grp.SuspendOrphan || len(scanErrs) > 0
	allowAbsence := !suspendOrphan

	// Apply every valid bundle; per-project fault isolation (a rejection keeps that project's
	// last-known-good, others still apply).
	presentKeys := make(map[string]bool, len(grp.Valid))
	for _, key := range sortedKeys(grp.Valid) {
		dp := grp.Valid[key]
		// Bounded: a bundle over max_monitors_per_bundle is rejected whole (never partially
		// applied) and FROZEN (present for orphan purposes → keeps its last-known-good).
		if len(dp.Monitors) > p.limits.MaxMonitorsPerBundle {
			presentKeys[key] = true
			rep.Rejected++
			rep.LastError = "max_monitors_per_bundle"
			p.warnThrottled(key, "file_provider_bundle_rejected", "org", dp.Organization, "project", dp.Project, "reason", "max_monitors_per_bundle")
			p.persistAttempt(ctx, dp.Organization, dp.Project, grp.Paths[key], "rejected", "max_monitors_per_bundle")
			continue
		}
		presentKeys[key] = true
		if _, aerr := session.ApplyFileManagedBundle(ctx, p.name, dp, grp.Paths[key], p.orphanGrace, p.limits.MaxManagedMonitors, allowAbsence); aerr != nil {
			rep.Rejected++
			reason, status := classify(aerr), "error"
			rep.LastError = reason
			var be *fileprovider.BundleError
			if errorsAs(aerr, &be) {
				status = "rejected"
			}
			p.warnThrottled(key, "file_provider_apply_failed", "org", dp.Organization, "project", dp.Project, "reason", reason)
			p.persistAttempt(ctx, dp.Organization, dp.Project, grp.Paths[key], status, reason)
			continue
		}
		rep.Applied++
	}

	// Whole-project disappearance: an owned project absent from the valid snapshot is orphaned
	// by applying an empty desired — but ONLY when absence is trusted this scan (computed above).
	if allowAbsence {
		owned, oerr := p.applier.FileProviderProjects(ctx, p.name)
		if oerr != nil {
			// A failed owned-set lookup is NOT a clean reconcile: degrade so last_success does
			// not advance (the stale-success alert must stay reliable, §16).
			rep.Degraded = true
			rep.LastError = "owned_lookup_failed"
			p.warnThrottled("owned", "file_provider_owned_lookup_failed", "error", oerr.Error())
			return rep
		}
		for _, t := range owned {
			key := t.Organization + "/" + t.Project
			if presentKeys[key] || grp.Frozen[key] {
				continue
			}
			empty := &fileprovider.DesiredProject{Organization: t.Organization, Project: t.Project, Monitors: map[string]fileprovider.DesiredMonitor{}}
			if _, aerr := session.ApplyFileManagedBundle(ctx, p.name, empty, "", p.orphanGrace, p.limits.MaxManagedMonitors, true); aerr != nil {
				// An orphan apply that failed is a real reconcile failure, not a clean scan.
				rep.Rejected++
				rep.Degraded = true
				rep.LastError = classify(aerr)
				p.warnThrottled(key, "file_provider_orphan_failed", "org", t.Organization, "project", t.Project, "reason", classify(aerr))
				continue
			}
			rep.Orphaned++
		}
	}
	return rep
}

func (p *Provider) setLeader(leader bool) {
	if p.metrics != nil {
		p.metrics.SetFileProviderLeader(p.name, leader)
	}
	if p.status != nil {
		p.status.setLeader(p.name, leader)
	}
}

// publishMetrics records the bounded reconcile outcome/duration/status to BOTH the metrics
// sink and the diagnostics registry (either may be nil).
func (p *Provider) publishMetrics(ctx context.Context, rep ReconcileReport, dur float64) {
	if p.metrics == nil && p.status == nil {
		return
	}
	outcome := "noop"
	switch {
	case rep.Degraded:
		outcome = "error"
	case rep.Applied > 0 || rep.Orphaned > 0:
		outcome = "applied"
	case rep.Rejected > 0:
		outcome = "rejected"
	}
	// last_success advances ONLY on a clean scan — a scan with any rejection (invalid/quota/
	// duplicate) is NOT a success, so a stale-success alert still fires (spec §16).
	var success int64
	if !rep.Degraded && rep.Rejected == 0 {
		success = time.Now().Unix()
	}
	// Read the post-apply owned counts. When they're unknown — a degraded scan, or a failed
	// lookup — do NOT publish misleading zero gauges and do NOT advance last_success on an
	// unverifiable scan (§16): leave the gauges at their last value (Prometheus keeps them) and
	// preserve the diagnostics counts.
	managed, orphaned, countsOK := 0, 0, false
	if !rep.Degraded {
		if m, o, err := p.applier.FileProviderCounts(ctx, p.name); err == nil {
			managed, orphaned, countsOK = m, o, true
		} else {
			success = 0
			if rep.LastError == "" {
				rep.LastError = "counts_unavailable"
			}
			p.warnThrottled("counts", "file_provider_counts_failed", "error", err.Error())
		}
	}
	if p.metrics != nil {
		p.metrics.RecordFileProviderReconcile(p.name, outcome)
		if countsOK {
			p.metrics.SetFileProviderStatus(p.name, dur, success, managed, orphaned, rep.Rejected)
		}
	}
	if p.status != nil {
		if countsOK {
			p.status.update(p.name, time.Now().Unix(), success, rep.LastError, managed, orphaned, rep.Rejected)
		} else {
			p.status.updateNoCounts(p.name, time.Now().Unix(), success, rep.LastError, rep.Rejected)
		}
	}
}

// fingerprint is a cheap directory signature (eligible file names + sizes + mtimes) used to
// detect changes between polls without reading file contents. A directory read error yields
// a sentinel so a vanished/replaced directory is observed as a change (then handled by
// reconcile, which keeps LKG rather than orphaning).
func (p *Provider) fingerprint() string {
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return "unreadable"
	}
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if ext := strings.ToLower(filepath.Ext(name)); ext != ".yaml" && ext != ".yml" {
			continue
		}
		info, ierr := os.Stat(filepath.Join(p.dir, name))
		if ierr != nil {
			parts = append(parts, name+":?")
			continue
		}
		parts = append(parts, name+":"+strconv.FormatInt(info.Size(), 10)+":"+strconv.FormatInt(info.ModTime().UnixNano(), 10))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

func sortedKeys(m map[string]*fileprovider.DesiredProject) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// classify maps an apply error to a bounded reason for logs (never raw YAML/secrets).
func classify(err error) string {
	var be *fileprovider.BundleError
	if errorsAs(err, &be) {
		return string(be.Reason)
	}
	if err == store.ErrBundleTenantNotFound {
		return "tenant_not_found"
	}
	return "apply_error"
}

// errorsAs is a tiny errors.As shim kept local to avoid an import solely for one call.
func errorsAs(err error, target **fileprovider.BundleError) bool {
	for err != nil {
		if be, ok := err.(*fileprovider.BundleError); ok {
			*target = be
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// storeApplier adapts *store.Store to the Applier interface. It is needed because
// *store.Store.TryBecomeLeaderSession returns the concrete *store.LeaderSession, which Go
// will not treat as satisfying an interface method that returns the LeaderSession interface
// (no return-type covariance); the adapter performs the widening.
type storeApplier struct{ st *store.Store }

// NewStoreApplier wraps a *store.Store as the reconcile-loop Applier.
func NewStoreApplier(st *store.Store) Applier { return storeApplier{st: st} }

func (a storeApplier) TryBecomeLeaderSession(ctx context.Context, key int64) (LeaderSession, bool, error) {
	ls, ok, err := a.st.TryBecomeLeaderSession(ctx, key)
	if err != nil || !ok {
		return nil, ok, err
	}
	return ls, true, nil
}
func (a storeApplier) FileProviderProjects(ctx context.Context, providerID string) ([]store.TenantRef, error) {
	return a.st.FileProviderProjects(ctx, providerID)
}
func (a storeApplier) FileProviderCounts(ctx context.Context, providerID string) (int, int, error) {
	return a.st.FileProviderCounts(ctx, providerID)
}
func (a storeApplier) RecordBundleAttempt(ctx context.Context, providerID, orgSlug, projSlug, sourcePath, status, lastErr string) error {
	return a.st.RecordBundleAttempt(ctx, providerID, orgSlug, projSlug, sourcePath, status, lastErr)
}
