package fileprovider

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/teamlead-com/cerbix/internal/config"
)

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func bundleYAML(org, project, uid string) string {
	return "format: 1\norganization: " + org + "\nproject: " + project +
		"\nmonitors:\n  " + uid + ":\n    name: M\n    type: http\n    target: https://x\n"
}

func defaultLimits() config.ProviderLimits {
	return config.ProviderLimits{MaxFiles: 1000, MaxFileBytes: 1 << 20, MaxTotalBytes: 16 << 20, MaxMonitorsPerBundle: 1000, MaxManagedMonitors: 5000}
}

func TestScanDirectoryEligibility(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.yaml", bundleYAML("acme", "payments", "api"))
	writeFile(t, dir, "b.yml", bundleYAML("acme", "billing", "api"))
	writeFile(t, dir, "notes.txt", "ignored")
	writeFile(t, dir, ".hidden.yaml", "ignored")
	if err := os.Mkdir(filepath.Join(dir, "sub.yaml"), 0o755); err != nil { // a directory named *.yaml
		t.Fatal(err)
	}

	cands, errs, err := ScanDirectory(dir, defaultLimits())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("unexpected scan errors: %+v", errs)
	}
	if len(cands) != 2 {
		names := []string{}
		for _, c := range cands {
			names = append(names, c.RelPath)
		}
		t.Fatalf("eligible files = %v, want a.yaml + b.yml only", names)
	}
}

func TestScanDirectorySizeBounds(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "big.yaml", bundleYAML("acme", "payments", "api"))
	lim := defaultLimits()
	lim.MaxFileBytes = 10 // smaller than any real bundle
	_, errs, err := ScanDirectory(dir, lim)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(errs) != 1 || errs[0].Err.Reason != ReasonInvalidFormat {
		t.Fatalf("over-size file must be rejected, got %+v", errs)
	}
}

func TestScanSymlinkEscapeRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, outside, "secret.yaml", bundleYAML("acme", "payments", "api"))
	// A symlink INSIDE the provider root pointing OUTSIDE it must be rejected.
	if err := os.Symlink(filepath.Join(outside, "secret.yaml"), filepath.Join(root, "link.yaml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cands, errs, err := ScanDirectory(root, defaultLimits())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("escaping symlink must not be a candidate, got %d", len(cands))
	}
	if len(errs) != 1 || errs[0].Err.Msg == "" {
		t.Fatalf("escaping symlink must produce a bounded error, got %+v", errs)
	}
}

func TestGroupBundlesValidAndDuplicate(t *testing.T) {
	scope := config.ProviderScopeConfig{Type: config.ProviderScopeInstance}

	// Two distinct projects → both valid.
	res := GroupBundles([]Candidate{
		{RelPath: "a.yaml", Data: []byte(bundleYAML("acme", "payments", "api"))},
		{RelPath: "b.yaml", Data: []byte(bundleYAML("acme", "billing", "api"))},
	}, scope)
	if len(res.Valid) != 2 || res.SuspendOrphan {
		t.Fatalf("two projects should both be valid: %+v suspend=%v", keysOf(res.Valid), res.SuspendOrphan)
	}
	if res.Paths["acme/payments"] != "a.yaml" {
		t.Fatalf("provenance path = %q", res.Paths["acme/payments"])
	}

	// Two files targeting the SAME project → both rejected, project frozen (not in Valid).
	dup := GroupBundles([]Candidate{
		{RelPath: "one.yaml", Data: []byte(bundleYAML("acme", "payments", "api"))},
		{RelPath: "two.yaml", Data: []byte(bundleYAML("acme", "payments", "web"))},
	}, scope)
	if _, ok := dup.Valid["acme/payments"]; ok {
		t.Fatal("duplicate project must be frozen (removed from Valid)")
	}
	var dupErr bool
	for _, e := range dup.Errors {
		if e.Err.Reason == ReasonDuplicateProject {
			dupErr = true
		}
	}
	if !dupErr {
		t.Fatalf("expected a duplicate_project error, got %+v", dup.Errors)
	}
}

func TestGroupBundlesUnboundSuspendsOrphan(t *testing.T) {
	scope := config.ProviderScopeConfig{Type: config.ProviderScopeInstance}
	// A syntactically broken file cannot be bound to a tenant → unbound → suspend orphaning,
	// while an independently valid bundle still groups.
	res := GroupBundles([]Candidate{
		{RelPath: "good.yaml", Data: []byte(bundleYAML("acme", "payments", "api"))},
		{RelPath: "broken.yaml", Data: []byte("format: 1\n  bad: [unclosed\n")},
	}, scope)
	if !res.SuspendOrphan {
		t.Fatal("an unbound invalid file must suspend orphaning")
	}
	if _, ok := res.Valid["acme/payments"]; !ok {
		t.Fatal("the independently valid bundle must still group (non-destructive apply)")
	}
	if len(res.Errors) == 0 {
		t.Fatal("the broken file must produce a bounded error")
	}
}

func keysOf(m map[string]*DesiredProject) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
