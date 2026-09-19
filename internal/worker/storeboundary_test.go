package worker

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FR-032 invariant 11, import half — placed BESIDE the package it guards rather than in one
// cross-cutting test that neither team owns.
//
// A DB-less executor stays DB-less. The ledger is written by core from the PUBLISHED job, never by
// the thing that ran it: an executor that could reach the store could also write a run fact that
// no dispatch produced, and the ledger's whole claim is that every fact comes from a job core
// issued.

func TestThisExecutorImportsNoStorePackage(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(path, "cerbix/internal/store") {
				t.Errorf("%s imports %q. This executor must stay DB-less: FR-032 run facts come "+
					"from the PUBLISHED job, recorded by core, never by the process that ran it",
					filepath.Base(name), path)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("no source files were scanned, so a store import would go unseen")
	}
}
