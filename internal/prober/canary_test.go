package prober

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-029 phase C: the executor, against a controlled local fixture. The fixture is reached through a
// dialer injected AT THE SEAM — never through a product flag, which would put the bypass in
// production. The URL policy itself is proven by the guard's own tests and by the strict guard the
// runner wires; what these cases prove is the workflow.

func canaryTestProber() canaryProber {
	return canaryProber{dial: (&net.Dialer{}).DialContext}
}

// canaryMonitor builds a monitor whose config is the canonical projection of a poll_json workflow
// pointed at the fixture server, with the binding's value already materialized the way the dispatch
// gate injects it for one execution.
func canaryMonitor(t *testing.T, base string, mutate func(w *domain.CanaryWorkflow)) domain.Monitor {
	t.Helper()
	w := domain.CanaryWorkflow{
		Kind:    domain.CanaryWorkflowKind,
		Secrets: map[string]string{"upload": "project-secret"},
		Submit: domain.CanarySubmit{
			Kind:           domain.CanarySubmitHTTPJSON,
			Method:         "POST",
			URL:            base + "/files/upload",
			SubmitTimeout:  10,
			AcceptedStatus: []int{202},
			Headers: []domain.CanaryHeader{
				{Name: "authorization", SecretRef: "upload"},
				{Name: "x-tenant", Value: "canary"},
			},
			Body: map[string]domain.CanaryValue{
				"only_audio": {Kind: domain.CanaryValueBool, Bool: false},
				"token":      {Kind: domain.CanaryValueSecret, SecretRef: "upload"},
			},
		},
		Correlate: domain.CanaryCorrelate{Source: domain.CanaryCorrelateResponseJSON, Path: "task_id"},
		Completion: domain.CanaryCompletion{
			Kind:    domain.CanaryCompletionPollJSON,
			URL:     base + "/tasks/{{ correlation_id }}",
			Timeout: 20,
			Poll: &domain.CanaryPoll{
				Interval:    1,
				MaxAttempts: 5,
				Success:     domain.CanaryPollMatch{Path: "status", Value: "completed"},
				Failure:     domain.CanaryPollMatch{Path: "status", Values: []string{"failed"}},
			},
		},
		Result: domain.CanaryResult{
			MaxLatency:         20,
			RequiredJSONFields: []string{"s3_path", "byte_size"},
			LifecyclePath:      "s3_path",
		},
		Cleanup: domain.CanaryCleanup{Kind: domain.CanaryCleanupLifecyclePrefix, Prefix: "canary/", Acknowledged: true},
	}
	if mutate != nil {
		mutate(&w)
	}
	cfg, err := domain.CanaryConfig(w)
	if err != nil {
		t.Fatalf("build config: %v", err)
	}
	// What the dispatch gate injects for ONE execution, and wipes afterwards.
	cfg[domain.CanaryBindingField("upload")] = "s3cr3t-canary-token"
	cfg[domain.CanaryRunKey] = "1788400000"
	return domain.Monitor{
		ID: "11111111-1111-1111-1111-111111111111", Type: domain.MonitorAsyncCanary,
		IntervalSeconds: 300, TimeoutSeconds: 300, ExecutionRevision: 7, Config: cfg,
	}
}

// canaryFixtureServer is the controlled local target: it accepts a submit, hands back a task id, and
// answers polls with the states the test asks for.
type canaryFixtureServer struct {
	*httptest.Server
	submits    int
	polls      int
	lastAuth   string
	lastKey    string
	lastBody   map[string]any
	states     []string // one per poll, last one repeats
	result     map[string]any
	submitCode int
}

