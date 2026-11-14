package appmux

// S9a (sec-hardening-2 H-9a): no request leaving a public status page carries a Referer. The
// unlisted page's capability is `?token=` in its URL, so the page, its JSON reads and its feeds
// are served with `Referrer-Policy: no-referrer`; everything else of the application port with
// `strict-origin-when-cross-origin`. Asserted on the response headers of the real composition.

import (
	"net/http"
	"testing"
)

func TestReferrerPolicyOnEverySurfaceOfTheApplicationPort(t *testing.T) {
	w := newWorld(t)
	tok := "?token=" + w.pageToken
	cases := []struct {
		path string
		opts []reqOpt
		want string
	}{
		// The public page (the SPA's document) and every public read behind it.
		{"/status/" + w.pageSlug + tok, nil, "no-referrer"},
		{"/status/" + w.pageSlug + "/history" + tok, nil, "no-referrer"},
		{"/api/v1/public/status-pages/" + w.pageSlug + tok, nil, "no-referrer"},
		{"/api/v1/public/status-pages/" + w.pageSlug + "/history" + tok, nil, "no-referrer"},
		{"/api/v1/public/status-pages/" + w.pageSlug + "/feed" + tok, nil, "no-referrer"},
		{"/api/v1/public/status-pages/" + w.pageSlug + "/feed" + tok + "&format=atom", nil, "no-referrer"},
		{"/api/v1/public/status-pages/no-such-page", nil, "no-referrer"}, // a 404 too
		{"/api/v1/public/branding", nil, "no-referrer"},
		// The SPA's routes match case-insensitively, so /STATUS/<slug> renders the public page too.
		{"/STATUS/" + w.pageSlug + tok, nil, "no-referrer"},
		{"/Status/" + w.pageSlug + "/history" + tok, nil, "no-referrer"},
		{"/API/V1/PUBLIC/status-pages/" + w.pageSlug + tok, nil, "no-referrer"},
		// The rest of the port.
		{"/", nil, "strict-origin-when-cross-origin"},
		{"/login", nil, "strict-origin-when-cross-origin"},
		{"/status-pages", nil, "strict-origin-when-cross-origin"},
		{"/auth/config", nil, "strict-origin-when-cross-origin"},
		{"/api/v1/me", []reqOpt{withCookie(w.adminSess)}, "strict-origin-when-cross-origin"},
		{"/api/v1/me", nil, "strict-origin-when-cross-origin"}, // the 401 too
	}
	for _, c := range cases {
		rec := w.do(http.MethodGet, c.path, nil, c.opts...)
		if got := rec.Header().Get("Referrer-Policy"); got != c.want {
			t.Errorf("GET %s (%d): Referrer-Policy = %q, want %q", c.path, rec.Code, got, c.want)
		}
	}
}
