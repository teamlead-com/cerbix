package prober

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-029 phase E: the SSE completion — the shape the first use case actually uses. What is asserted
// here is the terminal-event contract (§5.4): the result document is the payload of the event whose
// type equals `success_event`, and NOT an earlier event, the submit response, or a later event that
// happens to arrive before the connection closes.

// sseFixture serves a submit and then a stream of the lines the test gives it.
func sseFixture(t *testing.T, contentType string, lines ...string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/files/upload", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "task-42"})
	})
	mux.HandleFunc("/tasks/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		for _, l := range lines {
			fmt.Fprint(w, l)
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func sseMonitor(t *testing.T, base string, mutate func(w *domain.CanaryWorkflow)) domain.Monitor {
	return canaryMonitor(t, base, func(w *domain.CanaryWorkflow) {
		w.Completion = domain.CanaryCompletion{
			Kind:    domain.CanaryCompletionSSE,
			URL:     base + "/tasks/{{ correlation_id }}/events",
			Timeout: 10,
			SSE: &domain.CanarySSE{
				SuccessEvent:       "task.completed",
				FailureEvents:      []string{"task.failed"},
				RequiredJSONFields: []string{"s3_path"},
			},
		}
		if mutate != nil {
			mutate(w)
		}
	})
}

func TestCanarySSETakesTheSuccessEventAsTheResultDocument(t *testing.T) {
	srv := sseFixture(t, "text/event-stream",
		": keep-alive\n\n",
		"event: task.progress\ndata: {\"percent\":50}\n\n",
		"event: task.completed\ndata: {\"s3_path\":\"canary/out.wav\",\"byte_size\":10}\n\n",
		"event: task.failed\ndata: {\"why\":\"later events do not matter\"}\n\n",
	)
	m := sseMonitor(t, srv.URL, nil)
	res := canaryTestProber().Probe(context.Background(), m)
	if res.Msg != "" {
		t.Fatalf("a completed stream must pass: %s", res.Msg)
	}
	if !res.Connected {
		t.Fatal("a completed journey must report connected")
	}
}

func TestCanarySSEFailureAndMalformedCases(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		lines       []string
		want        string
	}{
		{"declared failure event", "text/event-stream",
			[]string{"event: task.failed\ndata: {}\n\n"}, "declared failure event"},
		{"stream ends with no terminal event", "text/event-stream",
			[]string{"event: task.progress\ndata: {}\n\n"}, "stream ended without a terminal event"},
		{"success payload is not JSON", "text/event-stream",
			[]string{"event: task.completed\ndata: not-json\n\n"}, "not JSON"},
		{"success event missing a required field", "text/event-stream",
			[]string{"event: task.completed\ndata: {\"byte_size\":1}\n\n"}, "missing a required field"},
		{"content type is not a stream", "application/json",
			[]string{"{\"status\":\"completed\"}"}, "content type is not text/event-stream"},
		{"one line past the bound", "text/event-stream",
			[]string{"event: task.completed\ndata: " + strings.Repeat("x", canaryMaxSSELineBytes+10) + "\n\n"}, "too large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := sseFixture(t, tc.contentType, tc.lines...)
			m := sseMonitor(t, srv.URL, nil)
			res := canaryTestProber().Probe(context.Background(), m)
			if !strings.HasPrefix(res.Msg, string(domain.CanaryStageAwaitResult)+":") {
				t.Fatalf("msg = %q, want an await_result failure", res.Msg)
			}
			if !strings.Contains(res.Msg, tc.want) {
				t.Fatalf("msg = %q, want %q", res.Msg, tc.want)
			}
		})
	}
}

// A multi-line `data:` field is ONE payload: a canary that kept only the last line would judge a
// truncated document and report a terminal state that never happened.
//
// What this pins is the ACCUMULATION. The separator itself — a newline, per the SSE grammar — is not
// distinguishable through a JSON payload, because JSON ignores whitespace between tokens: a mutation
// that joins the lines with nothing at all still produces the same document. Said here rather than
// left as a gap someone later mistakes for coverage; the separator is correct by reading, and a
// payload that could tell the difference would have to be a format this contract does not carry.
func TestCanarySSEAccumulatesMultiLineData(t *testing.T) {
	srv := sseFixture(t, "text/event-stream",
		"event: task.completed\ndata: {\"s3_path\":\ndata: \"canary/out.wav\", \"byte_size\": 10}\n\n")
	m := sseMonitor(t, srv.URL, nil)
	if res := canaryTestProber().Probe(context.Background(), m); res.Msg != "" {
		t.Fatalf("a multi-line data payload must be joined: %s", res.Msg)
	}
}
