package appmux

// The PostgreSQL harness of this package's tests (sec-hardening-2 iteration 3). S3 requires the
// route enumeration to run against PostgreSQL, not the fakes: the defects it guards against (a
// token's synthetic actor id compared with a uuid column) answer 500 on PostgreSQL and pass on a
// fake. The harness therefore drives the REAL composition — appmux.New over a real Authenticator,
// a real api.Handler and a real store — exactly as the binary mounts it.
//
// Isolation: `go test ./...` runs packages in parallel and internal/store truncates the shared
// test database, so this binary migrates and uses a database of its OWN, created by TestMain
// (never pre-dropped), and drops it by exact name at the end — the pattern of
// internal/cli/cli_adopt_test.go. Tests in this package do not run in parallel with each other.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/auth"
	"github.com/teamlead-com/cerbix/internal/buildinfo"
	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/events"
	"github.com/teamlead-com/cerbix/internal/metrics"
	"github.com/teamlead-com/cerbix/internal/settings"
	"github.com/teamlead-com/cerbix/internal/store"
	"github.com/teamlead-com/cerbix/internal/web"
)

// probeDSN is the DSN of this binary's own database; "" when CERBIX_TEST_DATABASE_DSN is unset.
var probeDSN string

func TestMain(m *testing.M) {
	os.Exit(runWithProbeDatabase(m))
}

func runWithProbeDatabase(m *testing.M) int {
	gate := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if gate == "" {
		return m.Run() // every PostgreSQL test skips
	}
	u, err := url.Parse(gate)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		fmt.Fprintf(os.Stderr, "CERBIX_TEST_DATABASE_DSN is not a postgres:// URL: %v\n", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	admin, err := pgx.Connect(ctx, gate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
		return 2
	}
	defer admin.Close(context.Background()) //nolint:errcheck // test teardown
	name := fmt.Sprintf("cerbix_test_appmux_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		fmt.Fprintf(os.Stderr, "create probe database %s (requires CREATEDB): %v\n", name, err)
		return 2
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
		defer dcancel()
		if _, err := admin.Exec(dctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			fmt.Fprintf(os.Stderr, "drop probe database %s: %v\n", name, err)
		}
	}()
	u.Path = "/" + name
	if err := store.Migrate(ctx, u.String()); err != nil {
		fmt.Fprintf(os.Stderr, "migrate probe database: %v\n", err)
		return 2
	}
	probeDSN = u.String()
	return m.Run()
}

// stack is one application port over a truncated probe database.
type stack struct {
	t     *testing.T
	ctx   context.Context
	st    *store.Store
	authn *auth.Authenticator
	h     http.Handler
}

const cookieName = "cerbix_session"

func newStack(t *testing.T) *stack { t.Helper(); return newStackWith(t, true) }

// newStackWith builds the port with the agent router mounted or not (cli mounts it only when a
// pull transport is configured).
func newStackWith(t *testing.T, agent bool) *stack {
	t.Helper()
	if probeDSN == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run the application-port PostgreSQL tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	st, err := store.Open(ctx, probeDSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.TruncateAll(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		Session: config.SessionConfig{CookieName: cookieName, TTL: config.Duration(time.Hour)},
		Local:   config.LocalAuthConfig{Enabled: true, MinPasswordLength: 8, LoginRateLimitPerMinute: 1000},
	}
	settingsSvc := settings.New(st, settings.Bootstrap{MinPasswordLen: 8, SessionTTLSeconds: 3600}, logger)
	if err := settingsSvc.Load(ctx); err != nil {
		t.Fatalf("settings load: %v", err)
	}
	authn, err := auth.New(ctx, cfg, st, logger)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	authn.WithSettings(settingsSvc)
	registry := metrics.New(buildinfo.Current(), "all")
	apiHandler := api.New(st, logger, 8).WithMetrics(registry).WithEvents(events.NewBroker()).WithSettings(settingsSvc).
		// The gate and change bounds of the binary's defaults: without them the gate and change
		// routes answer 501 `*_not_wired` and the enumeration would test nothing behind them.
		WithGate(api.GateLimits{InflightProcess: 8, InflightPrincipal: 2, RatePrincipalPerMinute: 600,
			RateProcessPerMinute: 6000, TxBudget: 5 * time.Second}, registry).
		WithChange(api.ChangeLimits{RecordInflightProcess: 32, RecordRatePrincipalPerMinute: 600,
			RecordRateProcessPerMinute: 6000, ReadInflightProcess: 64, MaxPast: 24 * time.Hour,
			MaxFuture: 5 * time.Minute}, registry)
	spa, err := web.New()
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return &stack{t: t, ctx: ctx, st: st, authn: authn,
		h: New(Config{Auth: authn, API: apiHandler, SPA: spa, Agent: agent})}
}

// ── requests ──────────────────────────────────────────────────────────────────────────────────

type reqOpt func(*http.Request)

func withCookie(token string) reqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: cookieName, Value: token}) }
}