func newCanaryFixture(t *testing.T) *canaryFixtureServer {
	t.Helper()
	f := &canaryFixtureServer{
		states:     []string{"completed"},
		result:     map[string]any{"s3_path": "canary/2026/out.wav", "byte_size": 1024, "media_type": "audio/wav"},
		submitCode: 202,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/files/upload", func(w http.ResponseWriter, r *http.Request) {
		f.submits++
		f.lastAuth = r.Header.Get("authorization")
		f.lastKey = r.Header.Get("Idempotency-Key")
		f.lastBody = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&f.lastBody)
		w.WriteHeader(f.submitCode)
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "task-42"})
	})
	mux.HandleFunc("/tasks/", func(w http.ResponseWriter, r *http.Request) {
		state := f.states[len(f.states)-1]
		if f.polls < len(f.states) {
			state = f.states[f.polls]
		}
		f.polls++
		doc := map[string]any{"status": state}
		if state == "completed" {
			for k, v := range f.result {
				doc[k] = v
			}
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func TestCanaryHappyPathSubmitsCorrelatesAwaitsAndAsserts(t *testing.T) {
	f := newCanaryFixture(t)
	m := canaryMonitor(t, f.URL, nil)

	res := canaryTestProber().Probe(context.Background(), m)
	if !res.Connected || res.Msg != "" {
		t.Fatalf("a healthy journey must pass: connected=%v msg=%q", res.Connected, res.Msg)
	}
	if f.submits != 1 || f.polls == 0 {
		t.Fatalf("submits=%d polls=%d, want one submit and at least one poll", f.submits, f.polls)
	}
	// The credential reached the target as the header the schema declared — from the envelope, not
	// from the document.
	if f.lastAuth != "s3cr3t-canary-token" {
		t.Fatalf("authorization = %q, want the materialized binding value", f.lastAuth)
	}
	if f.lastBody["token"] != "s3cr3t-canary-token" {
		t.Fatalf("body token = %v, want the materialized binding value", f.lastBody["token"])
	}
	// And the idempotency key is the runner's, derived from the scheduled run.
	if f.lastKey == "" {
		t.Fatal("submit carried no Idempotency-Key")
	}
	if f.lastKey != domain.CanaryIdempotencyKey(m.ID, m.ExecutionRevision, m.Config[domain.CanaryRunKey]) {
		t.Fatalf("idempotency key = %q, not the derived one", f.lastKey)
	}
}

func TestCanaryIdempotencyKeyIsStablePerRunAndDiffersAcrossRuns(t *testing.T) {
	f := newCanaryFixture(t)
	m := canaryMonitor(t, f.URL, nil)

	canaryTestProber().Probe(context.Background(), m)
	first := f.lastKey
	// A retry of the SAME scheduled run — a redelivery or a re-claim — carries the same key.
	canaryTestProber().Probe(context.Background(), m)
	if f.lastKey != first {
		t.Fatalf("a retry of the same run changed the key: %q → %q", first, f.lastKey)
	}
	// The next scheduled run is a different transaction.
	m.Config[domain.CanaryRunKey] = "1788400300"
	canaryTestProber().Probe(context.Background(), m)
	if f.lastKey == first {
		t.Fatal("the next scheduled run must carry a different key")
	}
	// And with no run key the header is ABSENT rather than unstable: an unstable key would look
	// like protection while creating a second external task on every retry.
	delete(m.Config, domain.CanaryRunKey)
	canaryTestProber().Probe(context.Background(), m)
	if f.lastKey != "" {
		t.Fatalf("without a run key the header must be absent, got %q", f.lastKey)
	}
}

func TestCanaryStageFailures(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *canaryFixtureServer, m *domain.Monitor)
		stage domain.CanaryStage
		class string
	}{
		{"status not accepted", func(f *canaryFixtureServer, m *domain.Monitor) {
			f.submitCode = 500
		}, domain.CanaryStageSubmit, "status not accepted"},
		{"declared failure state", func(f *canaryFixtureServer, m *domain.Monitor) {
			f.states = []string{"failed"}
		}, domain.CanaryStageAwaitResult, "declared failure state"},
		{"attempts exhausted", func(f *canaryFixtureServer, m *domain.Monitor) {
			f.states = []string{"processing"}
		}, domain.CanaryStageAwaitResult, "poll attempts exhausted"},
		{"missing required field", func(f *canaryFixtureServer, m *domain.Monitor) {
			f.result = map[string]any{"s3_path": "canary/x.wav"} // byte_size absent
		}, domain.CanaryStageAssertResult, "missing required field"},
		{"outside the prefix", func(f *canaryFixtureServer, m *domain.Monitor) {
			f.result = map[string]any{"s3_path": "elsewhere/x.wav", "byte_size": 1}
		}, domain.CanaryStageCleanupValidation, "outside the declared prefix"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCanaryFixture(t)
			m := canaryMonitor(t, f.URL, nil)
			tc.setup(f, &m)
			res := canaryTestProber().Probe(context.Background(), m)
			if res.Msg == "" {
				t.Fatal("expected a failure")
			}
			if !strings.HasPrefix(res.Msg, string(tc.stage)+":") {
				t.Fatalf("msg = %q, want stage %s", res.Msg, tc.stage)
			}
			if !strings.Contains(res.Msg, tc.class) {
				t.Fatalf("msg = %q, want class %q", res.Msg, tc.class)
			}
		})
	}
}

