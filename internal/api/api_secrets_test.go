package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// newSecretsHandler builds a router with the secret inventory feature ON.
// The plain newHandler builds it OFF (the default), which the feature-switch
// test relies on.
func newSecretsHandler(fs *fakeStore) http.Handler {
	return api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithSecretsEnabled(true).Router()
}

var p1Editor = authz.Principal{UserID: "pe", Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleEditor}}}

func TestSecretsFeatureDisabled(t *testing.T) {
	h := newHandler(seededStore()) // no WithSecretsEnabled → feature off
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/projects/p1/secrets", ""},
		{http.MethodPost, "/api/v1/projects/p1/secrets", `{"name":"db-pass","value":"v"}`},
		{http.MethodPatch, "/api/v1/projects/p1/secrets/db-pass", `{"value":"v2"}`},
		{http.MethodDelete, "/api/v1/projects/p1/secrets/db-pass", ""},
	} {
		rec := do(h, o1Admin, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s with feature off = %d, want 404", tc.method, tc.path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "feature_disabled") {
			t.Fatalf("%s %s body = %q, want feature_disabled", tc.method, tc.path, rec.Body.String())
		}
	}
}

func TestSecretsAuthzMatrix(t *testing.T) {
	h := newSecretsHandler(seededStore())

	// Viewer (ProjectRead) may list…
	if rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/secrets", ""); rec.Code != http.StatusOK {
		t.Fatalf("viewer list = %d, want 200", rec.Code)
	}
	// …but not mutate.
	if rec := do(h, p1Viewer, http.MethodPost, "/api/v1/projects/p1/secrets", `{"name":"db-pass","value":"v"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create = %d, want 403", rec.Code)
	}
	if rec := do(h, p1Viewer, http.MethodPatch, "/api/v1/projects/p1/secrets/db-pass", `{"value":"v2"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer update = %d, want 403", rec.Code)
	}
	if rec := do(h, p1Viewer, http.MethodDelete, "/api/v1/projects/p1/secrets/db-pass", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer delete = %d, want 403", rec.Code)
	}

	// Editor and org admin (ProjectWrite) may mutate.
	if rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/secrets", `{"name":"db-pass","value":"v"}`); rec.Code != http.StatusCreated {
		t.Fatalf("editor create = %d, want 201", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/secrets", `{"name":"api-key","value":"v"}`); rec.Code != http.StatusCreated {
		t.Fatalf("org admin create = %d, want 201", rec.Code)
	}
	if rec := do(h, p1Editor, http.MethodPatch, "/api/v1/projects/p1/secrets/api-key", `{"value":"v2"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("editor rotate = %d, want 204", rec.Code)
	}
	if rec := do(h, p1Editor, http.MethodDelete, "/api/v1/projects/p1/secrets/api-key", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("editor delete = %d, want 204", rec.Code)
	}

	// Outsider and foreign-project members get 404 (existence hidden), matching
	// the monitor endpoints.
	if rec := do(h, outsider, http.MethodGet, "/api/v1/projects/p1/secrets", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider list = %d, want 404", rec.Code)
	}
	if rec := do(h, p1Editor, http.MethodGet, "/api/v1/projects/p3/secrets", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign project list = %d, want 404", rec.Code)
	}
	if rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p3/secrets", `{"name":"x","value":"v"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign project create = %d, want 404", rec.Code)
	}
}

func TestSecretsNeverReturnValues(t *testing.T) {
	h := newSecretsHandler(seededStore())
	const value = "s3cr3t-value-never-echoed"

	rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/secrets", `{"name":"db-pass","value":"`+value+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201", rec.Code)
	}
	if strings.Contains(rec.Body.String(), value) {
		t.Fatalf("create response echoes the value: %s", rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if _, ok := created["value"]; ok {
		t.Fatalf("create response has a value field: %v", created)
	}
	for _, k := range []string{"id", "name", "created_at"} {
		if _, ok := created[k]; !ok {
			t.Fatalf("create response missing %q: %v", k, created)
		}
	}

	rec = do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/secrets", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("list Cache-Control = %q, want no-store", got)
	}
	if strings.Contains(rec.Body.String(), value) {
		t.Fatalf("list response contains the value: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"value"`) {
		t.Fatalf("list response has a value field: %s", rec.Body.String())
	}
	var list []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0]["name"] != "db-pass" {
		t.Fatalf("list = %v, want the one created secret", list)
	}
	usedBy, ok := list[0]["used_by"].(map[string]any)
	if !ok {
		t.Fatalf("list row has no used_by block: %v", list[0])
	}
	for _, k := range []string{"total", "file_managed"} {
		if _, ok := usedBy[k]; !ok {
			t.Fatalf("used_by missing %q: %v", k, usedBy)
		}
	}
}

