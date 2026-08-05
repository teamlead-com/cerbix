package domain

import "testing"

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
