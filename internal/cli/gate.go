package cli

// `cerbix gate check` — the CI/CD client of the reliability gate (FR-024, D16).
//
// This is the first verb that talks to a REMOTE cerbix over HTTP, and it is a security and
// operations surface rather than a convenience: it never opens the database, never reads a
// config file and never logs through the store logger. The server is CERBIX_URL, the credential
// is CERBIX_TOKEN — environment only; a --token or --url flag does not exist, because flags land
// in shell history and process lists — and CERBIX_CA_FILE adds one PEM CA to the system roots.
// There is no skip-verify option. The decision goes to stdout (one line, or the response JSON
// verbatim with --json); reasons and diagnostics go to stderr; the exit code follows `action`.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
)

const (
	gateDefaultTimeout = 10 * time.Second
	// gateMaxBody bounds the response the client is willing to hold; a decision is a few KiB.
	gateMaxBody = 4 << 20

	// Exit codes per D16. Usage errors share exit 2 with BLOCK, as every other verb's parse
	// error does — a pipeline that cannot even ask the gate must not proceed either.
	gateExitOK            = 0 // action ALLOW or WARN
	gateExitError         = 1 // transport, timeout, TLS, auth, 4xx/5xx, malformed response
	gateExitBlock         = 2 // action BLOCK (and usage errors)
	gateExitNotConfigured = 4 // state NOT_CONFIGURED, which has no action

	gateStateAllow         = "ALLOW"
	gateStateWarn          = "WARN"
	gateStateBlock         = "BLOCK"
	gateStateUnknown       = "UNKNOWN"
	gateStateNotConfigured = "NOT_CONFIGURED"
)

// gateReason is one `reasons[]` entry (D7). Only the fields the CLI renders are decoded; the
// server may add more without breaking this client.
type gateReason struct {
	Code       string          `json:"code"`
	Clause     string          `json:"clause,omitempty"`
	Assignment string          `json:"assignment,omitempty"`
	Value      json.RawMessage `json:"value,omitempty"`
	Details    string          `json:"details,omitempty"`
	Docs       string          `json:"docs,omitempty"`
}

type gateOverride struct {
	ID         string `json:"id"`
	ActorLabel string `json:"actor_label"`
}

// gateDecision is the subset of the D7 response the CLI needs for its stdout line and exit
// code. Unknown fields are ignored on purpose (the --json path prints the raw bytes anyway).
type gateDecision struct {
	SchemaVersion      int           `json:"schema_version"`
	DecisionID         string        `json:"decision_id"`
	EvaluatedAt        string        `json:"evaluated_at"`
	State              string        `json:"state"`
	Action             string        `json:"action,omitempty"`
	Reasons            []gateReason  `json:"reasons"`
	Override           *gateOverride `json:"override,omitempty"`
	UnoverriddenAction string        `json:"unoverridden_action,omitempty"`
}

// gateHTTPResult is what the wire returned, before any interpretation.
type gateHTTPResult struct {
	Status     int
	RetryAfter string
	Location   string
	Body       []byte
}

// runGate dispatches `cerbix gate <subcommand>`; `check` is the only subcommand in v1.
func runGate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "check" {
		if len(args) > 0 {
			_, _ = fmt.Fprintf(stderr, "gate: unknown subcommand %q\n", args[0])
		}
		_, _ = fmt.Fprintln(stderr, "usage: cerbix gate check --project <id> --service <id> [--json] [--timeout 10s]")
		return gateExitBlock
	}
	return runGateCheck(args[1:], stdout, stderr)
}

