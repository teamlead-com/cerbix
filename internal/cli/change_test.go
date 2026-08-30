package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// `cerbix change record` (FR-025 D13, invariant 18, §7 "CLI") against a fake server, the way the
// gate verb's tests run: the request contract asserted on every hit, the hit count a number,
// stdout/stderr captured, the exit code the subject.

const (
	changeTestProject = "p-1"
	changeTestService = "s-1"
	changeTestToken   = "cbx_change_token_456"
	changeTestPath    = "/api/v1/projects/p-1/services/s-1/changes"
)

// changeFakeServer is the record endpoint as the CLI must see it: it asserts method, path, bearer
// header, content types and user agent, keeps the LAST decoded request body for the test to
// inspect (the default `occurred_at` varies), counts hits, and answers a canned status/body.
type changeFakeServer struct {
	t        *testing.T
	status   int
	body     string
	headers  map[string]string
	wantPath string
	hits     atomic.Int32

	mu       sync.Mutex
	lastBody map[string]any
	lastRaw  []byte
}

func (f *changeFakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.hits.Add(1)
	if r.Method != http.MethodPost {
		f.t.Errorf("method = %s, want POST", r.Method)
	}
	wantPath := f.wantPath
	if wantPath == "" {
		wantPath = changeTestPath
	}
	// EscapedPath, not Path: the route must see the ids ESCAPED (a '/' inside one cannot add
	// a segment), and Path is the decoded form.
	if got := r.URL.EscapedPath(); got != wantPath {
		f.t.Errorf("path = %q, want %q", got, wantPath)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+changeTestToken {
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
	raw, _ := io.ReadAll(r.Body)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		f.t.Errorf("request body %q is not a JSON object: %v", raw, err)
	}
	f.mu.Lock()
	f.lastBody, f.lastRaw = decoded, raw
	f.mu.Unlock()
	for k, v := range f.headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.status)
	_, _ = io.WriteString(w, f.body)
}

func (f *changeFakeServer) last() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastBody
}

func newChangeServer(t *testing.T, status int, body string, headers map[string]string) (*httptest.Server, *changeFakeServer) {
	t.Helper()
	fake := &changeFakeServer{t: t, status: status, body: body, headers: headers}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return srv, fake
}

// changeRequiredArgs is the minimal valid invocation; tests append to it.
func changeRequiredArgs() []string {
	return []string{
		"record", "--project", changeTestProject, "--service", changeTestService,
		"--kind", "deploy", "--phase", "succeeded", "--source", "github-actions", "--external-id", "9177",
	}
}