// NFR-024's leak invariant, asserted by planting a distinctive value in every position a failure
// might carry it from: the credential, the correlation id, the object path and the URL.
func TestCanaryResultNeverCarriesSecretsURLsOrIdentifiers(t *testing.T) {
	f := newCanaryFixture(t)
	f.result = map[string]any{"s3_path": "elsewhere/PLANTED-OBJECT-PATH.wav", "byte_size": 1}
	m := canaryMonitor(t, f.URL, nil)

	res := canaryTestProber().Probe(context.Background(), m)
	if res.Msg == "" {
		t.Fatal("expected the cleanup refusal")
	}
	for _, planted := range []string{
		"s3cr3t-canary-token",    // the credential
		"PLANTED-OBJECT-PATH",    // the result object path
		"task-42",                // the correlation id
		f.URL,                    // the target URL
		"files/upload", "tasks/", // any fragment of it
	} {
		if strings.Contains(res.Msg, planted) {
			t.Fatalf("the result leaked %q: %s", planted, res.Msg)
		}
	}
}

// D3c1: the cross-host rule is OURS, not net/http's — which strips only `Authorization` and has
// never heard of `x-api-key`. The submit stage is the one that carries the credential, and the one
// revision 4 left uncovered.
func TestCanaryDropsEveryBindingHeaderOnACrossHostRedirect(t *testing.T) {
	// The submit's headers are what this case is about, so only that path records them: the
	// completion poll hits the same server, and letting it overwrite the record made the first
	// version of this test assert against the wrong request.
	var received http.Header
	targetMux := http.NewServeMux()
	targetMux.HandleFunc("/files/upload", func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Clone()
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "task-42"})
	})
	targetMux.HandleFunc("/tasks/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "s3_path": "canary/x.wav", "byte_size": 1})
	})
	target := httptest.NewServer(targetMux)
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/files/upload", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	m := canaryMonitor(t, redirector.URL, func(w *domain.CanaryWorkflow) {
		w.Submit.Headers = []domain.CanaryHeader{
			{Name: "authorization", SecretRef: "upload"},
			{Name: "x-api-key", SecretRef: "upload"},
			{Name: "x-tenant", Value: "canary"},
		}
		w.Completion.URL = target.URL + "/tasks/{{ correlation_id }}"
	})
	canaryTestProber().Probe(context.Background(), m)

	if received == nil {
		t.Fatal("the redirect target was never reached")
	}
	if got := received.Get("authorization"); got != "" {
		t.Fatalf("authorization survived a cross-host redirect: %q", got)
	}
	// The one Go's default would have let through — the whole reason this rule is ours.
	if got := received.Get("x-api-key"); got != "" {
		t.Fatalf("x-api-key survived a cross-host redirect: %q", got)
	}
	// A non-credential header is NOT dropped: the rule must not be a blanket strip that breaks
	// ordinary flows.
	if got := received.Get("x-tenant"); got != "canary" {
		t.Fatalf("x-tenant = %q, want it preserved", got)
	}
}

func TestCanaryKeepsBindingHeadersOnASameOriginRedirect(t *testing.T) {
	var received http.Header
	mux := http.NewServeMux()
	mux.HandleFunc("/files/upload", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/files/upload-v2", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/files/upload-v2", func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Clone()
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "task-42"})
	})
	mux.HandleFunc("/tasks/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "s3_path": "canary/x.wav", "byte_size": 1})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := canaryMonitor(t, srv.URL, nil)
	res := canaryTestProber().Probe(context.Background(), m)
	if res.Msg != "" {
		t.Fatalf("a same-origin redirect must not break the journey: %s", res.Msg)
	}
	if received.Get("authorization") != "s3cr3t-canary-token" {
		t.Fatalf("authorization = %q, want it preserved on a same-origin redirect", received.Get("authorization"))
	}
}

