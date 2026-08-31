package domain

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func TestApiTokenValidate(t *testing.T) {
	// Org-scoped editor token is valid.
	if err := (ApiToken{OrgID: "o1", Name: "ci", Role: RoleEditor}).Validate(); err != nil {
		t.Fatalf("valid org token rejected: %v", err)
	}
	// Project-scoped project_admin token is valid.
	if err := (ApiToken{OrgID: "o1", ProjectID: "p1", Name: "deploy", Role: RoleProjectAdmin}).Validate(); err != nil {
		t.Fatalf("valid project token rejected: %v", err)
	}
	// org_admin at project scope is invalid.
	if err := (ApiToken{OrgID: "o1", ProjectID: "p1", Name: "x", Role: RoleOrgAdmin}).Validate(); err == nil {
		t.Error("org_admin at project scope should fail")
	}
	// project_admin at org scope is invalid.
	if err := (ApiToken{OrgID: "o1", Name: "x", Role: RoleProjectAdmin}).Validate(); err == nil {
		t.Error("project_admin at org scope should fail")
	}
	for _, tc := range []struct {
		name string
		tok  ApiToken
	}{
		{"no org", ApiToken{Name: "x", Role: RoleEditor}},
		{"no name", ApiToken{OrgID: "o1", Role: RoleEditor}},
		{"bad role", ApiToken{OrgID: "o1", Name: "x", Role: "root"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.tok.Validate(); err == nil {
				t.Errorf("expected %s to fail", tc.name)
			}
		})
	}
}

// The `required` list of the published `ApiToken` schema must be the set of keys this type ALWAYS
// serializes — not a list somebody read off the struct tags and hoped stayed true. Review [14] of
// the close-out party found the schema had no `required` at all, so the generated client typed
// `actions?: string[] | null` while D12 and this type guarantee the key is present with an explicit
// null. Marshalling a zero token is the honest way to ask: whatever survives here is what a client
// may rely on, and openapi.yaml lists exactly these.
func TestApiTokenAlwaysSerializesTheseKeys(t *testing.T) {
	b, err := json.Marshal(ApiToken{})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"actions", "created_at", "id", "name", "org_id", "role"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("always-present keys = %v, want %v — openapi.yaml ApiToken.required must equal this set", keys, want)
	}
	if string(got["actions"]) != "null" {
		t.Fatalf("actions = %s, want an explicit null when unrestricted (D12)", got["actions"])
	}
}