// runChangeRecordWith runs the verb against baseURL with the standard env and returns the exit
// code and the two streams.
func runChangeRecordWith(t *testing.T, baseURL string, extra ...string) (int, string, string) {
	t.Helper()
	t.Setenv("CERBIX_URL", baseURL)
	t.Setenv("CERBIX_TOKEN", changeTestToken)
	t.Setenv("CERBIX_CA_FILE", "")
	var stdout, stderr bytes.Buffer
	code := runChange(append(changeRequiredArgs(), extra...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// The server's writeJSON ends every body with a newline (json.Encoder); the fixtures carry it so
// the --json assertions see what a live server sends.
const (
	changeBodyRecorded = `{"replayed":false,"change":{"service_id":"s-1","source":"github-actions","external_id":"9177","kind":"deploy","id":"chg-1","phase":"succeeded","occurred_at":"2026-08-30T10:00:00Z","ref":"v1.2.3","url":"","decision_id":null,"actor_label":"token:deploy-bot","actor_user_id":null,"via_token":true,"recorded_at":"2026-08-30T10:00:01Z"}}` + "\n"
	changeBodyReplayed = `{"replayed":true,"change":{"service_id":"s-1","source":"github-actions","external_id":"9177","kind":"deploy","id":"chg-1","phase":"succeeded","occurred_at":"2026-08-30T10:00:00Z","ref":"v1.2.3","url":"","actor_label":"token:deploy-bot","actor_user_id":null,"via_token":true,"recorded_at":"2026-08-30T10:00:01Z"}}` + "\n"
)

// ── exit 0: recorded, replayed ───────────────────────────────────────────────────────────────

func TestChangeRecordRecordedExitsZeroWithOneLine(t *testing.T) {
	srv, fake := newChangeServer(t, http.StatusCreated, changeBodyRecorded, nil)
	before := time.Now().UTC()
	code, stdout, stderr := runChangeRecordWith(t, srv.URL)
	after := time.Now().UTC()
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr)
	}
	if stdout != "recorded change=chg-1 kind=deploy phase=succeeded\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	if n := fake.hits.Load(); n != 1 {
		t.Fatalf("server saw %d requests, want 1", n)
	}

	// The body: the four required fields as given, occurred_at defaulted to the invocation
	// instant (D13), and NO ref/url/decision_id keys — absent, not empty strings.
	body := fake.last()
	for k, want := range map[string]string{"kind": "deploy", "phase": "succeeded", "source": "github-actions", "external_id": "9177"} {
		if got, _ := body[k].(string); got != want {
			t.Errorf("body[%s] = %v, want %q", k, body[k], want)
		}
	}
	for _, k := range []string{"ref", "url", "decision_id"} {
		if _, present := body[k]; present {
			t.Errorf("body has %q = %v; want the key absent when the flag is not given", k, body[k])
		}
	}
	if len(body) != 5 {
		t.Errorf("body has %d keys %v, want exactly kind, phase, occurred_at, source, external_id", len(body), body)
	}
	occurred, _ := body["occurred_at"].(string)
	at, err := time.Parse(time.RFC3339, occurred)
	if err != nil {
		t.Fatalf("occurred_at = %q is not RFC3339: %v", occurred, err)
	}
	if !strings.HasSuffix(occurred, "Z") {
		t.Errorf("occurred_at = %q, want UTC (Z)", occurred)
	}
	// RFC3339 carries seconds, so the instant is bounded by the run's start floored to the second
	// and its end: the invocation instant, within a second.
	if at.Before(before.Truncate(time.Second)) || at.After(after) {
		t.Errorf("occurred_at = %s, want the invocation instant (between %s and %s)", at, before, after)
	}
}

func TestChangeRecordReplayedExitsZero(t *testing.T) {
	srv, _ := newChangeServer(t, http.StatusOK, changeBodyReplayed, nil)
	code, stdout, stderr := runChangeRecordWith(t, srv.URL)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr)
	}
	if stdout != "replayed change=chg-1 kind=deploy phase=succeeded\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

// The stdout line carries the server's canonical kind/phase, not the CLI's spelling, and the word
// follows the body's `replayed` rather than the status code.
func TestChangeRecordSummaryFollowsTheBody(t *testing.T) {
	body := `{"replayed":true,"change":{"id":"chg-7","kind":"rollback","phase":"failed"}}`
	srv, _ := newChangeServer(t, http.StatusCreated, body, nil)
	code, stdout, _ := runChangeRecordWith(t, srv.URL)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout != "replayed change=chg-7 kind=rollback phase=failed\n" {
		t.Fatalf("stdout = %q", stdout)
	}
}

// ── the body: optional flags travel verbatim, nothing is validated locally (D2) ───────────────

func TestChangeRecordOptionalFlagsTravelVerbatim(t *testing.T) {
	srv, fake := newChangeServer(t, http.StatusCreated, changeBodyRecorded, nil)
	code, _, stderr := runChangeRecordWith(t, srv.URL,
		"--ref", "v1.2.3", "--url", "https://ci.example/run/9177", "--decision", "dec-42", "--at", "2026-08-30T09:58:00+02:00")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr)
	}
	body := fake.last()
	for k, want := range map[string]string{
		"ref": "v1.2.3", "url": "https://ci.example/run/9177", "decision_id": "dec-42",
		"occurred_at": "2026-08-30T09:58:00+02:00", // as given — not re-rendered in UTC, not re-parsed
	} {
		if got, _ := body[k].(string); got != want {
			t.Errorf("body[%s] = %v, want %q", k, body[k], want)
		}
	}
	if len(body) != 8 {
		t.Errorf("body has %d keys %v, want the eight of D2", len(body), body)
	}
}

