package config

import (
	"strings"
	"testing"
)

// Review of iter-0212, P2-4: the config file's post-logout URL obeys the rule the settings write
// enforces (domain.PostLogoutURLAllowed); a value that breaks it refuses startup instead of being
// served as "/" in silence. LogoutHandler still checks it when served, for rows saved earlier.
func TestPostLogoutRedirectURLIsALinkOrAStartupError(t *testing.T) {
	for _, bad := range []string{"javascript:alert(1)", "/\\evil.example", "//evil.example", "https://:443/x", "/bye "} {
		c := defaults()
		c.OIDC.PostLogoutRedirectURL = bad
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "oidc.post_logout_redirect_url") {
			t.Errorf("oidc.post_logout_redirect_url %q: Validate() = %v, want a refusal naming the key", bad, err)
		}
	}
	for _, good := range []string{"", "/", "/signed-out", "https://idp.example/realms/x/protocol/openid-connect/logout"} {
		c := defaults()
		c.OIDC.PostLogoutRedirectURL = good
		if err := c.Validate(); err != nil {
			t.Errorf("oidc.post_logout_redirect_url %q: %v", good, err)
		}
	}
}
