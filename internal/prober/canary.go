package prober

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// The FR-029 executor: ONE typed asynchronous transaction, end to end, in one probe (D2). Phase C
// carries `http_json` submit and `poll_json` completion; `multipart_fixture` and `sse` land in phase
// E. The stage list is fixed — submit, correlate, await_result, assert_result, cleanup_validation —
// and a failure names its stage and a bounded class and NOTHING else: no URL, no body, no header, no
// secret, no correlation id, no object path (NFR-024).
//
// The credential never travels inside the document. `dispatch.ValidateAndMaterialize` opens the
// envelope into `canary_secret_<binding>` config keys; this executor reads them when it builds a
// request and the dispatch cleanup wipes them afterwards. The stored workflow carries markers only.

// canaryRunBounds are the run-time limits of §4b. They are constants here rather than configuration
// because a limit an operator can raise is a limit an attacker can raise.
const (
	canaryMaxResponseBytes = 256 * 1024
	canaryMaxRedirects     = 3
	canaryDialTimeout      = 5 * time.Second
	canaryTLSTimeout       = 5 * time.Second
)

// canaryProber executes the workflow. `dial` is the seam: production wires a STRICT guard (no
// loopback, no link-local, no private, no metadata, validated after resolution and re-validated on
// every redirect hop), and a test wires a permissive dialer so a local fixture is reachable without
// a product flag that would also exist in production (§6.12, §7).
type canaryProber struct {
	dial  func(ctx context.Context, network, addr string) (net.Conn, error)
	clock func() time.Time
}

// canaryFailure is a stage plus a bounded class. It is the only thing that reaches a heartbeat.
type canaryFailure struct {
	stage domain.CanaryStage
	class string
}

func (f canaryFailure) String() string { return string(f.stage) + ": " + f.class }

func (p canaryProber) Probe(ctx context.Context, m domain.Monitor) Result {
	start := p.now()
	w, err := domain.ParseCanaryConfig(m.Config)
	if err != nil {
		// The stored document does not parse: a configuration fault, not a target fault, and it is
		// still THIS monitor's own failure rather than anything wider (§10).
		return Result{Msg: canaryFailure{domain.CanaryStageSubmit, "workflow unreadable"}.String()}
	}

	secrets := canaryBindingValues(m.Config, w)
	client := p.client()

	// ── submit ──────────────────────────────────────────────────────────────────────────────
	subCtx, cancel := context.WithTimeout(ctx, time.Duration(w.Submit.SubmitTimeout)*time.Second)
	body, contentType, err := canaryBuildBody(w, secrets)
	if err != nil {
		cancel()
		return Result{Msg: canaryFailure{domain.CanaryStageSubmit, err.Error()}.String()}
	}
	resp, respBody, fail := p.do(subCtx, client, "POST", w.Submit.URL, w.Submit.Headers, secrets,
		body, contentType, canaryIdempotencyKey(m), domain.CanaryStageSubmit)
	cancel()
	if fail != nil {
		return Result{LatencyMS: p.since(start), Msg: fail.String()}
	}
	if !canaryStatusAccepted(resp.StatusCode, w.Submit.AcceptedStatus) {
		return Result{
			Connected: true, Code: resp.StatusCode, LatencyMS: p.since(start),
			Msg: canaryFailure{domain.CanaryStageSubmit, "status not accepted"}.String(),
		}
	}

	// ── correlate ───────────────────────────────────────────────────────────────────────────
	correlation, cfail := canaryCorrelation(w.Correlate, resp, respBody)
	if cfail != nil {
		return Result{Connected: true, Code: resp.StatusCode, LatencyMS: p.since(start), Msg: cfail.String()}
	}

	// ── await_result ────────────────────────────────────────────────────────────────────────
	awaitCtx, cancelAwait := context.WithTimeout(ctx, time.Duration(w.Completion.Timeout)*time.Second)
	defer cancelAwait()
	doc, afail := p.await(awaitCtx, client, w, secrets, correlation)
	if afail != nil {
		return Result{Connected: true, LatencyMS: p.since(start), Msg: afail.String()}
	}

	// ── assert_result ───────────────────────────────────────────────────────────────────────
	for _, path := range w.Result.RequiredJSONFields {
		if _, ok := canaryLookup(doc, path); !ok {
			return Result{
				Connected: true, LatencyMS: p.since(start),
				Msg: canaryFailure{domain.CanaryStageAssertResult, "missing required field"}.String(),
			}
		}
	}
	lifecycle, ok := canaryLookupString(doc, w.Result.LifecyclePath)
	if !ok {
		return Result{
			Connected: true, LatencyMS: p.since(start),
			Msg: canaryFailure{domain.CanaryStageAssertResult, "missing lifecycle path"}.String(),
		}
	}

	// ── cleanup_validation ──────────────────────────────────────────────────────────────────
	// A VALIDATION and never a deletion (D10): cerbix has no rights on the object store and never
	// removes what it did not create.
	if w.Cleanup.Kind == domain.CanaryCleanupLifecyclePrefix &&
		!strings.HasPrefix(lifecycle, w.Cleanup.Prefix) {
		return Result{
			Connected: true, LatencyMS: p.since(start),
			Msg: canaryFailure{domain.CanaryStageCleanupValidation, "result outside the declared prefix"}.String(),
		}
	}

	// The PROMISE is checked last, over the whole journey including retry waits (§5.5) — a journey
	// that COMPLETED but too slowly is a distinct outcome from a probe that was stopped.
	elapsed := p.since(start)
	if elapsed > int64(w.Result.MaxLatency)*1000 {
		return Result{
			Connected: true, LatencyMS: elapsed,
			Msg: canaryFailure{domain.CanaryStageAssertResult, "latency exceeded"}.String(),
		}
	}
	return Result{Connected: true, LatencyMS: elapsed}
}

