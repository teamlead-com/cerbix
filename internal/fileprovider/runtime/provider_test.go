package runtime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/fileprovider"
	"github.com/teamlead-com/cerbix/internal/store"
)

type applyCall struct {
	org, project string
	monitors     int
	sourcePath   string
	allowAbsence bool
}

type fakeApplier struct {
	owned      []store.TenantRef
	session    *fakeSession
	ownedErr   error // injected: FileProviderProjects failure
	countsErr  error // injected: FileProviderCounts failure
	countsMgd  int
	countsOrph int
}

func (f *fakeApplier) sess() *fakeSession {
	if f.session == nil {
		f.session = &fakeSession{}
	}
	return f.session
}
func (f *fakeApplier) TryBecomeLeaderSession(context.Context, int64) (LeaderSession, bool, error) {
	return f.sess(), true, nil
}
func (f *fakeApplier) FileProviderProjects(_ context.Context, _ string) ([]store.TenantRef, error) {
	if f.ownedErr != nil {
		return nil, f.ownedErr
	}
	return f.owned, nil
}
func (f *fakeApplier) FileProviderCounts(_ context.Context, _ string) (int, int, error) {
	if f.countsErr != nil {
		return 0, 0, f.countsErr
	}
	return f.countsMgd, f.countsOrph, nil
}

// fakeSession is a fenced-apply stand-in that records the mutating calls + attempts.
type fakeSession struct {
	calls          []applyCall
	attempts       int
	attemptsByPath int
	noChange       bool // when true, every apply reports NoChange (steady-state no-op)
}

func (s *fakeSession) Check(context.Context) (bool, error) { return true, nil }
func (s *fakeSession) Release()                            {}
func (s *fakeSession) ApplyFileManagedBundle(_ context.Context, _ string, dp *fileprovider.DesiredProject, path string, _ time.Duration, _ int, allowAbsence bool) (store.ApplyResult, error) {
	s.calls = append(s.calls, applyCall{dp.Organization, dp.Project, len(dp.Monitors), path, allowAbsence})
	return store.ApplyResult{Organization: dp.Organization, Project: dp.Project, NoChange: s.noChange}, nil
}
func (s *fakeSession) RecordBundleAttempt(_ context.Context, _, _, _, _, _, _ string) error {
	s.attempts++
	return nil
}
func (s *fakeSession) RecordBundleAttemptByPath(_ context.Context, _, _, _, _ string) error {
	s.attemptsByPath++
	return nil
}