// The correlation id is TARGET-controlled and lands in a URL we then request (D4).
func TestCanaryCorrelationIsBoundedAndEscaped(t *testing.T) {
	var polledPath string
	mux := http.NewServeMux()
	nasty := "../../admin?x=1#f"
	mux.HandleFunc("/files/upload", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": nasty})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		polledPath = r.URL.EscapedPath()
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "s3_path": "canary/x.wav", "byte_size": 1})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := canaryMonitor(t, srv.URL, nil)
	res := canaryTestProber().Probe(context.Background(), m)
	if res.Msg != "" {
		t.Fatalf("an escaped id must still address the resource: %s", res.Msg)
	}
	if !strings.HasPrefix(polledPath, "/tasks/") {
		t.Fatalf("the request escaped its path: %q", polledPath)
	}
	if strings.Contains(polledPath, "/admin") {
		t.Fatalf("the correlation id changed the request's target: %q", polledPath)
	}

	// Over-long, control characters and invalid UTF-8 are refused at `correlate` rather than used.
	for _, bad := range []string{strings.Repeat("x", domain.CanaryMaxCorrelationBytes+1), "id\x00with-nul", "id\nwith-newline"} {
		badID := bad
		mux2 := http.NewServeMux()
		mux2.HandleFunc("/files/upload", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": badID})
		})
		s2 := httptest.NewServer(mux2)
		m2 := canaryMonitor(t, s2.URL, nil)
		res := canaryTestProber().Probe(context.Background(), m2)
		if !strings.HasPrefix(res.Msg, string(domain.CanaryStageCorrelate)+":") {
			t.Fatalf("a malformed correlation id must fail at correlate, got %q", res.Msg)
		}
		if strings.Contains(res.Msg, badID) {
			t.Fatalf("the refusal echoed the id: %q", res.Msg)
		}
		s2.Close()
	}
}

func TestCanaryCorrelationFromAHeader(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/files/upload", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("task-id", "task-77")
		w.WriteHeader(202)
	})
	mux.HandleFunc("/tasks/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "task-77") {
			http.Error(w, "wrong id", 404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "s3_path": "canary/x.wav", "byte_size": 1})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := canaryMonitor(t, srv.URL, func(w *domain.CanaryWorkflow) {
		w.Correlate = domain.CanaryCorrelate{Source: domain.CanaryCorrelateResponseHeader, HeaderName: "task-id"}
	})
	if res := canaryTestProber().Probe(context.Background(), m); res.Msg != "" {
		t.Fatalf("a header correlation must work: %s", res.Msg)
	}
}

// The envelope did not open, or the region could not resolve the ref: the executor must NOT send an
// empty credential and report UP — the lie FR-020's fail-closed rules exist for.
//
// Split into two cases on purpose. The first version put a binding in BOTH a header and the body, so
// the body's check alone kept it green: a mutation that deleted the HEADER check survived. A test
// that cannot tell which mechanism answered is not testing either of them.
func TestCanaryFailsClosedWhenTheCredentialWasNotMaterialized(t *testing.T) {
	t.Run("header only", func(t *testing.T) {
		f := newCanaryFixture(t)
		m := canaryMonitor(t, f.URL, func(w *domain.CanaryWorkflow) {
			w.Submit.Body = map[string]domain.CanaryValue{"only_audio": {Kind: domain.CanaryValueBool, Bool: false}}
		})
		delete(m.Config, domain.CanaryBindingField("upload"))

		res := canaryTestProber().Probe(context.Background(), m)
		if !strings.Contains(res.Msg, "credential unavailable") {
			t.Fatalf("msg = %q, want a credential-unavailable refusal from the HEADER path", res.Msg)
		}
		if f.submits != 0 {
			t.Fatalf("the target was contacted %d times before the refusal", f.submits)
		}
	})

	t.Run("body only", func(t *testing.T) {
		f := newCanaryFixture(t)
		m := canaryMonitor(t, f.URL, func(w *domain.CanaryWorkflow) {
			w.Submit.Headers = []domain.CanaryHeader{{Name: "x-tenant", Value: "canary"}}
		})
		delete(m.Config, domain.CanaryBindingField("upload"))

		res := canaryTestProber().Probe(context.Background(), m)
		if !strings.Contains(res.Msg, "credential unavailable") {
			t.Fatalf("msg = %q, want a credential-unavailable refusal from the BODY path", res.Msg)
		}
		if f.submits != 0 {
			t.Fatalf("the target was contacted %d times before the refusal", f.submits)
		}
	})
}

func TestCanaryLatencyPromiseIsSeparateFromTheTimeout(t *testing.T) {
	f := newCanaryFixture(t)
	f.states = []string{"processing", "processing", "completed"}
	m := canaryMonitor(t, f.URL, func(w *domain.CanaryWorkflow) {
		w.Result.MaxLatency = 1 // the journey needs two poll intervals, so it completes too slowly
	})
	res := canaryTestProber().Probe(context.Background(), m)
	if !res.Connected {
		t.Fatalf("a journey that COMPLETED must report connected: %q", res.Msg)
	}
	if !strings.Contains(res.Msg, "latency exceeded") {
		t.Fatalf("msg = %q, want the promise to fail distinctly from a timeout", res.Msg)
	}
}

