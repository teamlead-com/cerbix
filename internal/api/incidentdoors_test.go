package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// FR-026 D3 needs TWO guards, because they are two different mistakes and one test cannot see both.
//
// The compiler already catches a forgotten actor: the principal door takes one as a parameter. What
// it cannot catch is a handler reaching for the SYSTEM door, or a system door existing for a writer
// that has no machine caller — so the next handler that wants one finds an unaudited door already
// built.
//
// Both guards are driven by a FIXTURE that contains the violation as well as by the tree, because a
// guard nobody has watched fail is a guard nobody knows works. This one earned that rule the hard way:
// its first version exempted `handlers_alertmanager.go` by name, on the reasoning that the receiver is
// "a machine writer that happens to arrive over HTTP" — and the exemption hid a real defect, because
// D1 audits the receiver's create and resolve. Alertmanager posts with a project-write token, and that
// token is a principal. There is no exemption now.

// systemDoors are the writers that legitimately have one. The set is written out HERE so widening it
// is an edit a reviewer sees, rather than a method that quietly appears in `internal/store`.
var systemDoors = map[string]bool{
	"CreateIncidentBySystem":    true,
	"AddIncidentUpdateBySystem": true,
}

// systemDoorCallsIn reports every system-door call site in one parsed file. Guard 1 is this function;
// the test runs it over the tree and over a fixture that must fail it.
func systemDoorCallsIn(file *ast.File) []string {
	var hits []string
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if systemDoors[sel.Sel.Name] {
			hits = append(hits, sel.Sel.Name)
		}
		return true
	})
	return hits
}

// GUARD 1 — what is REACHED. No file in internal/api may call a system door, receiver included.
func TestTheAPINeverCallsASystemDoor(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, "../api", func(fi os.FileInfo) bool {
		return strings.HasSuffix(fi.Name(), ".go") && !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse internal/api: %v", err)
	}
	seen := 0
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			seen++
			for _, hit := range systemDoorCallsIn(file) {
				t.Errorf("%s calls the SYSTEM door %s: a principal write must be audited (FR-026 D3)",
					filepath.Base(path), hit)
			}
		}
	}
	if seen == 0 {
		t.Fatal("the guard parsed no files — it would pass over an empty package")
	}
}

func TestGuardOneFailsOnAFixtureThatViolatesIt(t *testing.T) {
	const violation = `package api
func (h *Handler) openSomething() error {
	_, err := h.store.CreateIncidentBySystem(ctx, inc, "body", "author")
	return err
}`
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", violation, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if hits := systemDoorCallsIn(file); len(hits) != 1 || hits[0] != "CreateIncidentBySystem" {
		t.Fatalf("the guard did not see the violation it exists to catch: %v", hits)
	}
}

// declaredSystemDoors is guard 2: the `…BySystem` methods a package declares.
func declaredSystemDoors(files []*ast.File) []string {
	declared := map[string]bool{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil {
				continue
			}
			if strings.HasSuffix(fn.Name.Name, "BySystem") {
				declared[fn.Name.Name] = true
			}
		}
	}
	var got []string
	for name := range declared {
		got = append(got, name)
	}
	sort.Strings(got)
	return got
}

// GUARD 2 — what EXISTS. Guard 1 is blind to a system door nobody calls yet: `AcknowledgeIncidentBySystem`
// declared in internal/store and called by nobody passes an internal/api scan cleanly, and the next
// handler to want it finds an unaudited door already built.
func TestTheStoreDeclaresExactlyTheSystemDoorsThatHaveMachineCallers(t *testing.T) {
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
	got := declaredSystemDoors(files)

	var want []string
	for name := range systemDoors {
		want = append(want, name)
	}
	sort.Strings(want)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("system doors declared in internal/store = %v, want exactly %v.\n"+
			"A new one means a machine caller exists: add it to `systemDoors` in the same change that adds the caller.",
			got, want)
	}
}

func TestGuardTwoFailsOnAFixtureThatViolatesIt(t *testing.T) {
	const violation = `package store
func (s *Store) AcknowledgeIncidentBySystem(ctx context.Context, id string) error { return nil }`
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", violation, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	got := declaredSystemDoors([]*ast.File{file})
	if len(got) != 1 || got[0] != "AcknowledgeIncidentBySystem" {
		t.Fatalf("the guard did not see the door it exists to catch: %v", got)
	}
	if systemDoors[got[0]] {
		t.Fatal("the fixture's door is in the allowed set — the fixture no longer violates anything")
	}
}
