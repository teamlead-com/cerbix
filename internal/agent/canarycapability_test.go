package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-029 invariant 6, pull half: the heartbeat IS this agent's announcement, so what it claims must
// come from the runner it actually has. An agent that announced a workflow it cannot execute would
// be handed exactly the jobs it must fail.
func TestTheHeartbeatAnnouncesWhatTheRunnerActuallyHas(t *testing.T) {
	var got struct {
		Capabilities struct {
			WorkflowKinds []string `json:"workflow_kinds"`
		} `json:"capabilities"`
	}
	seen := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("heartbeat body: %v", err)
		}
		seen++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// recordingRunner supports the canary; fixedRunner does not. Both are exercised, because the
	// interesting half is the agent that must announce NOTHING.
	New(srv.URL, "tok", "pull1", &recordingRunner{}, logger).heartbeat(context.Background())
	if len(got.Capabilities.WorkflowKinds) != 1 || got.Capabilities.WorkflowKinds[0] != domain.CanaryCapabilityOfThisBinary() {
		t.Fatalf("capable agent announced %#v, want this binary's token", got.Capabilities.WorkflowKinds)
	}

	New(srv.URL, "tok", "pull1", fixedRunner{}, logger).heartbeat(context.Background())
	if len(got.Capabilities.WorkflowKinds) != 0 {
		t.Fatalf("an agent with no canary runner announced %#v", got.Capabilities.WorkflowKinds)
	}
	if seen != 2 {
		t.Fatalf("the endpoint saw %d heartbeats, want 2", seen)
	}
}

// The claim carries the same declaration as the heartbeat, and for a different reason: the heartbeat
// tells core which regions are capable, the header decides which rows THIS request may take. An
// agent that announced in one and not the other would poll forever for a job it is never handed.
func TestTheClaimDeclaresTheSameCapabilityAsTheHeartbeat(t *testing.T) {
	var header string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("X-Cerbix-Workflow-Kinds")
		_, _ = w.Write([]byte(`{"jobs":[],"tokens":[]}`))
	}))
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, _, _, err := New(srv.URL, "tok", "pull1", &recordingRunner{}, logger).claim(context.Background()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if header != domain.CanaryCapabilityOfThisBinary() {
		t.Fatalf("claim declared %q, want this binary's token", header)
	}
	if _, _, _, err := New(srv.URL, "tok", "pull1", fixedRunner{}, logger).claim(context.Background()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if header != "" {
		t.Fatalf("an agent with no canary runner declared %q", header)
	}
}
