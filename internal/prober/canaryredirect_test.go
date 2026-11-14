package prober

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// sec-hardening-2 H-13 (invariant S13), the canary's column, amending D-0218 / D3c1:
// (a) the origin compared is the FIRST request's and leaving it is sticky; (b) a cross-origin
// 307/308 whose body or multipart fields resolved a secret_ref is refused; (c) the library's
// Referer is removed on every hop. The stripped header set, the hop limit and CanaryHopViolation
// are untouched.

// canaryHop records what one canary endpoint received.
type canaryHop struct {
	mu      sync.Mutex
	header  http.Header
	body    string
	referer string
	reached bool
}

func (h *canaryHop) record(r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.header, h.body, h.referer, h.reached = r.Header.Clone(), string(b), r.Referer(), true
}

// canaryAccept answers a submit the way the fixture journey expects.
func canaryAccept(w http.ResponseWriter) {
	w.WriteHeader(202)
	_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "task-42"})
}

func canaryCompleted(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "s3_path": "canary/x.wav", "byte_size": 1})
}

func credentialHeaders(w *domain.CanaryWorkflow) {
	w.Submit.Headers = []domain.CanaryHeader{
		{Name: "authorization", SecretRef: "upload"},
		{Name: "x-api-key", SecretRef: "upload"},
		{Name: "x-tenant", Value: "canary"},
	}
}

func assertBindingHeadersStripped(t *testing.T, where string, h *canaryHop) {
	t.Helper()
	if !h.reached {
		t.Fatalf("%s was never reached", where)
	}
	if h.header.Get("authorization") != "" || h.header.Get("x-api-key") != "" {
		t.Fatalf("%s received a binding-backed header: %v", where, h.header)
	}
	if h.header.Get("x-tenant") != "canary" {
		t.Fatalf("%s lost the ordinary header x-tenant: %v", where, h.header)
	}
}

// (a) A → B/1 → B/2: the library restores the first request's headers on hop 2, whose origin equals
// hop 1's — a previous-hop comparison keeps them. RED on main.
func TestCanaryTwoHopRedirectStaysStripped(t *testing.T) {
	var final canaryHop
	targetMux := http.NewServeMux()
	targetMux.HandleFunc("/hop1", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/files/upload", http.StatusTemporaryRedirect)
	})
	targetMux.HandleFunc("/files/upload", func(w http.ResponseWriter, r *http.Request) { final.record(r); canaryAccept(w) })
	targetMux.HandleFunc("/tasks/", canaryCompleted)
	target := httptest.NewTLSServer(targetMux)
	defer target.Close()
	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/hop1", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	m := canaryMonitor(t, redirector.URL, func(w *domain.CanaryWorkflow) {
		credentialHeaders(w)
		delete(w.Submit.Body, "token")
		w.Completion.URL = target.URL + "/tasks/{{ correlation_id }}"
	})
	canaryTLSProber(t, target, redirector).Probe(context.Background(), m)
	assertBindingHeadersStripped(t, "the second hop on the other origin", &final)
}

// (a) A → B → A: back on the first origin, the chain has still left it.
func TestCanaryRedirectBackToOriginStaysStripped(t *testing.T) {
	var back canaryHop
	var origin *httptest.Server
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, origin.URL+"/files/upload-back", http.StatusTemporaryRedirect)
	}))
	defer other.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/files/upload", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/away", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/files/upload-back", func(w http.ResponseWriter, r *http.Request) { back.record(r); canaryAccept(w) })
	mux.HandleFunc("/tasks/", canaryCompleted)
	origin = httptest.NewTLSServer(mux)
	defer origin.Close()

	m := canaryMonitor(t, origin.URL, func(w *domain.CanaryWorkflow) {
		credentialHeaders(w)
		delete(w.Submit.Body, "token")
	})
	canaryTLSProber(t, origin, other).Probe(context.Background(), m)
	assertBindingHeadersStripped(t, "the first origin after leaving it", &back)
}

