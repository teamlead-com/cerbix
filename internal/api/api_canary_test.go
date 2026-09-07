package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-029 through the API. The canary is MaC-first and has no UI form yet, but the API is the surface
// a bundle's rules must agree with — one validation path serves both, so a document a bundle refuses
// must be refused here too, in the same words.
func canaryWorkflowJSON(t *testing.T, mutate func(w *domain.CanaryWorkflow)) string {
	t.Helper()
	w := domain.CanaryWorkflow{
		Kind:    domain.CanaryWorkflowKind,
		Secrets: map[string]string{},
		Submit: domain.CanarySubmit{
			Kind: domain.CanarySubmitHTTPJSON, Method: "POST",
			URL: "https://files.example.com/files/upload", SubmitTimeout: 10,
			AcceptedStatus: []int{202},
			Body:           map[string]domain.CanaryValue{"tenant": {Kind: domain.CanaryValueString, Str: "canary"}},
		},
		Correlate: domain.CanaryCorrelate{Source: domain.CanaryCorrelateResponseJSON, Path: "task_id"},
		Completion: domain.CanaryCompletion{
			Kind: domain.CanaryCompletionPollJSON, URL: "https://files.example.com/t/{{ correlation_id }}",
			Timeout: 60,
			Poll: &domain.CanaryPoll{Interval: 5, MaxAttempts: 12,
				Success: domain.CanaryPollMatch{Path: "status", Value: "completed"}},
		},
		Result:  domain.CanaryResult{MaxLatency: 60, RequiredJSONFields: []string{"s3_path"}, LifecyclePath: "s3_path"},
		Cleanup: domain.CanaryCleanup{Kind: domain.CanaryCleanupNone, Acknowledged: true},
	}
	if mutate != nil {
		mutate(&w)
	}
	doc, err := domain.CanaryCanonicalJSON(w)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	return doc
}

func canaryCreateBody(t *testing.T, doc string, extra map[string]string) string {
	t.Helper()
	config := map[string]string{domain.CanaryWorkflowKey: doc}
	for k, v := range extra {
		config[k] = v
	}
	raw, err := json.Marshal(map[string]any{
		"name": "media upload journey", "type": "async_canary", "region": "core",
		"interval_seconds": 300, "timeout_seconds": 300, "config": config,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestACanaryIsCreatableThroughTheAPI(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors", canaryCreateBody(t, canaryWorkflowJSON(t, nil), nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		ID     string            `json:"id"`
		Config map[string]string `json:"config"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Config[domain.CanaryWorkflowKey] == "" {
		t.Fatalf("the stored monitor carries no workflow: %+v", created.Config)
	}
}

func TestTheAPIRefusesWhatABundleRefuses(t *testing.T) {
	cases := []struct {
		name   string
		doc    func(t *testing.T) string
		want   string
		forbid string
	}{
		{"a literal in a credential-bearing header", func(t *testing.T) string {
			return canaryWorkflowJSON(t, func(w *domain.CanaryWorkflow) {
				w.Submit.Headers = []domain.CanaryHeader{{Name: "authorization", Value: "Bearer api-literal-token"}}
			})
		}, "credential-bearing", "api-literal-token"},
		{"a placeholder outside the completion URL", func(t *testing.T) string {
			return canaryWorkflowJSON(t, func(w *domain.CanaryWorkflow) {
				w.Submit.URL = "https://files.example.com/upload/{{ correlation_id }}"
			})
		}, "only legal in completion.url", ""},
		{"plaintext http", func(t *testing.T) string {
			return canaryWorkflowJSON(t, func(w *domain.CanaryWorkflow) {
				w.Submit.URL = "http://files.example.com/upload"
			})
		}, "must be https", ""},
		{"a reserved header the runner owns", func(t *testing.T) string {
			return canaryWorkflowJSON(t, func(w *domain.CanaryWorkflow) {
				w.Submit.Headers = []domain.CanaryHeader{{Name: "idempotency-key", Value: "mine"}}
			})
		}, "set by the runner", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandler(seededStore())
			rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors", canaryCreateBody(t, tc.doc(t), nil))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("create = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("body = %s, want %q", rec.Body.String(), tc.want)
			}
			if tc.forbid != "" && strings.Contains(rec.Body.String(), tc.forbid) {
				t.Fatalf("the refusal echoed the credential: %s", rec.Body.String())
			}
		})
	}
}

// A `canary_secret_*` key on any other type is refused at the write boundary — the D6b rule, applied
// to the second binding mechanism so it cannot repeat the defect the first one had.
func TestACanaryBindingKeyIsRefusedOnAnotherType(t *testing.T) {
	h := api.New(seededStore(), slog.New(slog.NewTextHandler(io.Discard, nil)), 8).Router()
	body := `{"name":"api","type":"http","target":"https://x","interval_seconds":60,"timeout_seconds":10,` +
		`"config":{"canary_secret_upload_ref":"some-secret"}}`
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "canary_secret_upload_ref") {
		t.Fatalf("the refusal must name the key: %s", rec.Body.String())
	}
}

// B4 — an executor-owned config key is refused on every canary write surface.
//
// `async_canary` has no credential schema, so the registry's unknown-key rejection never ran for it
// and both handlers passed the config map through verbatim. A client could therefore set
// `canary_run`, the key the scheduler otherwise mints per window: the plain dispatch path YIELDS to
// a value already present, the executor derives its `Idempotency-Key` from it, and a target
// honouring that key returns the first task's answer forever. The canary then reports UP on a
// transaction it stopped performing — an unlimited, silent false claim from one legal-looking write.
//
// A whitelist and not a blacklist, so a key nobody has thought of yet is refused as well; the two
// executor-owned shapes are named individually so the refusal says which rule was met.
//
// The mutation that must kill this: refuse `canary_run` alone. The injected binding VALUE key then
// walks in — and that one puts a caller-chosen string where the gate puts a decrypted secret.
func TestACanaryRefusesAnExecutorOwnedConfigKey(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{"the scheduler's run key", domain.CanaryRunKey, "1788400000", "set by the scheduler"},
		{"an injected binding value", domain.CanaryBindingField("upload"), "pinned-secret", "injected by the dispatch gate"},
		{"a key of no known shape", "canary_debug", "1", "unknown key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandler(seededStore())
			body := canaryCreateBody(t, canaryWorkflowJSON(t, nil), map[string]string{tc.key: tc.value})
			rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("create with %s = %d, want 400 (%s)", tc.key, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("body = %s, want a refusal naming %q", rec.Body.String(), tc.want)
			}
			if strings.Contains(rec.Body.String(), tc.value) {
				t.Errorf("the refusal echoed the submitted value: %s", rec.Body.String())
			}
		})
	}

	// The legal keys are still legal: a whitelist that refused the author's own document would be a
	// worse defect than the one it closes.
	h := newHandler(seededStore())
	doc := canaryWorkflowJSON(t, func(w *domain.CanaryWorkflow) {
		w.Secrets = map[string]string{"upload": "upload-token"}
		w.Submit.Headers = []domain.CanaryHeader{{Name: "authorization", SecretRef: "upload"}}
	})
	body := canaryCreateBody(t, doc, map[string]string{domain.CanarySecretRefKey("upload"): "upload-token"})
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a canary with a binding ref was refused: %d (%s)", rec.Code, rec.Body.String())
	}
}
