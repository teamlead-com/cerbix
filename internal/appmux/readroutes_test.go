package appmux

// S3 (sec-hardening-2 H-3): the route enumeration. Every GET route of the session/bearer API is
// taken from the syntax tree of api.(*Handler).Router — not from a hand-written list, so a route
// added later is enumerated without anyone remembering to — and driven on PostgreSQL as every kind
// of principal. Two assertions:
//   - no route answers 5xx for any principal (the token's synthetic actor id once reached a uuid
//     column on /me, /organizations and /events and answered 500 — invisible on the fakes);
//   - a token whose allow-list names no read action, and a member of another organization, read
//     NOTHING of the fixture organization from any route but the three tenant-free ones.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// tenantFree is the ENTIRE allow-list of the enumeration (H-3 fix 5): reads open to any
// authenticated principal by design.
var tenantFree = map[string]bool{
	"GET /api/v1/version":          true,
	"GET /api/v1/regions":          true,
	"GET /api/v1/monitor-defaults": true,
}

// readRoutes returns every "GET …" pattern registered in api.(*Handler).Router.
func readRoutes(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../api/api.go", nil, 0)
	if err != nil {
		t.Fatalf("parse api.go: %v", err)
	}
	consts := map[string]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if i < len(vs.Values) {
					if bl, ok := vs.Values[i].(*ast.BasicLit); ok && bl.Kind == token.STRING {
						consts[n.Name], _ = strconv.Unquote(bl.Value)
					}
				}
			}
		}
	}
	var routes []string
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "Router" {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
				return true
			}
			var pattern string
			switch a := call.Args[0].(type) {
			case *ast.BasicLit:
				pattern, _ = strconv.Unquote(a.Value)
			case *ast.Ident:
				pattern = consts[a.Name]
			}
			if pattern == "" {
				t.Fatalf("Router registers a route whose pattern the enumeration cannot read at %s", fset.Position(call.Pos()))
			}
			if strings.HasPrefix(pattern, "GET ") {
				routes = append(routes, pattern)
			}
			return true
		})
	}
	if len(routes) < 50 {
		t.Fatalf("found %d GET routes in Router — the enumeration is not reading what it should", len(routes))
	}
	sort.Strings(routes)
	return routes
}

var pathParam = regexp.MustCompile(`\{([A-Za-z]+)\}`)

// fill substitutes the fixture's ids for path parameters; an id the fixture does not seed gets a
// well-formed uuid that names nothing.
func (w *world) fill(pattern string) string {
	path := strings.TrimPrefix(pattern, "GET ")
	known := map[string]string{
		"orgID": w.org, "projectID": w.project, "monitorID": w.monitor, "serviceID": w.service,
		"incidentID": w.incident, "pageID": w.page, "componentID": w.component,
	}
	path = pathParam.ReplaceAllStringFunc(path, func(m string) string {
		if id, ok := known[m[1:len(m)-1]]; ok {
			return id
		}
		return "00000000-0000-4000-8000-00000000abcd"
	})
	// A search that matches the seeded monitor ("<marker>-mon") without echoing an identifier.
	if strings.HasSuffix(path, "/search") {
		path += "?q=mon"
	}
	return path
}

// get drives one GET route; the SSE stream is cut after a short while.
func (w *world) get(pattern string, opts ...reqOpt) (code int, body string) {
	w.t.Helper()
	path := w.fill(pattern)
	if pattern == "GET /api/v1/events" {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		opts = append(opts, withContext(ctx))
	}
	rec := w.do(http.MethodGet, path, nil, opts...)
	return rec.Code, rec.Body.String()
}