// D2: the transport normalizes and the domain validates; the CLI sends what it was given. A kind
// outside the enum, a phase with stray whitespace, an http:// url and an unparseable --at all
// reach the server, whose refusal is printed verbatim with exit 2 — the CLI holds no copy of the
// rules.
func TestChangeRecordSendsWhatItWasGiven(t *testing.T) {
	srv, fake := newChangeServer(t, http.StatusBadRequest, `{"error":"kind_invalid (kind): must be one of deploy|rollback|flag, got \"Deploy\""}`, nil)
	t.Setenv("CERBIX_URL", srv.URL)
	t.Setenv("CERBIX_TOKEN", changeTestToken)
	t.Setenv("CERBIX_CA_FILE", "")
	var stdout, stderr bytes.Buffer
	code := runChange([]string{
		"record", "--project", changeTestProject, "--service", changeTestService,
		"--kind", "Deploy", "--phase", " succeeded ", "--source", "GitHub Actions", "--external-id", "9177",
		"--url", "http://plain.example", "--at", "yesterday",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (the server refused)", code)
	}
	if n := fake.hits.Load(); n != 1 {
		t.Fatalf("server saw %d requests, want 1 — the CLI must not refuse locally", n)
	}
	body := fake.last()
	for k, want := range map[string]string{"kind": "Deploy", "phase": " succeeded ", "source": "GitHub Actions", "url": "http://plain.example", "occurred_at": "yesterday"} {
		if got, _ := body[k].(string); got != want {
			t.Errorf("body[%s] = %v, want %q verbatim", k, body[k], want)
		}
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if stderr.String() != "cerbix: change: 400 kind_invalid (kind): must be one of deploy|rollback|flag, got \"Deploy\"\n" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// ── exit 2: refused by the contract (400/404/409) ────────────────────────────────────────────

func TestChangeRecord409PhaseOrderExitsTwoVerbatim(t *testing.T) {
	srv, fake := newChangeServer(t, http.StatusConflict, `{"error":"phase_order (phase): succeeded already recorded for github-actions/9177"}`+"\n", nil)
	code, stdout, stderr := runChangeRecordWith(t, srv.URL)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if stderr != "cerbix: change: 409 phase_order (phase): succeeded already recorded for github-actions/9177\n" {
		t.Fatalf("stderr = %q, want the server's error verbatim", stderr)
	}
	if n := fake.hits.Load(); n != 1 {
		t.Fatalf("server saw %d requests, want 1", n)
	}
}

func TestChangeRecordContractRefusalsExitTwo(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusBadRequest, `{"error":"url_invalid (url): must be empty or an absolute https:// URL"}`, "cerbix: change: 400 url_invalid (url): must be empty or an absolute https:// URL\n"},
		{http.StatusBadRequest, `{"error":"actor: unknown field"}`, "cerbix: change: 400 actor: unknown field\n"},
		{http.StatusBadRequest, `{"error":"occurred_at_out_of_bounds (occurred_at): more than 24h0m0s behind the server clock"}`, "cerbix: change: 400 occurred_at_out_of_bounds (occurred_at): more than 24h0m0s behind the server clock\n"},
		{http.StatusNotFound, `{"error":"not found"}`, "cerbix: change: 404 not found\n"},
		{http.StatusConflict, `{"error":"phase_exists (ref): differs from the recorded row"}`, "cerbix: change: 409 phase_exists (ref): differs from the recorded row\n"},
		{http.StatusConflict, `{"error":"kind_mismatch (kind): group is deploy"}`, "cerbix: change: 409 kind_mismatch (kind): group is deploy\n"},
		{http.StatusNotFound, `<html>nginx</html>`, "cerbix: change: 404 Not Found\n"},
	} {
		srv, _ := newChangeServer(t, tc.status, tc.body, nil)
		code, stdout, stderr := runChangeRecordWith(t, srv.URL)
		if code != 2 {
			t.Errorf("%d %q: exit = %d, want 2", tc.status, tc.body, code)
		}
		if stdout != "" {
			t.Errorf("%d: stdout = %q, want empty", tc.status, stdout)
		}
		if stderr != tc.want {
			t.Errorf("%d: stderr = %q, want %q", tc.status, stderr, tc.want)
		}
	}
}

// ── exit 1: the transport ────────────────────────────────────────────────────────────────────

func TestChangeRecordTransportStatusesExitOne(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusUnauthorized, `{"error":"unauthorized"}`, "cerbix: change: 401 unauthorized\n"},
		{http.StatusForbidden, `{"error":"forbidden"}`, "cerbix: change: 403 forbidden\n"},
		{http.StatusMethodNotAllowed, ``, "cerbix: change: 405 Method Not Allowed\n"},
		{http.StatusInternalServerError, `<html>boom</html>`, "cerbix: change: 500 Internal Server Error\n"},
		{http.StatusNotImplemented, `{"error":"change_not_wired"}`, "cerbix: change: 501 change_not_wired\n"},
		{http.StatusServiceUnavailable, `{"error":"snapshot_conflict"}`, "cerbix: change: 503 snapshot_conflict\n"},
		{http.StatusBadGateway, ``, "cerbix: change: 502 Bad Gateway\n"},
	} {
		srv, _ := newChangeServer(t, tc.status, tc.body, nil)
		code, stdout, stderr := runChangeRecordWith(t, srv.URL)
		if code != 1 {
			t.Errorf("%d: exit = %d, want 1", tc.status, code)
		}
		if stdout != "" {
			t.Errorf("%d: stdout = %q, want empty", tc.status, stdout)
		}
		if stderr != tc.want {
			t.Errorf("%d: stderr = %q, want %q", tc.status, stderr, tc.want)
		}
	}
}

