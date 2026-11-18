package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// FR-042 / NFR-035 — two source guards over every non-test Go file under internal/.
//
// I3: `incidents.impact` is written by the creating paths and by the rise step, and by nothing
// else. The writer scan finds every SQL text that inserts an incident or sets `impact` on one,
// records it by KIND — `insert` or `update` — and asserts the (scope, kind) pairs are exactly the
// known ones: the creating paths may INSERT, only the rise step may UPDATE. Being allowed to insert
// an incident grants no right to update its impact.
//
// NFR-035: impact is a classification, not a control. The reader scan records every SCOPE that
// reads an incident's impact, and by which rule — a `.Impact` selector, an SQL text naming the
// `impact` column, or a use of the shared `incidentColumns*` lists that include it — and asserts the
// set is exactly the creating path, rendering, audit, the rise step and the domain type. The
// granularity is the scope and the rule, not the file: an allowed file also holds the gate's
// incident lookup, and a predicate on impact added THERE must fail the test.
//
// A scope is a function by name; a method by its method expression, `(*T).M` for a pointer receiver
// and `T.M` for a value receiver, so two methods of one name on two types are two scopes; and a
// package-level const, var or type by `<package>:<identifier>`, so a new package-level statement
// does not hide behind an allowed one in the same file.
//
// Both scans parse; neither greps. A doc comment or a prose mention of "impact" is not a read.
//
// An "SQL text" is a string literal, or a whole `+` chain of string parts joined with each non-literal
// operand (an identifier such as `incidentColumns`) standing as ` ? `: a statement split across
// literals — `SELECT `+incidentColumns+` FROM incidents WHERE … AND impact <> 'minor'` — is judged
// as the one statement it is, not as fragments of which none carries both a verb and the column.
// Keywords are matched in any case, and `ORDER`/`GROUP` and `BY` may be split by any whitespace.
//
// Known limits — each is a way to read or write impact that these scans do NOT see:
//   - SQL assembled any other way than a `+` chain of literals: `+=` on a variable, `fmt.Sprintf`,
//     `strings.Join`, a `strings.Builder` (or any other runtime assembly) is judged only as the
//     separate literals it is built from, and a statement whose verb and `impact` land in different
//     literals is missed;
//   - `SELECT *` (or `x.*`) from incidents reads impact without naming it, and so does a column list
//     held in a constant not named `incidentColumns*`;
//   - a serialized incident — `json.Marshal` of a `domain.Incident`, a template rendering one —
//     carries impact to code that never writes `.Impact`;
//   - the reader scan tells SQL from prose by the column's neighbours (impactSQLColumnPosition) and
//     the text's statement shape, so a column with a bare alias and only a comma or a keyword
//     before it — `SELECT id, impact sev FROM incidents` — reads as prose and is missed (`impact AS
//     sev` is seen);
//   - a set, pattern or range test on the column is seen in any text with a keyword — a shapeless
//     fragment such as ` AND impact IN ('major')`, ` AND impact NOT IN ($1)`, ` OR impact LIKE
//     'crit%'`, ` AND impact ILIKE $2` or ` AND impact BETWEEN $1 AND $2` included — only when the
//     operator is followed by SQL-shaped material: `IN (`, `[I]LIKE` and a quoted literal, `$n` or
//     `?`, `BETWEEN x AND` with x a quoted literal, `$n`, `?` or a number. In a fragment with no
//     statement shape, a test whose operand is anything else is missed: `impact IN ` with its
//     parenthesis in another constant (a non-literal `+` operand stands as ` ? `, so `IN (` + list
//     + `)` is seen but `IN ` + list is not), `impact LIKE prefix || '%'`, `impact BETWEEN lo AND
//     hi`, `impact SIMILAR TO …`. With a statement shape, `IN`, `[I]LIKE` and `BETWEEN` after a
//     keyword are weak neighbours and seen whatever follows;
//   - known FALSE positives of the reader scan: prose that puts the column next to a strong SQL
//     neighbour — an operator ("impact = critical, and …"), an IS test ("and impact is not null"),
//     or a set, pattern or range test with SQL-shaped material after it ("and the impact in (most)
//     cases", "or impact like 'this' is rare", "and impact between 2 and 3 hours") — is reported
//     as a read in any text with a keyword. Prose with only a comma or a keyword on both sides of the
//     column is reported ONLY when the text also has a statement shape ("select title, impact,
//     status from the form"); without one ("title, impact, status and body are required", "when
//     impact is in doubt", "and impact between releases is not compared") it is not. A false
//     positive fails the test loudly, the safe direction;
//   - a known false positive of the writer scan: an UPDATE of incidents whose WHERE (not its SET)
//     has an `impact = …` predicate is counted as a write of impact — the safe direction, but the
//     violation is then named as a writer rather than a reader.

