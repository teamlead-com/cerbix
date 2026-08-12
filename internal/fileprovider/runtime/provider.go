// Package runtime hosts the Monitoring-as-Code file provider's live component: the
// per-provider leader election, the directory watch/reconcile loop, and the wiring that
// calls the pure fileprovider contract layer and the store's atomic apply. It is started
// only by cerbix serve --role api / --role all (spec §12); there is no controller role.
package runtime

import (
	"container/list"
	"context"
	"fmt"
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
}

// LeaderSession is a held leadership whose mutating operations run on the lock-owning
// connection, so a lost lock aborts them (fencing). Implemented by *store.LeaderSession.
// RecordBundleAttempt lives here (not on Applier) so a former leader cannot overwrite a new
// leader's applied diagnostics with a stale rejection (spec §17).
type LeaderSession interface {
	Check(ctx context.Context) (bool, error)
	Release()
	ApplyFileManagedBundle(ctx context.Context, providerID string, desired *fileprovider.DesiredProject, sourcePath string, orphanGrace time.Duration, maxManaged int, allowAbsence bool) (store.ApplyResult, error)
	RecordBundleAttempt(ctx context.Context, providerID, orgSlug, projSlug, sourcePath, status, lastErr string) error
	RecordBundleAttemptByPath(ctx context.Context, providerID, sourcePath, status, lastErr string) error
}

// MetricsSink receives the provider's bounded observability signals (spec §16). Nil-safe:
// a Provider without a sink simply emits none. Implemented by *metrics.Registry.
type MetricsSink interface {
	SetFileProviderLeader(name string, leader bool)
	RecordFileProviderReconcile(name, outcome string)
	SetFileProviderStatus(name string, durationSeconds float64, lastSuccessUnix int64, managed, orphaned, bundleErrors int)
	SetFileProviderReconcileStats(name string, durationSeconds float64, lastSuccessUnix int64, bundleErrors int)
}

// fileProviderLeaderNamespace tags the high byte of every file-provider leadership advisory
// key. It is disjoint from the scheduler (0x6365726269780001) and migrate
// (0x6365726269780002) fixed keys — those sit under the 0x6365726269780000 prefix — and
// leaves the low 56 bits for a per-provider identity hash.
const fileProviderLeaderNamespace int64 = 0x46 << 56 // 'F' for file provider

// leaderKeyFor derives a stable, provider-specific advisory-lock key from the provider name
// (a file provider's only persisted identity — provider_id is the name). The old scheme was
// base | FNV-1a-32(name): a single 32-bit hash under a fixed prefix, so two provider names
// whose 32-bit hash collided produced the SAME advisory key and serialized permanently — one
// provider never led while the other was healthy (spec §12). This hashes the name with
// FNV-1a-64 into the low 56 bits under the namespace byte, cutting the accidental-collision
// probability from ~2^-32 to ~2^-56; and AssertDistinctLeaderKeys makes leadership
// collision-FREE for any real deployment by refusing to start if two CONFIGURED names still
// map to one key.
func leaderKeyFor(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return fileProviderLeaderNamespace | int64(h.Sum64()&0x00FFFFFFFFFFFFFF)
}

// AssertDistinctLeaderKeys fails if any two provider names derive the same leadership advisory
// key. Provider names are operator-trusted and bounded, and this runs once at startup, so it
// turns the (already tiny) hash-collision risk into a hard, deterministic guarantee: a colliding
// pair refuses to start with an actionable message rather than silently letting one provider
// never lead (spec §12).
func AssertDistinctLeaderKeys(names []string) error {
	seen := make(map[int64]string, len(names))
	for _, name := range names {
		k := leaderKeyFor(name)
		if other, dup := seen[k]; dup {
			return fmt.Errorf("file providers %q and %q derive the same leadership lock key %#x; rename one of them", other, name, k)
		}
		seen[k] = name
	}
	return nil
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
	// throttle rate-limits repeated warnings per (msg,key). It is a bounded LRU: without a cap
	// a churning directory (up to ReadDirBounded distinct filenames per scan) would grow the
	// map for the process lifetime (spec §16/§17). Single reconcile goroutine (no lock).
	throttle    map[string]*list.Element
	throttleLRU *list.List // front = most-recently-touched; evicted from the back
}