func TestChangeRecord429PrintsRetryAfterAndNeverRetries(t *testing.T) {
	srv, fake := newChangeServer(t, http.StatusTooManyRequests, `{"error":"principal_rate"}`+"\n", map[string]string{"Retry-After": "3"})
	code, stdout, stderr := runChangeRecordWith(t, srv.URL)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if stderr != "cerbix: change: 429 principal_rate\nRetry-After: 3\n" {
		t.Fatalf("stderr = %q, want the status, the code and the seconds", stderr)
	}
	if n := fake.hits.Load(); n != 1 {
		t.Fatalf("server saw %d requests, want exactly 1 (no retry)", n)
	}
}

func TestChangeRecordTransportTimeoutExitsOne(t *testing.T) {
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
	code, stdout, stderr := runChangeRecordWith(t, srv.URL, "--timeout", "200ms")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if stderr != "cerbix: change: request timed out after 200ms (--timeout)\n" {
		t.Fatalf("stderr = %q, want the timeout diagnostic", stderr)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %s; --timeout did not bound the request", elapsed)
	}
}

func TestChangeRecordConnectionRefusedExitsOne(t *testing.T) {
	code, stdout, stderr := runChangeRecordWith(t, "http://127.0.0.1:1")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.HasPrefix(stderr, "cerbix: change: request failed: ") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestChangeRecordDoesNotFollowRedirects(t *testing.T) {
	srv, fake := newChangeServer(t, http.StatusFound, "", map[string]string{"Location": "https://elsewhere.example/"})
	code, stdout, stderr := runChangeRecordWith(t, srv.URL)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "302") || !strings.Contains(stderr, "not followed") {
		t.Fatalf("stderr = %q", stderr)
	}
	if n := fake.hits.Load(); n != 1 {
		t.Fatalf("server saw %d requests, want 1", n)
	}
}

