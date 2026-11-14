package domain

import (
	"net/url"
	"strings"
	"unicode"
)

// The URLs an administrator stores and the SPA later puts into a DOM sink (sec-hardening-2 H-9b,
// H-10). A browser executes `javascript:` from an `<a href>` and from `window.location.assign`,
// and resolves `//host` and `/\host` as another host, so each is admitted by an explicit shape —
// never by refusing known-bad schemes. Both checks run on write AND when the value is served:
// validation on write does not clean what an earlier release already saved.
//
// Common to both (review of iter-0212, P2-3): no whitespace at either edge and no control byte
// (<= 0x1f, 0x7f) anywhere — the write paths trim before they check, so such a value can only be
// one an earlier release stored —, and an http(s) URL needs a host NAME (u.Hostname(), so
// "https://:443" has none).

// SupportURLAllowed reports whether s may be served as branding's support_url (H-10, S10): http or
// https with a host name, or mailto: with an address free of whitespace and control bytes. The
// empty string means "no support link" and is allowed.
func SupportURLAllowed(s string) bool {
	if s == "" {
		return true
	}
	if !cleanEdgesAndBytes(s) {
		return false
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "http", "https":
		return httpWithHostName(u)
	case "mailto":
		addr, err := url.PathUnescape(u.Opaque)
		return err == nil && addr != "" && !strings.ContainsFunc(addr, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
	}
	return false
}

// PostLogoutURLAllowed reports whether s may be handed to the SPA as the destination after
// sign-out (H-9b): a local path — `/…`, not `//…`, containing no backslash — or http(s) with a host
// name. The empty string is allowed (no destination configured).
func PostLogoutURLAllowed(s string) bool {
	if s == "" {
		return true
	}
	if !cleanEdgesAndBytes(s) {
		return false
	}
	if strings.HasPrefix(s, "/") {
		return !strings.HasPrefix(s, "//") && !strings.ContainsRune(s, '\\')
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && httpWithHostName(u)
}

// cleanEdgesAndBytes: no whitespace at either edge, no control byte anywhere.
func cleanEdgesAndBytes(s string) bool {
	if strings.TrimSpace(s) != s {
		return false
	}
	for i := 0; i < len(s); i++ {
		if b := s[i]; b <= 0x1f || b == 0x7f {
			return false
		}
	}
	return true
}

// httpWithHostName: an http(s) URL with a host name and no opaque part ("https:evil.example").
func httpWithHostName(u *url.URL) bool {
	return u.Hostname() != "" && u.Opaque == ""
}