// runGateCheck implements `cerbix gate check`. Flags are parsed first (a usage error is 2),
// then the environment (a missing variable is 1 and names the variable), then ONE request.
func runGateCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gate check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	project := fs.String("project", "", "project id (required)")
	service := fs.String("service", "", "service id (required)")
	asJSON := fs.Bool("json", false, "print the API response verbatim instead of the one-line summary")
	timeout := fs.Duration("timeout", gateDefaultTimeout, "overall request deadline")
	if err := fs.Parse(args); err != nil {
		return gateExitBlock
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "gate check: unexpected argument %q\n", fs.Arg(0))
		return gateExitBlock
	}
	if *project == "" || *service == "" {
		_, _ = fmt.Fprintln(stderr, "gate check: --project and --service are required")
		return gateExitBlock
	}
	if *timeout <= 0 {
		_, _ = fmt.Fprintln(stderr, "gate check: --timeout must be positive")
		return gateExitBlock
	}

	target, err := gateTarget(os.Getenv("CERBIX_URL"), *project, *service)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cerbix: gate: %v\n", err)
		return gateExitError
	}
	token := strings.TrimSpace(os.Getenv("CERBIX_TOKEN"))
	if token == "" {
		_, _ = fmt.Fprintln(stderr, "cerbix: gate: CERBIX_TOKEN is not set (the API token that authenticates to the server; environment only, never a flag)")
		return gateExitError
	}
	client, err := gateHTTPClient(os.Getenv("CERBIX_CA_FILE"))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cerbix: gate: %v\n", err)
		return gateExitError
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	res, err := gateRequest(ctx, client, target, token)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			_, _ = fmt.Fprintf(stderr, "cerbix: gate: request timed out after %s (--timeout)\n", *timeout)
		} else {
			_, _ = fmt.Fprintf(stderr, "cerbix: gate: request failed: %v\n", err)
		}
		return gateExitError
	}
	return gateOutcome(res, *asJSON, stdout, stderr)
}

// gateTarget resolves CERBIX_URL into the decision endpoint. A trailing slash and a path prefix
// (a reverse proxy mounting cerbix under a sub-path) are tolerated; anything that is not a plain
// http(s) base URL is refused by name so the operator knows which variable to fix.
func gateTarget(base, project, service string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return "", errors.New("CERBIX_URL is not set (the server base URL, e.g. https://cerbix.example.com)")
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("CERBIX_URL is not a valid URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("CERBIX_URL must be an http:// or https:// base URL, got %q", base)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("CERBIX_URL must be a plain base URL without credentials, query or fragment (the credential is CERBIX_TOKEN)")
	}
	// JoinPath takes already-escaped elements and cleans doubled slashes, so an id holding a
	// '/' or '..' cannot change the route.
	target := u.JoinPath("api", "v1", "projects", url.PathEscape(project), "services", url.PathEscape(service), "gate")
	return target.String(), nil
}

// gateHTTPClient builds the dedicated client: system roots (plus CERBIX_CA_FILE when set),
// TLS >= 1.2, and NO redirect following — a redirect would either carry the bearer token to a
// host the operator did not name or turn the POST into a GET; the operator sets CERBIX_URL to
// the final address instead.
func gateHTTPClient(caFile string) (*http.Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pemBytes, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("CERBIX_CA_FILE: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system root certificates: %w", err)
		}
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("CERBIX_CA_FILE %q holds no PEM-encoded certificate", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       tlsCfg,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

// gateRequest performs exactly one POST and returns what came back. It never retries: a 429 is
// the load the limit exists to shed (D16), and a decision is not idempotent to re-ask.
func gateRequest(ctx context.Context, client *http.Client, target, token string) (*gateHTTPResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cerbix-cli/"+buildinfo.Current().Version)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, gateMaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(body) > gateMaxBody {
		return nil, fmt.Errorf("response body exceeds %d bytes", gateMaxBody)
	}
	return &gateHTTPResult{
		Status:     resp.StatusCode,
		RetryAfter: resp.Header.Get("Retry-After"),
		Location:   resp.Header.Get("Location"),
		Body:       body,
	}, nil
}

// gateOutcome turns the wire result into output and an exit code.
func gateOutcome(res *gateHTTPResult, asJSON bool, stdout, stderr io.Writer) int {
	switch {
	case res.Status == http.StatusOK || res.Status == http.StatusCreated:
		return gateRenderDecision(res.Body, asJSON, stdout, stderr)
	case res.Status == http.StatusTooManyRequests:
		_, _ = fmt.Fprintf(stderr, "cerbix: gate: 429 %s\n", gateErrorText(res))
		if res.RetryAfter != "" {
			_, _ = fmt.Fprintf(stderr, "Retry-After: %s\n", res.RetryAfter)
		}
		return gateExitError
	case res.Status >= 300 && res.Status < 400:
		_, _ = fmt.Fprintf(stderr, "cerbix: gate: %d redirect to %q not followed; set CERBIX_URL to the final address\n", res.Status, res.Location)
		return gateExitError
	default:
		_, _ = fmt.Fprintf(stderr, "cerbix: gate: %d %s\n", res.Status, gateErrorText(res))
		return gateExitError
	}
}

