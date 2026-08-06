package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccessLogLevelsAndFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := AccessLog(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("ok"))
	}))

	// API path → INFO with the full field set.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/monitors", nil))
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line: %v (%q)", err, buf.String())
	}
	if line["msg"] != "http_request" || line["level"] != "INFO" ||
		line["path"] != "/api/v1/monitors" || line["status"] != float64(418) || line["bytes"] != float64(2) {
		t.Fatalf("unexpected line: %v", line)
	}

	// Static catch-all → DEBUG.
	buf.Reset()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if !strings.Contains(buf.String(), `"level":"DEBUG"`) {
		t.Fatalf("static request not at DEBUG: %q", buf.String())
	}

	// The SSE stream is skipped entirely.
	buf.Reset()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))
	if buf.Len() != 0 {
		t.Fatalf("SSE request must not be logged: %q", buf.String())
	}
}
