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
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
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
)

// changeRecordBody is the POST …/changes body (D2), field for field. `ref`, `url` and
// `decision_id` are OMITTED when their flag was not given — the server defaults them, and for
// `decision_id` an empty string is not the same statement as absence.
type changeRecordBody struct {
	Kind       string  `json:"kind"`
	Phase      string  `json:"phase"`
	OccurredAt string  `json:"occurred_at"`
	Source     string  `json:"source"`
	ExternalID string  `json:"external_id"`
	Ref        string  `json:"ref,omitempty"`
	URL        string  `json:"url,omitempty"`
	DecisionID *string `json:"decision_id,omitempty"`
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

type changeRecordOptions struct {
	ProjectID   string
	ServiceID   string
	Kind        string
	Phase       string
	Source      string
	ExternalID  string
	Ref         string
	URL         string
	Decision    string
	At          string
	JSON        bool
	Timeout     time.Duration
	DecisionSet bool
	AtSet       bool
}

type changeRecordExecutor func(changeRecordOptions, io.Writer, io.Writer) error

func newChangeCommand(execute changeRecordExecutor) *cobra.Command {
	cmd := &cobra.Command{
		Use:                   "change",
		Short:                 "Record service changes.",
		Long:                  "Record and inspect CI/CD change commands.",
		Example:               "cerbix change record --help",
		GroupID:               rootGroupCICD,
		Args:                  unknownSubcommandArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_ = writeCommandHelp(cmd, cmd.ErrOrStderr())
			return printedExit(changeExitRefused)
		},
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	addOrderedCommands(cmd, newChangeRecordCommand(execute))
	return cmd
}

func newChangeRecordCommand(execute changeRecordExecutor) *cobra.Command {
	opts := changeRecordOptions{Timeout: changeDefaultTimeout}
	cmd := &cobra.Command{
		Use:   "record",
		Short: "Record a deploy, rollback or flag change.",
		Long: "Record one append-only change phase through the remote API. A successful identical replay exits " +
			"zero and is reported as replayed. Credentials are environment-only, TLS verification has no bypass, " +
			"and the command performs one request without retries.",
		Example: "CERBIX_URL=https://cerbix.example.com CERBIX_TOKEN=\"$CERBIX_TOKEN\" cerbix change record " +
			"--project 00000000-0000-4000-8000-000000000001 --service 00000000-0000-4000-8000-000000000002 " +
			"--kind deploy --phase succeeded --source github-actions --external-id 123456 --ref v1.2.3",
		Args:                  noArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			missing := missingRequiredFlags(cmd)
			if len(missing) > 0 {
				verb := "is"
				if len(missing) > 1 {
					verb = "are"
				}
				return usageExit(fmt.Errorf("change record: %s %s required", strings.Join(missing, ", "), verb))
			}
			if opts.Timeout <= 0 {
				return usageExit(fmt.Errorf("change record: --timeout must be positive"))
			}
			opts.DecisionSet = cmd.Flags().Changed("decision")
			opts.AtSet = cmd.Flags().Changed("at")
			return execute(opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().SortFlags = false
	addStringFlag(cmd, &opts.ProjectID, "project", "", "id", "project id", true)
	addStringFlag(cmd, &opts.ServiceID, "service", "", "id", "service id", true)
	addStringFlag(cmd, &opts.Kind, "kind", "", "deploy|rollback|flag", "change kind", true)
	addStringFlag(cmd, &opts.Phase, "phase", "", "started|succeeded|failed|cancelled", "change phase", true)
	addStringFlag(cmd, &opts.Source, "source", "", "slug", "the reporting system's slug, e.g. github-actions", true)
	addStringFlag(cmd, &opts.ExternalID, "external-id", "", "id", "the change's id at the source, e.g. the run id", true)
	addStringFlag(cmd, &opts.Ref, "ref", "", "label", "a label for the change, e.g. the version or commit", false)
	addStringFlag(cmd, &opts.URL, "url", "", "https-url", "an https:// link to the change", false)
	addStringFlag(cmd, &opts.Decision, "decision", "", "id", "the gate decision_id the release rested on", false)
	addStringFlag(cmd, &opts.At, "at", "", "RFC3339", "when the phase occurred (default: the invocation instant)", false)
	addBoolFlag(cmd, &opts.JSON, "json", false, "print the API response verbatim instead of the one-line summary")
	addDurationFlag(cmd, &opts.Timeout, "timeout", changeDefaultTimeout, "overall request deadline")
	setHelpSection(cmd, helpAnnotationEnvironment, "CERBIX_URL      Server base URL.\nCERBIX_TOKEN    API bearer token; environment only, never a flag.\nCERBIX_CA_FILE  Optional PEM CA file added to system roots.")
	setHelpSection(cmd, helpAnnotationExitCodes, "0  Recorded or replayed.\n1  Transport, timeout, TLS, authentication, 429, server, or malformed-response error.\n2  Contract refusal (400, 404, or 409) or CLI usage error.")
	return cmd
}

func executeChangeRecord(opts changeRecordOptions, stdout, stderr io.Writer) error {
	target, err := serviceRouteTarget(os.Getenv("CERBIX_URL"), opts.ProjectID, opts.ServiceID, "changes")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cerbix: change: %v\n", err)
		return printedExit(changeExitError)
	}
	token := strings.TrimSpace(os.Getenv("CERBIX_TOKEN"))
	if token == "" {
		_, _ = fmt.Fprintln(stderr, "cerbix: change: CERBIX_TOKEN is not set (the API token that authenticates to the server; environment only, never a flag)")
		return printedExit(changeExitError)
	}
	client, err := gateHTTPClient(os.Getenv("CERBIX_CA_FILE"))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cerbix: change: %v\n", err)
		return printedExit(changeExitError)
	}

	body := changeRecordBody{
		Kind: opts.Kind, Phase: opts.Phase, Source: opts.Source, ExternalID: opts.ExternalID,
		Ref: opts.Ref, URL: opts.URL, OccurredAt: opts.At,
	}
	if opts.Decision != "" || opts.DecisionSet {
		body.DecisionID = &opts.Decision
	}
	if !opts.AtSet {
		body.OccurredAt = time.Now().UTC().Format(time.RFC3339)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cerbix: change: encode request: %v\n", err)
		return printedExit(changeExitError)
	}

	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer cancel()
	res, err := changeRequest(ctx, client, target, token, payload)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			_, _ = fmt.Fprintf(stderr, "cerbix: change: request timed out after %s (--timeout)\n", opts.Timeout)
		} else {
			_, _ = fmt.Fprintf(stderr, "cerbix: change: request failed: %v\n", err)
		}
		return printedExit(changeExitError)
	}
	return exitFromCode(changeOutcome(res, opts.JSON, stdout, stderr))
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