func TestSecretsConflicts(t *testing.T) {
	fs := seededStore()
	h := newSecretsHandler(fs)

	if rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/secrets", `{"name":"db-pass","value":"v"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201", rec.Code)
	}
	// Duplicate create → 409 secret_exists.
	rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/secrets", `{"name":"db-pass","value":"v"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "secret_exists") {
		t.Fatalf("duplicate create = %d %q, want 409 secret_exists", rec.Code, rec.Body.String())
	}

	// Delete while referenced → 409 secret_in_use with the count.
	fs.secretRefs = map[string]int{"p1/db-pass": 3}
	rec = do(h, p1Editor, http.MethodDelete, "/api/v1/projects/p1/secrets/db-pass", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete in use = %d, want 409", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "secret_in_use" || body["count"] != float64(3) {
		t.Fatalf("delete in use body = %v, want secret_in_use count 3", body)
	}

	// Rename with file-managed references → 409 secret_renamed_in_use with the count.
	fs.secretFileRefs = map[string]int{"p1/db-pass": 2}
	rec = do(h, p1Editor, http.MethodPatch, "/api/v1/projects/p1/secrets/db-pass", `{"name":"db-pass-new"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("rename in use = %d, want 409", rec.Code)
	}
	body = map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "secret_renamed_in_use" || body["count"] != float64(2) {
		t.Fatalf("rename in use body = %v, want secret_renamed_in_use count 2", body)
	}

	// Rename onto an existing name → 409 secret_exists.
	fs.secretFileRefs = nil
	if rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/secrets", `{"name":"other","value":"v"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create other = %d, want 201", rec.Code)
	}
	rec = do(h, p1Editor, http.MethodPatch, "/api/v1/projects/p1/secrets/db-pass", `{"name":"other"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "secret_exists") {
		t.Fatalf("rename collision = %d %q, want 409 secret_exists", rec.Code, rec.Body.String())
	}
}

func TestSecretsValidation(t *testing.T) {
	h := newSecretsHandler(seededStore())

	// Bad slug → 400, message names the rule.
	rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/secrets", `{"name":"Bad_Name","value":"v"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid secret name") {
		t.Fatalf("bad slug = %d %q, want 400 invalid secret name", rec.Code, rec.Body.String())
	}
	// Empty value → 400, and the message never echoes a value.
	rec = do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/secrets", `{"name":"db-pass","value":""}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid secret value") {
		t.Fatalf("empty value = %d %q, want 400 invalid secret value", rec.Code, rec.Body.String())
	}
	// PATCH with neither field → 400.
	if rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/secrets", `{"name":"db-pass","value":"v"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201", rec.Code)
	}
	rec = do(h, p1Editor, http.MethodPatch, "/api/v1/projects/p1/secrets/db-pass", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty patch = %d, want 400", rec.Code)
	}
	// PATCH/DELETE of an unknown secret → 404.
	rec = do(h, p1Editor, http.MethodPatch, "/api/v1/projects/p1/secrets/missing", `{"value":"v2"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("patch missing = %d, want 404", rec.Code)
	}
	rec = do(h, p1Editor, http.MethodDelete, "/api/v1/projects/p1/secrets/missing", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing = %d, want 404", rec.Code)
	}
}

func TestSecretsAudit(t *testing.T) {
	fs := seededStore()
	h := newSecretsHandler(fs)
	const value = "audit-must-never-see-this"

	if rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/secrets", `{"name":"db-pass","value":"`+value+`"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201", rec.Code)
	}
	if rec := do(h, p1Editor, http.MethodPatch, "/api/v1/projects/p1/secrets/db-pass", `{"name":"db-pass-2","value":"`+value+`"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("update = %d, want 204", rec.Code)
	}
	if rec := do(h, p1Editor, http.MethodDelete, "/api/v1/projects/p1/secrets/db-pass-2", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", rec.Code)
	}

	got := map[string]domain.AuditEntry{}
	for _, e := range fs.audit {
		got[e.Action] = e
	}
	for _, action := range []string{"secret.create", "secret.update", "secret.delete"} {
		e, ok := got[action]
		if !ok {
			t.Fatalf("no %s audit entry; have %+v", action, fs.audit)
		}
		if e.OrgID != "o1" {
			t.Fatalf("%s audit org = %q, want o1", action, e.OrgID)
		}
		// The actor travels store.SecretActor → in-tx audit row, not a handler recorder.
		if e.ActorUserID != p1Editor.UserID || e.ViaToken {
			t.Fatalf("%s audit actor = (%q, via_token=%t), want (%q, false)", action, e.ActorUserID, e.ViaToken, p1Editor.UserID)
		}
		if strings.Contains(e.Target, value) {
			t.Fatalf("%s audit target contains the value: %q", action, e.Target)
		}
	}
	// The update entry records what happened (rename + rotate), names only.
	upd := got["secret.update"].Target
	for _, want := range []string{"db-pass", "renamed=true", "rotated=true", "repointed=0"} {
		if !strings.Contains(upd, want) {
			t.Fatalf("secret.update target = %q, want it to contain %q", upd, want)
		}
	}
}

// TestSecretsNoStoreEverywhere: §5 — the WHOLE secret surface is uncacheable, including
// error, 404 and 409 responses, not only the successful GET.
func TestSecretsNoStoreEverywhere(t *testing.T) {
	fs := seededStore()
	h := newSecretsHandler(fs)
	if rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/secrets", `{"name":"db-pass","value":"v"}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed create = %d", rec.Code)
	}
	cases := []struct {
		name         string
		principal    authz.Principal
		method, path string
		body         string
		wantCode     int
	}{
		{"get ok", p1Editor, http.MethodGet, "/api/v1/projects/p1/secrets", "", http.StatusOK},
		{"create conflict", p1Editor, http.MethodPost, "/api/v1/projects/p1/secrets", `{"name":"db-pass","value":"v"}`, http.StatusConflict},
		{"patch validation", p1Editor, http.MethodPatch, "/api/v1/projects/p1/secrets/db-pass", `{}`, http.StatusBadRequest},
		{"delete missing", p1Editor, http.MethodDelete, "/api/v1/projects/p1/secrets/nope", "", http.StatusNotFound},
		{"foreign 404", p1Editor, http.MethodGet, "/api/v1/projects/p3/secrets", "", http.StatusNotFound},
		{"viewer 403", p1Viewer, http.MethodDelete, "/api/v1/projects/p1/secrets/db-pass", "", http.StatusForbidden},
	}
	for _, tc := range cases {
		rec := do(h, tc.principal, tc.method, tc.path, tc.body)
		if rec.Code != tc.wantCode {
			t.Fatalf("%s = %d, want %d", tc.name, rec.Code, tc.wantCode)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("%s: Cache-Control = %q, want no-store", tc.name, cc)
		}
	}
	// Feature-off responses are uncacheable too.
	hOff := newHandler(seededStore())
	rec := do(hOff, o1Admin, http.MethodGet, "/api/v1/projects/p1/secrets", "")
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("feature-off: Cache-Control = %q, want no-store", cc)
	}
}