func withAuthorization(v string) reqOpt {
	return func(r *http.Request) { r.Header.Add("Authorization", v) }
}

func withBearer(tok string) reqOpt { return withAuthorization("Bearer " + tok) }

func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

func withHost(host string) reqOpt { return func(r *http.Request) { r.Host = host } }

func withContext(ctx context.Context) reqOpt {
	return func(r *http.Request) { *r = *r.WithContext(ctx) }
}

// jsonBody returns the body and the option that sets `Content-Type: application/json`.
func jsonBody(v any) (io.Reader, reqOpt) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return bytes.NewReader(b), withHeader("Content-Type", "application/json")
}

func (s *stack) do(method, path string, body io.Reader, opts ...reqOpt) *httptest.ResponseRecorder {
	s.t.Helper()
	r := httptest.NewRequest(method, path, body)
	r.Host = "cerbix.example.com"
	for _, o := range opts {
		o(r)
	}
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, r)
	return rec
}

// send issues a JSON request (or a body-less one when v is nil).
func (s *stack) send(method, path string, v any, opts ...reqOpt) *httptest.ResponseRecorder {
	s.t.Helper()
	if v == nil {
		return s.do(method, path, nil, opts...)
	}
	body, ct := jsonBody(v)
	return s.do(method, path, body, append([]reqOpt{ct}, opts...)...)
}

// must decodes a 2xx JSON answer into out, failing the test on any other status.
func (s *stack) must(rec *httptest.ResponseRecorder, want int, out any, what string) {
	s.t.Helper()
	if rec.Code != want {
		s.t.Fatalf("%s: status %d, want %d: %s", what, rec.Code, want, rec.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			s.t.Fatalf("%s: decode %q: %v", what, rec.Body.String(), err)
		}
	}
}

// ── principals ────────────────────────────────────────────────────────────────────────────────

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

const fixturePassword = "fixture-password-1"

// user creates a local user with fixturePassword and returns its id and a live session token.
func (s *stack) user(email string, globalAdmin bool) (id, session string) {
	s.t.Helper()
	hash, err := auth.HashPassword(fixturePassword)
	if err != nil {
		s.t.Fatalf("hash: %v", err)
	}
	u, err := s.st.CreateLocalUser(s.ctx, email, email, hash, globalAdmin)
	if err != nil {
		s.t.Fatalf("create user %s: %v", email, err)
	}
	session = "sess-" + randHex(16)
	if _, err := s.st.CreateSession(s.ctx, u.ID, session, time.Now().Add(time.Hour)); err != nil {
		s.t.Fatalf("session %s: %v", email, err)
	}
	return u.ID, session
}

// sessionAlive reports whether a session token still authenticates.
func (s *stack) sessionAlive(session string) bool {
	s.t.Helper()
	rec := s.do(http.MethodGet, "/api/v1/version", nil, withCookie(session))
	return rec.Code == http.StatusOK
}

// ── the fixture world ─────────────────────────────────────────────────────────────────────────

type world struct {
	*stack
	marker string // a string every seeded name carries — the leak oracle's needle

	adminSess string // global admin

	org, otherOrg               string
	project, sibling, otherProj string // project and sibling in org; otherProj in otherOrg
	monitor, service, incident  string
	page, pageSlug, pageToken   string
	component                   string

	orgAdminSess, orgEditorSess     string
	projAdminSess, projEditorSess   string
	projViewerSess, outsiderSess    string
	siblingSess                     string // project-only viewer of the SIBLING project
	projAdminEmail, projEditorEmail string
	projViewerEmail                 string
}

func (w *world) ids() []string {
	return []string{w.org, w.project, w.sibling, w.monitor, w.service, w.incident, w.page, w.pageSlug, w.pageToken, w.component, w.marker}
}

func newWorld(t *testing.T) *world { t.Helper(); return newWorldWith(t, true) }

