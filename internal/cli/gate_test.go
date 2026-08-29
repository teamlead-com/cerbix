package cli

import (
	"bytes"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	gateTestProject = "p-1"
	gateTestService = "s-1"
	gateTestToken   = "cbx_test_token_123"
	gateTestPath    = "/api/v1/projects/p-1/services/s-1/gate"
)

// gateFakeServer is the decision endpoint as the CLI must see it: it asserts the request
// contract (method, path, bearer header, content types, empty JSON body, user agent), counts
// hits so "never retries" is a number and not a belief, and answers a canned status/body.
type gateFakeServer struct {
	t        *testing.T
	status   int
	body     string
	headers  map[string]string
	wantPath string
	hits     atomic.Int32
}

func (f *gateFakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.hits.Add(1)
	if r.Method != http.MethodPost {
		f.t.Errorf("method = %s, want POST", r.Method)
	}
	wantPath := f.wantPath
	if wantPath == "" {
		wantPath = gateTestPath
	}
	if r.URL.Path != wantPath {
		f.t.Errorf("path = %q, want %q", r.URL.Path, wantPath)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+gateTestToken {
		f.t.Errorf("Authorization header = %q, want the bearer form with the token", got)
	}
	if got := r.Header.Get("Accept"); got != "application/json" {
		f.t.Errorf("Accept = %q", got)
	}
	if got := r.Header.Get("Content-Type"); got != "application/json" {
		f.t.Errorf("Content-Type = %q", got)
	}
	if got := r.Header.Get("User-Agent"); !strings.HasPrefix(got, "cerbix-cli/") {
		f.t.Errorf("User-Agent = %q, want cerbix-cli/<version>", got)
	}
	reqBody, _ := io.ReadAll(r.Body)
	if string(reqBody) != "{}" {
		f.t.Errorf("request body = %q, want {}", reqBody)
	}
	for k, v := range f.headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.status)
	_, _ = io.WriteString(w, f.body)
}

func newGateServer(t *testing.T, status int, body string, headers map[string]string) (*httptest.Server, *gateFakeServer) {
	t.Helper()
	fake := &gateFakeServer{t: t, status: status, body: body, headers: headers}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return srv, fake
}

// runGateCheckWith runs the verb against baseURL with the standard env and returns the exit
// code and the two streams.
func runGateCheckWith(t *testing.T, baseURL string, extra ...string) (int, string, string) {
	t.Helper()
	t.Setenv("CERBIX_URL", baseURL)
	t.Setenv("CERBIX_TOKEN", gateTestToken)
	t.Setenv("CERBIX_CA_FILE", "")
	var stdout, stderr bytes.Buffer
	args := append([]string{"check", "--project", gateTestProject, "--service", gateTestService}, extra...)
	code := runGate(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

const (
	gateBodyAllow = `{"schema_version":1,"decision_id":"dec-allow","evaluated_at":"2026-08-29T10:00:00Z","state":"ALLOW","action":"ALLOW","unoverridden_action":"ALLOW","reasons":[],"policy_revision":3,"facts_fresh_until":"2026-08-29T10:05:00Z"}`
	gateBodyWarn  = `{"schema_version":1,"decision_id":"dec-warn","evaluated_at":"2026-08-29T10:00:00Z","state":"WARN","action":"WARN","unoverridden_action":"WARN","reasons":[{"code":"budget_consumed_percent","clause":"budget_consumed_percent","assignment":"warn","value":91,"source":"sealed"}]}`
	gateBodyBlock = `{"schema_version":1,"decision_id":"dec-block","evaluated_at":"2026-08-29T10:00:00Z","state":"BLOCK","action":"BLOCK","unoverridden_action":"BLOCK","reasons":[{"code":"budget_exhausted","clause":"budget_exhausted","assignment":"block","value":true}]}`
)

func TestGateCheckAllowExitsZeroWithOneLine(t *testing.T) {
	srv, fake := newGateServer(t, http.StatusOK, gateBodyAllow, nil)
	code, stdout, stderr := runGateCheckWith(t, srv.URL)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr)
	}
	if stdout != "state=ALLOW action=ALLOW decision=dec-allow\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty (no reasons, no diagnostics)", stderr)
	}
	if n := fake.hits.Load(); n != 1 {
		t.Fatalf("server saw %d requests, want 1", n)
	}
}

