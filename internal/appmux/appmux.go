// Package appmux composes the application port's handler: the auth routes, the session/bearer
// API, the unauthenticated public router, the HTTP-pull agent router and the embedded SPA. It is
// the one place that decides what is mounted where, so the request-forgery and response-header
// rules that must cover ALL of it (sec-hardening-2 H-4, H-9a, H-9b) have exactly one place to stand, and
// a test can drive the real composition rather than a copy of it.
package appmux

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/auth"
)

// Config is what the application port serves.
type Config struct {
	Auth *auth.Authenticator
	API  *api.Handler
	// SPA is the catch-all: the embedded single-page application.
	SPA http.Handler
	// Agent mounts the HTTP-pull agent router (`/api/v1/agent/`). The caller configures the
	// handler's agent tokens and pull waiter before New.
	Agent bool
}

// New builds the application handler. Operational endpoints (health, readiness, metrics) are not
// here: httpsrv mounts them in front of this handler.
func New(c Config) http.Handler {
	mux := http.NewServeMux()
	c.Auth.Routes(mux)
	// HTTP-pull agent endpoints self-authenticate with the agent token(s) and are mounted outside
	// the session-auth middleware (agents are not users).
	if c.Agent {
		mux.Handle("/api/v1/agent/", c.API.AgentRouter())
	}
	mux.Handle("/api/", c.Auth.RequireAuth(c.API.APIRouter()))
	// Public status-page rendering is unauthenticated; its more-specific prefix takes precedence
	// over the auth-gated /api/ handler (ServeMux longest-match).
	mux.Handle("/api/v1/public/", c.API.PublicRouter())
	mux.Handle("/", c.SPA) // catch-all: serve the embedded SPA
	bypass := []string{"/api/v1/public/"}
	if c.Agent {
		bypass = append(bypass, "/api/v1/agent/")
	}
	return withResponseHeaders(forgeryGuard(mux, bypass))
}

// forgeryGuard is sec-hardening-2 H-4 fix 1: http.CrossOriginProtection in front of the WHOLE
// application mux, /auth/* included. It refuses an unsafe request that is not same-origin by
// Sec-Fetch-Site, or — without that header — whose Origin host differs from Host (no scheme
// comparison, so a TLS-terminating proxy is fine); a request with neither header is a non-browser
// client and passes. Two departures from the bare library, both deliberate:
//   - a refusal answers with the API's JSON error shape, not text/plain;
//   - a request the application mux would route to the /api/ handler and that presents a Bearer
//     credential is not checked — a token is not an ambient credential. That is sound only because
//     RequireAuth, deciding by the same predicate (auth.HasBearerAuth), never reads the cookie of
//     such a request. /auth/* is never exempt: POST /auth/logout reads the cookie itself, outside
//     RequireAuth.
//
// "Would route to /api/" is not approximated by a string test on the path: it is ASKED of a
// ServeMux holding the one pattern "/api/" (apiRoutes), the way CrossOriginProtection asks its own
// mux about bypass patterns. So the answer is the router's own — cleaned escaped path, unescaped
// segment matching, /%61pi included, redirects for unclean paths not exempt (second review of
// iter-0211, finding 2; a string predicate on the escaped path refused /%61pi/… that the mux
// routes to /api/).
//
// bypass lists the routers that hold no ambient credential and are reachable from any origin by
// design: the public router (status pages, subscriptions, push ingest) always, and the agent
// router (agent-token auth) only when it is mounted — otherwise /api/v1/agent/* falls to the
// session-authenticated /api/ handler, which must not be exempt (review of iter-0211, finding 2).
// FR-041's telephony router joins them when it exists.
func forgeryGuard(next http.Handler, bypass []string) http.Handler {
	cop := http.NewCrossOriginProtection()
	for _, pattern := range bypass {
		cop.AddInsecureBypassPattern(pattern)
	}
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "cross-origin request refused"})
	}))
	checked := cop.Handler(next)
	apiRoutes := http.NewServeMux()
	apiRoutes.Handle("/api/", apiRoute)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, _ := apiRoutes.Handler(r); h == apiRoute && auth.HasBearerAuth(r) {
			next.ServeHTTP(w, r)
			return
		}
		checked.ServeHTTP(w, r)
	})
}

// apiMarker is the handler apiRoutes returns for a path the application mux routes to /api/. It
// is never served; it is compared. A pointer to an empty struct is comparable, unlike a
// http.HandlerFunc, and no other handler a ServeMux returns (a redirect, NotFound) has its type.
type apiMarker struct{}

func (*apiMarker) ServeHTTP(http.ResponseWriter, *http.Request) {}

var apiRoute http.Handler = &apiMarker{}

// isPublicPage is THE predicate for "a public status page or one of its reads" — the one place it
// is decided, for every header that differs there (Referrer-Policy, X-Frame-Options, the CSP's
// frame-ancestors). The prefixes are compared case-insensitively: the SPA's router matches routes
// regardless of case, so /STATUS/<slug> renders the public page too.
func isPublicPage(path string) bool {
	p := strings.ToLower(path)
	return p == "/status" || strings.HasPrefix(p, "/status/") || strings.HasPrefix(p, "/api/v1/public/")
}

// The Content-Security-Policy of sec-hardening-2 H-9b, verbatim. `img-src` admits https: because
// branding.logo_url is a documented external URL; `style-src` admits inline styles because the
// components bind them. The two variants differ only in frame-ancestors: a public status page may
// be framed by the instance's own pages, nothing else may be framed at all.
const (
	cspBase   = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; "
	cspApp    = cspBase + "frame-ancestors 'none'"
	cspPublic = cspBase + "frame-ancestors 'self'"
)

// cspHeader is the header the policy rides in. This release ships it REPORT-ONLY: a violation
// is reported (the Playwright fixture fails a test on one, S9b) but nothing is blocked. The next
// release switches this constant to "Content-Security-Policy" and enforces it.
const cspHeader = "Content-Security-Policy-Report-Only"

// withResponseHeaders sets the security headers on every response of the application handler,
// refusals and 404s included — not on every response of the port: the path-canonicalization
// redirects and /healthz are answered by internal/httpsrv's outer multiplexer before this handler
// runs, and so are /readyz and /metrics when the process serves them on this port (without
// server.ops_listen); with ops_listen set the outer multiplexer answers those two 404 here and
// serves them on the ops port. None of them renders a page; httpsrv sets nosniff on its own.
//
//   - Referrer-Policy (H-9a): an unlisted status page's capability is the `?token=` in its URL, so
//     the public page and every public read are served `no-referrer` — no request leaving them
//     carries a Referer; everything else `strict-origin-when-cross-origin`, which never sends a
//     path or query to another origin.
//   - X-Content-Type-Options: nosniff (H-9b) everywhere.
//   - X-Frame-Options (H-9b): DENY, SAMEORIGIN on the public pages; the CSP's frame-ancestors says
//     the same for browsers that read it.
//   - the CSP (H-9b), report-only in this release.
func withResponseHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		if isPublicPage(r.URL.Path) {
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("X-Frame-Options", "SAMEORIGIN")
			h.Set(cspHeader, cspPublic)
		} else {
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("X-Frame-Options", "DENY")
			h.Set(cspHeader, cspApp)
		}
		next.ServeHTTP(w, r)
	})
}
