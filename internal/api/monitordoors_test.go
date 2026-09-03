package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// FR-026 §10 (D-0233): a monitor write says who wrote it. The API reaches three monitor writers and
// each takes an actor in its signature, so a forgotten actor is a compile error — guard 1 is the
// compiler here. What the compiler cannot see is an unaudited EXPORTED writer that nobody calls yet:
// the next handler to want one would find it already built. This is guard 2 for monitors, in the
// shape FR-026 gave the incident doors: what EXISTS in `internal/store`, over its non-test files.

// bareMonitorWriters are the unaudited spellings. They may exist unexported (the doors wrap them, and
// the tests reach them through export_test.go); they may not be exported methods of the product.
var bareMonitorWriters = map[string]bool{
	"CreateMonitor": true,
	"UpdateMonitor": true,
	"DeleteMonitor": true,
}

func exportedBareMonitorWritersIn(files []*ast.File) []string {
	found := map[string]bool{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil {
				continue
			}
			if bareMonitorWriters[fn.Name.Name] {
				found[fn.Name.Name] = true
			}
		}
	}
	var got []string
	for name := range found {
		got = append(got, name)
	}
	sort.Strings(got)
	return got
}

func TestTheStoreExportsNoUnauditedMonitorWriter(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, "../store", func(fi os.FileInfo) bool {
		return strings.HasSuffix(fi.Name(), ".go") && !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse internal/store: %v", err)
	}
	var files []*ast.File
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			files = append(files, file)
		}
	}
	if got := exportedBareMonitorWritersIn(files); len(got) != 0 {
		t.Fatalf("internal/store exports unaudited monitor writers %v — the product's only monitor doors take an AuditActor (FR-026 §10)", got)
	}
}

func TestTheMonitorWriterGuardFailsOnAFixtureThatViolatesIt(t *testing.T) {
	const violation = `package store
func (s *Store) UpdateMonitor(ctx context.Context, m domain.Monitor) (domain.Monitor, error) { return m, nil }`
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", violation, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if got := exportedBareMonitorWritersIn([]*ast.File{file}); len(got) != 1 || got[0] != "UpdateMonitor" {
		t.Fatalf("the guard did not see the writer it exists to catch: %v", got)
	}
}
