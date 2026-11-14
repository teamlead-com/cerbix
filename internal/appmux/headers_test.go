package appmux

// S9b's server half (sec-hardening-2 H-9b): every response of the application handler carries
// `X-Content-Type-Options: nosniff`, `X-Frame-Options` (DENY; SAMEORIGIN on the public status
// pages) and the Content-Security-Policy of the spec — in this release REPORT-ONLY, so no enforcing
// header yet. "Public" is one predicate (isPublicPage), asked in one place: for every
// path, Referrer-Policy no-referrer ⇔ X-Frame-Options SAMEORIGIN ⇔ frame-ancestors 'self'.

import (
	"net/http"
	"testing"
)

// The policy as the spec writes it, spelled out here rather than taken from the code's constants:
// a test that read them would agree with any typo.
const (
	wantCSPApp    = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'"
	wantCSPPublic = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'self'"
)

func TestSecurityHeadersOnEverySurfaceOfTheApplicationPort(t *testing.T) {
	w := newWorld(t)
	tok := "?token=" + w.pageToken
	cases := []struct {
		path   string
		opts   []reqOpt
		public bool
	}{
		{"/status/" + w.pageSlug + tok, nil, true},
		{"/status/" + w.pageSlug + "/history" + tok, nil, true},
		{"/STATUS/" + w.pageSlug + tok, nil, true},
		{"/Status/" + w.pageSlug + "/history" + tok, nil, true},
		{"/status", nil, true},
		{"/api/v1/public/status-pages/" + w.pageSlug + tok, nil, true},
		{"/API/V1/PUBLIC/status-pages/" + w.pageSlug + tok, nil, true},
		{"/api/v1/public/status-pages/" + w.pageSlug + "/feed" + tok, nil, true},
		{"/api/v1/public/status-pages/no-such-page", nil, true},
		{"/api/v1/public/branding", nil, true},
		{"/", nil, false},
		{"/login", nil, false},
		{"/statuspages", nil, false}, // not under /status/
		{"/status-pages", nil, false},
		{"/settings", nil, false},
		{"/favicon.svg", nil, false},
		{"/auth/config", nil, false},
		{"/auth/logout", nil, false}, // the 405
		{"/api/v1/me", []reqOpt{withCookie(w.adminSess)}, false},
		{"/api/v1/me", nil, false}, // the 401
		{"/api/v1/no-such-route", []reqOpt{withCookie(w.adminSess)}, false},
	}
	for _, c := range cases {
		rec := w.do(http.MethodGet, c.path, nil, c.opts...)
		h := rec.Header()
		if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s (%d): X-Content-Type-Options = %q, want nosniff", c.path, rec.Code, got)
		}
		wantXFO, wantCSP, wantRP := "DENY", wantCSPApp, "strict-origin-when-cross-origin"
		if c.public {
			wantXFO, wantCSP, wantRP = "SAMEORIGIN", wantCSPPublic, "no-referrer"
		}
		if got := h.Get("X-Frame-Options"); got != wantXFO {
			t.Errorf("GET %s (%d): X-Frame-Options = %q, want %q", c.path, rec.Code, got, wantXFO)
		}
		if got := h.Get("Content-Security-Policy-Report-Only"); got != wantCSP {
			t.Errorf("GET %s (%d): Content-Security-Policy-Report-Only = %q, want %q", c.path, rec.Code, got, wantCSP)
		}
		if got := h.Get("Content-Security-Policy"); got != "" {
			t.Errorf("GET %s (%d): an ENFORCING Content-Security-Policy %q in the report-only release", c.path, rec.Code, got)
		}
		if got := h.Get("Referrer-Policy"); got != wantRP {
			t.Errorf("GET %s (%d): Referrer-Policy = %q, want %q (one predicate decides both)", c.path, rec.Code, got, wantRP)
		}
	}
}