func TestGateCheckToleratesTrailingSlashAndPathPrefix(t *testing.T) {
	srv, fake := newGateServer(t, http.StatusOK, gateBodyAllow, nil)
	if code, _, stderr := runGateCheckWith(t, srv.URL+"/"); code != 0 {
		t.Fatalf("trailing slash: exit = %d; stderr=%q", code, stderr)
	}
	fake.wantPath = "/cerbix" + gateTestPath
	if code, _, stderr := runGateCheckWith(t, srv.URL+"/cerbix/"); code != 0 {
		t.Fatalf("path prefix: exit = %d; stderr=%q", code, stderr)
	}
}

func TestGateCheckAccepts201(t *testing.T) {
	srv, _ := newGateServer(t, http.StatusCreated, gateBodyAllow, nil)
	if code, _, stderr := runGateCheckWith(t, srv.URL); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr)
	}
}

func TestGateCheckWarnExitsZeroAndPrintsReasonsToStderr(t *testing.T) {
	srv, _ := newGateServer(t, http.StatusOK, gateBodyWarn, nil)
	code, stdout, stderr := runGateCheckWith(t, srv.URL)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout != "state=WARN action=WARN decision=dec-warn\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr != "budget_consumed_percent (warn): 91\n" {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestGateCheckBlockExitsTwo(t *testing.T) {
	srv, _ := newGateServer(t, http.StatusOK, gateBodyBlock, nil)
	code, stdout, stderr := runGateCheckWith(t, srv.URL)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if stdout != "state=BLOCK action=BLOCK decision=dec-block\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "budget_exhausted (block): true") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// D16: UNKNOWN exits by the operator's declared unknown_behavior, and the word UNKNOWN stays
// in the output regardless.
func TestGateCheckUnknownFollowsAction(t *testing.T) {
	unknown := func(action string) string {
		return `{"schema_version":1,"decision_id":"dec-unk","evaluated_at":"2026-08-29T10:00:00Z","state":"UNKNOWN","action":"` + action + `","reasons":[{"code":"seal_stale","clause":"budget_consumed_percent","assignment":"block","value":null,"details":"seal_lag 420s > max_seal_lag_seconds 300"}]}`
	}
	srv, _ := newGateServer(t, http.StatusOK, unknown("WARN"), nil)
	code, stdout, stderr := runGateCheckWith(t, srv.URL)
	if code != 0 {
		t.Fatalf("UNKNOWN/WARN exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "state=UNKNOWN action=WARN") {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "seal_stale (block): seal_lag 420s > max_seal_lag_seconds 300") {
		t.Fatalf("stderr = %q", stderr)
	}

	srv2, _ := newGateServer(t, http.StatusOK, unknown("BLOCK"), nil)
	code, stdout, _ = runGateCheckWith(t, srv2.URL)
	if code != 2 {
		t.Fatalf("UNKNOWN/BLOCK exit = %d, want 2", code)
	}
	if !strings.Contains(stdout, "state=UNKNOWN action=BLOCK") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestGateCheckNotConfiguredExitsFourWithDocsLink(t *testing.T) {
	const docs = "https://cerbix.example.com/docs/reliability-gate#not-configured"
	body := `{"schema_version":1,"decision_id":"dec-nc","evaluated_at":"2026-08-29T10:00:00Z","state":"NOT_CONFIGURED","reasons":[{"code":"not_configured","docs":"` + docs + `"}]}`
	srv, _ := newGateServer(t, http.StatusOK, body, nil)
	code, stdout, stderr := runGateCheckWith(t, srv.URL)
	if code != 4 {
		t.Fatalf("exit = %d, want 4", code)
	}
	if stdout != "state=NOT_CONFIGURED decision=dec-nc\n" {
		t.Fatalf("stdout = %q (no action= for NOT_CONFIGURED)", stdout)
	}
	if !strings.Contains(stderr, "not_configured") || !strings.Contains(stderr, docs) {
		t.Fatalf("stderr = %q, want the reason code and the docs link", stderr)
	}
}

func TestGateCheckOverrideAppearsInSummary(t *testing.T) {
	body := `{"schema_version":1,"decision_id":"dec-ov","evaluated_at":"2026-08-29T10:00:00Z","state":"BLOCK","action":"ALLOW","unoverridden_action":"BLOCK","override":{"id":"ov-9","actor_label":"token:deploy-bot","reason":"hotfix","expires_at":"2026-08-30T10:00:00Z"},"reasons":[{"code":"budget_exhausted","assignment":"block","value":true}]}`
	srv, _ := newGateServer(t, http.StatusOK, body, nil)
	code, stdout, _ := runGateCheckWith(t, srv.URL)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (action ALLOW under override)", code)
	}
	if stdout != "state=BLOCK action=ALLOW override=token:deploy-bot decision=dec-ov\n" {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestGateCheckJSONIsByteIdenticalToResponse(t *testing.T) {
	srv, _ := newGateServer(t, http.StatusOK, gateBodyBlock, nil)
	code, stdout, _ := runGateCheckWith(t, srv.URL, "--json")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (BLOCK still governs the exit under --json)", code)
	}
	if stdout != gateBodyBlock+"\n" {
		t.Fatalf("stdout = %q, want the body verbatim plus one newline", stdout)
	}

	// A server that ends its body with a newline (json.Encoder does) is not doubled.
	srv2, _ := newGateServer(t, http.StatusOK, gateBodyAllow+"\n", nil)
	code, stdout, _ = runGateCheckWith(t, srv2.URL, "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout != gateBodyAllow+"\n" {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestGateCheck429PrintsRetryAfterAndNeverRetries(t *testing.T) {
	srv, fake := newGateServer(t, http.StatusTooManyRequests, `{"error":"rate_limited"}`, map[string]string{"Retry-After": "7"})
	code, stdout, stderr := runGateCheckWith(t, srv.URL)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "Retry-After: 7") {
		t.Fatalf("stderr = %q, want the Retry-After value", stderr)
	}
	if !strings.Contains(stderr, "429 rate_limited") {
		t.Fatalf("stderr = %q, want the status and server error", stderr)
	}
	if n := fake.hits.Load(); n != 1 {
		t.Fatalf("server saw %d requests, want exactly 1 (no retry)", n)
	}
}

func TestGateCheck503SnapshotConflict(t *testing.T) {
	srv, _ := newGateServer(t, http.StatusServiceUnavailable, `{"error":"snapshot_conflict"}`, nil)
	code, stdout, stderr := runGateCheckWith(t, srv.URL)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "cerbix: gate: 503 snapshot_conflict") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestGateCheckOtherStatusesExitOne(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusUnauthorized, `{"error":"unauthorized"}`, "401 unauthorized"},
		{http.StatusForbidden, `{"error":"forbidden"}`, "403 forbidden"},
		{http.StatusNotFound, `{"error":"not_found"}`, "404 not_found"},
		{http.StatusBadRequest, `{"error":"invalid_service_id"}`, "400 invalid_service_id"},
		{http.StatusConflict, `{"error":"revision_conflict"}`, "409 revision_conflict"},
		{http.StatusInternalServerError, `<html>boom</html>`, "500 Internal Server Error"},
	} {
		srv, _ := newGateServer(t, tc.status, tc.body, nil)
		code, stdout, stderr := runGateCheckWith(t, srv.URL)
		if code != 1 {
			t.Errorf("%d: exit = %d, want 1", tc.status, code)
		}
		if stdout != "" {
			t.Errorf("%d: stdout = %q, want empty", tc.status, stdout)
		}
		if !strings.Contains(stderr, tc.want) {
			t.Errorf("%d: stderr = %q, want %q", tc.status, stderr, tc.want)
		}
	}
}

func TestGateCheckDoesNotFollowRedirects(t *testing.T) {
	srv, fake := newGateServer(t, http.StatusFound, "", map[string]string{"Location": "https://elsewhere.example/"})
	code, _, stderr := runGateCheckWith(t, srv.URL)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "302") || !strings.Contains(stderr, "not followed") {
		t.Fatalf("stderr = %q", stderr)
	}
	if n := fake.hits.Load(); n != 1 {
		t.Fatalf("server saw %d requests, want 1", n)
	}
}