// (b) A submit whose body resolved a secret_ref — at the top level, nested in a list, or in a
// multipart field — is refused on a cross-origin 307, and nothing reaches the other origin.
func TestCanaryRefusesSecretBearingBodyAcrossOrigin(t *testing.T) {
	for name, mutate := range map[string]func(w *domain.CanaryWorkflow){
		"top level": func(w *domain.CanaryWorkflow) {},
		"nested in a list": func(w *domain.CanaryWorkflow) {
			w.Submit.Body = map[string]domain.CanaryValue{
				"items": {Kind: domain.CanaryValueList, List: []domain.CanaryValue{
					{Kind: domain.CanaryValueString, Str: "a"},
					{Kind: domain.CanaryValueObject, Obj: map[string]domain.CanaryValue{
						"key": {Kind: domain.CanaryValueSecret, SecretRef: "upload"},
					}},
				}},
			}
		},
		"multipart field": func(w *domain.CanaryWorkflow) {
			w.Submit.Kind = domain.CanarySubmitMultipartFixture
			w.Submit.Body = nil
			w.Submit.FixtureRef = "small_wav_v1"
			w.Submit.Multipart = &domain.CanaryMultipart{
				FileField: "file",
				Fields:    map[string]domain.CanaryValue{"token": {Kind: domain.CanaryValueSecret, SecretRef: "upload"}},
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var landed canaryHop
			target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { landed.record(r); canaryAccept(w) }))
			defer target.Close()
			redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+"/files/upload", http.StatusTemporaryRedirect)
			}))
			defer redirector.Close()
			m := canaryMonitor(t, redirector.URL, mutate)
			res := canaryTLSProber(t, target, redirector).Probe(context.Background(), m)
			if landed.reached {
				t.Fatalf("the secret-bearing body reached another origin: %q", landed.body)
			}
			if !strings.Contains(res.Msg, "redirect refused") || strings.Contains(res.Msg, "://") || strings.Contains(res.Msg, "s3cr3t") {
				t.Fatalf("failure %q, want a bounded redirect-refused reason with no URL or secret", res.Msg)
			}
		})
	}
}

// A body without a secret still follows a cross-origin 307 — the upload-then-redirect-to-storage
// flow is legitimate — with the binding headers stripped.
func TestCanaryDeliversBodyWithoutSecretAcrossOrigin(t *testing.T) {
	var landed canaryHop
	targetMux := http.NewServeMux()
	targetMux.HandleFunc("/files/upload", func(w http.ResponseWriter, r *http.Request) { landed.record(r); canaryAccept(w) })
	targetMux.HandleFunc("/tasks/", canaryCompleted)
	target := httptest.NewTLSServer(targetMux)
	defer target.Close()
	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/files/upload", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	m := canaryMonitor(t, redirector.URL, func(w *domain.CanaryWorkflow) {
		credentialHeaders(w)
		delete(w.Submit.Body, "token")
		w.Completion.URL = target.URL + "/tasks/{{ correlation_id }}"
	})
	canaryTLSProber(t, target, redirector).Probe(context.Background(), m)
	assertBindingHeadersStripped(t, "the storage host", &landed)
	if !strings.Contains(landed.body, "only_audio") {
		t.Fatalf("the secret-free body was not delivered: %q", landed.body)
	}
}

// (c) The library's Referer — the previous hop's full URL — is removed on every hop.
func TestCanaryNeverSendsReferer(t *testing.T) {
	var landed canaryHop
	mux := http.NewServeMux()
	mux.HandleFunc("/files/upload", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/files/upload-v2", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/files/upload-v2", func(w http.ResponseWriter, r *http.Request) { landed.record(r); canaryAccept(w) })
	mux.HandleFunc("/tasks/", canaryCompleted)
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	m := canaryMonitor(t, srv.URL, nil)
	canaryTLSProber(t, srv).Probe(context.Background(), m)
	if !landed.reached || landed.referer != "" {
		t.Fatalf("reached=%v Referer=%q, want reached with no Referer", landed.reached, landed.referer)
	}
}