var (
	// The incidents table, with an optional schema qualifier and optional quotes: `INSERT INTO
	// public.incidents` and `"incidents"` are the same table; `incident_updates` is not.
	impactTable = `(?:"?\w+"?\.)?"?incidents"?\b`
	// The SET list of an UPDATE (plain or ON CONFLICT … DO UPDATE) that writes impact: `impact`,
	// quoted or not, followed by optional spaces and `=` (`impact_basis =` does not match), or
	// `impact` inside the column list of the row-constructor form `SET (status, impact) = (…)`.
	impactSetList = `\bSET\b(?:.*?\bimpact\b"?\s*=|\s*\([^)]*\bimpact\b[^)]*\)\s*=)`

	impactInsertRe = regexp.MustCompile(`(?is)\bINSERT\s+INTO\s+` + impactTable)
	impactUpdateRe = regexp.MustCompile(`(?is)\bUPDATE\s+(?:ONLY\s+)?` + impactTable + `.*?` + impactSetList)
	// An upsert into incidents whose conflict arm sets impact UPDATES an existing incident's impact.
	impactUpsertRe = regexp.MustCompile(`(?is)\bINSERT\s+INTO\s+` + impactTable +
		`.*?\bON\s+CONFLICT\b.*?\bDO\s+UPDATE\s+` + impactSetList)

	// An SQL keyword, in any case. The set includes the predicate words, so a WHERE-clause fragment
	// kept in its own constant still counts as SQL. A keyword alone does not make an SQL read — prose
	// is full of "and", "or", "set", "from", "when", "order by" — the column must also stand in an
	// SQL position (impactSQLColumnPosition), and a merely WEAK position needs a statement shape
	// (impactSQLShapeRe) as well.
	impactSQLKeywordRe = regexp.MustCompile(`(?i)\b(?:SELECT|INSERT|UPDATE|DELETE|RETURNING|WHERE|FROM|JOIN|SET|VALUES|AND|OR|CASE|WHEN|HAVING|ORDER\s+BY|GROUP\s+BY)\b`)
	// The column, in any case (an unquoted identifier is case-insensitive in SQL). `impact_basis`,
	// `incident_impacts` and `default_incident_impact` are other columns.
	impactSQLColumnRe = regexp.MustCompile(`(?i)\bimpact\b`)
	// A statement or clause SHAPE, in any case: more than a lone keyword. "title, impact, status and
	// body are required" has a column list's commas but no shape; "SELECT id, impact FROM" and
	// "ORDER BY impact" have one. A bare WHERE, AND, OR, WHEN or SET is not a shape.
	impactSQLShapeRe = regexp.MustCompile(`(?is)\bSELECT\b.*\bFROM\b|\bINSERT\s+INTO\b|\bUPDATE\b.*\bSET\b|` +
		`\bDELETE\s+FROM\b|\bRETURNING\b|\bVALUES\s*\(|\bON\s+CONFLICT\b|\b(?:ORDER|GROUP)\s+BY\b`)
	impactSQLLastWordRe  = regexp.MustCompile(`\w+$`)
	impactSQLFirstWordRe = regexp.MustCompile(`^\w+`)
)

// The neighbours that put a column in an SQL position. A STRONG neighbour is SQL on its own: an
// operator on either side, a qualifier dot TOUCHING the column on its left (`i.impact` — a
// sentence's full stop before a space is not one), `COLLATE`, an IS test (`IS [NOT] NULL`,
// `IS DISTINCT FROM`, …) or an SQL-shaped `[NOT] IN (`, `[I]LIKE $n` or `BETWEEN $1 AND` on its
// right. A WEAK one is SQL only
// with a weak (or strong) neighbour on the other side AND a statement shape in the text: a comma or
// a parenthesis, a clause keyword, the start or end of the text. "the impact of this order by the
// operator" has a word that is no keyword on each side of the column; "SELECT id, impact FROM",
// "WHERE impact <> 'minor'" and "ORDER BY impact" do not.
var (
	impactSQLStrongLeftOps    = []string{"<>", "!=", "<=", ">=", "||", "=", "<", ">"}
	impactSQLStrongRightOps   = []string{"<>", "!=", "<=", ">=", "||", "::", "!~", "~", "=", "<", ">"}
	impactSQLStrongRightWords = map[string]bool{"collate": true}
	// An SQL IS test right of the column is strong with no left constraint: `impact IS NULL`,
	// `IS NOT TRUE`, `IS DISTINCT FROM $1`. "impact is critical" is not one.
	impactSQLStrongRightIsRe = regexp.MustCompile(`^(?i)is\s+(?:not\s+)?(?:null|true|false|unknown|distinct\s+from)\b`)
	// A set, pattern or range test right of the column is strong with no left constraint when the
	// operator is followed by SQL-shaped material: `[NOT] IN (`; `[NOT] [I]LIKE` and a quoted literal,
	// a `$n` placeholder or a `?` (a non-literal operand of a `+` chain stands as ` ? `); `[NOT]
	// BETWEEN [SYMMETRIC] x AND` with x a quoted literal, a `$n`, a `?` or a number. "impact in the
	// UI", "the impact like a badge" and "impact between releases" are not one.
	impactSQLStrongRightTestRe = regexp.MustCompile(`^(?i)(?:not\s+)?(?:` +
		`in\s*\(|` +
		`i?like\s*(?:'|\$\d|\?)|` +
		`between\s+(?:symmetric\s+)?(?:'[^']*'|\$\d+|\?|-?\d+(?:\.\d+)?)\s+and\b)`)
	impactSQLWeakLeftOps   = []string{",", "("}
	impactSQLWeakRightOps  = []string{",", ")"}
	impactSQLWeakLeftWords = map[string]bool{
		"select": true, "distinct": true, "where": true, "and": true, "or": true, "not": true,
		"by": true, "set": true, "on": true, "when": true, "case": true, "then": true, "else": true,
		"returning": true, "having": true,
	}
	// "and"/"or" are left out on purpose: "severity, impact and status" is prose. A comparison's
	// other side is a strong neighbour anyway.
	impactSQLWeakRightWords = map[string]bool{
		"from": true, "asc": true, "desc": true, "nulls": true, "is": true, "in": true, "not": true,
		"between": true, "like": true, "ilike": true, "then": true, "else": true, "when": true,
		"end": true, "as": true, "where": true, "limit": true, "returning": true, "group": true,
		"order": true, "having": true,
	}
)

// The three reader rules.
const (
	ruleSelector        = "selector"
	ruleSQLLiteral      = "sql-literal"
	ruleIncidentColumns = "incidentColumns"
)

// The two writer kinds.
const (
	writeInsert = "insert"
	writeUpdate = "update"
)

// impactScanFile is one parsed source file and its path relative to internal/.
type impactScanFile struct {
	rel  string
	file *ast.File
}

