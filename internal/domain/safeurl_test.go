package domain

import "testing"

// Review of iter-0212, P2-3: the link-shaped settings (H-9b post_logout_url, H-10 support_url).
// Whitespace at either edge and control bytes (<= 0x1f, 0x7f) anywhere are refused by both; an
// http(s) URL needs a host NAME (u.Hostname(), so "https://:443" is not one); a mailto: needs an
// address without whitespace or control bytes.
func TestLinkShapedSettings(t *testing.T) {
	support := []struct {
		in string
		ok bool
	}{
		{"", true},
		{"https://help.example.com/support", true},
		{"http://intranet.example:8080/help", true},
		{"mailto:support@example.com", true},
		{"mailto:support@example.com?subject=cerbix", true},
		{"mailto:   ", false},
		{"mailto:   ?subject=x", false},
		{"mailto:a b@example.com", false},
		{"mailto:support@example.com\x7f", false}, // already refused by the parser; kept as a guard
		{"mailto:support@example.com\t", false},
		{"https://:443/x", false},
		{"http://:/", false},
		{"https://example.com/ ", false},
		{" https://example.com/", false},
		{"https://example.com/\x7f", false},
		{"https://example.com/\x1f", false},
		{"javascript:alert(1)", false},
		{"//evil.example", false},
	}
	for _, c := range support {
		if got := SupportURLAllowed(c.in); got != c.ok {
			t.Errorf("SupportURLAllowed(%q) = %v, want %v", c.in, got, c.ok)
		}
	}
	logout := []struct {
		in string
		ok bool
	}{
		{"", true},
		{"/", true},
		{"/bye", true},
		{"https://idp.example/logout?x=1", true},
		{"/bye ", false},
		{" /bye", false},
		{"/bye\x7f", false},
		{"https://:443/x", false},
		{"http://:/", false},
		{"https://example.com/ ", false},
		{"https://example.com/\x1f", false},
		{"/\\evil.example", false},
		{"//evil.example", false},
	}
	for _, c := range logout {
		if got := PostLogoutURLAllowed(c.in); got != c.ok {
			t.Errorf("PostLogoutURLAllowed(%q) = %v, want %v", c.in, got, c.ok)
		}
	}
}
