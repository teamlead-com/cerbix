package cli

// `cerbix change record` — the CI/CD client of change intelligence (FR-025, D13).
//
// The twin of `cerbix gate check` (FR-024 D16), verb for verb: it never opens the database, never
// reads a config file and never logs through the store logger. The server is CERBIX_URL, the
// credential is CERBIX_TOKEN — environment only; a --token flag does not exist, because flags land
// in shell history and process lists — and CERBIX_CA_FILE adds one PEM CA to the system roots.
// There is no skip-verify option. The record goes to stdout (one line, or the response JSON
// verbatim with --json); refusals and diagnostics go to stderr; the exit code follows D13.
//
// What differs from the gate is the exit-code table, which D13 fixes: a refusal by the CONTRACT
// (400/404/409 — the pipeline's own mistake, printed verbatim) is 2, so a CI step can tell "I sent
// something wrong" from "the server was unreachable" (1) without parsing stderr.
//
// The CLI is a thin client by design (D2): the transport normalizes and the domain validates, so
// this verb sends `--kind`, `--phase`, `--source`, `--external-id`, `--at` and the rest exactly as
// given and prints the server's refusal — it holds no copy of the enums, and extending one is a
// server-side schema decision that needs no CLI release.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
)

const (
	changeDefaultTimeout = 10 * time.Second
	// changeMaxBody bounds the response the client is willing to hold; a record is a few KiB.
	changeMaxBody = 4 << 20

	// Exit codes per D13. Usage errors share exit 2 with a contract refusal, as the gate verb's
	// share it with BLOCK: a pipeline that cannot even phrase the request has made its own mistake.
	changeExitOK      = 0 // 201 recorded or 200 replayed
	changeExitError   = 1 // transport, timeout, TLS, auth (401/403), 429, 5xx, malformed response
	changeExitRefused = 2 // 400/404/409 — refused by the contract (and usage errors)

	changeUsage = "usage: cerbix change record --project <id> --service <id> --kind deploy|rollback|flag --phase started|succeeded|failed|cancelled --source <slug> --external-id <id> [--ref <label>] [--url <https url>] [--decision <id>] [--at <RFC3339>] [--json] [--timeout 10s]"
)

// changeRecordBody is the POST …/changes body (D2), field for field. `ref`, `url` and
// `decision_id` are OMITTED when their flag was not given — the server defaults them, and for
// `decision_id` an empty string is not the same statement as absence.
type changeRecordBody struct {
	Kind       string `json:"kind"`
	Phase      string `json:"phase"`
	OccurredAt string `json:"occurred_at"`
	Source     string `json:"source"`
	ExternalID string `json:"external_id"`
	Ref        string `json:"ref,omitempty"`
	URL        string `json:"url,omitempty"`
	DecisionID string `json:"decision_id,omitempty"`
}

// changeRecorded is the subset of the 2xx response the CLI needs for its stdout line. Unknown
// fields are ignored on purpose (the --json path prints the raw bytes anyway).
type changeRecorded struct {
	Replayed bool `json:"replayed"`
	Change   struct {
		ID    string `json:"id"`
		Kind  string `json:"kind"`
		Phase string `json:"phase"`
	} `json:"change"`
}

// runChange dispatches `cerbix change <subcommand>`; `record` is the only subcommand in v1.
func runChange(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "record" {
		if len(args) > 0 {
			_, _ = fmt.Fprintf(stderr, "change: unknown subcommand %q\n", args[0])
		}
		_, _ = fmt.Fprintln(stderr, changeUsage)
		return changeExitRefused
	}
	return runChangeRecord(args[1:], stdout, stderr)
}

// runChangeRecord implements `cerbix change record`. Flags are parsed first (a usage error is 2),
// then the environment (a missing variable is 1 and names the variable, as the gate verb), then
// ONE request.
func runChangeRecord(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("change record", flag.ContinueOnError)
	fs.SetOutput(stderr)
	project := fs.String("project", "", "project id (required)")
	service := fs.String("service", "", "service id (required)")
	kind := fs.String("kind", "", "deploy|rollback|flag (required)")
	phase := fs.String("phase", "", "started|succeeded|failed|cancelled (required)")
	source := fs.String("source", "", "the reporting system's slug, e.g. github-actions (required)")
	externalID := fs.String("external-id", "", "the change's id at the source, e.g. the run id (required)")
	ref := fs.String("ref", "", "a label for the change, e.g. the version or commit")
	link := fs.String("url", "", "an https:// link to the change")
	decision := fs.String("decision", "", "the gate decision_id the release rested on")
	at := fs.String("at", "", "when the phase occurred, RFC3339 (default: the invocation instant)")
	asJSON := fs.Bool("json", false, "print the API response verbatim instead of the one-line summary")
	timeout := fs.Duration("timeout", changeDefaultTimeout, "overall request deadline")
	if err := fs.Parse(args); err != nil {
		return changeExitRefused
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "change record: unexpected argument %q\n", fs.Arg(0))
		return changeExitRefused
	}
	var missing []string
	for _, f := range []struct{ name, value string }{
		{"--project", *project}, {"--service", *service}, {"--kind", *kind}, {"--phase", *phase},
		{"--source", *source}, {"--external-id", *externalID},
	} {
		if f.value == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		verb := "is"
		if len(missing) > 1 {
			verb = "are"
		}
		_, _ = fmt.Fprintf(stderr, "change record: %s %s required\n", strings.Join(missing, ", "), verb)
		_, _ = fmt.Fprintln(stderr, changeUsage)
		return changeExitRefused
	}
	if *timeout <= 0 {
		_, _ = fmt.Fprintln(stderr, "change record: --timeout must be positive")
		return changeExitRefused
	}

	target, err := serviceRouteTarget(os.Getenv("CERBIX_URL"), *project, *service, "changes")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cerbix: change: %v\n", err)
		return changeExitError
	}
	token := strings.TrimSpace(os.Getenv("CERBIX_TOKEN"))
	if token == "" {
		_, _ = fmt.Fprintln(stderr, "cerbix: change: CERBIX_TOKEN is not set (the API token that authenticates to the server; environment only, never a flag)")
		return changeExitError
	}
	client, err := gateHTTPClient(os.Getenv("CERBIX_CA_FILE"))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cerbix: change: %v\n", err)
		return changeExitError
	}

	// D2: what was given travels as given — no local enum, slug, URL or timestamp check; the
	// server is the authority and its refusal is printed verbatim. Only the default for --at is
	// the CLI's own: the invocation instant (D13), RFC3339 in UTC.
	body := changeRecordBody{
		Kind: *kind, Phase: *phase, Source: *source, ExternalID: *externalID,
		Ref: *ref, URL: *link, DecisionID: *decision, OccurredAt: *at,
	}
	if body.OccurredAt == "" {
		body.OccurredAt = time.Now().UTC().Format(time.RFC3339)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cerbix: change: encode request: %v\n", err)
		return changeExitError
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	res, err := changeRequest(ctx, client, target, token, payload)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			_, _ = fmt.Fprintf(stderr, "cerbix: change: request timed out after %s (--timeout)\n", *timeout)
		} else {
			_, _ = fmt.Fprintf(stderr, "cerbix: change: request failed: %v\n", err)
		}
		return changeExitError
	}
	return changeOutcome(res, *asJSON, stdout, stderr)
}

