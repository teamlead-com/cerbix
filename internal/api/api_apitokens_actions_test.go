package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-025 D12 (invariant 17) on the token routes: the optional `actions` allow-list on create —
// stored as given, validated against the central catalogue (`action_unknown`) and against the
// role's own grants through authz.Can (`action_not_granted`), written into the `token.create`
// audit row, read back as an explicit `null` when unrestricted and as the array otherwise, and
// immutable because no route can change it.

func TestApiTokenActionsCreateStoredAndAudited(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/tokens", `{"name":"ci","role":"editor","actions":["gate:evaluate","change:record"]}`)
	out := wantStatus(t, rec, http.StatusCreated, "")
	stored := fs.apiTokens["tok-new"]
	if strings.Join(stored.Actions, ",") != "gate:evaluate,change:record" {
		t.Fatalf("stored actions = %v", stored.Actions)
	}
	var resp struct {
		ApiToken json.RawMessage `json:"api_token"`
	}
	_ = json.Unmarshal(out, &resp)
	var tok map[string]any
	_ = json.Unmarshal(resp.ApiToken, &tok)
	if got, _ := json.Marshal(tok["actions"]); string(got) != `["gate:evaluate","change:record"]` {
		t.Fatalf("read model actions = %s", got)
	}
	if len(fs.audit) != 1 || fs.audit[0].Action != "token.create" || fs.audit[0].Target != "editor · ci · actions: gate:evaluate,change:record" {
		t.Fatalf("audit = %+v", fs.audit)
	}

	// Without a list (absent or null): nil is stored, the read model says null, the audit row
	// reads exactly as before the list existed.
	for _, body := range []string{`{"name":"ops","role":"editor"}`, `{"name":"ops","role":"editor","actions":null}`} {
		fs := seededStore()
		h := newHandler(fs)
		out := wantStatus(t, do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/tokens", body), http.StatusCreated, "")
		if fs.apiTokens["tok-new"].Actions != nil {
			t.Fatalf("%s: stored actions = %v, want nil", body, fs.apiTokens["tok-new"].Actions)
		}
		if !strings.Contains(string(out), `"actions":null`) {
			t.Fatalf("%s: read model must say null: %s", body, out)
		}
		if fs.audit[0].Target != "editor · ops" {
			t.Fatalf("%s: audit = %q", body, fs.audit[0].Target)
		}
	}

	// An EMPTY list is a list — stored, shown, audited — and grants nothing (authz).
	fs = seededStore()
	h = newHandler(fs)
	out = wantStatus(t, do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/tokens", `{"name":"none","role":"editor","actions":[]}`), http.StatusCreated, "")
	if fs.apiTokens["tok-new"].Actions == nil || len(fs.apiTokens["tok-new"].Actions) != 0 {
		t.Fatalf("stored actions = %#v, want an empty non-nil list", fs.apiTokens["tok-new"].Actions)
	}
	if !strings.Contains(string(out), `"actions":[]`) || fs.audit[0].Target != "editor · none · actions: (none)" {
		t.Fatalf("empty list: %s / %q", out, fs.audit[0].Target)
	}
}

// `not:an:action` is 400 `action_unknown` naming it, and nothing is stored or audited.
func TestApiTokenActionsUnknownIsRefused(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/tokens", `{"name":"ci","role":"editor","actions":["change:record","not:an:action"]}`)
	out := wantStatus(t, rec, http.StatusBadRequest, "action_unknown")
	if got := errorOf(t, out); !strings.HasPrefix(got, "action_unknown (actions)") || !strings.Contains(got, `"not:an:action"`) {
		t.Fatalf("error = %q", got)
	}
	for _, bad := range []string{`["change:read"]`, `[""]`, `["CHANGE:RECORD"]`} {
		wantStatus(t, do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/tokens", `{"name":"ci","role":"editor","actions":`+bad+`}`), http.StatusBadRequest, "action_unknown")
	}
	if _, stored := fs.apiTokens["tok-new"]; stored || len(fs.audit) != 0 {
		t.Fatalf("a refused create stored or audited something: %v %v", fs.apiTokens, fs.audit)
	}
}

// The list can only NARROW the role: an entry the role does not grant is 400 `action_not_granted`
// — asked of authz.Can through a principal shaped as the auth layer will build it, so a
// project-scoped editor token may list change:record and a viewer may not.
func TestApiTokenActionsMustBeGrantedByTheRole(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	for body, want := range map[string]string{
		`{"name":"ci","role":"viewer","actions":["change:record"]}`:                       `action_not_granted (actions): "change:record" is not granted to role viewer`,
		`{"name":"ci","role":"editor","actions":["gate:override"]}`:                       `action_not_granted (actions): "gate:override" is not granted to role editor`,
		`{"name":"ci","role":"editor","actions":["gate:evaluate","org:manage"]}`:          `action_not_granted (actions): "org:manage" is not granted to role editor`,
		`{"name":"ci","role":"project_admin","project_id":"p1","actions":["org:manage"]}`: `action_not_granted (actions): "org:manage" is not granted to role project_admin`,
	} {
		out := wantStatus(t, do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/tokens", body), http.StatusBadRequest, "action_not_granted")
		if got := errorOf(t, out); got != want {
			t.Fatalf("%s: error = %q, want %q", body, got, want)
		}
	}
	if _, stored := fs.apiTokens["tok-new"]; stored {
		t.Fatal("a refused create stored a token")
	}
	wantStatus(t, do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/tokens", `{"name":"ci","role":"editor","project_id":"p1","actions":["gate:evaluate","change:record"]}`), http.StatusCreated, "")
	wantStatus(t, do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/tokens", `{"name":"ro","role":"viewer","actions":["gate:evaluate","project:read"]}`), http.StatusCreated, "")
	wantStatus(t, do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/tokens", `{"name":"pa","role":"project_admin","project_id":"p1","actions":["gate:override"]}`), http.StatusCreated, "")
}

// The read model: `actions` is an explicit null for every token without a list (the whole
// existing token inventory) and the array for one with a list.
func TestApiTokenActionsReadModelNullVsArray(t *testing.T) {
	fs := seededStore()
	fs.apiTokens["at2"] = domain.ApiToken{ID: "at2", OrgID: "o1", Name: "ci", Role: domain.RoleEditor, Actions: []string{"gate:evaluate", "change:record"}}
	fs.apiTokens["at3"] = domain.ApiToken{ID: "at3", OrgID: "o1", Name: "none", Role: domain.RoleEditor, Actions: []string{}}
	h := newHandler(fs)
	out := wantStatus(t, do(h, o1Admin, http.MethodGet, "/api/v1/organizations/o1/tokens", ""), http.StatusOK, "")
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(out, &list); err != nil || len(list) != 3 {
		t.Fatalf("list = %s (%v)", out, err)
	}
	byID := map[string]string{}
	for _, tok := range list {
		var id string
		_ = json.Unmarshal(tok["id"], &id)
		raw, has := tok["actions"]
		if !has {
			t.Fatalf("token %s has no actions key: %s", id, out)
		}
		byID[id] = string(raw)
	}
	if byID["at1"] != "null" || byID["at2"] != `["gate:evaluate","change:record"]` || byID["at3"] != "[]" {
		t.Fatalf("actions by id = %v", byID)
	}
}

// Immutable after create (D-0209 answer 5): no route changes a token — PATCH and PUT on a token
// are 405, and the body of a create is the only place the list is ever read.
func TestApiTokenActionsAreImmutable(t *testing.T) {
	h := newHandler(seededStore())
	for _, method := range []string{http.MethodPatch, http.MethodPut, http.MethodPost} {
		rec := do(h, o1Admin, method, "/api/v1/tokens/at1", `{"actions":["change:record"]}`)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /tokens/at1 = %d, want 405", method, rec.Code)
		}
	}
	// A wrong type for the list is the body decoder's refusal, not a stored surprise.
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/tokens", `{"name":"ci","role":"editor","actions":"change:record"}`)
	wantStatus(t, rec, http.StatusBadRequest, "invalid JSON body")
}