func newWorldWith(t *testing.T, agent bool) *world {
	t.Helper()
	s := newStackWith(t, agent)
	w := &world{stack: s, marker: "zq" + randHex(4)}
	_, w.adminSess = s.user("admin@fixture.test", true)
	as := withCookie(w.adminSess)

	var org, other struct{ ID string }
	s.must(s.send(http.MethodPost, "/api/v1/organizations", map[string]string{"slug": w.marker + "-org", "name": w.marker + " org"}, as), 201, &org, "create org")
	s.must(s.send(http.MethodPost, "/api/v1/organizations", map[string]string{"slug": "outside", "name": "Outside"}, as), 201, &other, "create other org")
	w.org, w.otherOrg = org.ID, other.ID

	var p, sib, op struct{ ID string }
	s.must(s.send(http.MethodPost, "/api/v1/organizations/"+w.org+"/projects", map[string]string{"slug": w.marker + "-p", "name": w.marker + " p"}, as), 201, &p, "create project")
	s.must(s.send(http.MethodPost, "/api/v1/organizations/"+w.org+"/projects", map[string]string{"slug": w.marker + "-sib", "name": w.marker + " sibling"}, as), 201, &sib, "create sibling")
	s.must(s.send(http.MethodPost, "/api/v1/organizations/"+w.otherOrg+"/projects", map[string]string{"slug": "outside-p", "name": "Outside P"}, as), 201, &op, "create other project")
	w.project, w.sibling, w.otherProj = p.ID, sib.ID, op.ID

	var mon struct{ ID string }
	s.must(s.send(http.MethodPost, "/api/v1/projects/"+w.project+"/monitors", map[string]any{
		"name": w.marker + "-mon", "type": "tcp", "target": "localhost:5432", "region": "core",
		"interval_seconds": 30, "timeout_seconds": 5, "enabled": false,
	}, as), 201, &mon, "create monitor")
	w.monitor = mon.ID

	var svcs []struct {
		Service struct{ ID string } `json:"service"`
	}
	s.must(s.send(http.MethodPost, "/api/v1/projects/"+w.project+"/services", map[string]string{"slug": w.marker + "-svc", "name": w.marker + " svc"}, as), 201, nil, "create service")
	s.must(s.do(http.MethodGet, "/api/v1/projects/"+w.project+"/services", nil, as), 200, &svcs, "list services")
	if len(svcs) != 1 {
		t.Fatalf("services = %+v, want one", svcs)
	}
	w.service = svcs[0].Service.ID

	var inc struct{ ID string }
	s.must(s.send(http.MethodPost, "/api/v1/projects/"+w.project+"/incidents", map[string]string{
		"title": w.marker + " incident", "impact": "minor", "body": "fixture",
	}, as), 201, &inc, "create incident")
	w.incident = inc.ID

	var page struct {
		ID            string `json:"id"`
		Slug          string `json:"slug"`
		UnlistedToken string `json:"unlisted_token"`
	}
	w.pageSlug = w.marker + "-page"
	s.must(s.send(http.MethodPost, "/api/v1/organizations/"+w.org+"/status-pages", map[string]string{
		"slug": w.pageSlug, "title": w.marker + " page", "visibility": "unlisted",
	}, as), 201, &page, "create status page")
	if page.UnlistedToken == "" {
		t.Fatal("an unlisted page was created without a token")
	}
	w.page, w.pageToken = page.ID, page.UnlistedToken

	var comp struct{ ID string }
	s.must(s.send(http.MethodPost, "/api/v1/status-pages/"+w.page+"/components", map[string]any{
		"name": w.marker + " comp", "monitor_id": w.monitor,
	}, as), 201, &comp, "create component")
	w.component = comp.ID

	member := func(email, project, role string) string {
		id, sess := s.user(email, false)
		body := map[string]string{"user_id": id, "role": role}
		org := w.org
		if project == w.otherProj {
			org = w.otherOrg
		}
		if project != "" && project != w.otherProj {
			body["project_id"] = project
		}
		s.must(s.send(http.MethodPost, "/api/v1/organizations/"+org+"/members", body, as), 201, nil, "add member "+email)
		return sess
	}
	w.orgAdminSess = member("orgadmin@fixture.test", "", "org_admin")
	w.orgEditorSess = member("orgeditor@fixture.test", "", "editor")
	w.projAdminEmail, w.projEditorEmail, w.projViewerEmail = "padmin@fixture.test", "peditor@fixture.test", "pviewer@fixture.test"
	w.projAdminSess = member(w.projAdminEmail, w.project, "project_admin")
	w.projEditorSess = member(w.projEditorEmail, w.project, "editor")
	w.projViewerSess = member(w.projViewerEmail, w.project, "viewer")
	w.outsiderSess = member("outsider@fixture.test", w.otherProj, "viewer")
	w.siblingSess = member("sibling@fixture.test", w.sibling, "viewer")
	return w
}

// token issues an API token in the fixture org as the global admin. actions nil leaves the role
// in charge; project "" makes it org-scoped. It returns the plaintext bearer secret.
func (w *world) token(role, project string, actions []string) string {
	w.t.Helper()
	body := map[string]any{"name": "tok-" + randHex(3), "role": role}
	if project != "" {
		body["project_id"] = project
	}
	if actions != nil {
		body["actions"] = actions
	}
	var out struct {
		Token string `json:"token"`
	}
	w.must(w.send(http.MethodPost, "/api/v1/organizations/"+w.org+"/tokens", body, withCookie(w.adminSess)), 201, &out, "create token")
	return out.Token
}

// leaks reports which seeded identifiers a body carries.
func (w *world) leaks(body string) []string {
	var found []string
	for _, id := range w.ids() {
		if id != "" && strings.Contains(body, id) {
			found = append(found, id)
		}
	}
	return found
}
