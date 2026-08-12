package config

import "testing"

func TestProviderScopeIncludes(t *testing.T) {
	cases := []struct {
		name     string
		scope    ProviderScopeConfig
		org, prj string
		want     bool
	}{
		{"instance sees all", ProviderScopeConfig{Type: ProviderScopeInstance}, "acme", "payments", true},
		{"org matches own org", ProviderScopeConfig{Type: ProviderScopeOrganization, Organization: "acme"}, "acme", "payments", true},
		{"org rejects other org", ProviderScopeConfig{Type: ProviderScopeOrganization, Organization: "acme"}, "beta", "payments", false},
		{"project matches exact pair", ProviderScopeConfig{Type: ProviderScopeProject, Organization: "acme", Project: "payments"}, "acme", "payments", true},
		{"project rejects other project", ProviderScopeConfig{Type: ProviderScopeProject, Organization: "acme", Project: "payments"}, "acme", "billing", false},
		{"project rejects other org", ProviderScopeConfig{Type: ProviderScopeProject, Organization: "acme", Project: "payments"}, "beta", "payments", false},
		{"unknown type rejects", ProviderScopeConfig{Type: "bogus"}, "acme", "payments", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.scope.Includes(c.org, c.prj); got != c.want {
				t.Fatalf("Includes(%q,%q) = %v, want %v", c.org, c.prj, got, c.want)
			}
		})
	}
}
