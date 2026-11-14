package appmux

// S4 (sec-hardening-2 H-4) on the real composition: a browser-originated unsafe request that is not
// same-origin is refused before any handler runs, with the API's JSON error shape; a request that
// presents a Bearer credential is never authenticated by its cookie; a request authenticated by its
// cookie is never exempt; a JSON body is decoded only under a JSON content type; no GET changes
// state — GET /auth/logout is gone (405).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const (
	sameOrigin = "http://cerbix.example.com" // the Host every request of the harness carries
	sibling    = "https://wiki.example.com"  // the SameSite=Lax gap: a sibling host
	foreign    = "https://evil.example.net"
)

func TestCrossOriginUnsafeRequestsAreRefusedBeforeAnyHandler(t *testing.T) {
	w := newWorld(t)
	tok := w.token("org_admin", "", nil)
	pagePath := "/api/v1/status-pages/" + w.page
	// The body-less route: a DELETE of a monitor that does not exist answers 404 once admitted,
	// so admission (404), refusal (403) and authentication failure (401) are three answers.
	bodyless := "/api/v1/monitors/00000000-0000-4000-8000-00000000abcd"

	type row struct {
		name   string
		body   bool // PATCH the page's title (JSON body) instead of the body-less DELETE
		opts   []reqOpt
		want   int
		denied bool // the refusal must be the forgery guard's JSON 403
	}
	cookie := withCookie(w.orgAdminSess)
	rows := []row{
		// The session cookie, by Sec-Fetch-Site and Origin.
		{"cookie, same-origin by Sec-Fetch-Site", true, []reqOpt{cookie, withHeader("Sec-Fetch-Site", "same-origin"), withHeader("Origin", sameOrigin)}, 200, false},
		{"cookie, Sec-Fetch-Site none (user-initiated)", true, []reqOpt{cookie, withHeader("Sec-Fetch-Site", "none")}, 200, false},
		{"cookie, sibling host (same-site)", true, []reqOpt{cookie, withHeader("Sec-Fetch-Site", "same-site"), withHeader("Origin", sibling)}, 403, true},
		{"cookie, cross-site", true, []reqOpt{cookie, withHeader("Sec-Fetch-Site", "cross-site"), withHeader("Origin", foreign)}, 403, true},
		{"cookie, body-less, sibling host", false, []reqOpt{cookie, withHeader("Sec-Fetch-Site", "same-site"), withHeader("Origin", sibling)}, 403, true},
		{"cookie, no Sec-Fetch-Site, foreign Origin", true, []reqOpt{cookie, withHeader("Origin", foreign)}, 403, true},
		{"cookie, no Sec-Fetch-Site, Origin host = Host (TLS proxy, scheme differs)", true, []reqOpt{cookie, withHeader("Origin", "https://cerbix.example.com")}, 200, false},
		{"cookie, proxy rewrote Host, same-origin by Sec-Fetch-Site", true, []reqOpt{cookie, withHost("cerbix-internal:8080"), withHeader("Sec-Fetch-Site", "same-origin"), withHeader("Origin", "https://cerbix.example.com")}, 200, false},
		// A proxy that does not forward Host (nginx proxy_pass without proxy_set_header Host), as seen
		// over plain HTTP: browsers send Sec-Fetch-* only to potentially trustworthy origins
		// (HTTPS, localhost), so the check falls back to Origin vs Host — and refuses. Over HTTPS
		// the browser's Sec-Fetch-Site: same-origin decides. Pinned behaviour, documented in the
		// runbook; not a defect of the guard.
		{"cookie, plain HTTP behind a Host-rewriting proxy: no Sec-Fetch-Site, Origin ≠ Host", true, []reqOpt{cookie, withHost("cerbix:8080"), withHeader("Origin", "http://cerbix.company.com")}, 403, true},
		{"cookie, HTTPS behind a Host-rewriting proxy: Sec-Fetch-Site same-origin, Origin ≠ Host", true, []reqOpt{cookie, withHost("cerbix:8080"), withHeader("Sec-Fetch-Site", "same-origin"), withHeader("Origin", "http://cerbix.company.com")}, 200, false},
		{"cookie, neither header (curl, the CLI, page.request)", true, []reqOpt{cookie}, 200, false},
		{"cookie, body-less, neither header", false, []reqOpt{cookie}, 404, false},
		// A bearer credential is not ambient: exempt on /api/*, whatever the Origin.
		{"bearer, foreign Origin", true, []reqOpt{withBearer(tok), withHeader("Sec-Fetch-Site", "cross-site"), withHeader("Origin", foreign)}, 200, false},
		{"bearer, body-less, foreign Origin", false, []reqOpt{withBearer(tok), withHeader("Sec-Fetch-Site", "cross-site"), withHeader("Origin", foreign)}, 404, false},
		// A Bearer header disables the cookie: junk, empty or duplicated → 401, never the session.
		{"cookie + Bearer junk, foreign Origin", true, []reqOpt{cookie, withAuthorization("Bearer junk"), withHeader("Origin", foreign)}, 401, false},
		{"cookie + empty Bearer, foreign Origin", true, []reqOpt{cookie, withAuthorization("Bearer "), withHeader("Origin", foreign)}, 401, false},
		{"cookie + bare Bearer scheme, foreign Origin", true, []reqOpt{cookie, withAuthorization("bearer"), withHeader("Origin", foreign)}, 401, false},
		{"cookie + duplicated Bearer, foreign Origin", true, []reqOpt{cookie, withBearer(tok), withBearer(tok), withHeader("Origin", foreign)}, 401, false},
		{"cookie + Bearer junk, body-less, foreign Origin", false, []reqOpt{cookie, withAuthorization("Bearer junk"), withHeader("Origin", foreign)}, 401, false},
		// A proxy's Basic beside the cookie: the cookie authenticates, and is checked.
		{"cookie + Basic, same-origin (the proxy case)", true, []reqOpt{cookie, withAuthorization("Basic dXNlcjpwYXNz"), withHeader("Sec-Fetch-Site", "same-origin"), withHeader("Origin", sameOrigin)}, 200, false},
		{"cookie + Basic, foreign Origin", true, []reqOpt{cookie, withAuthorization("Basic dXNlcjpwYXNz"), withHeader("Sec-Fetch-Site", "cross-site"), withHeader("Origin", foreign)}, 403, true},
		{"cookie + Negotiate, foreign Origin", false, []reqOpt{cookie, withAuthorization("Negotiate YII="), withHeader("Origin", foreign)}, 403, true},
	}
	for _, r := range rows {
		var rec = func() (code int, body, ct string) {
			if r.body {
				resp := w.send(http.MethodPatch, pagePath, map[string]string{"title": "renamed"}, r.opts...)
				return resp.Code, resp.Body.String(), resp.Header().Get("Content-Type")
			}
			resp := w.do(http.MethodDelete, bodyless, nil, r.opts...)
			return resp.Code, resp.Body.String(), resp.Header().Get("Content-Type")
		}
		code, body, ct := rec()
		if code != r.want {
			t.Errorf("%s: %d, want %d: %s", r.name, code, r.want, strings.TrimSpace(body))
			continue
		}
		if r.denied {
			var e struct {
				Error string `json:"error"`
			}
			if !strings.HasPrefix(ct, "application/json") || json.Unmarshal([]byte(body), &e) != nil || e.Error == "" {
				t.Errorf("%s: the refusal is %q %q, want the API's JSON error shape", r.name, ct, body)
			}
		}
	}

	// A safe method is never refused: the SPA's GETs and the EventSource carry any Origin.
	if rec := w.do(http.MethodGet, pagePath, nil, cookie, withHeader("Sec-Fetch-Site", "cross-site"), withHeader("Origin", foreign)); rec.Code != http.StatusOK {
		t.Errorf("cross-site GET = %d, want 200", rec.Code)
	}

	// The bypass patterns: the public router (status pages, subscribe, push) and the agent router
	// reach their own handlers from any Origin — they hold no ambient credential.
	cross := []reqOpt{withHeader("Sec-Fetch-Site", "cross-site"), withHeader("Origin", foreign)}
	pub := w.send(http.MethodPost, "/api/v1/public/status-pages/"+w.pageSlug+"/subscribers?token="+w.pageToken,
		map[string]string{"email": "reader@fixture.test"}, cross...)
	if pub.Code == http.StatusForbidden {
		t.Errorf("cross-site public subscribe = 403 (%s): the public router must bypass the check", pub.Body.String())
	}
	// The agent router authenticates with its own token: 401 from it, not the forgery 403.
	agent := w.send(http.MethodPost, "/api/v1/agent/heartbeat", map[string]string{}, append(cross, withBearer("agent-nope"))...)
	if agent.Code == http.StatusForbidden {
		t.Errorf("cross-site agent request = 403 (%s): the agent router must bypass the check", agent.Body.String())
	}
}

