package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/teamlead-com/cerbix/internal/api"
)

// The test-connection request contract (iteration 3 of sec-hardening-2, outside its spec): the
// schema openapi.yaml names for POST /api/v1/projects/{projectID}/monitors/test must describe the
// body the server decodes. It referenced CreateMonitor, whose `name`, `retries`,
// `failure_threshold` and the rest the strict decoder refuses with 400 — a client generated from
// the contract could not call the endpoint. Every property the schema declares is sent, one at a
// time, and none may be refused as an unknown field; the type is required, as the server requires.
func TestTestConnectionBodySchemaIsWhatTheServerDecodes(t *testing.T) {
	data, err := os.ReadFile("../../openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	dig := func(v any, keys ...string) any {
		for _, k := range keys {
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[k]
		}
		return v
	}
	ref, _ := dig(doc, "paths", "/api/v1/projects/{projectID}/monitors/test", "post", "requestBody",
		"content", "application/json", "schema", "$ref").(string)
	name := strings.TrimPrefix(ref, "#/components/schemas/")
	schema, _ := dig(doc, "components", "schemas", name).(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		t.Fatalf("test-connection body schema %q declares no properties", ref)
	}
	if req, _ := schema["required"].([]any); len(req) != 1 || req[0] != "type" {
		t.Errorf("schema %s requires %v; the server requires exactly type", name, req)
	}
	// The server applies its own defaults (timeout 10, interval 60) to an omitted or non-positive
	// value; an OpenAPI `default` would tell a generated client to send them, and for async_canary
	// interval 60 against a larger timeout is a 400. The defaults live in the descriptions (review of
	// iter-0211, finding 6).
	for p, v := range props {
		if m, _ := v.(map[string]any); m != nil {
			if _, has := m["default"]; has {
				t.Errorf("schema %s property %q declares a default; the server's own default belongs in its description", name, p)
			}
		}
	}
	if dig(doc, "paths", "/api/v1/projects/{projectID}/monitors/test", "post", "responses", "501") == nil {
		t.Error("test-connection does not document its 501 (no prober wired to the API)")
	}

	sample := map[string]any{"type": "tcp", "target": "localhost:5432", "method": "GET", "region": "core",
		"timeout_seconds": 5, "interval_seconds": 30, "conditions": []string{}, "config": map[string]string{}}
	names := make([]string, 0, len(props))
	for p := range props {
		names = append(names, p)
	}
	sort.Strings(names)
	tester := &countingProbe{}
	h := api.New(seededStore(), slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithTester(tester).Router()
	for _, p := range names {
		body := map[string]any{"type": "tcp", "target": "localhost:5432"}
		v, ok := sample[p]
		if !ok {
			t.Errorf("schema %s declares %q, which the server's test body does not decode", name, p)
			continue
		}
		body[p] = v
		raw, _ := json.Marshal(body)
		rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/monitors/test", string(raw))
		if rec.Code != http.StatusOK {
			t.Errorf("schema %s property %q: the server answered %d %s", name, p, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}