func (p canaryProber) now() time.Time {
	if p.clock != nil {
		return p.clock()
	}
	return time.Now()
}

func (p canaryProber) since(start time.Time) int64 {
	return p.now().Sub(start).Milliseconds()
}

// client builds an HTTP client whose redirect policy is OURS. `net/http` strips only `Authorization`
// across hosts and has never heard of `x-api-key`, so relying on its default would leak every other
// credential-bearing header to whatever a redirect points at (D3c1).
func (p canaryProber) client() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // a proxy would bypass the address guard the dialer enforces
	transport.DialContext = p.dial
	transport.TLSHandshakeTimeout = canaryTLSTimeout
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= canaryMaxRedirects {
				return fmt.Errorf("too many redirects")
			}
			prev := via[len(via)-1].URL
			if canaryOrigin(req.URL) != canaryOrigin(prev) {
				// Normalized host OR PORT changed: drop every header a binding produced, and do it
				// for the whole binding-backed set rather than for the one name Go knows about.
				for name := range req.Header {
					if canaryBindingBacked(via[0], name) {
						req.Header.Del(name)
					}
				}
			}
			return nil
		},
	}
}

// canaryBindingBackedHeader marks a header as carrying a credential, so the redirect policy can drop
// exactly those. It is carried on the ORIGINAL request and read back on each hop, because the
// schema — not the transport — is what knows which headers those are.
const canaryBindingBackedHeader = "X-Cerbix-Binding-Backed"

func canaryBindingBacked(original *http.Request, name string) bool {
	marked := original.Header.Get(canaryBindingBackedHeader)
	if marked == "" {
		return false
	}
	for _, n := range strings.Split(marked, ",") {
		if strings.EqualFold(strings.TrimSpace(n), name) {
			return true
		}
	}
	return false
}

func canaryOrigin(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return host + ":" + port
}

// do performs one request with the declared headers, the binding values injected, and bounded
// reading. It returns the response and its body, or a stage failure that never carries a URL.
func (p canaryProber) do(ctx context.Context, client *http.Client, method, rawURL string,
	headers []domain.CanaryHeader, secrets map[string]string, body []byte, contentType, idempotency string,
	stage domain.CanaryStage) (*http.Response, []byte, *canaryFailure) {

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, nil, &canaryFailure{stage, "request could not be built"}
	}
	var bindingBacked []string
	for _, h := range headers {
		name := strings.ToLower(strings.TrimSpace(h.Name))
		if h.SecretRef != "" {
			value, ok := secrets[h.SecretRef]
			if !ok {
				// Fail closed: a declared binding with no materialized value must never be sent as
				// an empty header, which would authenticate as nobody and report UP.
				return nil, nil, &canaryFailure{stage, "credential unavailable"}
			}
			req.Header.Set(name, value)
			bindingBacked = append(bindingBacked, name)
			continue
		}
		req.Header.Set(name, h.Value)
	}
	if len(bindingBacked) > 0 {
		req.Header.Set(canaryBindingBackedHeader, strings.Join(bindingBacked, ","))
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if idempotency != "" {
		// The runner owns this header (the schema refuses an author's own), and the value is stable
		// across a redelivery, a re-claim and a transport retry of the SAME scheduled run (D8).
		req.Header.Set("Idempotency-Key", idempotency)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, &canaryFailure{stage, canaryTransportClass(err)}
	}
	defer resp.Body.Close()
	req.Header.Del(canaryBindingBackedHeader)

	limited := io.LimitReader(resp.Body, canaryMaxResponseBytes+1)
	read, err := io.ReadAll(limited)
	if err != nil {
		return nil, nil, &canaryFailure{stage, "response could not be read"}
	}
	if len(read) > canaryMaxResponseBytes {
		return nil, nil, &canaryFailure{stage, "response too large"}
	}
	return resp, read, nil
}