func TestAuthRoutesAreBehindTheForgeryCheckAndLogoutIsPostOnly(t *testing.T) {
	w := newWorld(t)
	tok := w.token("org_admin", "", nil)

	// POST /auth/logout reads the cookie itself, outside RequireAuth: a bearer beside it does not
	// exempt it, and a foreign Origin leaves the session alive.
	_, sess := w.user("logout-a@fixture.test", false)
	rec := w.do(http.MethodPost, "/auth/logout", nil, withCookie(sess), withBearer(tok),
		withHeader("Sec-Fetch-Site", "cross-site"), withHeader("Origin", foreign))
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST /auth/logout with cookie + bearer = %d, want 403", rec.Code)
	}
	if !w.sessionAlive(sess) {
		t.Error("a cross-site POST /auth/logout ended the session")
	}

	// No GET changes state: GET /auth/logout is 405 and the session survives it.
	rec = w.do(http.MethodGet, "/auth/logout", nil, withCookie(sess))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /auth/logout = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Errorf("GET /auth/logout Allow = %q, want POST", allow)
	}
	if !w.sessionAlive(sess) {
		t.Error("GET /auth/logout ended the session")
	}

	// The same-origin POST still signs out.
	rec = w.do(http.MethodPost, "/auth/logout", nil, withCookie(sess), withHeader("Sec-Fetch-Site", "same-origin"), withHeader("Origin", sameOrigin))
	// 200 {"redirect_to": …} since sec-hardening-2 H-9b (it was a 302): the SPA navigates itself.
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" || !strings.Contains(rec.Body.String(), `"redirect_to":"/"`) {
		t.Errorf("same-origin POST /auth/logout = %d %q %q, want 200 {\"redirect_to\":\"/\"}", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if w.sessionAlive(sess) {
		t.Error("a same-origin POST /auth/logout left the session alive")
	}

	// Login CSRF: the unauthenticated /auth/local/* routes are checked too.
	body, ct := jsonBody(map[string]string{"username": "admin@fixture.test", "password": fixturePassword})
	rec = w.do(http.MethodPost, "/auth/local/login", body, ct, withHeader("Sec-Fetch-Site", "cross-site"), withHeader("Origin", foreign))
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST /auth/local/login = %d, want 403", rec.Code)
	}
	if setsCookie := len(rec.Result().Cookies()) > 0; setsCookie {
		t.Error("a cross-site login set a cookie")
	}
}