// gateErrorText is the server's `{"error": …}` field when the body has that shape (every
// writeError does), else the HTTP status text.
func gateErrorText(res *gateHTTPResult) string {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(res.Body, &payload); err == nil && payload.Error != "" {
		return payload.Error
	}
	return http.StatusText(res.Status)
}

// gateRenderDecision decodes a 2xx body, writes reasons to stderr and the decision to stdout,
// and maps state/action to the exit code. Unknown fields are ignored; a `state` outside the
// five of D4, or a missing/unknown `action` on a state that must carry one, is a malformed
// response (exit 1) because the exit code would otherwise be undefined.
func gateRenderDecision(body []byte, asJSON bool, stdout, stderr io.Writer) int {
	var d gateDecision
	if err := json.Unmarshal(body, &d); err != nil {
		_, _ = fmt.Fprintf(stderr, "cerbix: gate: malformed response: %v\n", err)
		return gateExitError
	}
	switch d.State {
	case gateStateAllow, gateStateWarn, gateStateBlock, gateStateUnknown, gateStateNotConfigured:
	default:
		_, _ = fmt.Fprintf(stderr, "cerbix: gate: malformed response: unknown state %q\n", d.State)
		return gateExitError
	}
	if d.State != gateStateNotConfigured {
		switch d.Action {
		case gateStateAllow, gateStateWarn, gateStateBlock:
		default:
			_, _ = fmt.Fprintf(stderr, "cerbix: gate: malformed response: state %s with action %q\n", d.State, d.Action)
			return gateExitError
		}
	}

	for _, r := range d.Reasons {
		_, _ = fmt.Fprintln(stderr, r.line())
	}

	if asJSON {
		// Byte-identical to the API response (D16, §7 CLI): exactly the body bytes, nothing
		// appended — not even a newline. A consumer that diffs, hashes or signs the output must
		// see what the server sent; a shell that wants a newline pipes through one.
		_, _ = stdout.Write(body)
	} else {
		_, _ = fmt.Fprintln(stdout, d.summaryLine())
	}

	switch {
	case d.State == gateStateNotConfigured:
		return gateExitNotConfigured
	case d.Action == gateStateBlock:
		return gateExitBlock
	default:
		return gateExitOK
	}
}

// summaryLine is the stdout grammar:
//
//	state=<STATE> [action=<ACTION>] [override=<actor_label>] decision=<decision_id>
//
// `action=` only when the response carries one (never for NOT_CONFIGURED), `override=` only
// when an override was applied. UNKNOWN stays visible as the state whatever the action.
func (d gateDecision) summaryLine() string {
	var b strings.Builder
	b.WriteString("state=")
	b.WriteString(d.State)
	if d.Action != "" {
		b.WriteString(" action=")
		b.WriteString(d.Action)
	}
	if d.Override != nil {
		label := d.Override.ActorLabel
		if label == "" {
			label = d.Override.ID
		}
		b.WriteString(" override=")
		b.WriteString(label)
	}
	b.WriteString(" decision=")
	b.WriteString(d.DecisionID)
	return b.String()
}

// line renders one reason for stderr:
//
//	<code>[ (<assignment>)][: <value or details>][ see <docs>]
//
// e.g. `budget_consumed_percent (block): 93` or `not_configured: see https://…`.
func (r gateReason) line() string {
	var b strings.Builder
	if r.Code == "" {
		b.WriteString("(unnamed reason)")
	} else {
		b.WriteString(r.Code)
	}
	if r.Assignment != "" {
		b.WriteString(" (")
		b.WriteString(r.Assignment)
		b.WriteString(")")
	}
	detail := gateScalar(r.Value)
	if detail == "" {
		detail = r.Details
	}
	switch {
	case detail != "" && r.Docs != "":
		b.WriteString(": ")
		b.WriteString(detail)
		b.WriteString(" (see ")
		b.WriteString(r.Docs)
		b.WriteString(")")
	case detail != "":
		b.WriteString(": ")
		b.WriteString(detail)
	case r.Docs != "":
		b.WriteString(": see ")
		b.WriteString(r.Docs)
	}
	return b.String()
}

// gateScalar renders a reason's `value` for humans: JSON strings unquoted, null/absent as
// empty, everything else (numbers, booleans, objects) as received.
func gateScalar(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := json.Unmarshal(raw, &str); err == nil {
			return str
		}
	}
	return s
}
