package runtime

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
}

type fakeApplier struct {
	owned   []store.TenantRef
	session *fakeSession
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
	return f.owned, nil
}
func (f *fakeApplier) FileProviderCounts(_ context.Context, _ string) (int, int, error) {
	return 0, 0, nil
}

// fakeSession is a fenced-apply stand-in that records the mutating calls.
type fakeSession struct{ calls []applyCall }

func (s *fakeSession) Check(context.Context) (bool, error) { return true, nil }
func (s *fakeSession) Release()                            {}
func (s *fakeSession) ApplyFileManagedBundle(_ context.Context, _ string, dp *fileprovider.DesiredProject, path string, _ time.Duration, _ int) (store.ApplyResult, error) {
	s.calls = append(s.calls, applyCall{dp.Organization, dp.Project, len(dp.Monitors), path})
	return store.ApplyResult{Organization: dp.Organization, Project: dp.Project}, nil
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
// per name and namespaced away from the scheduler (…0001) and migrate (…0002) keys.
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
		if k&fileProviderLeaderBaseKey != fileProviderLeaderBaseKey {
			t.Fatalf("provider leader key %#x is not in the file-provider namespace", k)
		}
	}
	// Deterministic across calls.
	if leaderKeyFor("platform") != a {
		t.Fatal("leader key must be stable for a given name")
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
