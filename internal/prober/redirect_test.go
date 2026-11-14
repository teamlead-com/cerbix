package prober

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// sec-hardening-2 H-13 (invariant S13), the Runner's column: on any hop of any chain no header the
// first request carried, no body and no Referer reaches an origin other than the first request's
// (save the strict http:80 → https:443 upgrade), and nothing goes over a weaker scheme.

// hit is what one fixture endpoint received.
type hit struct {
	path    string
	header  http.Header
	body    string
	referer string
}

// recorder is an httptest server whose handler is chosen per path and that records every hit.
type recorder struct {
	*httptest.Server
	mu    sync.Mutex
	hits  []hit
	route map[string]http.HandlerFunc
}

func newRecorder(t *testing.T, tlsServer bool) *recorder {
	t.Helper()
	r := &recorder{route: map[string]http.HandlerFunc{}}
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.hits = append(r.hits, hit{path: req.URL.Path, header: req.Header.Clone(), body: string(body), referer: req.Referer()})
		fn := r.route[req.URL.Path]
		r.mu.Unlock()
		if fn != nil {
			fn(w, req)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	if tlsServer {
		r.Server = httptest.NewTLSServer(h)
	} else {
		r.Server = httptest.NewServer(h)
	}
	t.Cleanup(r.Close)
	return r
}

func (r *recorder) redirect(path, to string, code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.route[path] = func(w http.ResponseWriter, req *http.Request) { http.Redirect(w, req, to, code) }
}

func (r *recorder) got(path string) []hit {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []hit
	for _, h := range r.hits {
		if h.path == path {
			out = append(out, h)
		}
	}
	return out
}

// runnerClient is the Runner's client shape with the redirect rule; the TLS fixture's self-signed
// certificate is accepted so the scheme rules can be exercised.
func runnerClient() *http.Client {
	return &http.Client{
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // test fixture
		CheckRedirect: runnerCheckRedirect,
	}
}

func credentialed(t *testing.T, method, target, body string) *http.Request {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, target, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Basic c2VjcmV0")
	req.Header.Set("X-Api-Key", "k-secret")
	return req
}

func assertNoCredential(t *testing.T, where string, hits []hit) {
	t.Helper()
	if len(hits) == 0 {
		t.Fatalf("%s: never reached", where)
	}
	for _, h := range hits {
		if h.header.Get("Authorization") != "" || h.header.Get("X-Api-Key") != "" {
			t.Fatalf("%s received a credential the first request carried: %v", where, h.header)
		}
		if h.referer != "" {
			t.Fatalf("%s received a Referer: %q", where, h.referer)
		}
	}
}

func TestRunnerRedirectCrossOriginStripsHeaders(t *testing.T) {
	a, b := newRecorder(t, false), newRecorder(t, false) // same host, another port = another origin
	a.redirect("/start", b.URL+"/land", http.StatusFound)
	resp, err := runnerClient().Do(credentialed(t, http.MethodGet, a.URL+"/start", ""))
	if err != nil {
		t.Fatalf("a cross-origin GET redirect must be followed: %v", err)
	}
	_ = resp.Body.Close()
	assertNoCredential(t, "B", b.got("/land"))
}

func TestRunnerRedirectTwoHopStaysStripped(t *testing.T) {
	a, b := newRecorder(t, false), newRecorder(t, false)
	a.redirect("/start", b.URL+"/1", http.StatusFound)
	b.redirect("/1", b.URL+"/2", http.StatusFound)
	resp, err := runnerClient().Do(credentialed(t, http.MethodGet, a.URL+"/start", ""))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	assertNoCredential(t, "B/1", b.got("/1"))
	assertNoCredential(t, "B/2", b.got("/2"))
}

func TestRunnerRedirectBackToOriginStaysStripped(t *testing.T) {
	a, b := newRecorder(t, false), newRecorder(t, false)
	a.redirect("/start", b.URL+"/x", http.StatusFound)
	b.redirect("/x", a.URL+"/back", http.StatusFound)
	resp, err := runnerClient().Do(credentialed(t, http.MethodGet, a.URL+"/start", ""))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	assertNoCredential(t, "A after leaving", a.got("/back"))
}

func TestRunnerRedirectSameOriginKeepsHeaders(t *testing.T) {
	a := newRecorder(t, false)
	a.redirect("/start", a.URL+"/next", http.StatusFound)
	resp, err := runnerClient().Do(credentialed(t, http.MethodGet, a.URL+"/start?token=t", ""))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	hits := a.got("/next")
	if len(hits) != 1 || hits[0].header.Get("X-Api-Key") != "k-secret" || hits[0].header.Get("Authorization") == "" {
		t.Fatalf("a same-origin hop must keep the request's headers: %+v", hits)
	}
	if hits[0].referer != "" {
		t.Fatalf("Referer sent on a same-origin hop: %q", hits[0].referer)
	}
}

// The upgrade exception and the origin comparison, as units: an http:80 → https:443 hop of the
// first request's own host does not leave; any other change does, and leaving is sticky.
func TestChainLeftOrigin(t *testing.T) {
	r := func(u string) *http.Request {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	for _, tc := range []struct {
		name    string
		chain   []string
		upgrade bool
		left    bool
	}{
		{"same origin", []string{"http://a/x", "http://a/y"}, true, false},
		{"default port spelled out", []string{"http://a/x", "http://A:80/y"}, true, false},
		{"other port", []string{"http://a:8080/x", "http://a:8081/y"}, true, true},
		{"other host", []string{"http://a/x", "http://b/y"}, true, true},
		{"strict upgrade", []string{"http://a/x", "https://a/y"}, true, false},
		{"strict upgrade, canary rule", []string{"http://a/x", "https://a/y"}, false, true},
		{"upgrade on other ports", []string{"http://a:8080/x", "https://a:8443/y"}, true, true},
		{"upgrade then away", []string{"http://a/x", "https://a/y", "https://b/z"}, true, true},
		{"back to the start", []string{"http://a/x", "http://b/y", "http://a/z"}, true, true},
	} {
		var via []*http.Request
		for _, u := range tc.chain[:len(tc.chain)-1] {
			via = append(via, r(u))
		}
		if got := chainLeftOrigin(r(tc.chain[len(tc.chain)-1]), via, tc.upgrade); got != tc.left {
			t.Errorf("%s: left = %v, want %v", tc.name, got, tc.left)
		}
	}
}

func TestRunnerRedirectRefusesDowngradeUserinfoAndScheme(t *testing.T) {
	b := newRecorder(t, false)
	secure := newRecorder(t, true)
	a := newRecorder(t, false)
	bURL, _ := url.Parse(b.URL)
	secure.redirect("/down", b.URL+"/plain", http.StatusFound)
	a.redirect("/userinfo", "http://u:p@"+bURL.Host+"/ui", http.StatusFound)
	a.redirect("/ftp", "ftp://files.internal/x", http.StatusFound)
	for name, target := range map[string]string{
		"https → http": secure.URL + "/down",
		"userinfo":     a.URL + "/userinfo",
		"ftp scheme":   a.URL + "/ftp",
	} {
		_, err := runnerClient().Do(credentialed(t, http.MethodGet, target, ""))
		if err == nil {
			t.Errorf("%s: followed", name)
			continue
		}
		reason := failureClass(err)
		if !strings.HasPrefix(reason, "redirect refused:") || strings.Contains(reason, "://") {
			t.Errorf("%s: reason %q, want a redirect-refused reason with no URL", name, reason)
		}
	}
	if len(b.got("/plain")) != 0 || len(b.got("/ui")) != 0 {
		t.Fatal("a refused hop reached its target")
	}
}

func TestRunnerRedirectNeverSendsReferer(t *testing.T) {
	a, b := newRecorder(t, false), newRecorder(t, false)
	a.redirect("/start", b.URL+"/land", http.StatusFound)
	resp, err := runnerClient().Do(credentialed(t, http.MethodGet, a.URL+"/start?token=t0p", ""))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	for _, h := range b.got("/land") {
		if h.referer != "" {
			t.Fatalf("B received Referer %q", h.referer)
		}
	}
}

func TestRunnerRedirectRefusesCrossOriginBody(t *testing.T) {
	a, b := newRecorder(t, false), newRecorder(t, false)
	a.redirect("/away", b.URL+"/land", http.StatusTemporaryRedirect)
	a.redirect("/stay", a.URL+"/next", http.StatusTemporaryRedirect)
	_, err := runnerClient().Do(credentialed(t, http.MethodPost, a.URL+"/away", "password=hunter2"))
	if err == nil || !strings.HasPrefix(failureClass(err), "redirect refused:") {
		t.Fatalf("a cross-origin 307 with a body must be refused, got %v", err)
	}
	if len(b.got("/land")) != 0 {
		t.Fatal("the body reached another origin")
	}
	resp, err := runnerClient().Do(credentialed(t, http.MethodPost, a.URL+"/stay", "password=hunter2"))
	if err != nil {
		t.Fatalf("a same-origin 307 with a body must be delivered: %v", err)
	}
	_ = resp.Body.Close()
	if hits := a.got("/next"); len(hits) != 1 || hits[0].body != "password=hunter2" {
		t.Fatalf("same-origin 307 delivered %+v", hits)
	}
}

// The real Runner wires the rule into every HTTP client it builds — the synthetic one driven
// through the real executor gate, so the secret substitution is dispatch's, not a test's.
func TestSyntheticSecretBodyNeverCrossesOrigin(t *testing.T) {
	ring, err := dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{ID: "k-syn", Key: bytes.Repeat([]byte{9}, 32)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	probe := NewRunner().registry[domain.MonitorSynthetic]
	run := func(t *testing.T, scenario string) Result {
		t.Helper()
		m := domain.Monitor{
			ID: "33333333-3333-3333-3333-333333333333", Region: "core", ExecutionRevision: 1, Type: domain.MonitorSynthetic,
			Config: map[string]string{domain.SyntheticScenarioKey: scenario, domain.ScenarioSecretRefKey("login"): "login-token"},
		}
		env, err := ring.Seal(dispatch.SealContext{EnvelopeVersion: dispatch.EnvelopeV2, Region: m.Region, JobID: "job-syn",
			MonitorID: m.ID, Revision: m.ExecutionRevision, Body: m},
			map[string][]byte{domain.ScenarioBindingField("login"): []byte("s3cr3t-login")})
		if err != nil {
			t.Fatal(err)
		}
		mat, err := dispatch.ValidateAndMaterialize(ring, dispatch.DeliveredJob{
			Job:               dispatch.CheckJob{Monitor: m, ProtocolVersion: dispatch.ProtocolV3, CredentialEnvelope: env},
			CarrierGeneration: dispatch.ProtocolV3,
		})
		if err != nil {
			t.Fatalf("materialize: %v", err)
		}
		defer mat.Cleanup()
		return probe.Probe(context.Background(), mat.Monitor)
	}
	for name, body := range map[string]string{
		"secret placeholder": `{{secret:login}}`,
		"extracted var":      `{{session}}`,
		"static":             `plain=1`,
	} {
		t.Run(name, func(t *testing.T) {
			a, b := newRecorder(t, false), newRecorder(t, false)
			a.route["/session"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "derived-from-secret") }
			a.redirect("/away", b.URL+"/land", http.StatusTemporaryRedirect)
			a.redirect("/stay", a.URL+"/next", http.StatusTemporaryRedirect)
			steps := func(path string) string {
				return `{"steps":[{"url":"` + a.URL + `/session","headers":{"authorization":"{{secret:login}}"},"extract":[{"var":"session","from":"body"}]},` +
					`{"method":"POST","url":"` + a.URL + path + `","body":"` + body + `"}]}`
			}
			if res := run(t, steps("/away")); res.Connected || !strings.Contains(res.Msg, "redirect refused:") {
				t.Fatalf("cross-origin 307 with a body: %+v, want a refusal", res)
			}
			if len(b.got("/land")) != 0 {
				t.Fatal("the body reached another origin")
			}
			if res := run(t, steps("/stay")); !res.Connected {
				t.Fatalf("same-origin 307 with a body must be delivered: %+v", res)
			}
		})
	}
	// Review Focus 4: a step with no body follows a cross-origin 307 (nothing to protect).
	a, b := newRecorder(t, false), newRecorder(t, false)
	a.redirect("/away", b.URL+"/land", http.StatusTemporaryRedirect)
	if res := run(t, `{"steps":[{"method":"POST","url":"`+a.URL+`/away","headers":{"authorization":"{{secret:login}}"}}]}`); !res.Connected || len(b.got("/land")) != 1 {
		t.Fatalf("an empty-body 307 must be followed: %+v", res)
	}
}

func TestRabbitSkipVerifyClientKeepsTheRedirectRule(t *testing.T) {
	a, b := newRecorder(t, true), newRecorder(t, true) // https → https: only the origin changes
	a.redirect("/api/overview", b.URL+"/land", http.StatusFound)
	probe := NewRunner().registry[domain.MonitorRabbitMQ]
	res := probe.Probe(context.Background(), domain.Monitor{Type: domain.MonitorRabbitMQ, Target: a.URL,
		Config: map[string]string{"mode": "management", "tls": "true", "tls_skip_verify": "true", "username": "guest", "password": "guest"}})
	_ = res
	assertNoCredential(t, "B", b.got("/land"))
}

// External review of iter-0210 (P3-1): the redirect rule reaches every HTTP client the real
// NewRunner builds — the http prober, promql with Basic Auth, and the RabbitMQ management prober's
// main client (no tls_skip_verify). Each probe is redirected across origins; the second origin must
// receive neither a credential nor a Referer. Without the rule net/http sends a Referer on an
// http → http hop, so unwiring any one client fails its case.
func TestRunnerClientsKeepTheRedirectRule(t *testing.T) {
	for name, tc := range map[string]struct {
		path string
		mon  func(base string) domain.Monitor
	}{
		"http": {"/health", func(base string) domain.Monitor {
			return domain.Monitor{Type: domain.MonitorHTTP, Target: base + "/health"}
		}},
		"promql basic auth": {"/api/v1/query", func(base string) domain.Monitor {
			return domain.Monitor{Type: domain.MonitorPromQL, Target: base,
				Config: map[string]string{"query": "up", "username": "scanner", "password": "s3cr3t"}}
		}},
		"rabbitmq management": {"/api/overview", func(base string) domain.Monitor {
			return domain.Monitor{Type: domain.MonitorRabbitMQ, Target: base,
				Config: map[string]string{"mode": "management", "username": "guest", "password": "s3cr3t"}}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			a, b := newRecorder(t, false), newRecorder(t, false)
			a.redirect(tc.path, b.URL+"/land", http.StatusFound)
			m := tc.mon(a.URL)
			_ = NewRunner().registry[m.Type].Probe(context.Background(), m)
			if hits := a.got(tc.path); len(hits) != 1 || (m.Config["username"] != "" && hits[0].header.Get("Authorization") == "") {
				t.Fatalf("A: %d hit(s), want one carrying the credential: %+v", len(hits), hits)
			}
			assertNoCredential(t, "B", b.got("/land"))
		})
	}
}
