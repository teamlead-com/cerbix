package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
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

// interfaceMethodsNamed returns the methods an interface declares whose names end in `suffix`.
// interfaceIndex maps every interface type declared in `files` to its AST.
func interfaceIndex(files []*ast.File) map[string]*ast.InterfaceType {
	out := map[string]*ast.InterfaceType{}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			if it, ok := spec.Type.(*ast.InterfaceType); ok {
				out[spec.Name.Name] = it
			}
			return true
		})
	}
	return out
}

// interfaceMethodsNamed reports the methods of `iface` whose names end in `suffix`, INCLUDING the
// methods it gains by EMBEDDING another interface, and separately reports every embedded type it
// could not resolve.
//
// The embedded half is a reviewer P2 (party [322]) on the first version, and it is the guard-idiom
// hole in its purest form: the walk read `m.Names`, which is empty for an embedded entry, so
//
//	type SystemStore interface{ CreateIncidentBySystem(...) ... }
//	type Store interface { SystemStore; GetIncident(...) ... }
//
// put a system door on `Store` — reachable through it, forced onto every fake — while the guard
// returned nothing and stayed green. The guard was reading the SPELLING of the contract instead of
// the contract.
//
// `unresolved` is not a convenience: an embedded name this package cannot see (another package's
// interface, a generic instantiation) means the guard cannot know what the contract offers, and a
// guard that cannot know must SAY so rather than answer "nothing found". The caller fails on it.
// The visited set is the cycle guard — an interface graph may be malformed or mutually recursive,
// and this must report a hole rather than hang while doing it.
func interfaceMethodsNamed(files []*ast.File, iface, suffix string) (got []string, unresolved []string) {
	index := interfaceIndex(files)
	visited := map[string]bool{}

	// ONE walk over interface ELEMENTS, and both kinds of embed re-enter it. The first version had a
	// second, hand-written expansion for the inline case that read one level and stopped — so
	//
	//	type Store interface { interface{ interface{ CreateIncidentBySystem() } }; GetIncident() }
	//
	// which the compiler accepts, put the door on `Store` while the guard returned nothing (reviewer
	// P2, party [324]). A special case that duplicates the general one is a place for the general
	// one's rules to stop applying, and here they stopped at depth two.
	var walkIface func(it *ast.InterfaceType)
	var walkNamed func(name string)

	walkNamed = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		it, ok := index[name]
		if !ok {
			unresolved = append(unresolved, name)
			return
		}
		walkIface(it)
	}

	walkIface = func(it *ast.InterfaceType) {
		for _, m := range it.Methods.List {
			if len(m.Names) == 0 {
				switch t := m.Type.(type) {
				case *ast.Ident: // embedded interface declared in this package
					walkNamed(t.Name)
				case *ast.InterfaceType: // an inline interface literal, at any depth
					walkIface(t)
				default: // qualified (`other.Iface`), generic, or a type expression this cannot read
					unresolved = append(unresolved, types.ExprString(m.Type))
				}
				continue
			}
			for _, n := range m.Names {
				if strings.HasSuffix(n.Name, suffix) {
					got = append(got, n.Name)
				}
			}
		}
	}
	walkNamed(iface)

	sort.Strings(got)
	sort.Strings(unresolved)
	return got, unresolved
}

// GUARD 3 — what the API's own CONTRACT offers (D4).
//
// Guards 1 and 2 are both blind to this. Guard 1 scans for CALLS, and there are none; guard 2 scans
// `internal/store`, where the doors legitimately exist for the reconciler. The API's `Store`
// interface declared both of them anyway, with no caller in the package and no possibility of one —
// so an unaudited door sat in the API's own contract, every fake had to implement it, and the next
// handler that wanted one would have found it already built and already wired.
//
// Note what does NOT discharge this. The compiler proves nothing here: there is no caller to refuse,
// and a fake carrying extra methods still satisfies a narrower interface. Only an assertion over the
// declaration can see it, which is why this is a guard and not a deletion.
//
// The mutation that must kill this: put either method back on the interface.
func TestTheAPIStoreInterfaceDeclaresNoSystemDoor(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, "../api", func(fi os.FileInfo) bool {
		return strings.HasSuffix(fi.Name(), ".go") && !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse internal/api: %v", err)
	}
	var files []*ast.File
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			files = append(files, file)
		}
	}
	if len(files) == 0 {
		t.Fatal("the guard parsed no files — it would pass over an empty package")
	}
	// The interface exists: a rename would otherwise make this guard silently vacuous.
	all, unresolved := interfaceMethodsNamed(files, "Store", "")
	if len(all) == 0 {
		t.Fatal("no `Store` interface with methods was found in internal/api")
	}
	// An embed this walk cannot follow is a part of the contract the guard is blind to, so it is a
	// FAILURE and not a note: "no system door found" would then mean "none found where I looked".
	if len(unresolved) != 0 {
		t.Fatalf("the Store interface embeds %v, which this guard cannot resolve — it cannot claim "+
			"the contract declares no system door until it can read all of it", unresolved)
	}
	got, _ := interfaceMethodsNamed(files, "Store", "BySystem")
	if len(got) != 0 {
		t.Errorf("internal/api's Store interface declares the system door(s) %v. The API may not "+
			"call one (guard 1), so declaring one puts an unaudited door in its contract and makes "+
			"every fake implement it (FR-026 D3).", got)
	}
}