func TestJSONBodiesNeedAJSONContentType(t *testing.T) {
	w := newWorld(t)
	cookie := withCookie(w.orgAdminSess)
	path := "/api/v1/status-pages/" + w.page
	for _, c := range []struct {
		ct   string
		want int
	}{
		{"", http.StatusUnsupportedMediaType},
		{"text/plain", http.StatusUnsupportedMediaType},
		{"application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
		{"application/json", http.StatusOK},
		{"application/json; charset=utf-8", http.StatusOK},
		{"Application/JSON", http.StatusOK},
	} {
		opts := []reqOpt{cookie}
		if c.ct != "" {
			opts = append(opts, withHeader("Content-Type", c.ct))
		}
		rec := w.do(http.MethodPatch, path, strings.NewReader(`{"title":"t"}`), opts...)
		if rec.Code != c.want {
			t.Errorf("PATCH with Content-Type %q = %d, want %d: %s", c.ct, rec.Code, c.want, rec.Body.String())
		}
	}
	// Body-less mutations need no content type: acknowledge an incident.
	if rec := w.do(http.MethodPost, "/api/v1/incidents/"+w.incident+"/acknowledge", nil, cookie); rec.Code >= 400 {
		t.Errorf("body-less acknowledge = %d: %s", rec.Code, rec.Body.String())
	}
}

// Review of iter-0211, finding 2: the agent router's bypass exists only where the agent router is
// mounted. Without a pull transport, /api/v1/agent/* falls to the session-authenticated /api/
// handler, and a cross-origin request there is checked like any other — never run as the session.
func TestAgentBypassOnlyWhereTheAgentRouterIsMounted(t *testing.T) {
	w := newWorldWith(t, false)
	_, sess := w.user("agent-bypass@fixture.test", false)
	rec := w.send(http.MethodPost, "/api/v1/agent/jobs", map[string]string{}, withCookie(sess),
		withHeader("Sec-Fetch-Site", "cross-site"), withHeader("Origin", foreign))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "cross-origin request refused") {
		t.Errorf("no agent router, cookie, cross-site POST /api/v1/agent/jobs = %d %s, want the forgery 403", rec.Code, rec.Body.String())
	}
	if !w.sessionAlive(sess) {
		t.Error("the session did not survive")
	}
}

