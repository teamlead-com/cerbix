package auth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// S4 (sec-hardening-2 H-4, the boundary as the owner settled it on 2026-11-14): a JSON body is
// decoded only under a JSON content type — on the three unauthenticated /auth/local/* routes too,
// which decode with their own json.NewDecoder rather than the API's decodeJSON. The check runs
// BEFORE every limiter and before the body is read: a refused request spends no attempt, and it
// answers identically for an account that exists and one that does not.
func TestLocalAuthRoutesRefuseANonJSONBodyBeforeAnyLimiter(t *testing.T) {
	type route struct {
		path    string
		key     string // the per-IP limiter key prefix the route spends
		body    func(email string) string
		handler func(a *Authenticator) http.HandlerFunc
		ok      int // the status a well-formed JSON request gets afterwards
	}
	routes := []route{
		{"/auth/local/login", "login|", func(e string) string { return `{"username":"` + e + `","password":"` + rightPassword + `"}` },
			func(a *Authenticator) http.HandlerFunc { return a.LocalLoginHandler }, http.StatusOK},
		{"/auth/local/reset/request", "reset|", func(e string) string { return `{"email":"` + e + `"}` },
			func(a *Authenticator) http.HandlerFunc { return a.ResetRequestHandler }, http.StatusOK},
		{"/auth/local/reset/confirm", "reset|", func(string) string { return `{"token":"nope","new_password":"long-enough-1"}` },
			func(a *Authenticator) http.HandlerFunc { return a.ResetConfirmHandler }, http.StatusBadRequest},
	}
	const ip = "10.0.0.7"
	for _, rt := range routes {
		a, _ := limitedAuthenticator(t, 2) // two attempts per IP per minute
		h := rt.handler(a)
		send := func(ct, email string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, rt.path, strings.NewReader(rt.body(email)))
			req.RemoteAddr = ip + ":40000"
			if ct != "" {
				req.Header.Set("Content-Type", ct)
			}
			rec := httptest.NewRecorder()
			h(rec, req)
			return rec
		}
		var first string
		for i, ct := range []string{"text/plain", "", "text/plain; charset=utf-8", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
			for _, email := range []string{"u@x.com", "nobody@x.com"} { // an account that exists, one that does not
				rec := send(ct, email)
				if rec.Code != http.StatusUnsupportedMediaType {
					t.Errorf("%s with Content-Type %q (%s): %d %s, want 415", rt.path, ct, email, rec.Code, strings.TrimSpace(rec.Body.String()))
					continue
				}
				if i == 0 && email == "u@x.com" {
					first = rec.Body.String()
				} else if rec.Body.String() != first {
					t.Errorf("%s: the refusal differs between requests (%q vs %q) — an existence oracle", rt.path, rec.Body.String(), first)
				}
			}
		}
		a.limits.perIP.mu.Lock()
		spent := len(a.limits.perIP.hits[rt.key+ip])
		a.limits.perIP.mu.Unlock()
		if spent != 0 {
			t.Errorf("%s: ten refused requests spent %d per-IP attempts, want 0", rt.path, spent)
		}
		// The same route under a JSON content type behaves as before — and is not limited, because
		// nothing above spent an attempt (the limit is two).
		for _, ct := range []string{"application/json", "application/json; charset=utf-8"} {
			if rec := send(ct, "u@x.com"); rec.Code != rt.ok {
				t.Errorf("%s with Content-Type %q: %d %s, want %d", rt.path, ct, rec.Code, strings.TrimSpace(rec.Body.String()), rt.ok)
			}
		}
	}
}

// jsonPost is a POST that declares its JSON, as the SPA and every real client of /auth/local/*
// does — the shape every other test of these handlers sends since S4 covers them.
func jsonPost(target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, body)
	req.Header.Set("Content-Type", "application/json")
	return req
}
