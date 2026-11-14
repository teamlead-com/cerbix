package appmux

// S3 (sec-hardening-2 H-3), route by route, on PostgreSQL: the status-page reads follow the D-0212
// two-step and the any-membership predicate; the unlisted token reaches org managers only; the
// adjacent reads keep one contract per route; and a project-only admin, editor and viewer still
// sign in and load the SPA workspace.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

type pageView struct {
	ID               string  `json:"id"`
	UnlistedToken    *string `json:"unlisted_token"`
	HasUnlistedToken *bool   `json:"has_unlisted_token"`
}

// statusPageReads are every read of one page that the authenticated SPA makes.
func (w *world) statusPageReads() []string {
	base := "/api/v1/status-pages/" + w.page
	return []string{
		"/api/v1/organizations/" + w.org + "/status-pages",
		base, base + "/render", base + "/history", base + "/feed", base + "/components",
	}
}

func TestStatusPageReadsAreTheTwoStepAndTheTokenIsForManagersOnly(t *testing.T) {
	w := newWorld(t)

	// In the org, without the action → 403 on every read; outside the org → 404.
	narrowed := withBearer(w.token("editor", "", []string{"project:read"}))
	for _, path := range w.statusPageReads() {
		if rec := w.do(http.MethodGet, path, nil, narrowed); rec.Code != http.StatusForbidden {
			t.Errorf("token [project:read]: GET %s = %d, want 403: %s", path, rec.Code, rec.Body.String())
		}
		if rec := w.do(http.MethodGet, path, nil, withCookie(w.outsiderSess)); rec.Code != http.StatusNotFound {
			t.Errorf("member of another org: GET %s = %d, want 404", path, rec.Code)
		}
	}

	// A token that lists the new action reads the pages — without the token in clear.
	var listed []pageView
	readerTok := w.token("viewer", "", []string{"status_page:read"}) // 201 only once the action exists
	w.must(w.do(http.MethodGet, "/api/v1/organizations/"+w.org+"/status-pages", nil, withBearer(readerTok)), 200, &listed, "token [status_page:read] lists pages")
	for _, path := range w.statusPageReads() {
		if rec := w.do(http.MethodGet, path, nil, withBearer(readerTok)); rec.Code != http.StatusOK {
			t.Errorf("token [status_page:read]: GET %s = %d, want 200: %s", path, rec.Code, rec.Body.String())
		}
	}

	type reader struct {
		name      string
		opt       reqOpt
		seesToken bool
	}
	readers := []reader{
		{"global admin", withCookie(w.adminSess), true},
		{"org admin", withCookie(w.orgAdminSess), true},
		{"org editor", withCookie(w.orgEditorSess), false},
		{"project admin (project-only)", withCookie(w.projAdminSess), false},
		{"project editor (project-only)", withCookie(w.projEditorSess), false},
		{"project viewer (project-only)", withCookie(w.projViewerSess), false},
		{"token [status_page:read]", withBearer(readerTok), false},
		{"org admin token, role decides", withBearer(w.token("org_admin", "", nil)), true},
	}
	for _, rd := range readers {
		var list []pageView
		w.must(w.do(http.MethodGet, "/api/v1/organizations/"+w.org+"/status-pages", nil, rd.opt), 200, &list, rd.name+" lists pages")
		var one pageView
		w.must(w.do(http.MethodGet, "/api/v1/status-pages/"+w.page, nil, rd.opt), 200, &one, rd.name+" reads the page")
		if len(list) != 1 {
			t.Fatalf("%s: listed %d pages, want 1", rd.name, len(list))
		}
		for where, v := range map[string]pageView{"list": list[0], "get": one} {
			got := ""
			if v.UnlistedToken != nil {
				got = *v.UnlistedToken
			}
			switch {
			case rd.seesToken && got != w.pageToken:
				t.Errorf("%s (%s): unlisted_token = %q, want the page's token", rd.name, where, got)
			case !rd.seesToken && got != "":
				t.Errorf("%s (%s): received the unlisted token without org:manage", rd.name, where)
			}
			if v.HasUnlistedToken == nil || !*v.HasUnlistedToken {
				t.Errorf("%s (%s): has_unlisted_token = %v, want true", rd.name, where, v.HasUnlistedToken)
			}
		}
	}
}

