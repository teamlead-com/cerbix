package prober

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.example.com/monitoring/cerbix/internal/domain"
)

func synthMonitor(t *testing.T, sc domain.Scenario) domain.Monitor {
	t.Helper()
	raw, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	return domain.Monitor{
		ID: "m", Type: domain.MonitorSynthetic, IntervalSeconds: 60, TimeoutSeconds: 5,
		Config: map[string]string{domain.SyntheticScenarioKey: string(raw)},
	}
}

func TestSyntheticScenarioSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"token": "s3cr3t"}})
		case "/me":
			if r.Header.Get("Authorization") != "Bearer s3cr3t" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": "alice"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	r := NewRunnerWithGuard(NewGuard(true, false)) // allow loopback for httptest
	sc := domain.Scenario{Steps: []domain.SyntheticStep{
		{
			Name: "login", Method: "POST", URL: srv.URL + "/login",
			Extract: []domain.SyntheticExtract{{Var: "token", From: "json", Path: "data.token"}},
			Assert:  []domain.SyntheticAssert{{That: "status", Value: "200"}},
		},
		{
			Name: "profile", Method: "GET", URL: srv.URL + "/me",
			Headers: map[string]string{"Authorization": "Bearer {{token}}"},
			Assert: []domain.SyntheticAssert{
				{That: "status", Value: "200"},
				{That: "json", Path: "ok", Value: "true"},
				{That: "body_contains", Value: "alice"},
			},
		},
	}}
	hb := r.Run(context.Background(), synthMonitor(t, sc))
	if !hb.Up {
		t.Fatalf("scenario should pass: up=%v msg=%q", hb.Up, hb.Msg)
	}
}

func TestSyntheticScenarioAssertFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	r := NewRunnerWithGuard(NewGuard(true, false))
	sc := domain.Scenario{Steps: []domain.SyntheticStep{
		{Name: "health", URL: srv.URL + "/", Assert: []domain.SyntheticAssert{{That: "status", Value: "200"}}},
	}}
	hb := r.Run(context.Background(), synthMonitor(t, sc))
	if hb.Up {
		t.Fatalf("scenario should fail on status assert")
	}
	if hb.Msg == "" || hb.Code != 500 {
		t.Fatalf("expected step-scoped msg + code 500, got msg=%q code=%d", hb.Msg, hb.Code)
	}
}

func TestSyntheticMissingExtractFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"nope": 1})
	}))
	defer srv.Close()
	r := NewRunnerWithGuard(NewGuard(true, false))
	sc := domain.Scenario{Steps: []domain.SyntheticStep{
		{URL: srv.URL + "/", Extract: []domain.SyntheticExtract{{Var: "t", From: "json", Path: "data.token"}}},
	}}
	hb := r.Run(context.Background(), synthMonitor(t, sc))
	if hb.Up {
		t.Fatal("scenario should fail when an extract path is missing")
	}
}

func TestSyntheticSubstAndJSONPath(t *testing.T) {
	if got, ok := jsonPath(`{"items":[{"id":"x"},{"id":"y"}]}`, "items.1.id"); !ok || got != "y" {
		t.Fatalf("jsonPath array = %q ok=%v, want y", got, ok)
	}
	if got := subst("Bearer {{tok}}", map[string]string{"tok": "abc"}); got != "Bearer abc" {
		t.Fatalf("subst = %q", got)
	}
	if got := subst("no vars here", map[string]string{}); got != "no vars here" {
		t.Fatalf("subst passthrough = %q", got)
	}
}
