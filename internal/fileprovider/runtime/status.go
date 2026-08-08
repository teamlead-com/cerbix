package runtime

import (
	"sort"
	"sync"
)

// ProviderStatus is this PROCESS's live view of one file provider (spec §15). Leadership and
// last-scan are process-local: a follower replica honestly reports Leader=false and its own
// last scan. Counts/error mirror the last reconcile. A configured-but-idle provider still has
// an entry (registered at New), so "configured-but-empty" providers appear in diagnostics.
type ProviderStatus struct {
	Provider        string `json:"provider"`
	ScopeType       string `json:"scope_type"`
	ScopeOrg        string `json:"scope_org,omitempty"`
	ScopeProject    string `json:"scope_project,omitempty"`
	Leader          bool   `json:"leader"`
	LastScanUnix    int64  `json:"last_scan_unix,omitempty"`
	LastSuccessUnix int64  `json:"last_success_unix,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	Managed         int    `json:"managed_monitors"`
	Orphaned        int    `json:"orphaned_monitors"`
	Rejected        int    `json:"bundle_errors"`
}

// StatusRegistry is the process-local registry of file-provider status, shared by every
// Provider (writers) and read by the diagnostics API. Thread-safe.
type StatusRegistry struct {
	mu sync.RWMutex
	m  map[string]ProviderStatus
}

// NewStatusRegistry builds an empty registry.
func NewStatusRegistry() *StatusRegistry { return &StatusRegistry{m: map[string]ProviderStatus{}} }

// register seeds a provider's entry (Leader=false) so a configured-but-idle provider is
// visible before its first reconcile.
func (r *StatusRegistry) register(name, scopeType, scopeOrg, scopeProject string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.m[name]
	s.Provider, s.ScopeType, s.ScopeOrg, s.ScopeProject = name, scopeType, scopeOrg, scopeProject
	r.m[name] = s
}

// setLeader records this process's leadership for a provider.
func (r *StatusRegistry) setLeader(name string, leader bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.m[name]
	s.Provider = name
	s.Leader = leader
	r.m[name] = s
}

// update records the post-reconcile status (preserving scope/leadership fields).
func (r *StatusRegistry) update(name string, lastScan, lastSuccess int64, lastErr string, managed, orphaned, rejected int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.m[name]
	s.Provider = name
	s.LastScanUnix = lastScan
	if lastSuccess > 0 {
		s.LastSuccessUnix = lastSuccess
	}
	s.LastError = lastErr
	s.Managed, s.Orphaned, s.Rejected = managed, orphaned, rejected
	r.m[name] = s
}

// updateNoCounts records a post-reconcile status whose owned counts are UNKNOWN (a degraded
// scan or a failed counts lookup) WITHOUT clobbering the last-known managed/orphaned figures to
// zero — a silent zero would read as "no monitors" in diagnostics. Scan time, last error, and
// rejected are updated; last success is only advanced when lastSuccess > 0 (kept sticky).
func (r *StatusRegistry) updateNoCounts(name string, lastScan, lastSuccess int64, lastErr string, rejected int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.m[name]
	s.Provider = name
	s.LastScanUnix = lastScan
	if lastSuccess > 0 {
		s.LastSuccessUnix = lastSuccess
	}
	s.LastError = lastErr
	s.Rejected = rejected
	r.m[name] = s
}

// Snapshot returns all provider statuses, sorted by name (deterministic diagnostics).
func (r *StatusRegistry) Snapshot() []ProviderStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ProviderStatus, 0, len(r.m))
	for _, s := range r.m {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}