// TestSecretsBodyBoundContract: a legal 4096-BYTE value may JSON-encode at up to ~6x
// (a control character escapes to a six-byte \u00XX sequence), so the transport cap must
// not reject it; 4097 decoded bytes still reject at the store rule.
func TestSecretsBodyBoundContract(t *testing.T) {
	h := newSecretsHandler(seededStore())
	big, err := json.Marshal(strings.Repeat("\x00", 4096))
	if err != nil {
		t.Fatal(err)
	}
	body := `{"name":"ctrl-value","value":` + string(big) + `}`
	if len(body) < 20<<10 {
		t.Fatalf("test setup: body should exceed the old 16KiB cap, got %d bytes", len(body))
	}
	if rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/secrets", body); rec.Code != http.StatusCreated {
		t.Fatalf("4096-byte control-char value = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	over, _ := json.Marshal(strings.Repeat("a", 4097))
	if rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/secrets", `{"name":"too-big","value":`+string(over)+`}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("4097-byte value = %d, want 400", rec.Code)
	}
}

// TestSecretsPatchNoOpAuditsNothing: PATCH{name: current} persists nothing → no audit row
// (audit is written in the store tx only on actual change).
func TestSecretsPatchNoOpAuditsNothing(t *testing.T) {
	fs := seededStore()
	h := newSecretsHandler(fs)
	if rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/secrets", `{"name":"db-pass","value":"v"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}
	before := len(fs.audit)
	if rec := do(h, p1Editor, http.MethodPatch, "/api/v1/projects/p1/secrets/db-pass", `{"name":"db-pass"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("no-op rename = %d, want 204", rec.Code)
	}
	if len(fs.audit) != before {
		t.Fatalf("no-op PATCH must not audit; rows grew %d -> %d: %+v", before, len(fs.audit), fs.audit[before:])
	}
}

// TestSecretsForeignProjectMutations: PATCH and DELETE against a foreign project are hidden
// as 404 exactly like GET/POST (completes the authz regression).
func TestSecretsForeignProjectMutations(t *testing.T) {
	h := newSecretsHandler(seededStore())
	if rec := do(h, p1Editor, http.MethodPatch, "/api/v1/projects/p3/secrets/db-pass", `{"value":"v"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign patch = %d, want 404", rec.Code)
	}
	if rec := do(h, p1Editor, http.MethodDelete, "/api/v1/projects/p3/secrets/db-pass", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign delete = %d, want 404", rec.Code)
	}
	if rec := do(h, outsider, http.MethodPatch, "/api/v1/projects/p1/secrets/db-pass", `{"value":"v"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider patch = %d, want 404", rec.Code)
	}
}

// TestSecretsAPITokenActorContract: a Cerbix API-token principal carries the synthetic
// "apitoken:<id>" UserID — the audit-identity contract maps it to a NULL actor with
// via_token=true, so mutations COMMIT (pre-fix the in-tx uuid cast aborted them all).
// An OIDC client-credentials principal has a real JIT user uuid and keeps attribution.
func TestSecretsAPITokenActorContract(t *testing.T) {
	fs := seededStore()
	h := newSecretsHandler(fs)
	tokenPrincipal := authz.Principal{
		UserID:      authz.SyntheticTokenActorPrefix + "tok1",
		Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleEditor}},
		ViaToken:    true,
	}

	if rec := do(h, tokenPrincipal, http.MethodPost, "/api/v1/projects/p1/secrets", `{"name":"db-pass","value":"v"}`); rec.Code != http.StatusCreated {
		t.Fatalf("api-token create = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	if rec := do(h, tokenPrincipal, http.MethodPatch, "/api/v1/projects/p1/secrets/db-pass", `{"value":"v2"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("api-token rotate = %d, want 204", rec.Code)
	}
	if rec := do(h, tokenPrincipal, http.MethodDelete, "/api/v1/projects/p1/secrets/db-pass", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("api-token delete = %d, want 204", rec.Code)
	}
	seen := 0
	for _, e := range fs.audit {
		if !strings.HasPrefix(e.Action, "secret.") {
			continue
		}
		seen++
		if e.ActorUserID != "" {
			t.Fatalf("%s actor = %q, want NULL for a synthetic token identity", e.Action, e.ActorUserID)
		}
		if !e.ViaToken {
			t.Fatalf("%s must be attributed via_token=true", e.Action)
		}
	}
	if seen != 3 {
		t.Fatalf("expected 3 secret audit rows, got %d", seen)
	}

	// OIDC client-credentials: a REAL user uuid + ViaToken — attribution is kept.
	ccPrincipal := authz.Principal{
		UserID:      "cc-user-uuid",
		Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleEditor}},
		ViaToken:    true,
	}
	if rec := do(h, ccPrincipal, http.MethodPost, "/api/v1/projects/p1/secrets", `{"name":"cc-pass","value":"v"}`); rec.Code != http.StatusCreated {
		t.Fatalf("cc create = %d, want 201", rec.Code)
	}
	if e := fs.audit[0]; e.Action != "secret.create" || e.ActorUserID != "cc-user-uuid" || !e.ViaToken {
		t.Fatalf("cc audit = %+v, want secret.create by cc-user-uuid via_token", e)
	}
}