// parseInternalTree parses every non-test .go file under internal/ (this test runs in
// internal/store, so internal/ is ".."). There are no Go files under internal/web/dist.
func parseInternalTree(t *testing.T) []impactScanFile {
	t.Helper()
	root := ".."
	fset := token.NewFileSet()
	var files []impactScanFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		files = append(files, impactScanFile{rel: filepath.ToSlash(rel), file: file})
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	// A guard that parsed nothing would pass vacuously; the store package alone has well over 50.
	if len(files) < 50 {
		t.Fatalf("parsed only %d files under internal/ — the guard would pass vacuously", len(files))
	}
	return files
}

// structTags returns the struct-tag literals of a file, which are never SQL.
func structTags(file *ast.File) map[*ast.BasicLit]bool {
	tags := map[*ast.BasicLit]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if f, ok := n.(*ast.Field); ok && f.Tag != nil {
			tags[f.Tag] = true
		}
		return true
	})
	return tags
}

// stringLiteralText is a STRING literal's value with its quotes and escapes resolved.
func stringLiteralText(lit *ast.BasicLit) string {
	if s, err := strconv.Unquote(lit.Value); err == nil {
		return s
	}
	return lit.Value
}

// flattenConcat appends the parts of a `+` chain to b — a string literal as its text, any other
// operand as ` ? ` (collected in others, so it is still inspected on its own) — and reports whether
// the chain held a string literal at all (a numeric sum holds none).
func flattenConcat(e ast.Expr, b *strings.Builder, others *[]ast.Expr) bool {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return flattenConcat(x.X, b, others)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			l := flattenConcat(x.X, b, others)
			r := flattenConcat(x.Y, b, others)
			return l || r
		}
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			b.WriteString(stringLiteralText(x))
			return true
		}
	}
	b.WriteString(" ? ")
	*others = append(*others, e)
	return false
}

// sqlTexts returns every SQL text under root: each lone non-tag string literal, and each maximal
// `+` chain holding a string literal, joined.
func sqlTexts(root ast.Node, tags map[*ast.BasicLit]bool) []string {
	var out []string
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BasicLit:
			if x.Kind == token.STRING && !tags[x] {
				out = append(out, stringLiteralText(x))
			}
		case *ast.BinaryExpr:
			if x.Op != token.ADD {
				return true
			}
			var b strings.Builder
			var others []ast.Expr
			if !flattenConcat(x, &b, &others) {
				return true
			}
			out = append(out, b.String())
			for _, o := range others {
				ast.Inspect(o, visit)
			}
			return false
		}
		return true
	}
	ast.Inspect(root, visit)
	return out
}

// methodScope is a method's method expression — `(*T).M` for a pointer receiver, `T.M` for a value
// receiver, type parameters dropped — or a plain function's name.
func methodScope(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t, ptr := fd.Recv.List[0].Type, false
	for {
		switch x := t.(type) {
		case *ast.ParenExpr:
			t = x.X
			continue
		case *ast.StarExpr:
			t, ptr = x.X, true
			continue
		case *ast.IndexExpr:
			t = x.X
			continue
		case *ast.IndexListExpr:
			t = x.X
			continue
		}
		break
	}
	recv := "?"
	if id, ok := t.(*ast.Ident); ok {
		recv = id.Name
	}
	if ptr {
		return "(*" + recv + ")." + fd.Name.Name
	}
	return recv + "." + fd.Name.Name
}

// packageScope names a package-level declaration.
func packageScope(name string) string { return "<package>:" + name }

// declScopes calls fn with each scope of a file and the nodes that belong to it: a function or
// method (methodScope) with its whole declaration, and each package-level const, var or type
// (packageScope) with its name, its type and its own value. A `var a, b = f()` — fewer values than
// names — is one scope, `<package>:a,b`. Imports are not scanned.
func declScopes(file *ast.File, fn func(scope string, nodes []ast.Node)) {
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			fn(methodScope(d), []ast.Node{d})
		case *ast.GenDecl:
			if d.Tok == token.IMPORT {
				continue
			}
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					fn(packageScope(s.Name.Name), []ast.Node{s})
				case *ast.ValueSpec:
					if len(s.Values) != 0 && len(s.Values) != len(s.Names) {
						names := make([]string, len(s.Names))
						for i, n := range s.Names {
							names[i] = n.Name
						}
						fn(packageScope(strings.Join(names, ",")), []ast.Node{s})
						continue
					}
					for i, n := range s.Names {
						nodes := []ast.Node{n}
						if s.Type != nil {
							nodes = append(nodes, s.Type)
						}
						if len(s.Values) != 0 {
							nodes = append(nodes, s.Values[i])
						}
						fn(packageScope(n.Name), nodes)
					}
				}
			}
		}
	}
}

// incidentImpactWrites returns the kinds of write one SQL text makes to `incidents.impact`: an
// insert writes it explicitly or by its column default; an update — a plain UPDATE, or an upsert's
// ON CONFLICT … DO UPDATE arm — must set `impact`. An upsert that sets impact is both.
func incidentImpactWrites(sql string) []string {
	var kinds []string
	if impactInsertRe.MatchString(sql) {
		kinds = append(kinds, writeInsert)
	}
	if impactUpdateRe.MatchString(sql) || impactUpsertRe.MatchString(sql) {
		kinds = append(kinds, writeUpdate)
	}
	return kinds
}

// impactWriterSites returns "<rel>:<scope>:<kind>" for every SQL text that writes
// `incidents.impact`, sorted and de-duplicated.
func impactWriterSites(files []impactScanFile) []string {
	seen := map[string]bool{}
	for _, f := range files {
		tags := structTags(f.file)
		declScopes(f.file, func(scope string, nodes []ast.Node) {
			for _, node := range nodes {
				for _, sql := range sqlTexts(node, tags) {
					for _, kind := range incidentImpactWrites(sql) {
						seen[f.rel+":"+scope+":"+kind] = true
					}
				}
			}
		})
	}
	return sortedKeys(seen)
}

