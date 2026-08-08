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
	owned []store.TenantRef
	calls []applyCall
}

func (f *fakeApplier) ApplyFileManagedBundle(_ context.Context, _ string, dp *fileprovider.DesiredProject, path string, _ time.Duration) (store.ApplyResult, error) {
	f.calls = append(f.calls, applyCall{dp.Organization, dp.Project, len(dp.Monitors), path})
	return store.ApplyResult{Organization: dp.Organization, Project: dp.Project}, nil
}
func (f *fakeApplier) FileProviderProjects(_ context.Context, _ string) ([]store.TenantRef, error) {
	return f.owned, nil
}
func (f *fakeApplier) FileProviderCounts(_ context.Context, _ string) (int, int, error) {
	return 0, 0, nil
}
func (f *fakeApplier) TryBecomeLeader(context.Context, int64) (func(), func(context.Context) (bool, error), bool, error) {
	return func() {}, func(context.Context) (bool, error) { return true, nil }, true, nil
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
	rep := testProvider(dir, fa).reconcile(context.Background())
	if rep.Applied != 2 || rep.Rejected != 0 || rep.Orphaned != 0 {
		t.Fatalf("report = %+v, want 2 applied", rep)
	}
	for _, c := range fa.calls {
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
	rep := testProvider(dir, fa).reconcile(context.Background())
	if rep.Orphaned != 1 {
		t.Fatalf("report = %+v, want 1 orphaned", rep)
	}
	var orphanEmpty bool
	for _, c := range fa.calls {
		if c.project == "billing" && c.monitors == 0 {
			orphanEmpty = true
		}
	}
	if !orphanEmpty {
		t.Fatalf("disappeared project must be orphaned with an empty desired: %+v", fa.calls)
	}
}

func TestReconcileSuspendsOrphanOnUnbound(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", bundle("acme", "payments"))
	write(t, dir, "broken.yaml", "format: 1\n  bad: [unclosed\n") // unbound → suspend
	fa := &fakeApplier{owned: []store.TenantRef{{Organization: "acme", Project: "payments"}, {Organization: "acme", Project: "billing"}}}
	rep := testProvider(dir, fa).reconcile(context.Background())
	if rep.Rejected == 0 {
		t.Fatal("broken file must be rejected")
	}
	if rep.Orphaned != 0 {
		t.Fatalf("unbound file must suspend orphaning, got %d orphaned", rep.Orphaned)
	}
	for _, c := range fa.calls {
		if c.monitors == 0 {
			t.Fatalf("no empty-orphan apply must happen while suspended: %+v", c)
		}
	}
}

func TestReconcileDegradedOnUnreadableDir(t *testing.T) {
	fa := &fakeApplier{}
	rep := testProvider(filepath.Join(t.TempDir(), "does-not-exist"), fa).reconcile(context.Background())
	if !rep.Degraded || len(fa.calls) != 0 {
		t.Fatalf("unreadable dir must degrade with no apply: %+v calls=%d", rep, len(fa.calls))
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