// await runs the completion stage. Phase C implements `poll_json`; `sse` arrives in phase E and is
// refused here rather than silently treated as a poll.
func (p canaryProber) await(ctx context.Context, client *http.Client, w domain.CanaryWorkflow,
	secrets map[string]string, correlation string) (any, *canaryFailure) {

	if w.Completion.Kind != domain.CanaryCompletionPollJSON {
		return nil, &canaryFailure{domain.CanaryStageAwaitResult, "completion kind not supported by this executor"}
	}
	target := canarySubstitute(w.Completion.URL, correlation)
	poll := w.Completion.Poll
	for attempt := 0; attempt < poll.MaxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, &canaryFailure{domain.CanaryStageAwaitResult, "completion timed out"}
			case <-time.After(time.Duration(poll.Interval) * time.Second):
			}
		}
		resp, body, fail := p.do(ctx, client, "GET", target, w.Completion.Headers, secrets, nil, "", "",
			domain.CanaryStageAwaitResult)
		if fail != nil {
			return nil, fail
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			// A polling endpoint answering non-2xx is a target fault, not a terminal outcome.
			return nil, &canaryFailure{domain.CanaryStageAwaitResult, "poll status " + canaryStatusClass(resp.StatusCode)}
		}
		var doc any
		if err := json.Unmarshal(body, &doc); err != nil {
			return nil, &canaryFailure{domain.CanaryStageAwaitResult, "poll response is not JSON"}
		}
		if v, ok := canaryLookupString(doc, poll.Success.Path); ok && v == poll.Success.Value {
			return doc, nil
		}
		if poll.Failure.Path != "" {
			if v, ok := canaryLookupString(doc, poll.Failure.Path); ok {
				for _, bad := range poll.Failure.Values {
					if v == bad {
						return nil, &canaryFailure{domain.CanaryStageAwaitResult, "declared failure state"}
					}
				}
			}
		}
	}
	return nil, &canaryFailure{domain.CanaryStageAwaitResult, "poll attempts exhausted"}
}

// canaryBuildBody renders the submit body. `http_json` encodes the typed algebra with binding values
// substituted at the leaves; `multipart_fixture` is phase E and is refused here rather than sent as
// something else.
func canaryBuildBody(w domain.CanaryWorkflow, secrets map[string]string) ([]byte, string, error) {
	switch w.Submit.Kind {
	case domain.CanarySubmitHTTPJSON:
		rendered, err := canaryRenderMap(w.Submit.Body, secrets)
		if err != nil {
			return nil, "", err
		}
		encoded, err := json.Marshal(rendered)
		if err != nil {
			return nil, "", fmt.Errorf("body could not be encoded")
		}
		return encoded, "application/json", nil
	case domain.CanarySubmitMultipartFixture:
		return nil, "", fmt.Errorf("submit kind not supported by this executor")
	default:
		return nil, "", fmt.Errorf("submit kind not supported by this executor")
	}
}

func canaryRenderMap(m map[string]domain.CanaryValue, secrets map[string]string) (map[string]any, error) {
	out := make(map[string]any, len(m))
	for k, v := range m {
		rendered, err := canaryRenderValue(v, secrets)
		if err != nil {
			return nil, err
		}
		out[k] = rendered
	}
	return out, nil
}

func canaryRenderValue(v domain.CanaryValue, secrets map[string]string) (any, error) {
	switch v.Kind {
	case domain.CanaryValueString:
		return v.Str, nil
	case domain.CanaryValueBool:
		return v.Bool, nil
	case domain.CanaryValueNumber:
		return json.RawMessage(v.Num.String()), nil
	case domain.CanaryValueSecret:
		value, ok := secrets[v.SecretRef]
		if !ok {
			return nil, fmt.Errorf("credential unavailable")
		}
		return value, nil
	case domain.CanaryValueList:
		out := make([]any, 0, len(v.List))
		for _, item := range v.List {
			rendered, err := canaryRenderValue(item, secrets)
			if err != nil {
				return nil, err
			}
			out = append(out, rendered)
		}
		return out, nil
	case domain.CanaryValueObject:
		return canaryRenderMap(v.Obj, secrets)
	default:
		return nil, fmt.Errorf("body could not be encoded")
	}
}

