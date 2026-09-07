package scheduler

import (
	"go/ast"
	"sort"
	"testing"
)

// G1's shape, guarded rather than asserted by a green suite.
//
// The finding G1 names is three resolvers each keeping its OWN copy of the pull-region exclusion.
// The record fixes that only for as long as nobody reads the map beside it, and no behavioural test
// can see a second reader: `caps.servedByAgents(r)` and `s.pullRegions[r]` return the same bool for
// every input, because the record is built FROM that map. A duplicate owner is therefore invisible
// to every run and visible only in the shape — which is what this reads.
//
// The first implementation of G1 had exactly that hole: `canaryAnnouncements` kept three direct
// reads and `localCanaryTokenFor` a fourth, while the comment beside the record said all three
// resolvers shared one exclusion. Green everywhere. This is the assertion that would have failed.
//
// It guards the SET and not a count: a new legitimate reader has to be added here by NAME, which is
// the point at which someone decides whether it is a routing decision or a capability one.
func TestThePullRegionExclusionHasONEOwner(t *testing.T) {
	files := parseSchedulerPackage(t, ".")

	// publishScheduledJob is the one legitimate direct reader: it picks the TRANSPORT for a job
	// already decided upon — pull queue or broker — which is routing and not a statement about what
	// a region's executors can consume. Everything else asks the record.
	allowed := map[string]bool{"publishScheduledJob": true}

	var offenders []string
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				idx, ok := n.(*ast.IndexExpr)
				if !ok {
					return true
				}
				sel, ok := idx.X.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "pullRegions" {
					return true
				}
				if !allowed[fn.Name.Name] {
					offenders = append(offenders, fn.Name.Name)
				}
				return true
			})
		}
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("these functions index pullRegions directly instead of asking the capability record: %v\n"+
			"that is a second owner of the pull-region exclusion, and it is indistinguishable from the "+
			"first in every behavioural test", offenders)
	}
}

// The negation must be reachable, or the guard above is green over a pattern it never matches.
//
// Not a mutation of the product: the guard is asserted against a source tree it does not scan, so
// the assertion proves the MATCHER works rather than proving today's tree is clean. Both halves are
// needed — see the note about a guard that is silent about what it is missing.
func TestThePullRegionOwnerGuardCanFail(t *testing.T) {
	files := parseSchedulerPackage(t, ".")
	var readers []string
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				idx, ok := n.(*ast.IndexExpr)
				if !ok {
					return true
				}
				sel, ok := idx.X.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "pullRegions" {
					return true
				}
				readers = append(readers, fn.Name.Name)
				return true
			})
		}
	}
	if len(readers) == 0 {
		t.Fatal("the matcher found no direct pullRegions index anywhere, so the guard above " +
			"cannot distinguish a clean tree from a broken matcher")
	}
}