func TestEveryReadRouteAnswersWithoutAServerErrorForEveryPrincipal(t *testing.T) {
	w := newWorld(t)
	principals := []struct {
		name string
		opt  reqOpt
	}{
		{"global admin session", withCookie(w.adminSess)},
		{"org admin session", withCookie(w.orgAdminSess)},
		{"org editor session", withCookie(w.orgEditorSess)},
		{"project admin session", withCookie(w.projAdminSess)},
		{"project editor session", withCookie(w.projEditorSess)},
		{"project viewer session", withCookie(w.projViewerSess)},
		{"outsider session", withCookie(w.outsiderSess)},
		{"org admin token, role decides", withBearer(w.token("org_admin", "", nil))},
		{"project viewer token, role decides", withBearer(w.token("viewer", w.project, nil))},
		{"editor token [change:record]", withBearer(w.token("editor", "", []string{"change:record"}))},
		{"editor token [project:read]", withBearer(w.token("editor", "", []string{"project:read"}))},
		{"org admin token [gate:evaluate]", withBearer(w.token("org_admin", "", []string{"gate:evaluate"}))},
	}
	routes := readRoutes(t)
	for _, p := range principals {
		for _, route := range routes {
			code, body := w.get(route, p.opt)
			if code >= 500 {
				t.Errorf("%s: %s → %d %s", p.name, route, code, strings.TrimSpace(body))
			}
		}
	}
}

// statusPageRoutes are the authenticated status-page reads (H-3 fixes 2–3: status_page:read
// through any membership of the org).
var statusPageRoutes = map[string]bool{
	"GET /api/v1/organizations/{orgID}/status-pages": true,
	"GET /api/v1/status-pages/{pageID}":              true,
	"GET /api/v1/status-pages/{pageID}/render":       true,
	"GET /api/v1/status-pages/{pageID}/history":      true,
	"GET /api/v1/status-pages/{pageID}/feed":         true,
	"GET /api/v1/status-pages/{pageID}/components":   true,
}

// readerCase is one principal of the enumeration: which routes may answer 2xx (each with the
// clause that allows it), which identifiers must never appear in a 2xx body, and whether every
// other route must answer 403 or 404.
type readerCase struct {
	name    string
	opt     reqOpt
	allowed map[string]string // route → the spec clause that lets it answer 2xx
	strict  bool              // every route outside tenantFree and allowed answers 403 or 404
	secrets func(route string) []string
}