// errorLogEvery rate-limits repeated parse/apply/watcher error logs (spec §16).
const errorLogEvery = 30 * time.Second

// warnThrottleMax bounds the number of distinct (msg,key) throttle entries kept in memory.
// Well below ReadDirBounded (50k) so per-file/per-tenant churn cannot grow it without bound,
// yet large enough to retain the working set of a realistic broken snapshot.
const warnThrottleMax = 2048

// throttleEntry is one LRU entry: the throttle key and when it last actually logged.
type throttleEntry struct {
	id   string
	last time.Time
}

// warnThrottled logs a warning at most once per errorLogEvery per (msg,key) so a persistently
// broken bundle doesn't flood the log. Backed by a bounded LRU so a churning directory cannot
// grow the throttle map without bound: every touch (even a suppressed one) refreshes recency,
// so a persistently-hot key stays resident and stays rate-limited, while stale keys age out
// and are evicted from the back once the cap is reached. Runs on the single reconcile
// goroutine (no lock).
func (p *Provider) warnThrottled(key, msg string, kv ...any) {
	id := msg + "|" + key
	now := time.Now()
	if p.throttle == nil {
		p.throttle = map[string]*list.Element{}
		p.throttleLRU = list.New()
	}
	if el, ok := p.throttle[id]; ok {
		p.throttleLRU.MoveToFront(el) // touched → most-recently-used, so a hot key is never evicted
		ent := el.Value.(*throttleEntry)
		if now.Sub(ent.last) < errorLogEvery {
			return // still inside the window: suppress (do NOT reset the window)
		}
		ent.last = now // window elapsed: log again and restart the window
		p.logger.Warn(msg, kv...)
		return
	}
	p.throttle[id] = p.throttleLRU.PushFront(&throttleEntry{id: id, last: now})
	for p.throttleLRU.Len() > warnThrottleMax {
		back := p.throttleLRU.Back()
		p.throttleLRU.Remove(back)
		delete(p.throttle, back.Value.(*throttleEntry).id)
	}
	p.logger.Warn(msg, kv...)
}

// persistAttempt records a bindable rejection on the bundle's diagnostics row (best-effort),
// through the LEADER's fenced connection so a former leader cannot clobber a new leader's
// applied state with a stale error (spec §17).
func (p *Provider) persistAttempt(ctx context.Context, session LeaderSession, org, proj, path, status, reason string) {
	if err := session.RecordBundleAttempt(ctx, p.name, org, proj, path, status, reason); err != nil {
		p.warnThrottled(org+"/"+proj, "file_provider_attempt_persist_failed", "error", err.Error())
	}
}

