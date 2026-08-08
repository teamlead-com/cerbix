package fileprovider

import (
	"regexp"
	"sort"
	"strings"
)

// uidRe is the immutable provider-local monitor identity syntax (spec §6.2).
var uidRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// decodeError maps a strict yaml.v3 decode failure to a bounded reason without leaking the
// document. yaml.v3 reports unknown fields ("field X not found") and duplicate mapping keys
// ("already defined") distinctly; both are contract violations.
func decodeError(err error) *BundleError {
	msg := err.Error()
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "already defined"):
		return &BundleError{Reason: ReasonDuplicateKey, Msg: "duplicate mapping key"}
	case strings.Contains(low, "not found in type"):
		return &BundleError{Reason: ReasonUnknownField, Msg: "unknown or server-owned field"}
	default:
		return &BundleError{Reason: ReasonInvalidFormat, Msg: "invalid YAML bundle"}
	}
}

// normStringSet sorts + dedupes a set-like slice (tags, dependency UIDs). Order-insensitive
// per spec §7; empty entries dropped. Returns nil for an empty result.
func normStringSet(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// checkDependencyDAG validates in-bundle dependency edges (spec §6.3): every target is a
// UID in the same bundle, no self-edge, and the graph is acyclic. Cross-project/cross-org/
// UI-managed/other-provider targets are impossible here (only same-bundle UIDs are
// expressible) and are additionally enforced at the store layer in a later iteration.
func checkDependencyDAG(dp *DesiredProject) error {
	for uid, dm := range dp.Monitors {
		for _, dep := range dm.DependsOn {
			if dep == uid {
				return rejectf(ReasonDependencyInvalid, uid, "monitor depends on itself")
			}
			if _, ok := dp.Monitors[dep]; !ok {
				return rejectf(ReasonDependencyInvalid, uid, "depends_on %q is not a monitor in this bundle", dep)
			}
		}
	}
	// Cycle detection via DFS with a color map (white/gray/black).
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(dp.Monitors))
	var visit func(uid string) *BundleError
	visit = func(uid string) *BundleError {
		color[uid] = gray
		for _, dep := range dp.Monitors[uid].DependsOn {
			switch color[dep] {
			case gray:
				return rejectf(ReasonDependencyCycle, uid, "dependency cycle through %q", dep)
			case white:
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		color[uid] = black
		return nil
	}
	// Deterministic traversal order for stable errors.
	uids := make([]string, 0, len(dp.Monitors))
	for uid := range dp.Monitors {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	for _, uid := range uids {
		if color[uid] == white {
			if err := visit(uid); err != nil {
				return err
			}
		}
	}
	return nil
}