func TestAdjacentReadsKeepOneContractPerRoute(t *testing.T) {
	w := newWorld(t)
	org := "/api/v1/organizations/" + w.org
	project := "/api/v1/projects/" + w.project
	sibling := "/api/v1/projects/" + w.sibling

	// A token whose list names no read: in the org, so 403 — never the data, never a 500.
	writeOnly := withBearer(w.token("org_admin", "", []string{"change:record"}))
	for _, path := range []string{project, org + "/projects", org, "/api/v1/me", "/api/v1/events"} {
		if rec := w.do(http.MethodGet, path, nil, writeOnly); rec.Code != http.StatusForbidden {
			t.Errorf("token [change:record]: GET %s = %d, want 403: %s", path, rec.Code, rec.Body.String())
		}
	}
	var orgs []struct{ ID string }
	w.must(w.do(http.MethodGet, "/api/v1/organizations", nil, writeOnly), 200, &orgs, "token [change:record] lists organizations")
	if len(orgs) != 0 {
		t.Errorf("token [change:record] listed organizations %+v, want none", orgs)
	}

	// A project-scoped token with the role in charge: its project, its org, nothing else.
	scoped := withBearer(w.token("viewer", w.project, nil))
	w.must(w.do(http.MethodGet, project, nil, scoped), 200, nil, "scoped token reads its project")
	w.must(w.do(http.MethodGet, org, nil, scoped), 200, nil, "scoped token reads its org")
	if rec := w.do(http.MethodGet, sibling, nil, scoped); rec.Code != http.StatusNotFound {
		t.Errorf("scoped token: GET sibling project = %d, want 404", rec.Code)
	}
	var projects []struct{ ID string }
	w.must(w.do(http.MethodGet, org+"/projects", nil, scoped), 200, &projects, "scoped token lists projects")
	if len(projects) != 1 || projects[0].ID != w.project {
		t.Errorf("scoped token listed projects %+v, want only its own", projects)
	}
	w.must(w.do(http.MethodGet, "/api/v1/organizations", nil, scoped), 200, &orgs, "scoped token lists organizations")
	if len(orgs) != 1 || orgs[0].ID != w.org {
		t.Errorf("scoped token listed organizations %+v, want only its own", orgs)
	}
	if rec := w.do(http.MethodGet, "/api/v1/me", nil, scoped); rec.Code != http.StatusForbidden {
		t.Errorf("token: GET /api/v1/me = %d, want 403 (it describes a user)", rec.Code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if rec := w.do(http.MethodGet, "/api/v1/events", nil, scoped, withContext(ctx)); rec.Code != http.StatusOK {
		t.Errorf("token with project:read: GET /api/v1/events = %d, want 200 (the stream)", rec.Code)
	}

	// A token naming org:read but not project:read reads the organization and lists no project.
	orgReader := withBearer(w.token("editor", "", []string{"org:read"}))
	w.must(w.do(http.MethodGet, org, nil, orgReader), 200, nil, "token [org:read] reads its org")
	if rec := w.do(http.MethodGet, org+"/projects", nil, orgReader); rec.Code != http.StatusForbidden {
		t.Errorf("token [org:read]: GET projects = %d, want 403", rec.Code)
	}

	// Outside the org: 404 on all three.
	for _, path := range []string{project, org + "/projects", org} {
		if rec := w.do(http.MethodGet, path, nil, withCookie(w.outsiderSess)); rec.Code != http.StatusNotFound {
			t.Errorf("member of another org: GET %s = %d, want 404", path, rec.Code)
		}
	}
}

// login signs in through the real /auth/local/login and returns the session cookie value.
func (w *world) login(email string) string {
	w.t.Helper()
	body, ct := jsonBody(map[string]string{"username": email, "password": fixturePassword})
	rec := w.do(http.MethodPost, "/auth/local/login", body, ct, withHeader("Origin", "http://cerbix.example.com"))
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent && rec.Code != http.StatusFound {
		w.t.Fatalf("login %s: %d %s", email, rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName && c.Value != "" {
			return c.Value
		}
	}
	w.t.Fatalf("login %s set no session cookie", email)
	return ""
}

func TestProjectOnlyMembersSignInAndLoadTheWorkspace(t *testing.T) {
	w := newWorld(t)
	for _, email := range []string{w.projAdminEmail, w.projEditorEmail, w.projViewerEmail} {
		sess := withCookie(w.login(email))
		// The SPA's start: who am I, which organizations, which projects (workspace.ts).
		var me struct {
			Memberships []struct {
				ProjectID string `json:"project_id"`
			} `json:"memberships"`
		}
		w.must(w.do(http.MethodGet, "/api/v1/me", nil, sess), 200, &me, email+" /me")
		var orgs []struct{ ID string }
		w.must(w.do(http.MethodGet, "/api/v1/organizations", nil, sess), 200, &orgs, email+" organizations")
		if len(orgs) != 1 || orgs[0].ID != w.org {
			t.Errorf("%s: organizations %+v, want the fixture org", email, orgs)
		}
		var projects []struct{ ID string }
		w.must(w.do(http.MethodGet, "/api/v1/organizations/"+w.org+"/projects", nil, sess), 200, &projects, email+" projects")
		if len(projects) != 1 || projects[0].ID != w.project {
			t.Errorf("%s: projects %+v, want only the member's project", email, projects)
		}
		w.must(w.do(http.MethodGet, "/api/v1/organizations/"+w.org, nil, sess), 200, nil, email+" organization")
		w.must(w.do(http.MethodGet, "/api/v1/projects/"+w.project, nil, sess), 200, nil, email+" project")
		if rec := w.do(http.MethodGet, "/api/v1/projects/"+w.sibling, nil, sess); rec.Code != http.StatusNotFound {
			t.Errorf("%s: sibling project = %d, want 404", email, rec.Code)
		}
		// The org's status pages are an org-level aggregation (OWNER, 2026-11-13): readable, and
		// the unlisted token is not theirs to receive.
		var pages []json.RawMessage
		w.must(w.do(http.MethodGet, "/api/v1/organizations/"+w.org+"/status-pages", nil, sess), 200, &pages, email+" status pages")
		var page pageView
		w.must(w.do(http.MethodGet, "/api/v1/status-pages/"+w.page, nil, sess), 200, &page, email+" status page")
		if page.UnlistedToken != nil && *page.UnlistedToken != "" {
			t.Errorf("%s: received the unlisted token", email)
		}
		if page.HasUnlistedToken == nil || !*page.HasUnlistedToken {
			t.Errorf("%s: has_unlisted_token missing", email)
		}
		w.must(w.do(http.MethodGet, "/api/v1/status-pages/"+w.page+"/render", nil, sess), 200, nil, email+" preview render")
	}
}