func testProvider(dir string, applier Applier) *Provider {
	resync := config.Duration(30 * time.Second)
	grace := config.Duration(0)
	cfg := config.FileProviderConfig{
		Directory:         dir,
		Debounce:          config.Duration(100 * time.Millisecond),
		ResyncInterval:    &resync,
		OrphanGracePeriod: &grace,
		Scope:             config.ProviderScopeConfig{Type: config.ProviderScopeInstance},
		Limits:            config.ProviderLimits{MaxFiles: 1000, MaxFileBytes: 1 << 20, MaxTotalBytes: 16 << 20, MaxMonitorsPerBundle: 1000, MaxManagedMonitors: 5000},
	}
	return New("platform", cfg, applier, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func bundle(org, project string) string {
	return "format: 1\norganization: " + org + "\nproject: " + project +
		"\nmonitors:\n  api:\n    name: M\n    type: http\n    target: https://x\n"
}

func TestReconcileAppliesValidBundles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", bundle("acme", "payments"))
	write(t, dir, "b.yaml", bundle("acme", "billing"))
	fa := &fakeApplier{}
	rep := testProvider(dir, fa).reconcile(context.Background(), fa.sess())
	if rep.Applied != 2 || rep.Rejected != 0 || rep.Orphaned != 0 {
		t.Fatalf("report = %+v, want 2 applied", rep)
	}
	for _, c := range fa.sess().calls {
		if c.monitors == 0 {
			t.Fatalf("valid apply must carry monitors: %+v", c)
		}
	}
}

func TestReconcileOrphansDisappearedProject(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", bundle("acme", "payments"))
	// The provider owns payments (present) and billing (file gone) → billing is orphaned.
	fa := &fakeApplier{owned: []store.TenantRef{{Organization: "acme", Project: "payments"}, {Organization: "acme", Project: "billing"}}}
	rep := testProvider(dir, fa).reconcile(context.Background(), fa.sess())
	if rep.Orphaned != 1 {
		t.Fatalf("report = %+v, want 1 orphaned", rep)
	}
	var orphanEmpty bool
	for _, c := range fa.sess().calls {
		if c.project == "billing" && c.monitors == 0 {
			orphanEmpty = true
		}
	}
	if !orphanEmpty {
		t.Fatalf("disappeared project must be orphaned with an empty desired: %+v", fa.sess().calls)
	}
}

func TestReconcileSuspendsOrphanOnUnbound(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", bundle("acme", "payments"))
	write(t, dir, "broken.yaml", "format: 1\n  bad: [unclosed\n") // unbound → suspend
	fa := &fakeApplier{owned: []store.TenantRef{{Organization: "acme", Project: "payments"}, {Organization: "acme", Project: "billing"}}}
	rep := testProvider(dir, fa).reconcile(context.Background(), fa.sess())
	if rep.Rejected == 0 {
		t.Fatal("broken file must be rejected")
	}
	if rep.Orphaned != 0 {
		t.Fatalf("unbound file must suspend orphaning, got %d orphaned", rep.Orphaned)
	}
	for _, c := range fa.sess().calls {
		if c.monitors == 0 {
			t.Fatalf("no empty-orphan apply must happen while suspended: %+v", c)
		}
	}
}

func TestReconcileDegradedOnUnreadableDir(t *testing.T) {
	fa := &fakeApplier{}
	rep := testProvider(filepath.Join(t.TempDir(), "does-not-exist"), fa).reconcile(context.Background(), fa.sess())
	if !rep.Degraded || len(fa.sess().calls) != 0 {
		t.Fatalf("unreadable dir must degrade with no apply: %+v calls=%d", rep, len(fa.sess().calls))
	}
}

// TestLeaderKeyDerivation covers §12: each provider elects on its own advisory key, distinct
// per name, in the file-provider namespace and away from the scheduler (…0001) and migrate
// (…0002) keys.
func TestLeaderKeyDerivation(t *testing.T) {
	a := leaderKeyFor("platform")
	b := leaderKeyFor("acme")
	if a == b {
		t.Fatal("distinct provider names must derive distinct leader keys")
	}
	const schedulerKey = 0x6365726269780001
	const migrateKey = 0x6365726269780002
	for _, k := range []int64{a, b} {
		if k == schedulerKey || k == migrateKey {
			t.Fatalf("provider leader key %#x collides with a reserved key", k)
		}
		if k>>56 != 0x46 {
			t.Fatalf("provider leader key %#x is not in the file-provider namespace (top byte != 0x46)", k)
		}
	}
	// Deterministic across calls.
	if leaderKeyFor("platform") != a {
		t.Fatal("leader key must be stable for a given name")
	}
}

// TestLeaderKeyNoFNV32Collision guards the #6 regression: two names whose FNV-1a-32 hashes
// collide (0x7d48fcb7) must NOT map to the same advisory key under the FNV-1a-64 derivation.
func TestLeaderKeyNoFNV32Collision(t *testing.T) {
	const n1, n2 = "p18s8xllr6dp", "p1lxyx1cpu7tz"
	if leaderKeyFor(n1) == leaderKeyFor(n2) {
		t.Fatalf("names %q and %q (equal FNV-32) must derive distinct leader keys", n1, n2)
	}
	if err := AssertDistinctLeaderKeys([]string{n1, n2, "platform", "acme"}); err != nil {
		t.Fatalf("distinct names should pass the collision guard: %v", err)
	}
}

// TestFingerprintDetectsChanges covers the watcher's change-detection (§11/§18.3): the
// directory fingerprint changes on create, content write, and rename, and is stable when
// nothing changed — so the poll loop coalesces to exactly the rescans it should.
func TestFingerprintDetectsChanges(t *testing.T) {
	dir := t.TempDir()
	p := testProvider(dir, &fakeApplier{})

	empty := p.fingerprint()
	write(t, dir, "a.yaml", bundle("acme", "payments"))
	created := p.fingerprint()
	if created == empty {
		t.Fatal("fingerprint must change when a file is created")
	}
	if p.fingerprint() != created {
		t.Fatal("fingerprint must be stable when nothing changed")
	}
	// Content write (different bytes) changes it.
	write(t, dir, "a.yaml", bundle("acme", "billing"))
	written := p.fingerprint()
	if written == created {
		t.Fatal("fingerprint must change on a content write")
	}
	// Atomic-rename style replacement (new name) changes it.
	if err := os.Rename(filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml")); err != nil {
		t.Fatal(err)
	}
	if p.fingerprint() == written {
		t.Fatal("fingerprint must change on a rename")
	}
	// A vanished directory yields the sentinel (observed as a change → reconcile keeps LKG).
	if fp := testProvider(filepath.Join(dir, "gone"), &fakeApplier{}).fingerprint(); fp != "unreadable" {
		t.Fatalf("missing dir fingerprint = %q, want sentinel", fp)
	}
}

// TestReconcileDuplicateDoesNotOrphan is the §9.1 regression: a duplicate-target project the
// provider already owns must be FROZEN (kept alive), never orphaned by the disappearance path.
func TestReconcileDuplicateDoesNotOrphan(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "one.yaml", bundle("acme", "payments")) // both target acme/payments → duplicate
	write(t, dir, "two.yaml", bundle("acme", "payments"))
	fa := &fakeApplier{owned: []store.TenantRef{{Organization: "acme", Project: "payments"}}}
	rep := testProvider(dir, fa).reconcile(context.Background(), fa.sess())
	if rep.Orphaned != 0 {
		t.Fatalf("a frozen duplicate project must NOT be orphaned, got %d", rep.Orphaned)
	}
	for _, c := range fa.sess().calls {
		if c.monitors == 0 {
			t.Fatalf("no empty-orphan apply must touch a frozen project: %+v", c)
		}
	}
}

