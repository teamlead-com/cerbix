package fileprovider

import (
	"bytes"
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

// TestReadDirBoundedTruncates covers the enumeration bound: reading stops after the cap and
// reports truncated, so a pathologically large provider directory is never fully materialized.
func TestReadDirBoundedTruncates(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 20; i++ {
		writeFile(t, dir, "f"+string(rune('a'+i))+".yaml", "x")
	}
	// Cap below the entry count → truncated, and exactly cap entries returned.
	entries, truncated, err := readDirBoundedN(dir, 5)
	if err != nil {
		t.Fatalf("readDirBoundedN: %v", err)
	}
	if !truncated || len(entries) != 5 {
		t.Fatalf("want truncated with 5 entries, got truncated=%v len=%d", truncated, len(entries))
	}
	// Cap above the entry count → not truncated, all entries.
	entries, truncated, err = readDirBoundedN(dir, 1000)
	if err != nil || truncated || len(entries) != 20 {
		t.Fatalf("want all 20 entries untruncated, got truncated=%v len=%d err=%v", truncated, len(entries), err)
	}
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

// TestGroupBundlesDuplicateFreezesNotSuspend covers the §9.1 fix: a duplicate-target project
// is FROZEN (per-project, kept out of orphaning) and does NOT set the provider-wide
// SuspendOrphan — while an independently valid project still applies.
func TestGroupBundlesDuplicateFreezesNotSuspend(t *testing.T) {
	scope := config.ProviderScopeConfig{Type: config.ProviderScopeInstance}
	res := GroupBundles([]Candidate{
		{RelPath: "one.yaml", Data: []byte(bundleYAML("acme", "payments", "api"))},
		{RelPath: "two.yaml", Data: []byte(bundleYAML("acme", "payments", "web"))}, // dup target
		{RelPath: "ok.yaml", Data: []byte(bundleYAML("acme", "billing", "api"))},
	}, scope)
	if _, ok := res.Valid["acme/payments"]; ok {
		t.Fatal("duplicate project must be out of Valid")
	}
	if !res.Frozen["acme/payments"] {
		t.Fatal("duplicate project must be marked Frozen (kept out of orphaning)")
	}
	if res.SuspendOrphan {
		t.Fatal("a bindable duplicate must NOT suspend orphaning provider-wide")
	}
	if _, ok := res.Valid["acme/billing"]; !ok {
		t.Fatal("the independently valid project must still apply")
	}
}

// TestScanTotalBytesBound covers the cumulative byte limit and the bounded reader: a second
// file that pushes total past max_total_bytes is rejected (never fully loaded).
func TestScanTotalBytesBound(t *testing.T) {
	dir := t.TempDir()
	body := bundleYAML("acme", "payments", "api")
	writeFile(t, dir, "a.yaml", body)
	writeFile(t, dir, "b.yaml", bundleYAML("acme", "billing", "api"))
	lim := defaultLimits()
	lim.MaxTotalBytes = int64(len(body)) + 5 // only the first file fits
	cands, errs, err := ScanDirectory(dir, lim)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("only the first file should fit the total budget, got %d", len(cands))
	}
	var totalErr bool
	for _, e := range errs {
		if e.Err.Msg == "provider total bytes exceeds max_total_bytes" {
			totalErr = true
		}
	}
	if !totalErr {
		t.Fatalf("expected a total-bytes rejection, got %+v", errs)
	}
}

// TestScanRejectsOversizeFileByStat covers the cheap early reject: a file whose STAT size
// already exceeds max_file_bytes is rejected before any read. (This is the stat path, not the
// bounded reader — the bounded reader itself is proven deterministically by TestReadBounded.)
func TestScanRejectsOversizeFileByStat(t *testing.T) {
	dir := t.TempDir()
	lim := defaultLimits()
	lim.MaxFileBytes = 64
	writeFile(t, dir, "big.yaml", string(make([]byte, 4096))) // 4 KiB >> 64 B
	cands, errs, err := ScanDirectory(dir, lim)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(cands) != 0 || len(errs) != 1 || errs[0].Err.Msg != "file exceeds max_file_bytes" {
		t.Fatalf("oversized file must be rejected: cands=%d errs=%+v", len(cands), errs)
	}
}

// TestReadBounded proves the memory-safe bound directly (the "stat allowed → source grew →
// read stops at cap+1" case that the stat early-reject can never exercise): the reader never
// yields more than cap+1 bytes and flags overflow, regardless of how large the source is.
func TestReadBounded(t *testing.T) {
	// Source far larger than the cap → overflow, and EXACTLY cap+1 bytes materialized (not 4096).
	data, over, err := readBounded(bytes.NewReader(make([]byte, 4096)), 64)
	if err != nil || !over {
		t.Fatalf("oversized source must overflow: over=%v err=%v", over, err)
	}
	if int64(len(data)) != 65 {
		t.Fatalf("bounded read materialized %d bytes, want cap+1=65 (never the full 4096)", len(data))
	}
	// Source within the cap → no overflow, exact length.
	data, over, err = readBounded(bytes.NewReader(make([]byte, 32)), 64)
	if err != nil || over || len(data) != 32 {
		t.Fatalf("within-cap read: len=%d over=%v err=%v", len(data), over, err)
	}
	// cap 0 (no remaining total budget) → any non-empty source overflows.
	if _, over, _ = readBounded(bytes.NewReader([]byte("x")), 0); !over {
		t.Fatal("cap 0 must overflow on any byte")
	}
}