// hasAnyPrefix returns the first of ops that s starts with, or "".
func hasAnyPrefix(s string, ops []string) string {
	for _, op := range ops {
		if strings.HasPrefix(s, op) {
			return op
		}
	}
	return ""
}

// hasAnySuffix returns the first of ops that s ends with, or "".
func hasAnySuffix(s string, ops []string) string {
	for _, op := range ops {
		if strings.HasSuffix(s, op) {
			return op
		}
	}
	return ""
}

// The strength of a column occurrence's SQL position.
const (
	impactPositionNone = iota
	impactPositionWeak
	impactPositionStrong
)

// impactSQLColumnPosition rates the column occurrence sql[i:j]: strong with a strong neighbour on
// either side, weak with a weak neighbour on both (see impactSQLStrongLeftOps), none otherwise.
// Quotes around the column (`"impact"`) are stepped over.
func impactSQLColumnPosition(sql string, i, j int) int {
	if i > 0 && sql[i-1] == '"' && j < len(sql) && sql[j] == '"' {
		i, j = i-1, j+1
	}
	// The qualifier dot is judged BEFORE trimming: it must touch the column.
	if i > 0 && sql[i-1] == '.' {
		return impactPositionStrong
	}
	left := strings.TrimRight(sql[:i], " \t\r\n")
	right := strings.TrimLeft(sql[j:], " \t\r\n")
	firstRight := strings.ToLower(impactSQLFirstWordRe.FindString(right))
	if hasAnySuffix(left, impactSQLStrongLeftOps) != "" || hasAnyPrefix(right, impactSQLStrongRightOps) != "" ||
		impactSQLStrongRightWords[firstRight] || impactSQLStrongRightIsRe.MatchString(right) ||
		impactSQLStrongRightTestRe.MatchString(right) {
		return impactPositionStrong
	}
	weakLeft := left == "" || hasAnySuffix(left, impactSQLWeakLeftOps) != "" ||
		impactSQLWeakLeftWords[strings.ToLower(impactSQLLastWordRe.FindString(left))]
	weakRight := right == "" || hasAnyPrefix(right, impactSQLWeakRightOps) != "" ||
		impactSQLWeakRightWords[firstRight]
	if weakLeft && weakRight {
		return impactPositionWeak
	}
	return impactPositionNone
}

// readsImpactSQL reports whether one SQL text names the `impact` column: it holds an SQL keyword,
// and an occurrence of the column stands in a strong SQL position, or in a weak one in a text with a
// statement shape.
func readsImpactSQL(sql string) bool {
	if !impactSQLKeywordRe.MatchString(sql) {
		return false
	}
	shaped := impactSQLShapeRe.MatchString(sql)
	for _, loc := range impactSQLColumnRe.FindAllStringIndex(sql, -1) {
		switch impactSQLColumnPosition(sql, loc[0], loc[1]) {
		case impactPositionStrong:
			return true
		case impactPositionWeak:
			if shaped {
				return true
			}
		}
	}
	return false
}

// impactReaderSites returns "<rel>:<scope>:<rule>" for every read of an incident's impact (scope as
// declScopes names it):
//   - selector: a `X.Impact` selector (a composite-literal key `Impact:` is a write, not a selector);
//   - sql-literal: an SQL text naming the `impact` column;
//   - incidentColumns: a use of an `incidentColumns*` identifier — the shared column lists include
//     `impact`, so a `SELECT `+incidentColumns+` …` read is otherwise invisible.
func impactReaderSites(files []impactScanFile) []string {
	seen := map[string]bool{}
	for _, f := range files {
		tags := structTags(f.file)
		declScopes(f.file, func(scope string, nodes []ast.Node) {
			site := f.rel + ":" + scope + ":"
			for _, node := range nodes {
				ast.Inspect(node, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.SelectorExpr:
						if x.Sel.Name == "Impact" {
							seen[site+ruleSelector] = true
						}
					case *ast.Ident:
						if strings.HasPrefix(x.Name, "incidentColumns") {
							seen[site+ruleIncidentColumns] = true
						}
					}
					return true
				})
				for _, sql := range sqlTexts(node, tags) {
					if readsImpactSQL(sql) {
						seen[site+ruleSQLLiteral] = true
					}
				}
			}
		})
	}
	return sortedKeys(seen)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func parseImpactFixture(t *testing.T, rel, src string) []impactScanFile {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", src, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return []impactScanFile{{rel: rel, file: file}}
}

// impactWriterAllowList is every (scope, kind) that may write incidents.impact, with the reason. An
// INSERT is allowed only on a creating path, an UPDATE only in the rise step; neither grants the
// other.
var impactWriterAllowList = map[string]string{
	// The one INSERT every non-service door goes through: the principal door (manual and
	// Alertmanager) and the system door (a monitor auto-incident reads its project's
	// default_incident_impact inside this INSERT).
	"store/incidents.go:(*Store).createIncident:insert": "creating path: the INSERT of every non-service door",
	// A service auto-incident opens at mapping[state] (§4.2).
	"store/serviceincidents.go:(*Store).OpenServiceIncidentTx:insert": "creating path: the service open's INSERT at mapping[state]",
	// The rise step: the only UPDATE of an existing incident's impact (§4.3).
	"store/serviceincidents.go:raiseServiceIncidentImpactTx:update": "rise step: the only UPDATE of an existing incident's impact",
}

// unallowedImpactWriters is the part of a writer scan the allow-list does not cover.
func unallowedImpactWriters(sites []string) []string {
	var out []string
	for _, s := range sites {
		if _, ok := impactWriterAllowList[s]; !ok {
			out = append(out, s)
		}
	}
	return out
}

