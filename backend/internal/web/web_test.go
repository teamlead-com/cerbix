package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServesIndexAtRoot(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("root = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="app"`) {
		t.Fatalf("root did not serve the SPA shell: %s", rec.Body.String())
	}
}

func TestSPAFallbackForClientRoute(t *testing.T) {
	h, _ := New()
	rec := httptest.NewRecorder()
	// A client-side route with no matching file must fall back to index.html.
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/orgs/acme/projects/api", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("client route = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("fallback content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), `id="app"`) {
		t.Fatal("fallback did not serve index.html")
	}
}

func TestServesRealAsset(t *testing.T) {
	h, _ := New()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/placeholder.txt", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("asset = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cerbix SPA asset") {
		t.Fatalf("asset body = %q", rec.Body.String())
	}
	// A real asset is not the HTML shell.
	if strings.Contains(rec.Body.String(), `id="app"`) {
		t.Fatal("asset request should not return index.html")
	}
}