// changeRequest performs exactly one POST and returns what came back. It never retries: a 429 is
// the load the §5a limit exists to shed (D13), and a retry with `--at` defaulted would carry a
// different instant — a `phase_exists` in the making, not a replay.
func changeRequest(ctx context.Context, client *http.Client, target, token string, payload []byte) (*gateHTTPResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, changeMaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(body) > changeMaxBody {
		return nil, fmt.Errorf("response body exceeds %d bytes", changeMaxBody)
	}
	return &gateHTTPResult{
		Status:     resp.StatusCode,
		RetryAfter: resp.Header.Get("Retry-After"),
		Location:   resp.Header.Get("Location"),
		Body:       body,
	}, nil
}

// changeOutcome turns the wire result into output and an exit code (D13): 2xx renders the
// record; 400/404/409 is the contract refusing — the server's `error` string verbatim on stderr,
// exit 2; everything else is the transport's — exit 1, with `Retry-After` printed on a 429.
func changeOutcome(res *gateHTTPResult, asJSON bool, stdout, stderr io.Writer) int {
	switch {
	case res.Status == http.StatusOK || res.Status == http.StatusCreated:
		return changeRenderRecorded(res.Body, asJSON, stdout, stderr)
	case res.Status == http.StatusTooManyRequests:
		_, _ = fmt.Fprintf(stderr, "cerbix: change: 429 %s\n", gateErrorText(res))
		if res.RetryAfter != "" {
			_, _ = fmt.Fprintf(stderr, "Retry-After: %s\n", res.RetryAfter)
		}
		return changeExitError
	case res.Status >= 300 && res.Status < 400:
		_, _ = fmt.Fprintf(stderr, "cerbix: change: %d redirect to %q not followed; set CERBIX_URL to the final address\n", res.Status, res.Location)
		return changeExitError
	case res.Status == http.StatusBadRequest || res.Status == http.StatusNotFound || res.Status == http.StatusConflict:
		_, _ = fmt.Fprintf(stderr, "cerbix: change: %d %s\n", res.Status, gateErrorText(res))
		return changeExitRefused
	default:
		_, _ = fmt.Fprintf(stderr, "cerbix: change: %d %s\n", res.Status, gateErrorText(res))
		return changeExitError
	}
}

// changeRenderRecorded decodes a 2xx body and writes the one stdout line (or the body verbatim
// under --json). A body that is not JSON or names no change is a malformed response (exit 1):
// the record may well have been written, but the pipeline cannot tell, and this client does not
// guess.
func changeRenderRecorded(body []byte, asJSON bool, stdout, stderr io.Writer) int {
	var rec changeRecorded
	if err := json.Unmarshal(body, &rec); err != nil {
		_, _ = fmt.Fprintf(stderr, "cerbix: change: malformed response: %v\n", err)
		return changeExitError
	}
	if rec.Change.ID == "" || rec.Change.Kind == "" || rec.Change.Phase == "" {
		_, _ = fmt.Fprintln(stderr, "cerbix: change: malformed response: no change id, kind and phase")
		return changeExitError
	}
	if asJSON {
		// Byte-identical to the API response (D13, §7 CLI): exactly the body bytes, nothing
		// appended — not even a newline (the server's encoder already ends the body with one). A
		// consumer that diffs, hashes or signs the output must see what the server sent.
		_, _ = stdout.Write(body)
	} else {
		_, _ = fmt.Fprintln(stdout, rec.summaryLine())
	}
	return changeExitOK
}

// summaryLine is the stdout grammar of D13:
//
//	recorded change=<id> kind=<k> phase=<p>
//	replayed change=<id> kind=<k> phase=<p>
//
// The word follows the body's `replayed`, the values are the server's canonical ones.
func (r changeRecorded) summaryLine() string {
	word := "recorded"
	if r.Replayed {
		word = "replayed"
	}
	return word + " change=" + r.Change.ID + " kind=" + r.Change.Kind + " phase=" + r.Change.Phase
}