// Phase C refused SSE outright and this case asserted that refusal. Phase E implements it, so the
// case became a lie about the product — replaced rather than deleted, because what still needs
// pinning is that an UNKNOWN completion kind is refused explicitly instead of being treated as one
// of the two that exist.
func TestCanaryAnUnknownCompletionKindIsRefusedExplicitly(t *testing.T) {
	f := newCanaryFixture(t)
	m := canaryMonitor(t, f.URL, func(w *domain.CanaryWorkflow) {
		w.Completion.Kind = "websocket_stream"
		w.Completion.Poll = nil
	})
	res := canaryTestProber().Probe(context.Background(), m)
	if !strings.Contains(res.Msg, "not supported by this executor") {
		t.Fatalf("msg = %q, want an explicit refusal for an unknown completion kind", res.Msg)
	}
}

func TestCanaryRespectsTheCompletionDeadline(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/files/") {
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "task-42"})
			return
		}
		time.Sleep(2 * time.Second)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "processing"})
	}))
	defer slow.Close()

	m := canaryMonitor(t, slow.URL, func(w *domain.CanaryWorkflow) {
		w.Completion.Timeout = 1
		w.Completion.Poll.MaxAttempts = 60
		w.Result.MaxLatency = 20
	})
	start := time.Now()
	res := canaryTestProber().Probe(context.Background(), m)
	if res.Msg == "" {
		t.Fatal("expected the completion deadline to fire")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the deadline did not bound the stage: %s", elapsed)
	}
}

// The redirect policy needs to know WHICH headers a binding produced. That provenance must not be
// something the target can read: the first version carried it in an `X-Cerbix-Binding-Backed`
// request header and deleted it only after `client.Do` returned, so the initial target and every
// redirect hop received an undeclared internal header naming exactly which of the request's headers
// hold credentials — a map of what to attack. Found by the independent reviewer as a P0 on the
// v0.1.9 canary range.
//
// This asserts BOTH ends of that: no internal header reaches either target, and the redirect
// stripping it exists to drive still works.
func TestCanaryProvenanceNeverReachesEitherTargetOnTheWire(t *testing.T) {
	var firstHop, secondHop http.Header
	targetMux := http.NewServeMux()
	targetMux.HandleFunc("/files/upload", func(w http.ResponseWriter, r *http.Request) {
		secondHop = r.Header.Clone()
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "task-42"})
	})
	targetMux.HandleFunc("/tasks/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "s3_path": "canary/x.wav", "byte_size": 1})
	})
	target := httptest.NewServer(targetMux)
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstHop = r.Header.Clone()
		http.Redirect(w, r, target.URL+"/files/upload", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	m := canaryMonitor(t, redirector.URL, func(w *domain.CanaryWorkflow) {
		w.Submit.Headers = []domain.CanaryHeader{
			{Name: "authorization", SecretRef: "upload"},
			{Name: "x-api-key", SecretRef: "upload"},
			{Name: "x-tenant", Value: "canary"},
		}
		w.Completion.URL = target.URL + "/tasks/{{ correlation_id }}"
	})
	canaryTestProber().Probe(context.Background(), m)

	if firstHop == nil {
		t.Fatal("the first hop was never reached")
	}
	if secondHop == nil {
		t.Fatal("the redirect target was never reached")
	}
	// Nothing internal on EITHER hop. Asserted by PREFIX rather than by the one name, so a future
	// marker with a different spelling cannot slip onto the wire past this test.
	for hop, h := range map[string]http.Header{"first hop": firstHop, "redirect target": secondHop} {
		for name := range h {
			if strings.HasPrefix(strings.ToLower(name), "x-cerbix-") {
				t.Errorf("%s received an internal header %q: provenance must not travel on the wire", hop, name)
			}
		}
	}
	// And the rule that provenance exists to drive is intact: credentials dropped cross-host, the
	// ordinary header kept.
	if got := secondHop.Get("authorization"); got != "" {
		t.Fatalf("authorization survived a cross-host redirect: %q", got)
	}
	if got := secondHop.Get("x-api-key"); got != "" {
		t.Fatalf("x-api-key survived a cross-host redirect: %q", got)
	}
	if got := secondHop.Get("x-tenant"); got != "canary" {
		t.Fatalf("x-tenant = %q, want it preserved", got)
	}
}
