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

// Applier is the store surface the reconcile loop needs (implemented by *store.Store).
type Applier interface {
	ApplyFileManagedBundle(ctx context.Context, providerID string, desired *fileprovider.DesiredProject, sourcePath string, orphanGrace time.Duration) (store.ApplyResult, error)
	FileProviderProjects(ctx context.Context, providerID string) ([]store.TenantRef, error)
	TryBecomeLeader(ctx context.Context, key int64) (release func(), check func(context.Context) (bool, error), ok bool, err error)
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

	leaderCheckEvery time.Duration
	pollEvery        time.Duration
}

// New builds a Provider from its static config (already validated/defaulted by config.Load).
func New(name string, cfg config.FileProviderConfig, applier Applier, logger *slog.Logger) *Provider {
	return &Provider{
		name:             name,
		dir:              cfg.Directory,
		scope:            cfg.Scope,
		limits:           cfg.Limits,
		debounce:         cfg.Debounce.Std(),
		resync:           cfg.ResyncInterval.Std(),
		orphanGrace:      cfg.OrphanGracePeriod.Std(),
		leaderKey:        leaderKeyFor(name),
		applier:          applier,
		logger:           logger.With("provider", name),
		leaderCheckEvery: 5 * time.Second,
		pollEvery:        time.Second,
	}
}

// ReconcileReport is a bounded per-scan summary for logs/metrics (no per-file identifiers).
type ReconcileReport struct {
	Applied  int // valid bundles applied (any outcome incl. no-op)
	Rejected int // per-file rejections (decode/scope/duplicate/size/apply)
	Orphaned int // whole-project disappearances processed
	Degraded bool
}

// Run elects leadership for this provider and, while leader, runs the watch/reconcile loop.
// Loss of the advisory lock stops apply before re-contention (spec §12); a follower simply
// retries. Blocks until ctx is cancelled.
func (p *Provider) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		release, check, ok, err := p.applier.TryBecomeLeader(ctx, p.leaderKey)
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
		p.leaderLoop(ctx, check)
		if release != nil {
			release()
		}
		p.logger.Info("file_provider_leader_released")
	}
}

// leaderLoop runs while this process holds leadership: an initial reconcile, then reconciles
// on a debounced directory change, on the mandatory periodic resync (a lost/coalesced event
// must not stall progress), and steps down if the advisory lock is lost.
func (p *Provider) leaderLoop(ctx context.Context, check func(context.Context) (bool, error)) {
	p.reconcile(ctx)
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
			if held, err := check(ctx); err != nil || !held {
				p.logger.Warn("file_provider_leadership_lost")
				return
			}
		case <-resyncT.C:
			p.reconcile(ctx) // mandatory resync (§11): the lost-notification recovery path
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
				p.reconcile(ctx)
				lastFP = p.fingerprint()
			}
		}
	}
}

// reconcile performs one full bounded scan → group → apply, honoring last-known-good and the
// unbound orphan-suspension rule. Best-effort: per-project failures are logged and isolated;
// a directory read error degrades without touching the committed runtime.
func (p *Provider) reconcile(ctx context.Context) ReconcileReport {
	var rep ReconcileReport
	cands, scanErrs, err := fileprovider.ScanDirectory(p.dir, p.limits)
	if err != nil {
		// Directory unreadable/replaced: keep last-known-good, mark degraded, never orphan.
		rep.Degraded = true
		p.logger.Warn("file_provider_scan_failed", "error", err.Error())
		return rep
	}
	for _, se := range scanErrs {
		rep.Rejected++
		p.logger.Warn("file_provider_file_rejected", "path", se.RelPath, "reason", string(se.Err.Reason))
	}
	grp := fileprovider.GroupBundles(cands, p.scope)
	for _, se := range grp.Errors {
		rep.Rejected++
		p.logger.Warn("file_provider_bundle_rejected", "path", se.RelPath, "reason", string(se.Err.Reason))
	}

	// Apply every valid bundle; per-project fault isolation (a rejection keeps that project's
	// last-known-good, others still apply).
	presentKeys := make(map[string]bool, len(grp.Valid))
	for _, key := range sortedKeys(grp.Valid) {
		dp := grp.Valid[key]
		presentKeys[key] = true
		if _, aerr := p.applier.ApplyFileManagedBundle(ctx, p.name, dp, grp.Paths[key], p.orphanGrace); aerr != nil {
			rep.Rejected++
			p.logger.Warn("file_provider_apply_failed", "org", dp.Organization, "project", dp.Project, "reason", classify(aerr))
			continue
		}
		rep.Applied++
	}

	// Whole-project disappearance: an owned project absent from the valid snapshot is orphaned
	// by applying an empty desired — UNLESS an unbound file suspended orphaning this scan
	// (a broken/half-written replacement must not read as intentional deletion, §9.1/§10).
	if !grp.SuspendOrphan {
		owned, oerr := p.applier.FileProviderProjects(ctx, p.name)
		if oerr != nil {
			p.logger.Warn("file_provider_owned_lookup_failed", "error", oerr.Error())
			return rep
		}
		for _, t := range owned {
			key := t.Organization + "/" + t.Project
			if presentKeys[key] {
				continue
			}
			empty := &fileprovider.DesiredProject{Organization: t.Organization, Project: t.Project, Monitors: map[string]fileprovider.DesiredMonitor{}}
			if _, aerr := p.applier.ApplyFileManagedBundle(ctx, p.name, empty, "", p.orphanGrace); aerr != nil {
				p.logger.Warn("file_provider_orphan_failed", "org", t.Organization, "project", t.Project, "reason", classify(aerr))
				continue
			}
			rep.Orphaned++
		}
	}
	return rep
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