// I3 — the exhaustive list of writers of incidents.impact, by kind.
func TestOnlyTheCreatingDoorsAndTheRiseWriteIncidentImpact(t *testing.T) {
	want := sortedKeys(func() map[string]bool {
		m := map[string]bool{}
		for site := range impactWriterAllowList {
			m[site] = true
		}
		return m
	}())
	got := impactWriterSites(parseInternalTree(t))
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the writers of incidents.impact changed (I3):\n  not allowed %v\n  got  %v\n  want %v\n"+
			"an existing incident's impact is written only by the rise step; a new creating path "+
			"must be added here deliberately, and nothing else may write it", unallowedImpactWriters(got), got, want)
	}
}

func TestTheImpactWriterScanSeesAViolation(t *testing.T) {
	const violation = `package store
func setImpact() { _ = ` + "`UPDATE incidents SET status = $2, impact = $3 WHERE id = $1`" + ` }
func newPath() {
	_ = ` + "`INSERT INTO incidents (project_id, title, status) VALUES ($1, $2, $3)`" + `
}
func qualifiedInsert() { _ = ` + "`INSERT INTO public.incidents (project_id, title) VALUES ($1, $2)`" + ` }
func qualifiedUpdate() { _ = ` + "`UPDATE public.incidents SET impact = $2 WHERE id = $1`" + ` }
func rowConstructor() { _ = ` + "`UPDATE incidents SET (status, impact) = ($2, $3) WHERE id = $1`" + ` }
func splitAcrossLiterals() { _ = ` + "`UPDATE incidents SET status = $2, `" + ` + col + ` + "` = $3, impact = $4`" + ` }
func lowerCase() { _ = ` + "`update incidents set impact = $2 where id = $1`" + ` }
func quotedSet() { _ = ` + "`UPDATE incidents SET \"impact\" = $2 WHERE id = $1`" + ` }
func quotedRow() { _ = ` + "`UPDATE \"incidents\" SET (status, \"impact\") = ($2, $3) WHERE id = $1`" + ` }
func quotedInsert() { _ = ` + "`INSERT INTO incidents (project_id, \"impact\") VALUES ($1, $2)`" + ` }
func upsert() {
	_ = ` + "`INSERT INTO incidents (id, project_id, impact) VALUES ($1, $2, $3)\n\t\t ON CONFLICT (id) DO UPDATE SET status = EXCLUDED.status, impact = EXCLUDED.impact`" + `
}
func upsertQuoted() {
	_ = ` + "`insert into incidents (id, project_id) values ($1, $2) on conflict (id) do update set \"impact\" = 'critical'`" + `
}
func upsertRow() {
	_ = ` + "`INSERT INTO incidents (id, project_id) VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET (status, impact) = ('open', 'major')`" + `
}
func upsertNoImpact() {
	_ = ` + "`INSERT INTO incidents (id, project_id) VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET status = EXCLUDED.status`" + `
}
func (s *Store) method() { _ = ` + "`UPDATE incidents SET impact = $2 WHERE id = $1`" + ` }
var pkgUpdate = ` + "`UPDATE incidents SET impact = $2 WHERE id = $1`" + `
func basisOnly() { _ = ` + "`UPDATE incidents SET impact_basis = $2 WHERE id = $1`" + ` }
func basisQuoted() { _ = ` + "`UPDATE incidents SET \"impact_basis\" = $2 WHERE id = $1`" + ` }
func basisRow() { _ = ` + "`UPDATE incidents SET (status, impact_basis) = ($2, $3) WHERE id = $1`" + ` }
func seqOnly() { _ = ` + "`UPDATE incidents SET event_seq = event_seq + 1 WHERE id = $1`" + ` }
func updates() { _ = ` + "`INSERT INTO incident_updates (incident_id, body) VALUES ($1, $2)`" + ` }
func otherUpsert() {
	_ = ` + "`INSERT INTO service_impacts (id) VALUES ($1) ON CONFLICT (id) DO UPDATE SET impact = EXCLUDED.impact`" + `
}
type row struct {
	Impact string ` + "`json:\"INSERT INTO incidents\"`" + `
}
`
	got := impactWriterSites(parseImpactFixture(t, "store/fixture.go", violation))
	want := []string{
		"store/fixture.go:(*Store).method:update",
		"store/fixture.go:<package>:pkgUpdate:update",
		"store/fixture.go:lowerCase:update",
		"store/fixture.go:newPath:insert",
		"store/fixture.go:qualifiedInsert:insert",
		"store/fixture.go:qualifiedUpdate:update",
		"store/fixture.go:quotedInsert:insert",
		"store/fixture.go:quotedRow:update",
		"store/fixture.go:quotedSet:update",
		"store/fixture.go:rowConstructor:update",
		"store/fixture.go:setImpact:update",
		"store/fixture.go:splitAcrossLiterals:update",
		// An upsert inserts an incident AND, in its conflict arm, updates an existing one's impact.
		"store/fixture.go:upsert:insert", "store/fixture.go:upsert:update",
		"store/fixture.go:upsertNoImpact:insert",
		"store/fixture.go:upsertQuoted:insert", "store/fixture.go:upsertQuoted:update",
		"store/fixture.go:upsertRow:insert", "store/fixture.go:upsertRow:update",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the writer scan did not report exactly the violations:\n  got  %v\n  want %v", got, want)
	}
	for _, sql := range []string{
		"UPDATE incidents SET impact_basis = $2",
		`UPDATE incidents SET "impact_basis" = $2`,
		"UPDATE incidents SET event_seq = event_seq + 1",
		"INSERT INTO incident_updates (incident_id, body) VALUES ($1, $2) ON CONFLICT DO UPDATE SET impact = 1",
	} {
		if kinds := incidentImpactWrites(sql); len(kinds) != 0 {
			t.Errorf("%q does not write incidents.impact, but the matcher says %v", sql, kinds)
		}
	}

	// A function allowed to INSERT gains no right to UPDATE: the creating paths, each adding an
	// update of impact (plain, and an upsert's conflict arm), and the rise step adding an insert.
	const creators = `package store
func (s *Store) createIncident() {
	_ = ` + "`INSERT INTO incidents (project_id, impact) VALUES ($1, $2)`" + `
	_ = ` + "`UPDATE incidents SET impact = $2 WHERE id = $1`" + `
}
func (s *Store) OpenServiceIncidentTx() {
	_ = ` + "`INSERT INTO incidents (id, impact) VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET impact = EXCLUDED.impact`" + `
}
func raiseServiceIncidentImpactTx() {
	_ = ` + "`UPDATE incidents SET impact = $2 WHERE id = $1`" + `
	_ = ` + "`INSERT INTO incidents (project_id, impact) VALUES ($1, $2)`" + `
}
`
	for _, rel := range []string{"store/incidents.go", "store/serviceincidents.go"} {
		got := impactWriterSites(parseImpactFixture(t, rel, creators))
		var bad []string
		for _, s := range unallowedImpactWriters(got) {
			bad = append(bad, strings.TrimPrefix(s, rel+":"))
		}
		var want string
		switch rel {
		case "store/incidents.go":
			// Only createIncident's insert is allowed in this file.
			want = "(*Store).OpenServiceIncidentTx:insert,(*Store).OpenServiceIncidentTx:update," +
				"(*Store).createIncident:update,raiseServiceIncidentImpactTx:insert,raiseServiceIncidentImpactTx:update"
		case "store/serviceincidents.go":
			want = "(*Store).OpenServiceIncidentTx:update,(*Store).createIncident:insert," +
				"(*Store).createIncident:update,raiseServiceIncidentImpactTx:insert"
		}
		if strings.Join(bad, ",") != want {
			t.Errorf("%s: the writer scan did not hold each writer to its kind:\n  unallowed %v\n  want      %s\n  all %v",
				rel, bad, want, got)
		}
	}
}