// persistAttemptByPath marks a KNOWN-path bundle rejected when its file can no longer be bound to
// a tenant (a decode/scope/scan failure at a path a bundle was previously applied from). Best-
// effort and fenced; a path with no existing bundle row is a no-op (stays process-local, §9.1).
func (p *Provider) persistAttemptByPath(ctx context.Context, session LeaderSession, path, reason string) {
	if path == "" {
		return
	}
	if err := session.RecordBundleAttemptByPath(ctx, p.name, path, "rejected", reason); err != nil {
		p.warnThrottled(path, "file_provider_attempt_persist_failed", "error", err.Error())
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
		// A scan-level rejection at a previously-applied path marks that known bundle rejected.
		p.persistAttemptByPath(ctx, session, se.RelPath, string(se.Err.Reason))
	}
	grp := fileprovider.GroupBundles(cands, p.scope)
	for _, se := range grp.Errors {
		rep.Rejected++
		rep.LastError = string(se.Err.Reason)
		p.warnThrottled(se.RelPath, "file_provider_bundle_rejected", "path", se.RelPath, "reason", string(se.Err.Reason))
		// A decode/scope failure at a known path marks that known LKG bundle rejected (§9.1);
		// a duplicate is also persisted per-tenant by the Frozen loop below.
		p.persistAttemptByPath(ctx, session, se.RelPath, string(se.Err.Reason))
	}
	// Persist tenant-bound (duplicate) rejections to diagnostics so a frozen project SHOWS as
	// rejected, not silently process-local (§15). Unbound decode/scope/scan rejections have no
	// tenant to pin, so they remain log-only (documented limitation).
	for key := range grp.Frozen {
		if org, proj, ok := splitTenantKey(key); ok {
			p.persistAttempt(ctx, session, org, proj, grp.Paths[key], "rejected", string(fileprovider.ReasonDuplicateProject))
		}
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
			p.persistAttempt(ctx, session, dp.Organization, dp.Project, grp.Paths[key], "rejected", "max_monitors_per_bundle")
			continue
		}
		presentKeys[key] = true
		res, aerr := session.ApplyFileManagedBundle(ctx, p.name, dp, grp.Paths[key], p.orphanGrace, p.limits.MaxManagedMonitors, allowAbsence)
		if aerr != nil {
			rep.Rejected++
			reason, status := classify(aerr), "error"
			rep.LastError = reason
			var be *fileprovider.BundleError
			if errorsAs(aerr, &be) {
				status = "rejected"
			}
			p.warnThrottled(key, "file_provider_apply_failed", "org", dp.Organization, "project", dp.Project, "reason", reason)
			p.persistAttempt(ctx, session, dp.Organization, dp.Project, grp.Paths[key], status, reason)
			continue
		}
		// A pure no-op (unchanged bundle) is NOT an "applied" change — leaving rep.Applied at 0
		// keeps reconcile_total{outcome="noop"} reachable in steady state (§16/§17).
		if !res.NoChange {
			rep.Applied++
		}
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
	// Read the post-apply owned counts FIRST: a failed lookup means we cannot verify the scan's
	// result, so it is an ERROR outcome (not a false applied/noop) and last_success must not
	// advance — but the duration/error gauges still update, and the last-known managed/orphaned
	// counts are preserved rather than clobbered to a misleading zero (§16).
	managed, orphaned, countsOK := 0, 0, false
	if !rep.Degraded {
		if m, o, err := p.applier.FileProviderCounts(ctx, p.name); err == nil {
			managed, orphaned, countsOK = m, o, true
		} else {
			if rep.LastError == "" {
				rep.LastError = "counts_unavailable"
			}
			p.warnThrottled("counts", "file_provider_counts_failed", "error", err.Error())
		}
	}
	// Outcome reflects the WHOLE reconcile, counts lookup included.
	outcome := "noop"
	switch {
	case rep.Degraded || !countsOK:
		outcome = "error"
	case rep.Applied > 0 || rep.Orphaned > 0:
		outcome = "applied"
	case rep.Rejected > 0:
		outcome = "rejected"
	}
	// last_success advances ONLY on a clean, fully-verified scan — no rejection, not degraded,
	// counts known (spec §16), so a stale-success alert still fires while anything is wrong.
	var success int64
	if !rep.Degraded && rep.Rejected == 0 && countsOK {
		success = time.Now().Unix()
	}
	if p.metrics != nil {
		p.metrics.RecordFileProviderReconcile(p.name, outcome)
		if countsOK {
			p.metrics.SetFileProviderStatus(p.name, dur, success, managed, orphaned, rep.Rejected)
		} else {
			// Update duration/error without clobbering the managed/orphaned gauges.
			p.metrics.SetFileProviderReconcileStats(p.name, dur, success, rep.Rejected)
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
	entries, truncated, err := fileprovider.ReadDirBounded(p.dir)
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
	sig := strings.Join(parts, "|")
	if truncated {
		// Fold the truncation state in so entering/leaving the bounded regime is seen as a change.
		sig = "truncated|" + sig
	}
	return sig
}

func sortedKeys(m map[string]*fileprovider.DesiredProject) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// splitTenantKey splits a grouping key "org/project" into its slugs. ok=false if malformed.
func splitTenantKey(key string) (org, project string, ok bool) {
	i := strings.IndexByte(key, '/')
	if i <= 0 || i == len(key)-1 {
		return "", "", false
	}
	return key[:i], key[i+1:], true
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
