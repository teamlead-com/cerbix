package api_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestPublicFeedFormatsAndGate(t *testing.T) {
	h := newPublicHandler(seededStore())

	// Default format is RSS; the page's incident (inc1 in p1 via component c1) shows.
	rec := do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status/feed", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("public feed = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "rss") {
		t.Fatalf("default content-type = %q, want rss", ct)
	}
	if !strings.Contains(rec.Body.String(), "api degraded") {
		t.Fatal("feed should list the seeded incident 'api degraded'")
	}

	// JSON Feed.
	rec = do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status/feed?format=json", "")
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "feed+json") {
		t.Fatalf("json content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "jsonfeed.org") {
		t.Fatal("json feed should carry the version marker")
	}

	// Atom.
	rec = do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status/feed?format=atom", "")
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "atom") {
		t.Fatalf("atom content-type = %q", ct)
	}

	// Internal page is hidden from the public feed.
	if rec := do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/internal-status/feed", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("internal feed = %d, want 404", rec.Code)
	}

	// Unlisted: token required.
	if rec := do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/secret-status/feed", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unlisted feed without token = %d, want 404", rec.Code)
	}
	if rec := do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/secret-status/feed?token=tok123", ""); rec.Code != http.StatusOK {
		t.Fatalf("unlisted feed with token = %d, want 200", rec.Code)
	}
}

func TestAuthedFeedIsolation(t *testing.T) {
	h := newHandler(seededStore())
	if rec := do(h, o1Viewer, http.MethodGet, "/api/v1/status-pages/sp1/feed", ""); rec.Code != http.StatusOK {
		t.Fatalf("member feed = %d, want 200", rec.Code)
	}
	if rec := do(h, outsider, http.MethodGet, "/api/v1/status-pages/sp1/feed", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider feed = %d, want 404", rec.Code)
	}
}