func TestChangeRecordMalformedResponsesExitOne(t *testing.T) {
	for name, body := range map[string]string{
		"not json":     `<html>ok</html>`,
		"empty":        ``,
		"no change":    `{"replayed":false}`,
		"no id":        `{"replayed":false,"change":{"kind":"deploy","phase":"succeeded"}}`,
		"no kind":      `{"replayed":false,"change":{"id":"chg-1","phase":"succeeded"}}`,
		"wrong shapes": `{"replayed":"yes","change":[]}`,
	} {
		srv, _ := newChangeServer(t, http.StatusCreated, body, nil)
		code, stdout, stderr := runChangeRecordWith(t, srv.URL)
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

// ── --json ───────────────────────────────────────────────────────────────────────────────────

func TestChangeRecordJSONIsByteIdenticalToResponse(t *testing.T) {
	// The live server's body ends with the encoder's newline; it is passed through — and only it.
	srv, _ := newChangeServer(t, http.StatusCreated, changeBodyRecorded, nil)
	code, stdout, stderr := runChangeRecordWith(t, srv.URL, "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr)
	}
	if stdout != changeBodyRecorded {
		t.Fatalf("stdout = %q, want the body byte for byte", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}

	// A body without a trailing newline is passed through WITHOUT one: nothing is appended (the
	// gate's rule, iter-0163). The mutation that appends a newline fails here.
	bare := strings.TrimSuffix(changeBodyReplayed, "\n")
	srv2, _ := newChangeServer(t, http.StatusOK, bare, nil)
	code, stdout, _ = runChangeRecordWith(t, srv2.URL, "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout != bare {
		t.Fatalf("stdout = %q, want the body byte for byte (no newline appended)", stdout)
	}

	// A refusal under --json: the exit follows the status, stdout stays empty, the refusal is on
	// stderr as without --json.
	srv3, _ := newChangeServer(t, http.StatusConflict, `{"error":"phase_order (phase): started after a terminal"}`+"\n", nil)
	code, stdout, stderr = runChangeRecordWith(t, srv3.URL, "--json")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty on a refusal", stdout)
	}
	if stderr != "cerbix: change: 409 phase_order (phase): started after a terminal\n" {
		t.Fatalf("stderr = %q", stderr)
	}
}

// ── environment and usage ────────────────────────────────────────────────────────────────────

// The credential and the server come from the environment only. A missing variable is exit 1 and
// names the variable — the gate verb's convention, kept so the two remote verbs agree; it is not a
// usage error (the flags were fine) and not a contract refusal (no request was made).
func TestChangeRecordRequiresEnvironment(t *testing.T) {
	srv, fake := newChangeServer(t, http.StatusCreated, changeBodyRecorded, nil)
	var stdout, stderr bytes.Buffer
	args := changeRequiredArgs()

	t.Setenv("CERBIX_URL", srv.URL)
	t.Setenv("CERBIX_TOKEN", "")
	if code := runChange(args, &stdout, &stderr); code != 1 {
		t.Fatalf("missing token: exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "CERBIX_TOKEN") {
		t.Fatalf("missing token: stderr = %q, want the variable named", stderr.String())
	}

	stderr.Reset()
	t.Setenv("CERBIX_URL", "")
	t.Setenv("CERBIX_TOKEN", changeTestToken)
	if code := runChange(args, &stdout, &stderr); code != 1 {
		t.Fatalf("missing url: exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "CERBIX_URL") {
		t.Fatalf("missing url: stderr = %q, want the variable named", stderr.String())
	}

	for _, bad := range []string{"ftp://cerbix.example", "cerbix.example", "https://", "https://user:pw@cerbix.example", "https://cerbix.example/?x=1", "https://cerbix.example/#f"} {
		stderr.Reset()
		t.Setenv("CERBIX_URL", bad)
		if code := runChange(args, &stdout, &stderr); code != 1 {
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

// D13: the credential is environment only, never a flag. `--url` EXISTS on this verb — it is the
// change's link, a body field — so it must land in the body and never move the target.
func TestChangeRecordRejectsTokenFlagAndKeepsURLInTheBody(t *testing.T) {
	srv, fake := newChangeServer(t, http.StatusCreated, changeBodyRecorded, nil)
	code, _, stderr := runChangeRecordWith(t, srv.URL, "--token", "x")
	if code != 2 {
		t.Fatalf("--token: exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "flag provided but not defined: -token") {
		t.Fatalf("--token: stderr = %q, want the flag error", stderr)
	}
	if n := fake.hits.Load(); n != 0 {
		t.Fatalf("server saw %d requests, want 0", n)
	}

	code, _, stderr = runChangeRecordWith(t, srv.URL, "--url", "https://other.example/run/1")
	if code != 0 {
		t.Fatalf("--url: exit = %d, want 0; stderr=%q", code, stderr)
	}
	if n := fake.hits.Load(); n != 1 {
		t.Fatalf("server (CERBIX_URL) saw %d requests, want 1 — --url is the change's link, not the server", n)
	}
	if got, _ := fake.last()["url"].(string); got != "https://other.example/run/1" {
		t.Fatalf("body[url] = %q", got)
	}
}

func TestChangeRecordUsageErrors(t *testing.T) {
	t.Setenv("CERBIX_URL", "http://127.0.0.1:1")
	t.Setenv("CERBIX_TOKEN", changeTestToken)
	base := []string{"record", "--project", "p", "--service", "s", "--kind", "deploy", "--phase", "started", "--source", "ci", "--external-id", "1"}
	without := func(flag string) []string {
		out := []string{}
		for i := 0; i < len(base); i++ {
			if base[i] == flag {
				i++ // skip the value too
				continue
			}
			out = append(out, base[i])
		}
		return out
	}
	var stdout, stderr bytes.Buffer
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no subcommand":       {nil, "usage: cerbix change record"},
		"unknown sub":         {[]string{"frob"}, `unknown subcommand "frob"`},
		"missing project":     {without("--project"), "--project is required"},
		"missing service":     {without("--service"), "--service is required"},
		"missing kind":        {without("--kind"), "--kind is required"},
		"missing phase":       {without("--phase"), "--phase is required"},
		"missing source":      {without("--source"), "--source is required"},
		"missing external-id": {without("--external-id"), "--external-id is required"},
		"only record":         {[]string{"record"}, "--project, --service, --kind, --phase, --source, --external-id are required"},
		"zero timeout":        {append(without(""), "--timeout", "0"), "--timeout must be positive"},
		"bad timeout value":   {append(without(""), "--timeout", "soon"), "invalid value"},
		"positional extra":    {append(without(""), "extra"), `unexpected argument "extra"`},
		"undefined flag":      {append(without(""), "--actor", "me"), "flag provided but not defined: -actor"},
	} {
		stderr.Reset()
		if code := runChange(tc.args, &stdout, &stderr); code != 2 {
			t.Errorf("%s: exit = %d, want 2 (stderr=%q)", name, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%s: stderr = %q, want it to contain %q", name, stderr.String(), tc.want)
		}
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

// ── the token never leaves the Authorization header ──────────────────────────────────────────

func TestChangeRecordTokenNeverPrinted(t *testing.T) {
	check := func(name string, code int, stdout, stderr string) {
		t.Helper()
		if strings.Contains(stdout, changeTestToken) || strings.Contains(stderr, changeTestToken) {
			t.Errorf("%s (exit %d): the token appears in the output; stdout=%q stderr=%q", name, code, stdout, stderr)
		}
	}
	srv, _ := newChangeServer(t, http.StatusCreated, changeBodyRecorded, nil)
	code, stdout, stderr := runChangeRecordWith(t, srv.URL, "--json")
	check("recorded --json", code, stdout, stderr)

	for name, tc := range map[string]struct {
		status  int
		body    string
		headers map[string]string
	}{
		"401":      {http.StatusUnauthorized, `{"error":"unauthorized"}`, nil},
		"409":      {http.StatusConflict, `{"error":"phase_order (phase): x"}`, nil},
		"429":      {http.StatusTooManyRequests, `{"error":"principal_rate"}`, map[string]string{"Retry-After": "1"}},
		"redirect": {http.StatusFound, ``, map[string]string{"Location": "https://elsewhere.example/"}},
		"500":      {http.StatusInternalServerError, `boom`, nil},
	} {
		s, _ := newChangeServer(t, tc.status, tc.body, tc.headers)
		code, stdout, stderr = runChangeRecordWith(t, s.URL)
		check(name, code, stdout, stderr)
	}
	code, stdout, stderr = runChangeRecordWith(t, "http://127.0.0.1:1")
	check("connection refused", code, stdout, stderr)
	code, stdout, stderr = runChangeRecordWith(t, srv.URL, "--bogus")
	check("usage", code, stdout, stderr)
}

// ── routing, TLS, dispatch ───────────────────────────────────────────────────────────────────

func TestChangeRecordToleratesTrailingSlashAndPathPrefix(t *testing.T) {
	srv, fake := newChangeServer(t, http.StatusCreated, changeBodyRecorded, nil)
	if code, _, stderr := runChangeRecordWith(t, srv.URL+"/"); code != 0 {
		t.Fatalf("trailing slash: exit = %d; stderr=%q", code, stderr)
	}
	fake.wantPath = "/cerbix" + changeTestPath
	if code, _, stderr := runChangeRecordWith(t, srv.URL+"/cerbix/"); code != 0 {
		t.Fatalf("path prefix: exit = %d; stderr=%q", code, stderr)
	}
}

// An id holding a slash cannot change the route: it is path-escaped, as the gate's are.
func TestChangeRecordEscapesPathIDs(t *testing.T) {
	srv, fake := newChangeServer(t, http.StatusCreated, changeBodyRecorded, nil)
	fake.wantPath = "/api/v1/projects/p%2F..%2Fx/services/s-1/changes"
	t.Setenv("CERBIX_URL", srv.URL)
	t.Setenv("CERBIX_TOKEN", changeTestToken)
	t.Setenv("CERBIX_CA_FILE", "")
	var stdout, stderr bytes.Buffer
	args := changeRequiredArgs()
	args[2] = "p/../x"
	if code := runChange(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr.String())
	}
}

// The shared client verifies TLS by default; there is no skip-verify option on this verb either.
func TestChangeRecordTLSVerifies(t *testing.T) {
	fake := &changeFakeServer{t: t, status: http.StatusCreated, body: changeBodyRecorded}
	srv := httptest.NewTLSServer(fake)
	t.Cleanup(srv.Close)
	code, stdout, stderr := runChangeRecordWith(t, srv.URL)
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
}

// The verb is reachable through Main, i.e. the dispatch case is wired, and the usage text names it.
func TestMainDispatchesChange(t *testing.T) {
	srv, fake := newChangeServer(t, http.StatusConflict, `{"error":"phase_order (phase): x"}`, nil)
	t.Setenv("CERBIX_URL", srv.URL)
	t.Setenv("CERBIX_TOKEN", changeTestToken)
	t.Setenv("CERBIX_CA_FILE", "")
	if code := Main(append([]string{"change"}, changeRequiredArgs()...)); code != 2 {
		t.Fatalf("Main(change record) on 409 = %d, want 2", code)
	}
	if n := fake.hits.Load(); n != 1 {
		t.Fatalf("server saw %d requests, want 1", n)
	}
	if code := Main([]string{"change"}); code != 2 {
		t.Fatalf("Main(change) = %d, want 2", code)
	}
	var u bytes.Buffer
	usage(&u)
	if !strings.Contains(u.String(), "cerbix change record --project <id> --service <id> --kind deploy|rollback|flag") {
		t.Fatalf("usage lacks the change verb: %q", u.String())
	}
}

// Review [52]: a flag EXPLICITLY given travels as given, even empty — `--at ""` is not the
// invocation instant, and `--decision ""` is not an omission.
func TestChangeRecordExplicitEmptyAtAndDecisionTravelVerbatim(t *testing.T) {
	t.Run("at empty travels empty", func(t *testing.T) {
		srv, fake := newChangeServer(t, http.StatusBadRequest, `{"error":"occurred_at: must be an RFC3339 timestamp"}`+"\n", nil)
		code, _, stderr := runChangeRecordWith(t, srv.URL, "--at", "")
		if code != changeExitRefused {
			t.Fatalf("exit = %d, want %d; stderr %q", code, changeExitRefused, stderr)
		}
		if got, ok := fake.last()["occurred_at"]; !ok || got != "" {
			t.Fatalf("occurred_at = %v (present %v), want the explicit empty string", got, ok)
		}
	})
	t.Run("decision empty travels empty", func(t *testing.T) {
		srv, fake := newChangeServer(t, http.StatusBadRequest, `{"error":"decision_unknown (decision_id): not a decision of this service"}`+"\n", nil)
		code, _, _ := runChangeRecordWith(t, srv.URL, "--decision", "")
		if code != changeExitRefused {
			t.Fatalf("exit = %d, want %d", code, changeExitRefused)
		}
		if got, ok := fake.last()["decision_id"]; !ok || got != "" {
			t.Fatalf("decision_id = %v (present %v), want the explicit empty string", got, ok)
		}
	})
	t.Run("decision omitted stays absent", func(t *testing.T) {
		srv, fake := newChangeServer(t, http.StatusCreated, changeBodyRecorded, nil)
		code, _, _ := runChangeRecordWith(t, srv.URL)
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if _, ok := fake.last()["decision_id"]; ok {
			t.Fatal("decision_id present in the body although --decision was never given")
		}
	})
	t.Run("at given travels verbatim", func(t *testing.T) {
		srv, fake := newChangeServer(t, http.StatusCreated, changeBodyRecorded, nil)
		if code, _, _ := runChangeRecordWith(t, srv.URL, "--at", "2026-08-30T10:00:00Z"); code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if got := fake.last()["occurred_at"]; got != "2026-08-30T10:00:00Z" {
			t.Fatalf("occurred_at = %v, want the given instant", got)
		}
	})
}