// impactReaderAllowList is every scope that may read an incident's impact, by rule, with the
// reason. Each is the creating path, rendering, audit, the rise step or the domain type; no SLO,
// budget, burn, gate, coverage, delegation, suppression or escalation computation is here, nor may
// one be added. A scope allowed `incidentColumns` reads the whole row for a caller and may not
// also name `impact` in its SQL — a predicate or an ORDER BY on impact there is a computation.
//
// The incidentColumns entries are ROW reads: the scope selects or returns the whole incident for
// its caller and decides nothing on impact itself. What a caller then does with `.Impact` is scanned
// in the caller, under the selector rule.
var impactReaderAllowList = map[string]string{
	// Domain type.
	"domain/incident.go:Incident.Validate:selector": "domain type: an impact must be valid (R1)",

	// Creating path.
	"api/handlers_incidents.go:(*Handler).createIncident:selector":             "creating path: the manual create's default impact and its log line",
	"store/incidents.go:(*Store).CreateIncidentByPrincipal:selector":           "creating path: the principal door refuses an empty impact",
	"store/incidents.go:(*Store).CreateIncidentBySystem:selector":              "creating path: the system door refuses a supplied impact",
	"store/incidents.go:(*Store).createIncident:selector":                      "creating path: the INSERT's impact parameter",
	"store/incidents.go:(*Store).createIncident:sql-literal":                   "creating path: the INSERT names impact (and reads default_incident_impact)",
	"store/incidents.go:(*Store).createIncident:incidentColumns":               "creating path: RETURNING the created row",
	"store/serviceincidents.go:(*Store).OpenServiceIncidentTx:sql-literal":     "creating path: the service open's INSERT at mapping[state]",
	"store/serviceincidents.go:(*Store).OpenServiceIncidentTx:incidentColumns": "creating path: RETURNING the opened row",

	// Rise step.
	"store/serviceincidents.go:raiseServiceIncidentImpactTx:sql-literal":     "rise step: reads the current impact and basis, writes the raised impact",
	"store/serviceincidents.go:raiseServiceIncidentImpactTx:incidentColumns": "rise step: the raised row for its event",

	// Audit.
	"store/incidentaudit.go:IncidentCreateTarget:selector": "audit: the create target names the impact",

	// Rendering.
	"api/handlers_feed.go:(*Handler).buildFeed:selector": "rendering: the feed entry's summary",
	"subscribe/subscribe.go:render:selector":             "rendering: subscriber mail",

	// The row scan and the shared column lists.
	"store/incidents.go:scanIncident:selector":                                  "row scan: &inc.Impact",
	"store/incidents.go:scanIncidentWithFlag:selector":                          "row scan: &inc.Impact",
	"store/incidents.go:<package>:incidentColumns:incidentColumns":              "the column list's own declaration",
	"store/statuspagecontext.go:<package>:incidentColumnsI:incidentColumns":     "the alias-qualified column list's own declaration, built from incidentColumns",
	"store/statuspagecontext.go:<package>:pastIncidentsPageSQL:incidentColumns": "row read for the public status page's past incidents (rendering)",
	"store/statuspagecontext.go:<package>:historyMonthSQL:incidentColumns":      "row read for the public incident history, first page (rendering)",
	"store/statuspagecontext.go:<package>:historyMonthAfterSQL:incidentColumns": "row read for the public incident history, later pages (rendering)",

	// Row reads (no predicate, no ordering on impact).
	"store/incidents.go:(*Store).FindOpenAutoIncidentByMonitor:incidentColumns":        "row read for a caller",
	"store/incidents.go:(*Store).FindOpenIncidentByExternalKey:incidentColumns":        "row read for a caller (Alertmanager dedupe by key)",
	"store/incidents.go:(*Store).GetIncident:incidentColumns":                          "row read for a caller (detail rendering)",
	"store/incidents.go:(*Store).IncidentByRef:incidentColumns":                        "row read for a caller (the /i/:ref resolver, FR-041a D21)",
	"store/incidents.go:(*Store).ListIncidentsByProject:incidentColumns":               "row read for a caller (list rendering)",
	"store/incidents.go:(*Store).acknowledgeIncident:incidentColumns":                  "row read: RETURNING the acknowledged row",
	"store/incidents.go:(*Store).addIncidentUpdate:incidentColumns":                    "row read: RETURNING the updated row",
	"store/serviceincidents.go:(*Store).FindOpenAutoIncidentByService:incidentColumns": "row read for a caller (the gate's open-incident predicate is on existence, not impact)",
	"store/serviceincidents.go:resolveServiceIncidentTx:incidentColumns":               "row read: RETURNING the resolved row",
	"store/statuspage.go:(*Store).ListOpenIncidentsByProject:incidentColumns":          "row read for the public status page (rendering)",
	"store/statuspagecontext.go:(*Store).incidentsByPredicate:incidentColumns":         "row read for the public status-page context (rendering)",
}