func TestGateCheckMalformedResponsesExitOne(t *testing.T) {
	for name, body := range map[string]string{
		"unknown state":  `{"schema_version":1,"decision_id":"d","state":"MAYBE","action":"ALLOW","reasons":[]}`,
		"missing action": `{"schema_version":1,"decision_id":"d","state":"ALLOW","reasons":[]}`,
		"bad action":     `{"schema_version":1,"decision_id":"d","state":"ALLOW","action":"SHRUG","reasons":[]}`,
		"not json":       `<html>ok</html>`,
		"empty":          ``,
	} {
		srv, _ := newGateServer(t, http.StatusOK, body, nil)
		code, stdout, stderr := runGateCheckWith(t, srv.URL)
		if code != 1 {
			t.Errorf("%s: exit = %d, want 1", name, code)
		}
		if stdout != "" {
			t.Errorf("%s: stdout = %q, want empty", name, stdout)
		}
		if !strings.Contains(stderr, "malformed response") {
			t.Errorf("%s: stderr = %q", name, stderr)
		}
	}
}

func TestGateCheckRequiresEnvironment(t *testing.T) {
	srv, fake := newGateServer(t, http.StatusOK, gateBodyAllow, nil)
	var stdout, stderr bytes.Buffer
	args := []string{"check", "--project", gateTestProject, "--service", gateTestService}

	t.Setenv("CERBIX_URL", srv.URL)
	t.Setenv("CERBIX_TOKEN", "")
	if code := runGate(args, &stdout, &stderr); code != 1 {
		t.Fatalf("missing token: exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "CERBIX_TOKEN") {
		t.Fatalf("missing token: stderr = %q, want the variable named", stderr.String())
	}

	stderr.Reset()
	t.Setenv("CERBIX_URL", "")
	t.Setenv("CERBIX_TOKEN", gateTestToken)
	if code := runGate(args, &stdout, &stderr); code != 1 {
		t.Fatalf("missing url: exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "CERBIX_URL") {
		t.Fatalf("missing url: stderr = %q, want the variable named", stderr.String())
	}

	for _, bad := range []string{"ftp://cerbix.example", "cerbix.example", "https://", "https://user:pw@cerbix.example", "https://cerbix.example/?x=1"} {
		stderr.Reset()
		t.Setenv("CERBIX_URL", bad)
		if code := runGate(args, &stdout, &stderr); code != 1 {
			t.Errorf("CERBIX_URL=%q: exit = %d, want 1", bad, code)
		}
		if !strings.Contains(stderr.String(), "CERBIX_URL") {
			t.Errorf("CERBIX_URL=%q: stderr = %q", bad, stderr.String())
		}
	}
	if n := fake.hits.Load(); n != 0 {
		t.Fatalf("server saw %d requests, want 0 (environment is validated before any request)", n)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

// D16: the credential is environment only, never a flag.
func TestGateCheckRejectsTokenAndURLFlags(t *testing.T) {
	srv, fake := newGateServer(t, http.StatusOK, gateBodyAllow, nil)
	code, _, stderr := runGateCheckWith(t, srv.URL, "--token", "x")
	if code != 2 {
		t.Fatalf("--token: exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "flag provided but not defined: -token") {
		t.Fatalf("--token: stderr = %q, want the flag error", stderr)
	}
	code, _, stderr = runGateCheckWith(t, srv.URL, "--url", "http://x")
	if code != 2 {
		t.Fatalf("--url: exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "flag provided but not defined: -url") {
		t.Fatalf("--url: stderr = %q", stderr)
	}
	if n := fake.hits.Load(); n != 0 {
		t.Fatalf("server saw %d requests, want 0", n)
	}
}

func TestGateCheckUsageErrors(t *testing.T) {
	t.Setenv("CERBIX_URL", "http://127.0.0.1:1")
	t.Setenv("CERBIX_TOKEN", gateTestToken)
	var stdout, stderr bytes.Buffer
	for name, args := range map[string][]string{
		"no subcommand":     nil,
		"unknown sub":       {"frob"},
		"missing service":   {"check", "--project", "p"},
		"missing project":   {"check", "--service", "s"},
		"zero timeout":      {"check", "--project", "p", "--service", "s", "--timeout", "0"},
		"positional extra":  {"check", "--project", "p", "--service", "s", "extra"},
		"bad timeout value": {"check", "--project", "p", "--service", "s", "--timeout", "soon"},
	} {
		stderr.Reset()
		if code := runGate(args, &stdout, &stderr); code != 2 {
			t.Errorf("%s: exit = %d, want 2 (stderr=%q)", name, code, stderr.String())
		}
		if stderr.Len() == 0 {
			t.Errorf("%s: expected a diagnostic on stderr", name)
		}
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

// The verb is reachable through Main, i.e. the dispatch case is wired.
func TestMainDispatchesGate(t *testing.T) {
	srv, fake := newGateServer(t, http.StatusOK, gateBodyBlock, nil)
	t.Setenv("CERBIX_URL", srv.URL)
	t.Setenv("CERBIX_TOKEN", gateTestToken)
	t.Setenv("CERBIX_CA_FILE", "")
	if code := Main([]string{"gate", "check", "--project", gateTestProject, "--service", gateTestService}); code != 2 {
		t.Fatalf("Main(gate check) on BLOCK = %d, want 2", code)
	}
	if n := fake.hits.Load(); n != 1 {
		t.Fatalf("server saw %d requests, want 1", n)
	}
	if code := Main([]string{"gate"}); code != 2 {
		t.Fatalf("Main(gate) = %d, want 2", code)
	}
}

func TestGateCheckTransportTimeoutExitsOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body so the server notices the client hanging up (the background read
		// only starts once the body hits EOF); otherwise srv.Close waits the full timer.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	start := time.Now()
	code, stdout, stderr := runGateCheckWith(t, srv.URL, "--timeout", "200ms")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "timed out") {
		t.Fatalf("stderr = %q, want a timeout diagnostic", stderr)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %s; --timeout did not bound the request", elapsed)
	}
}

func TestGateCheckConnectionRefusedExitsOne(t *testing.T) {
	code, stdout, stderr := runGateCheckWith(t, "http://127.0.0.1:1")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "request failed") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// D16: TLS verifies by default; CERBIX_CA_FILE adds a CA; there is no skip-verify option.
func TestGateCheckTLSVerifiesAndHonoursCAFile(t *testing.T) {
	fake := &gateFakeServer{t: t, status: http.StatusOK, body: gateBodyAllow}
	srv := httptest.NewTLSServer(fake)
	t.Cleanup(srv.Close)

	code, stdout, stderr := runGateCheckWith(t, srv.URL)
	if code != 1 {
		t.Fatalf("unknown CA: exit = %d, want 1 (stderr=%q)", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("unknown CA: stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "certificate") {
		t.Fatalf("unknown CA: stderr = %q, want a certificate verification failure", stderr)
	}
	if n := fake.hits.Load(); n != 0 {
		t.Fatalf("handler ran %d times under a failed handshake, want 0", n)
	}

	caPath := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CERBIX_URL", srv.URL)
	t.Setenv("CERBIX_TOKEN", gateTestToken)
	t.Setenv("CERBIX_CA_FILE", caPath)
	var out, errb bytes.Buffer
	code = runGate([]string{"check", "--project", gateTestProject, "--service", gateTestService}, &out, &errb)
	if code != 0 {
		t.Fatalf("with CA file: exit = %d, want 0 (stderr=%q)", code, errb.String())
	}
	if out.String() != "state=ALLOW action=ALLOW decision=dec-allow\n" {
		t.Fatalf("with CA file: stdout = %q", out.String())
	}
	if n := fake.hits.Load(); n != 1 {
		t.Fatalf("server saw %d requests, want 1", n)
	}
}

func TestGateCheckCAFileErrorsAreNamed(t *testing.T) {
	srv, fake := newGateServer(t, http.StatusOK, gateBodyAllow, nil)
	t.Setenv("CERBIX_URL", srv.URL)
	t.Setenv("CERBIX_TOKEN", gateTestToken)
	args := []string{"check", "--project", gateTestProject, "--service", gateTestService}

	var stdout, stderr bytes.Buffer
	t.Setenv("CERBIX_CA_FILE", filepath.Join(t.TempDir(), "missing.pem"))
	if code := runGate(args, &stdout, &stderr); code != 1 {
		t.Fatalf("missing CA file: exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "CERBIX_CA_FILE") {
		t.Fatalf("missing CA file: stderr = %q", stderr.String())
	}

	stderr.Reset()
	junk := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CERBIX_CA_FILE", junk)
	if code := runGate(args, &stdout, &stderr); code != 1 {
		t.Fatalf("junk CA file: exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "CERBIX_CA_FILE") {
		t.Fatalf("junk CA file: stderr = %q", stderr.String())
	}
	if n := fake.hits.Load(); n != 0 {
		t.Fatalf("server saw %d requests, want 0", n)
	}
}

func TestGateReasonLineGrammar(t *testing.T) {
	for _, tc := range []struct {
		in   gateReason
		want string
	}{
		{gateReason{Code: "budget_consumed_percent", Assignment: "block", Value: []byte(`93`)}, "budget_consumed_percent (block): 93"},
		{gateReason{Code: "ticket_burn_firing", Assignment: "warn", Value: []byte(`"fast"`)}, "ticket_burn_firing (warn): fast"},
		{gateReason{Code: "never_sealed", Value: []byte(`null`)}, "never_sealed"},
		{gateReason{Code: "not_configured", Docs: "https://d/x"}, "not_configured: see https://d/x"},
		{gateReason{Code: "x", Details: "why", Docs: "https://d/y"}, "x: why (see https://d/y)"},
		{gateReason{}, "(unnamed reason)"},
	} {
		if got := tc.in.line(); got != tc.want {
			t.Errorf("line(%+v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
