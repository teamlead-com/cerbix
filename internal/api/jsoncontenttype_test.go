package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// S4 (sec-hardening-2 H-4 fix 2): a JSON body is decoded only under a JSON content type. Every
// decoder of the API is held to it — decodeJSON names it in the spec, and the other three decode
// the same request bodies by other routes (service declarations, gate policies and overrides,
// the gate's optional body). A body-less request is unaffected: the gate's empty POST needs none.
func TestEveryJSONDecoderRefusesANonJSONContentType(t *testing.T) {
	decoders := map[string]func(w http.ResponseWriter, r *http.Request) bool{
		"decodeJSON": func(w http.ResponseWriter, r *http.Request) bool {
			var v struct{ A string }
			return decodeJSON(w, r, &v)
		},
		"decodeJSONBody": func(w http.ResponseWriter, r *http.Request) bool {
			var v struct{ A string }
			return decodeJSONBody(w, r, 1<<10, &v)
		},
		"gateDecodeBody": func(w http.ResponseWriter, r *http.Request) bool {
			var v struct{ A string }
			return gateDecodeBody(w, r, &v)
		},
		"gateDecodeEmptyBody": func(w http.ResponseWriter, r *http.Request) bool {
			return gateDecodeEmptyBody(w, r)
		},
	}
	for name, decode := range decoders {
		body := `{"A":"x"}`
		if name == "gateDecodeEmptyBody" {
			body = `{}`
		}
		for _, c := range []struct {
			ct string
			ok bool
		}{
			{"", false},
			{"text/plain", false},
			{"text/plain; charset=utf-8", false},
			{"application/x-www-form-urlencoded", false},
			{"multipart/form-data; boundary=x", false},
			{"application/jsonp", false},
			{"application/json", true},
			{"application/json; charset=utf-8", true},
			{"APPLICATION/JSON", true},
		} {
			r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
			if c.ct != "" {
				r.Header.Set("Content-Type", c.ct)
			}
			w := httptest.NewRecorder()
			got := decode(w, r)
			switch {
			case got != c.ok:
				t.Errorf("%s with Content-Type %q: decoded = %v, want %v (%d %s)", name, c.ct, got, c.ok, w.Code, w.Body.String())
			case !c.ok && w.Code != http.StatusUnsupportedMediaType:
				t.Errorf("%s with Content-Type %q: %d, want 415", name, c.ct, w.Code)
			}
		}
	}
	// An empty body carries nothing to decode: each decoder answers as it did (its own 400 for a
	// required body), never 415.
	for name, decode := range decoders {
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(""))
		w := httptest.NewRecorder()
		decode(w, r)
		if w.Code == http.StatusUnsupportedMediaType {
			t.Errorf("%s with an empty body answered 415", name)
		}
	}
	// No body: the gate's optional body is unaffected, whatever the content type says.
	for _, ct := range []string{"", "text/plain"} {
		r := httptest.NewRequest(http.MethodPost, "/x", nil)
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}
		if w := httptest.NewRecorder(); !gateDecodeEmptyBody(w, r) {
			t.Errorf("gate POST without a body and Content-Type %q refused: %d", ct, w.Code)
		}
	}
}

// Review of iter-0211, finding 3: the gate's optional body checks its content type FIRST, before it
// reads anything. A body of whitespace is still a body — under text/plain it is 415, where it was
// trimmed to "empty" and admitted. No body and no Content-Type, and `{}` under JSON, answer as before.
func TestGateEmptyBodyChecksTheContentTypeBeforeReading(t *testing.T) {
	cases := []struct {
		name, body, ct string
		ok             bool
		code           int
	}{
		{"whitespace under text/plain", "  \n\t ", "text/plain", false, http.StatusUnsupportedMediaType},
		{"whitespace without a content type", "   ", "", false, http.StatusUnsupportedMediaType},
		{"no body, no content type", "", "", true, http.StatusOK},
		{"{} under application/json", "{}", "application/json", true, http.StatusOK},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/gate", strings.NewReader(c.body))
		if c.ct != "" {
			r.Header.Set("Content-Type", c.ct)
		}
		w := httptest.NewRecorder()
		if got := gateDecodeEmptyBody(w, r); got != c.ok || (!c.ok && w.Code != c.code) {
			t.Errorf("%s: admitted = %v (%d), want %v (%d)", c.name, got, w.Code, c.ok, c.code)
		}
	}
}

// Second external review of iter-0211, finding 1: a body of UNKNOWN length (ContentLength -1 —
// HTTP/1.1 chunked, HTTP/2 without content-length) that turns out to be empty is "no body", and
// needs no content type. One byte — a space too — is a body, refused 415 without reading the rest.
// httptest gives a *strings.Reader a known length, so the bodies are wrapped to hide it.
func TestGateEmptyBodyOfUnknownLength(t *testing.T) {
	cases := []struct {
		name, body, ct string
		ok             bool
	}{
		{"chunked, zero bytes, no content type", "", "", true},
		{"chunked, zero bytes, text/plain", "", "text/plain", true},
		{"chunked, one space, no content type", " ", "", false},
		{"chunked, a body, text/plain", "{}", "text/plain", false},
		{"chunked, {} under JSON", "{}", "application/json", true},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/gate", io.NopCloser(strings.NewReader(c.body)))
		r.ContentLength = -1
		if c.ct != "" {
			r.Header.Set("Content-Type", c.ct)
		}
		w := httptest.NewRecorder()
		got := gateDecodeEmptyBody(w, r)
		if got != c.ok || (!c.ok && w.Code != http.StatusUnsupportedMediaType) {
			t.Errorf("%s: admitted = %v (%d), want %v", c.name, got, w.Code, c.ok)
		}
	}
}