// unallowedImpactReaders is the part of a reader scan the allow-list does not cover.
func unallowedImpactReaders(sites []string) []string {
	var out []string
	for _, s := range sites {
		if _, ok := impactReaderAllowList[s]; !ok {
			out = append(out, s)
		}
	}
	return out
}

// NFR-035 — the exhaustive list of scopes that read an incident's impact, and how.
func TestNothingButCreationRenderingAuditAndTheRiseReadsIncidentImpact(t *testing.T) {
	got := impactReaderSites(parseInternalTree(t))
	want := make([]string, 0, len(impactReaderAllowList))
	for site := range impactReaderAllowList {
		want = append(want, site)
	}
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the readers of incident impact changed (NFR-035):\n  not allowed %v\n  got  %v\n  want %v\n"+
			"impact is a classification, not a control: only creation, rendering, audit, the rise "+
			"step and the domain type may read it", unallowedImpactReaders(got), got, want)
	}
}

func TestTheImpactReaderScanSeesAViolation(t *testing.T) {
	const violation = `package gate
func budget(inc domain.Incident) bool { return inc.Impact == "critical" }
`
	if got := impactReaderSites(parseImpactFixture(t, "gate/fixture.go", violation)); strings.Join(got, ",") != "gate/fixture.go:budget:selector" {
		t.Fatalf("the reader scan did not report the selector read: %v", got)
	}

	// An ALLOWED function that gains an impact predicate: the gate's incident lookup, allowed to read
	// the row through incidentColumns, filtering on impact in the WHERE of the same `+` chain.
	const predicate = `package store
func (s *Store) FindOpenAutoIncidentByService(ctx context.Context, q dbConn, serviceID string) (domain.Incident, error) {
	row := q.QueryRow(ctx,
		` + "`SELECT `" + `+incidentColumns+` + "`" + `
		   FROM incidents
		  WHERE service_id = $1 AND source = 'auto' AND status <> 'resolved' AND impact <> 'minor'
		  ORDER BY started_at DESC LIMIT 1` + "`" + `, serviceID)
	return scanIncident(row)
}
`
	got := impactReaderSites(parseImpactFixture(t, "store/serviceincidents.go", predicate))
	if bad := unallowedImpactReaders(got); strings.Join(bad, ",") != "store/serviceincidents.go:(*Store).FindOpenAutoIncidentByService:sql-literal" {
		t.Fatalf("the reader scan did not report an impact predicate in an allowed function: unallowed %v (all %v)", bad, got)
	}

	// Two methods of one name on two receivers are two scopes: the allowed Incident.Validate does
	// not cover an IncidentUpdate.Validate that starts reading impact, nor a (*Gate).Validate.
	const receivers = `package domain
func (i Incident) Validate() error { return checkImpact(i.Impact) }
func (u IncidentUpdate) Validate() error { return checkImpact(u.Impact) }
func (g *Gate) Validate(inc Incident) bool { return inc.Impact != "critical" }
`
	got = impactReaderSites(parseImpactFixture(t, "domain/incident.go", receivers))
	if bad := unallowedImpactReaders(got); strings.Join(bad, ",") != "domain/incident.go:(*Gate).Validate:selector,domain/incident.go:IncidentUpdate.Validate:selector" {
		t.Fatalf("the reader scan did not tell methods of one name apart by receiver: unallowed %v (all %v)", bad, got)
	}

	// Two package-level declarations are two scopes: an allowed column-list declaration does not
	// cover a new package-level statement in the same file that reads the row.
	const pkgDecls = `package store
var incidentColumnsI = qualifyColumns("i", incidentColumns)
var gateIncidentsSQL = ` + "`SELECT `" + ` + incidentColumnsI + ` + "` FROM incidents i WHERE i.service_id = $1`" + `
`
	got = impactReaderSites(parseImpactFixture(t, "store/statuspagecontext.go", pkgDecls))
	if bad := unallowedImpactReaders(got); strings.Join(bad, ",") != "store/statuspagecontext.go:<package>:gateIncidentsSQL:incidentColumns" {
		t.Fatalf("the reader scan did not tell package-level declarations apart: unallowed %v (all %v)", bad, got)
	}

	for name, src := range map[string]string{
		"sql":             "package x\nfunc f() { _ = `SELECT id, impact FROM incidents` }\n",
		"incidentColumns": "package x\nfunc f() { _ = `SELECT ` + incidentColumns + ` FROM incidents` }\n",
		// The prefix: the status page's own column list.
		"incidentColumnsI": "package x\nfunc f() { _ = `SELECT ` + incidentColumnsI + ` FROM incidents i` }\n",
		// A predicate fragment kept in its own constant, with no verb.
		"fragment": "package x\nconst openCritical = ` AND impact = 'critical'`\n",
		// Keywords in any case.
		"lowerCase":  "package x\nfunc f() { _ = `select id from incidents where impact <> 'minor'` }\n",
		"mixedCase":  "package x\nfunc f() { _ = `Select id From incidents Where Impact = 'critical'` }\n",
		"lowerOrder": "package x\nconst byImpact = ` order by impact desc`\n",
		// ORDER/GROUP and BY split by any whitespace, in a fragment with no other keyword.
		"orderTwoSpaces": "package x\nconst byImpact = ` ORDER  BY impact DESC`\n",
		"orderNewline":   "package x\nconst byImpact = ` ORDER\n BY impact DESC`\n",
		"groupNewline":   "package x\nconst byImpact = ` group\n\tby impact`\n",
		// The quoted column.
		"quoted":          "package x\nfunc f() { _ = `SELECT \"impact\" FROM incidents` }\n",
		"quotedPredicate": "package x\nconst p = ` AND \"impact\" <> 'minor'`\n",
		"qualified":       "package x\nconst p = ` AND i.impact <> 'minor'`\n",
		// Pattern matches and a collation, right of the column.
		"regexMatch":   "package x\nfunc f() { _ = `SELECT id FROM incidents WHERE impact ~ 'crit'` }\n",
		"regexMatchCI": "package x\nconst p = ` AND impact ~* 'CRIT'`\n",
		"regexNot":     "package x\nconst p = ` AND impact !~ 'minor'`\n",
		"regexNotCI":   "package x\nconst p = ` AND impact !~* 'MINOR'`\n",
		"collate":      "package x\nfunc f() { _ = `SELECT id FROM incidents ORDER BY impact COLLATE \"C\"` }\n",
		// An IS test in a fragment kept in its own constant, with no statement shape.
		"isNull":         "package x\nconst p = ` AND impact IS NULL`\n",
		"isNotNull":      "package x\nconst p = ` AND impact IS NOT NULL`\n",
		"isTrue":         "package x\nconst p = ` OR impact IS TRUE`\n",
		"isDistinctFrom": "package x\nconst p = `WHERE impact IS DISTINCT FROM $1`\n",
		"isNotNullLower": "package x\nconst p = ` and impact is not null`\n",
		// A set, pattern or range test in a fragment kept in its own constant, with no statement
		// shape: the operator is followed by SQL-shaped material.
		"in":              "package x\nconst p = ` AND impact IN ('major','critical')`\n",
		"notIn":           "package x\nconst p = ` AND impact NOT IN ($1)`\n",
		"inChain":         "package x\nconst p = ` AND impact IN (` + list + `)`\n",
		"like":            "package x\nconst p = ` OR impact LIKE 'crit%'`\n",
		"iLike":           "package x\nconst p = ` AND impact ILIKE $2`\n",
		"notLikeChain":    "package x\nconst p = ` AND impact NOT LIKE ` + pattern\n",
		"between":         "package x\nconst p = ` AND impact BETWEEN $1 AND $2`\n",
		"betweenLiterals": "package x\nconst p = ` AND impact NOT BETWEEN 'major' AND 'minor'`\n",
		"inLower":         "package x\nconst p = ` and impact in ('major')`\n",
		"likeLower":       "package x\nconst p = ` or impact ilike ?`\n",
		"betweenLower":    "package x\nconst p = ` and impact between 1 and 3`\n",
	} {
		if got := impactReaderSites(parseImpactFixture(t, "x/"+name+".go", src)); len(got) == 0 {
			t.Errorf("%s: the reader scan did not report the read", name)
		}
	}

	// Not reads: a composite-literal key is a write, `impact_basis` and `incident_impacts` are other
	// columns, prose is not SQL even with an SQL keyword in it, and a struct tag is not SQL.
	const clean = `package x
func f() {
	_ = domain.Incident{Impact: "major"}
	_ = ` + "`SELECT impact_basis FROM incidents`" + `
	_ = ` + "`select \"impact_basis\" from incidents`" + `
	_ = ` + "`SELECT 1 FROM incident_impacts`" + `
	_ = "select the impact you want"
	_ = "the impact of this order by the operator"
	_ = "Impact: set by the service mapping, and raised only by the rise step"
	_ = errors.New("invalid impact: want none, minor, major or critical")
	_ = fmt.Errorf("store: raise impact from %s to %s: %w", from, to, err)
	_ = fmt.Sprintf("impact %q is not a valid impact for this order", v)
	_ = "severity, impact and status are shown where the incident is"
	_ = "Resolved automatically. Impact was raised from minor to major"
	_ = "Raised from minor. impact now major"
	_ = "title, impact, status and body are required"
	_ = "when impact is critical, page the on-call"
	_ = "set by the mapping; and impact is not a control"
	_ = "and impact is not a control"
	_ = "impact in the UI"
	_ = "the impact like a badge"
	_ = "impact between releases"
	_ = "when impact is in doubt"
	_ = "show the impact in the UI and on the status page"
	_ = "render the impact like a badge, or as plain text"
	_ = "and impact between releases is not compared"
	_ = "where impact in practice is minor, or like 'none'"
	_ = "or impact between minor and major"
}
type row struct {
	Impact string ` + "`json:\"SELECT impact\"`" + `
}
`
	// Each prose line of the fixture on its own, so a failure names the string that fired.
	for _, line := range strings.Split(clean, "\n") {
		if q := strings.Index(line, `"`); strings.HasPrefix(line, "\t_ = ") && q >= 0 && !strings.Contains(line, "`") {
			if prose, err := strconv.Unquote(line[q : strings.LastIndex(line, `"`)+1]); err == nil && readsImpactSQL(prose) {
				t.Errorf("prose %q reads as SQL naming impact", prose)
			}
		}
	}
	if got := impactReaderSites(parseImpactFixture(t, "x/clean.go", clean)); len(got) != 0 {
		t.Fatalf("the reader scan reported a file that does not read impact: %v", got)
	}
}