// TestReconcileScanErrorSuspendsOrphan is the §9.1 regression: a scan-level rejected file
// (unbindable to a tenant) suspends orphaning provider-wide, so an owned project whose file is
// (temporarily) absent is NOT orphaned this scan.
func TestReconcileScanErrorSuspendsOrphan(t *testing.T) {
	dir := t.TempDir()
	// A symlink escaping the root is a scan-level rejection (unbindable).
	outside := t.TempDir()
	write(t, outside, "x.yaml", bundle("acme", "payments"))
	if err := os.Symlink(filepath.Join(outside, "x.yaml"), filepath.Join(dir, "link.yaml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	fa := &fakeApplier{owned: []store.TenantRef{{Organization: "acme", Project: "billing"}}}
	rep := testProvider(dir, fa).reconcile(context.Background(), fa.sess())
	if rep.Rejected == 0 {
		t.Fatal("scan-level rejection expected")
	}
	if rep.Orphaned != 0 {
		t.Fatalf("a scan-level (unbindable) rejection must suspend orphaning, got %d orphaned", rep.Orphaned)
	}
}

// TestReconcileRejectsOversizedBundle covers max_monitors_per_bundle enforcement: a bundle
// over the limit is rejected whole (no apply) and frozen (not orphaned).
func TestReconcileRejectsOversizedBundle(t *testing.T) {
	dir := t.TempDir()
	// Two monitors in one bundle; limit set to 1.
	write(t, dir, "p.yaml", "format: 1\norganization: acme\nproject: payments\nmonitors:\n"+
		"  a: {name: A, type: http, target: https://a}\n  b: {name: B, type: http, target: https://b}\n")
	fa := &fakeApplier{owned: []store.TenantRef{{Organization: "acme", Project: "payments"}}}
	p := testProvider(dir, fa)
	p.limits.MaxMonitorsPerBundle = 1
	rep := p.reconcile(context.Background(), fa.sess())
	if rep.Applied != 0 {
		t.Fatalf("oversized bundle must not apply, got Applied=%d", rep.Applied)
	}
	if rep.Rejected == 0 {
		t.Fatal("oversized bundle must be rejected")
	}
	if rep.Orphaned != 0 || len(fa.sess().calls) != 0 {
		t.Fatalf("a rejected (frozen) bundle must NOT be applied or orphaned: orphaned=%d calls=%d", rep.Orphaned, len(fa.sess().calls))
	}
}

// TestReconcileAbsenceGuardThreadedToApply is the P0 ordering guard (spec §9.1): suspendOrphan
// is computed BEFORE any apply, so when the scan is ambiguous the PER-BUNDLE apply of a present
// project runs with allowAbsence=false — a UID dropped from that project keeps its LKG. Without
// a scan error the same apply runs with allowAbsence=true.
func TestReconcileAbsenceGuardThreadedToApply(t *testing.T) {
	// Clean scan → allowAbsence=true on the present-project apply.
	dirOK := t.TempDir()
	write(t, dirOK, "a.yaml", bundle("acme", "payments"))
	faOK := &fakeApplier{}
	if rep := testProvider(dirOK, faOK).reconcile(context.Background(), faOK.sess()); rep.Rejected != 0 {
		t.Fatalf("clean scan unexpectedly rejected: %+v", rep)
	}
	if len(faOK.sess().calls) != 1 || !faOK.sess().calls[0].allowAbsence {
		t.Fatalf("clean scan must apply with allowAbsence=true, got %+v", faOK.sess().calls)
	}

	// Ambiguous scan (an unreadable/oversized sibling → scanErr) → allowAbsence=false, even
	// though the payments project itself is present and valid.
	dirBad := t.TempDir()
	write(t, dirBad, "a.yaml", bundle("acme", "payments"))
	// An oversized file trips a scan-level rejection (bounded reader), not a decode error.
	big := make([]byte, 2<<20)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(filepath.Join(dirBad, "big.yaml"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	faBad := &fakeApplier{}
	p := testProvider(dirBad, faBad)
	p.limits.MaxFileBytes = 1 << 20 // 1 MiB → big.yaml is rejected at scan
	rep := p.reconcile(context.Background(), faBad.sess())
	if rep.Rejected == 0 {
		t.Fatalf("oversized sibling must produce a scan rejection: %+v", rep)
	}
	var appliedPayments bool
	for _, c := range faBad.sess().calls {
		if c.project == "payments" {
			appliedPayments = true
			if c.allowAbsence {
				t.Fatalf("ambiguous scan must apply the present project with allowAbsence=false, got %+v", c)
			}
		}
	}
	if !appliedPayments {
		t.Fatalf("present valid project must still apply (non-destructively) under an ambiguous scan: %+v", faBad.sess().calls)
	}
}

// TestReconcileOwnedLookupErrorDegrades covers §16: a failed owned-set lookup is NOT a clean
// reconcile — it degrades so last_success does not advance (stale-success alert stays reliable).
func TestReconcileOwnedLookupErrorDegrades(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", bundle("acme", "payments"))
	fa := &fakeApplier{ownedErr: errors.New("db down")}
	fm := &fakeMetrics{}
	rep := testProvider(dir, fa).WithMetrics(fm).reconcile(context.Background(), fa.sess())
	if !rep.Degraded || rep.LastError != "owned_lookup_failed" {
		t.Fatalf("owned-lookup error must degrade with a reason, got %+v", rep)
	}
	if fm.lastSuccess != 0 {
		t.Fatalf("a degraded reconcile must NOT advance last_success, got %d", fm.lastSuccess)
	}
}

// TestReconcileCountsErrorNoFalseSuccess covers §16: if the post-apply owned-count lookup
// fails, the reconcile must not publish a false success or misleading zero gauges.
func TestReconcileCountsErrorNoFalseSuccess(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", bundle("acme", "payments"))
	fa := &fakeApplier{countsErr: errors.New("count failed")}
	fm := &fakeMetrics{}
	rep := testProvider(dir, fa).WithMetrics(fm).reconcile(context.Background(), fa.sess())
	if rep.Degraded {
		t.Fatalf("a clean apply with only a counts-lookup failure should not be marked degraded: %+v", rep)
	}
	if fm.lastSuccess != 0 {
		t.Fatalf("an unverifiable scan (counts lookup failed) must NOT advance last_success, got %d", fm.lastSuccess)
	}
	if fm.lastOutcome != "error" {
		t.Fatalf("a counts-lookup failure must record outcome=error, got %q", fm.lastOutcome)
	}
	// The duration/error gauges are still updated (via the counts-preserving path), not skipped.
	if fm.statsCalls != 1 || fm.statusCalls != 0 {
		t.Fatalf("counts-unknown must update gauges via SetFileProviderReconcileStats, got status=%d stats=%d", fm.statusCalls, fm.statsCalls)
	}
}

// TestReconcileNoopNotCountedApplied covers §16/§17: a bundle apply that reports NoChange must
// not increment rep.Applied, so reconcile_total{outcome="noop"} is reachable in steady state.
func TestReconcileNoopNotCountedApplied(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", bundle("acme", "payments"))
	fa := &fakeApplier{session: &fakeSession{noChange: true}}
	rep := testProvider(dir, fa).reconcile(context.Background(), fa.sess())
	if rep.Applied != 0 || rep.Rejected != 0 {
		t.Fatalf("a no-op apply must not count as applied/rejected: %+v", rep)
	}
	if len(fa.sess().calls) != 1 {
		t.Fatalf("the bundle must still be applied (as a no-op), got %d calls", len(fa.sess().calls))
	}
}

// TestReconcilePersistsDuplicateRejection covers §15/P1#7: a duplicate-target (frozen) project
// is tenant-bound, so its rejection is persisted to diagnostics through the fenced session —
// not left only in the process log.
func TestReconcilePersistsDuplicateRejection(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", bundle("acme", "payments"))
	write(t, dir, "b.yaml", bundle("acme", "payments")) // same tenant → duplicate → frozen
	fa := &fakeApplier{}
	rep := testProvider(dir, fa).reconcile(context.Background(), fa.sess())
	if rep.Rejected == 0 {
		t.Fatalf("duplicate project must be rejected: %+v", rep)
	}
	if fa.sess().attempts == 0 {
		t.Fatal("a duplicate (tenant-bound) rejection must be persisted via the fenced session")
	}
}

// TestReconcilePersistsInvalidByPath covers P1#5 (§9.1): a decode error is persisted to the
// known bundle row through the fenced by-path path, not left only in the process log.
func TestReconcilePersistsInvalidByPath(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", bundle("acme", "payments"))
	write(t, dir, "broken.yaml", "format: 1\n  bad: [unclosed\n") // decode error → grp.Errors
	fa := &fakeApplier{}
	rep := testProvider(dir, fa).reconcile(context.Background(), fa.sess())
	if rep.Rejected == 0 {
		t.Fatalf("broken file must be rejected: %+v", rep)
	}
	if fa.sess().attemptsByPath == 0 {
		t.Fatal("a decode error must be persisted via the fenced by-path attempt")
	}
}

// TestReconcileConcurrencyGate covers the §17 process-wide bound: with a full semaphore,
// reconcile blocks on the gate and honors ctx cancellation (no apply while it can't get a slot).
func TestReconcileConcurrencyGate(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", bundle("acme", "payments"))
	fa := &fakeApplier{}
	sem := make(chan struct{}, 1)
	sem <- struct{}{} // occupy the only slot
	p := testProvider(dir, fa).WithReconcileLimiter(sem)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	rep := p.reconcile(ctx, fa.sess())
	if rep.Applied != 0 || len(fa.sess().calls) != 0 {
		t.Fatalf("reconcile must not run while the concurrency gate is full: %+v calls=%d", rep, len(fa.sess().calls))
	}
	// Free the slot → reconcile proceeds.
	<-sem
	if rep := p.reconcile(context.Background(), fa.sess()); rep.Applied != 1 {
		t.Fatalf("reconcile must run once a slot is free, got Applied=%d", rep.Applied)
	}
}

// fakeMetrics captures the last status push + outcome for assertions.
type fakeMetrics struct {
	lastSuccess int64
	lastOutcome string
	statusCalls int // full SetFileProviderStatus calls (counts known)
	statsCalls  int // SetFileProviderReconcileStats calls (counts unknown)
}

func (m *fakeMetrics) SetFileProviderLeader(string, bool) {}
func (m *fakeMetrics) RecordFileProviderReconcile(_ string, outcome string) {
	m.lastOutcome = outcome
}
func (m *fakeMetrics) SetFileProviderStatus(_ string, _ float64, lastSuccessUnix int64, _, _, _ int) {
	m.lastSuccess = lastSuccessUnix
	m.statusCalls++
}
func (m *fakeMetrics) SetFileProviderReconcileStats(_ string, _ float64, lastSuccessUnix int64, _ int) {
	m.lastSuccess = lastSuccessUnix
	m.statsCalls++
}

// TestLastSuccessOnlyOnCleanScan covers §16: last_success advances on a clean scan but NOT on
// a scan with any rejection (so a stale-success alert still fires while bundles are broken).
func TestLastSuccessOnlyOnCleanScan(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", bundle("acme", "payments"))
	fa := &fakeApplier{}
	fm := &fakeMetrics{}
	p := testProvider(dir, fa).WithMetrics(fm)
	if rep := p.reconcile(context.Background(), fa.sess()); rep.Rejected != 0 {
		t.Fatalf("clean scan unexpectedly rejected: %+v", rep)
	}
	if fm.lastSuccess == 0 {
		t.Fatal("clean scan must advance last_success")
	}
	// Add a broken file → the scan now has a rejection → last_success must NOT advance.
	fm.lastSuccess = 0
	write(t, dir, "broken.yaml", "format: 1\n  bad: [unclosed\n")
	if rep := p.reconcile(context.Background(), fa.sess()); rep.Rejected == 0 {
		t.Fatalf("expected a rejection, got %+v", rep)
	}
	if fm.lastSuccess != 0 {
		t.Fatalf("a scan with rejections must NOT advance last_success, got %d", fm.lastSuccess)
	}
}

// TestLeaderLoopEventStormBounded proves the §17 backpressure invariant: the watch is a SINGLE
// dirty bit + debounce, not an unbounded event queue. Under a sustained storm of directory
// changes (hundreds of rewrites, each changing the file's size so every poll observes a change),
// the number of reconciles stays tiny — bounded by debounce settling, NOT proportional to the
// number of changes. A naive per-event queue would apply once per change.
func TestLeaderLoopEventStormBounded(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", bundle("acme", "payments"))
	fa := &fakeApplier{}
	p := testProvider(dir, fa)
	// Tight watch cadence; resync/leader-check far out so only the poll+debounce path is exercised.
	p.debounce = 40 * time.Millisecond
	p.pollEvery = 3 * time.Millisecond
	p.resync = 30 * time.Second
	p.leaderCheckEvery = 30 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	var done sync.WaitGroup
	done.Add(1)
	go func() { defer done.Done(); p.leaderLoop(ctx, fa.sess()) }()

	// Storm: rewrite the same file ~continuously for 300ms, each write a different size so the
	// fingerprint changes on every poll and the debounce keeps re-arming (never settles).
	var writes atomic.Int64
	stormEnd := make(chan struct{})
	var storm sync.WaitGroup
	storm.Add(1)
	go func() {
		defer storm.Done()
		i := 0
		for {
			select {
			case <-stormEnd:
				return
			default:
			}
			i++
			body := bundle("acme", "payments") + "\n# " + strings.Repeat("x", i%400+1)
			if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(body), 0o600); err == nil {
				writes.Add(1)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	time.Sleep(300 * time.Millisecond)
	close(stormEnd)
	storm.Wait()
	// Let the debounce settle and fire at most one coalesced reconcile.
	time.Sleep(150 * time.Millisecond)
	cancel()
	done.Wait()

	n := int64(writes.Load())
	if n < 100 {
		t.Fatalf("storm too weak to be meaningful: only %d writes", n)
	}
	applies := int64(len(fa.sess().calls))
	// 1 initial reconcile + ~1 post-settle. Anything near the write count means unbounded queuing.
	if applies > 10 {
		t.Fatalf("event storm was not coalesced: %d applies for %d writes (want a small constant)", applies, n)
	}
	if applies < 1 {
		t.Fatalf("expected at least the initial reconcile, got %d", applies)
	}
	t.Logf("coalesced %d directory changes into %d reconciles", n, applies)
}