// Review of iter-0211, finding 5: the guard judges the path the mux routes — the cleaned ESCAPED
// path — not the decoded one. "/api/%2e%2e%2fauth/logout" decodes to a path that cleans to
// /auth/logout, but the mux routes it under /api/ (its segment is "%2e%2e%2fauth"): a Bearer
// request there must reach RequireAuth, not the forgery refusal. And no escaped spelling lets a
// cookie request run as the session from another origin.
func TestForgeryGuardJudgesThePathTheMuxRoutes(t *testing.T) {
	w := newWorld(t)
	tok := w.token("org_admin", "", nil)
	cross := []reqOpt{withHeader("Sec-Fetch-Site", "cross-site"), withHeader("Origin", foreign)}

	rec := w.do(http.MethodPost, "/api/%2e%2e%2fauth/logout", nil, append([]reqOpt{withBearer(tok)}, cross...)...)
	if strings.Contains(rec.Body.String(), "cross-origin request refused") {
		t.Errorf("Bearer, cross-site, /api/%%2e%%2e%%2fauth/logout: refused by the forgery check (%d), want it to reach RequireAuth", rec.Code)
	}

	_, sess := w.user("escaped@fixture.test", false)
	cookie := withCookie(sess)
	page := "/api%2Fv1/status-pages/" + w.page
	if rec := w.do(http.MethodPatch, page, strings.NewReader(`{"title":"forged"}`), append([]reqOpt{withCookie(w.orgAdminSess), withHeader("Content-Type", "application/json")}, cross...)...); rec.Code < 400 {
		t.Errorf("cookie, cross-site PATCH %s = %d, want refused", page, rec.Code)
	}
	var got struct{ Title string }
	w.must(w.do(http.MethodGet, "/api/v1/status-pages/"+w.page, nil, withCookie(w.adminSess)), 200, &got, "read the page back")
	if got.Title == "forged" {
		t.Error("an escaped /api%2Fv1 path ran a cross-site PATCH as the session")
	}
	for _, opts := range [][]reqOpt{{cookie}, {cookie, withBearer(tok)}} {
		rec := w.do(http.MethodPost, "/api/%2e%2e/auth/logout", nil, append(opts, cross...)...)
		if !w.sessionAlive(sess) {
			t.Fatalf("cross-site POST /api/%%2e%%2e/auth/logout (%d) ended the session", rec.Code)
		}
	}
}

// Second external review of iter-0211, finding 2: the Bearer exemption is decided by asking a
// ServeMux, not by a string predicate that approximates one. "/%61pi/v1/…" is routed by the mux to
// the /api/ handler (it matches on unescaped segments), so a Bearer request there must reach
// RequireAuth and run as the token — while a cookie request on any escaped spelling is still
// checked, and never runs as the session from another origin.
func TestForgeryGuardAsksTheMuxWhichRoutesAreAPI(t *testing.T) {
	w := newWorld(t)
	tok := w.token("org_admin", "", nil)
	cross := []reqOpt{withHeader("Sec-Fetch-Site", "cross-site"), withHeader("Origin", foreign)}
	escaped := "/%61pi/v1/status-pages/" + w.page

	rec := w.send(http.MethodPatch, escaped, map[string]string{"title": "by token"}, append([]reqOpt{withBearer(tok)}, cross...)...)
	if rec.Code != http.StatusOK {
		t.Errorf("Bearer, cross-site PATCH %s = %d %s, want 200 (routed to /api/, run as the token)", escaped, rec.Code, strings.TrimSpace(rec.Body.String()))
	}

	rec = w.send(http.MethodPatch, escaped, map[string]string{"title": "forged"}, append([]reqOpt{withCookie(w.orgAdminSess)}, cross...)...)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cookie, cross-site PATCH %s = %d, want the forgery 403", escaped, rec.Code)
	}
	var got struct{ Title string }
	w.must(w.do(http.MethodGet, "/api/v1/status-pages/"+w.page, nil, withCookie(w.adminSess)), 200, &got, "read the page back")
	if got.Title == "forged" {
		t.Error("a cross-site cookie PATCH on /%61pi/… ran as the session")
	}

	_, sess := w.user("escaped-auth@fixture.test", false)
	for _, opts := range [][]reqOpt{{withCookie(sess)}, {withCookie(sess), withBearer(tok)}} {
		rec := w.do(http.MethodPost, "/%61uth/logout", nil, append(opts, cross...)...)
		if !w.sessionAlive(sess) {
			t.Fatalf("cross-site POST /%%61uth/logout (%d) ended the session", rec.Code)
		}
	}
}