// canaryBindingValues collects the materialized credential values the dispatch gate injected. They
// live in the ephemeral config copy for one execution and are wiped by that gate's cleanup; nothing
// here persists or logs them.
func canaryBindingValues(config map[string]string, w domain.CanaryWorkflow) map[string]string {
	out := map[string]string{}
	for binding := range w.Secrets {
		if v, ok := config[domain.CanaryBindingField(binding)]; ok && v != "" {
			out[binding] = v
		}
	}
	return out
}

// canaryIdempotencyKey is derived from the monitor and its EXECUTION REVISION plus the job's own
// scheduled second, so a redelivery, a re-claim after a lease expiry and a transport retry all carry
// the same key while the next scheduled run carries a different one (D8). The derivation lives in
// domain so the core and the executor cannot disagree about it.
func canaryIdempotencyKey(m domain.Monitor) string {
	// The run key is set by the scheduler when it materializes the job (phase D). Until then it is
	// absent and this returns "", and `do` then sends NO Idempotency-Key at all: an UNSTABLE key
	// would be worse than none, because it would look like protection while creating a second
	// external task on every retry.
	return domain.CanaryIdempotencyKey(m.ID, m.ExecutionRevision, m.Config[domain.CanaryRunKey])
}

func canaryStatusAccepted(code int, accepted []int) bool {
	for _, a := range accepted {
		if a == code {
			return true
		}
	}
	return false
}

// canaryStatusClass reduces a status to its class, so a heartbeat carries "5xx" and never a body or
// a URL that happened to be in the error.
func canaryStatusClass(code int) string {
	return strconv.Itoa(code/100) + "xx"
}

// canaryTransportClass names a transport failure without echoing the address it happened to. The
// classes are the ones FR-028 stage 0 introduced for every prober.
func canaryTransportClass(err error) string {
	return failureClass(err)
}

// canaryCorrelation extracts the id and enforces the bounds that make it safe to put in a URL: a
// target-controlled value goes into a request we then make, so length, encoding and control
// characters are checked before it is used (D4).
func canaryCorrelation(c domain.CanaryCorrelate, resp *http.Response, body []byte) (string, *canaryFailure) {
	var raw string
	switch c.Source {
	case domain.CanaryCorrelateResponseHeader:
		values := resp.Header.Values(c.HeaderName)
		if len(values) == 0 {
			return "", &canaryFailure{domain.CanaryStageCorrelate, "correlation header missing"}
		}
		if len(values) > 1 {
			// Two values are not a correlation id, and picking one would be a guess.
			return "", &canaryFailure{domain.CanaryStageCorrelate, "correlation header repeated"}
		}
		raw = values[0]
	case domain.CanaryCorrelateResponseJSON:
		var doc any
		if err := json.Unmarshal(body, &doc); err != nil {
			return "", &canaryFailure{domain.CanaryStageCorrelate, "submit response is not JSON"}
		}
		v, ok := canaryLookupString(doc, c.Path)
		if !ok {
			return "", &canaryFailure{domain.CanaryStageCorrelate, "correlation path missing"}
		}
		raw = v
	default:
		return "", &canaryFailure{domain.CanaryStageCorrelate, "unsupported correlation source"}
	}
	if err := domain.ValidateCanaryCorrelationID(raw); err != nil {
		return "", &canaryFailure{domain.CanaryStageCorrelate, err.Error()}
	}
	return raw, nil
}

// canarySubstitute puts the correlation id into the completion URL as ONE percent-encoded path
// segment, so a `/`, `?`, `#` or `@` inside it stays data and cannot change the request's shape or
// target. The substituted URL is what the dialer's guard then validates.
func canarySubstitute(rawURL, correlation string) string {
	return strings.ReplaceAll(rawURL, domain.CanaryCorrelationPlaceholder, url.PathEscape(correlation))
}

// canaryLookup walks the restricted path grammar of D5 over a decoded JSON document.
func canaryLookup(doc any, path string) (any, bool) {
	cur := doc
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

// canaryLookupString addresses one value and renders it as a string for equality assertions. Numbers
// and booleans are rendered rather than refused: a target that answers `"status": 2` is answering.
func canaryLookupString(doc any, path string) (string, bool) {
	v, ok := canaryLookup(doc, path)
	if !ok {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case json.Number:
		return t.String(), true
	default:
		return "", false
	}
}