func TestNoReadRouteServesWhatTheAllowListOmitsOrAnotherTenantOwns(t *testing.T) {
	w := newWorld(t)
	all := func(string) []string { return w.ids() }
	siblingReader := w.token("editor", w.sibling, []string{"project:read"})
	readers := []readerCase{
		{
			// An org admin's token narrowed to a write: the role would grant every read, the list none.
			name: "org admin token [change:record]", opt: withBearer(w.token("org_admin", "", []string{"change:record"})),
			allowed: map[string]string{
				"GET /api/v1/organizations": "H-3 fix 5: a token that may read no organization gets an empty list",
			},
			strict: true, secrets: all,
		},
		{
			name: "member of another organization", opt: withCookie(w.outsiderSess),
			allowed: map[string]string{
				"GET /api/v1/organizations": "its own organization (H-3 fix 5: list by membership)",
				"GET /api/v1/me":            "describes the caller (H-3 fix 5)",
				"GET /api/v1/search":        "searches the caller's own scope (VisibleScope)",
				"GET /api/v1/events":        "streams the caller's own scope (H-3 fix 5: VisibleScope)",
			},
			strict: true, secrets: all,
		},
		{
			name: "viewer token [status_page:read]", opt: withBearer(w.token("viewer", "", []string{"status_page:read"})),
			allowed: func() map[string]string {
				m := map[string]string{"GET /api/v1/organizations": "H-3 fix 5: no org:read or project:read in the list — an empty list"}
				for r := range statusPageRoutes {
					m[r] = "H-3 fixes 2–3: status_page:read is listed"
				}
				return m
			}(),
			strict: true,
			// The page itself is what it reads, never its token (H-3 fix 4: no org:manage); every
			// other route — /organizations included, which must be [] — serves no fixture id.
			secrets: func(route string) []string {
				if statusPageRoutes[route] {
					return []string{w.pageToken}
				}
				return w.ids()
			},
		},
		{
			// A project-scoped [project:read] token of the SIBLING project, driven at the fixture
			// project's ids: it may read its org and its own project list and nothing of the other
			// project. (Its own-project gate routes are checked below: the editor role grants
			// gate:evaluate, the list cuts it.)
			name: "sibling-project editor token [project:read]", opt: withBearer(siblingReader),
			allowed: map[string]string{
				"GET /api/v1/organizations":                  "H-3 fix 5: its own organization (project:read is listed)",
				"GET /api/v1/organizations/{orgID}":          "H-3 fix 5: a token with project:read reads its organization",
				"GET /api/v1/organizations/{orgID}/projects": "H-3 fix 5: filtered to the projects it may read — its own",
				"GET /api/v1/search":                         "its own scope (VisibleScope(project:read))",
				"GET /api/v1/events":                         "H-3 fix 5: its own scope (VisibleScope(project:read))",
			},
			strict: true,
			secrets: func(string) []string {
				return []string{w.project, w.monitor, w.service, w.incident, w.page, w.pageSlug, w.pageToken, w.component}
			},
		},
		{
			// A project-only member of the sibling project: its org, its own project list, its own
			// scope, and the org's status pages (owner, 2026-11-13: an org-level aggregation, which
			// shows every project's incidents) — nothing else of the other project.
			name: "viewer of the sibling project", opt: withCookie(w.siblingSess),
			allowed: func() map[string]string {
				m := map[string]string{
					"GET /api/v1/me":                             "H-3 fix 5: describes the caller",
					"GET /api/v1/organizations":                  "H-3 fix 5: its organization, by membership",
					"GET /api/v1/organizations/{orgID}":          "H-3 fix 5: any human member reads the organization's metadata",
					"GET /api/v1/organizations/{orgID}/projects": "H-3 fix 5: filtered to its own project",
					"GET /api/v1/search":                         "its own scope (VisibleScope)",
					"GET /api/v1/events":                         "H-3 fix 5: its own scope (VisibleScope)",
				}
				for r := range statusPageRoutes {
					m[r] = "H-3 fix 3 (owner, 2026-11-13): project-only members read the org's status pages"
				}
				return m
			}(),
			strict: true,
			secrets: func(route string) []string {
				if statusPageRoutes[route] {
					return []string{w.pageToken}
				}
				return []string{w.project, w.monitor, w.service, w.incident, w.pageToken}
			},
		},
	}
	for _, rd := range readers {
		for _, route := range readRoutes(t) {
			if tenantFree[route] {
				continue
			}
			code, body := w.get(route, rd.opt)
			if code >= 500 {
				t.Errorf("%s: %s → %d", rd.name, route, code)
				continue
			}
			ok := code >= 200 && code < 300
			if rd.strict {
				if _, allowed := rd.allowed[route]; ok && !allowed {
					t.Errorf("%s: %s → %d, want 403 or 404 (no clause lets this reader read it): %.200s", rd.name, route, code, strings.TrimSpace(body))
				}
				if !ok && code != http.StatusForbidden && code != http.StatusNotFound {
					t.Errorf("%s: %s → %d, want 403 or 404", rd.name, route, code)
				}
			}
			if ok {
				for _, id := range rd.secrets(route) {
					if id != "" && strings.Contains(body, id) {
						t.Errorf("%s: %s → %d serves %q: %.200s", rd.name, route, code, id, strings.TrimSpace(body))
					}
				}
			}
		}
	}
	// The sibling [project:read] token on its OWN project's gate routes: the editor role grants
	// gate:evaluate, the list does not — 403, never the ledger or a policy.
	gates := 0
	for _, route := range readRoutes(t) {
		if !strings.Contains(route, "/gate") {
			continue
		}
		gates++
		path := strings.Replace(w.fill(route), "/projects/"+w.project+"/", "/projects/"+w.sibling+"/", 1)
		rec := w.do(http.MethodGet, path, nil, withBearer(siblingReader))
		if rec.Code != http.StatusForbidden {
			t.Errorf("token [project:read] on its own project: %s → %d, want 403 (gate:evaluate is not listed)", route, rec.Code)
		}
	}
	if gates < 5 {
		t.Fatalf("found %d gate read routes — the enumeration is not reading what it should", gates)
	}
	// The [project:read] token is refused every status-page read: the list does not name it.
	tok := withBearer(w.token("editor", "", []string{"project:read"}))
	for route := range statusPageRoutes {
		if code, _ := w.get(route, tok); code != http.StatusForbidden {
			t.Errorf("token [project:read]: %s → %d, want 403", route, code)
		}
	}
}