func TestGuardThreeFailsOnAFixtureThatViolatesIt(t *testing.T) {
	const violation = `package api
type Store interface {
	GetIncident(ctx context.Context, id string) (domain.Incident, error)
	CreateIncidentBySystem(ctx context.Context, inc domain.Incident, openingBody, author string) (domain.Incident, error)
}`
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", violation, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	got, unresolved := interfaceMethodsNamed([]*ast.File{file}, "Store", "BySystem")
	if len(unresolved) != 0 {
		t.Fatalf("the fixture resolves fully; %v says the walk is broken", unresolved)
	}
	if len(got) != 1 || got[0] != "CreateIncidentBySystem" {
		t.Fatalf("the guard did not see the violation it exists to catch: %v", got)
	}
}

// The EMBEDDED violation, which the first version of guard 3 passed over in silence (reviewer P2,
// party [322]). The door is not written on `Store`; it arrives through an interface `Store` embeds,
// and it is just as reachable, just as forced onto every fake, and just as unaudited.
func TestGuardThreeSeesASystemDoorArrivingThroughAnEmbeddedInterface(t *testing.T) {
	const violation = `package api
type SystemStore interface {
	CreateIncidentBySystem(ctx context.Context, inc domain.Incident) (domain.Incident, error)
}
type Store interface {
	SystemStore
	GetIncident(ctx context.Context, id string) (domain.Incident, error)
}`
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", violation, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	got, unresolved := interfaceMethodsNamed([]*ast.File{file}, "Store", "BySystem")
	if len(unresolved) != 0 {
		t.Fatalf("both interfaces are in the fixture; %v means the walk did not follow the embed", unresolved)
	}
	if len(got) != 1 || got[0] != "CreateIncidentBySystem" {
		t.Fatalf("the embedded system door was not seen: %v", got)
	}
}

// An embed the walk CANNOT resolve must be reported, not silently treated as "nothing there".
// Without this the guard answers a narrower question than the one it is asked, which is how it was
// green over the case above.
func TestGuardThreeReportsAnEmbedItCannotResolve(t *testing.T) {
	const opaque = `package api
type Store interface {
	other.AuditStore
	Missing
	GetIncident(ctx context.Context, id string) (domain.Incident, error)
}`
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", opaque, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	_, unresolved := interfaceMethodsNamed([]*ast.File{file}, "Store", "BySystem")
	if len(unresolved) != 2 || unresolved[0] != "Missing" || unresolved[1] != "other.AuditStore" {
		t.Fatalf("an unreadable embed must be named, got %v", unresolved)
	}
}

// The INLINE embed, at a depth the first fix did not reach (reviewer P2, party [324]). Go accepts an
// interface literal embedded in an interface literal, and the compiler was asked before this test
// was written: `go tool compile` takes the form below.
func TestGuardThreeSeesADoorNestedInsideInlineInterfaces(t *testing.T) {
	const violation = `package api
type Store interface {
	interface {
		interface {
			CreateIncidentBySystem(ctx context.Context) error
		}
	}
	GetIncident(ctx context.Context, id string) error
}`
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", violation, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	got, unresolved := interfaceMethodsNamed([]*ast.File{file}, "Store", "BySystem")
	if len(unresolved) != 0 {
		t.Fatalf("nothing here is unresolvable; %v means the walk misread an inline element", unresolved)
	}
	if len(got) != 1 || got[0] != "CreateIncidentBySystem" {
		t.Fatalf("a door two inline embeds deep was not seen: %v", got)
	}
}

// Mixed nesting: a named embed reached THROUGH an inline one, which is where a walk with two
// separate expansions loses the named half's cycle guard and resolution rules.
func TestGuardThreeFollowsANamedEmbedFoundInsideAnInlineOne(t *testing.T) {
	const violation = `package api
type SystemStore interface {
	AcknowledgeIncidentBySystem(ctx context.Context, id string) error
}
type Store interface {
	interface {
		SystemStore
	}
	GetIncident(ctx context.Context, id string) error
}`
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", violation, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	got, unresolved := interfaceMethodsNamed([]*ast.File{file}, "Store", "BySystem")
	if len(unresolved) != 0 {
		t.Fatalf("SystemStore is declared in the fixture; %v is wrong", unresolved)
	}
	if len(got) != 1 || got[0] != "AcknowledgeIncidentBySystem" {
		t.Fatalf("a named embed inside an inline one was not followed: %v", got)
	}
}

// And fail-closed survives the inline path: an unreadable element nested inside a literal is still
// reported, rather than being dropped by the branch that handles literals.
func TestGuardThreeReportsAnUnreadableElementNestedInAnInlineInterface(t *testing.T) {
	const opaque = `package api
type Store interface {
	interface {
		other.AuditStore
	}
	GetIncident(ctx context.Context, id string) error
}`
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", opaque, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	_, unresolved := interfaceMethodsNamed([]*ast.File{file}, "Store", "BySystem")
	if len(unresolved) != 1 || unresolved[0] != "other.AuditStore" {
		t.Fatalf("an unreadable element inside a literal must still be named, got %v", unresolved)
	}
}

// A cycle must end the walk rather than hang it. A malformed or mutually recursive interface graph
// is a thing a guard meets on a bad day, and hanging is the worst way to report it.
func TestGuardThreeTerminatesOnACycle(t *testing.T) {
	const cyclic = `package api
type A interface {
	B
	AlphaBySystem(ctx context.Context) error
}
type B interface {
	A
	BetaBySystem(ctx context.Context) error
}`
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", cyclic, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	got, unresolved := interfaceMethodsNamed([]*ast.File{file}, "A", "BySystem")
	if len(unresolved) != 0 {
		t.Fatalf("both sides of the cycle are declared here; %v is wrong", unresolved)
	}
	if len(got) != 2 || got[0] != "AlphaBySystem" || got[1] != "BetaBySystem" {
		t.Fatalf("the cycle must be walked once and completely, got %v", got)
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
