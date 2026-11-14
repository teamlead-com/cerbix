package web

// sec-hardening-2 H-9b: the SPA's document carries no inline script, because an inline script
// cannot pass `script-src 'self'`. The pre-paint theme script is a static file the document loads.

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var scriptTag = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)

func TestTheDocumentHasNoInlineScript(t *testing.T) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		t.Fatal(err)
	}
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	tags := scriptTag.FindAllStringSubmatch(string(index), -1)
	if len(tags) == 0 {
		t.Fatal("index.html loads no script at all")
	}
	theme := false
	for _, m := range tags {
		attrs, body := m[1], strings.TrimSpace(m[2])
		if !strings.Contains(attrs, "src=") || body != "" {
			t.Errorf("an inline script in index.html: <script%s>%.60s…", attrs, body)
		}
		if strings.Contains(attrs, `src="/theme-init.js"`) {
			theme = true
		}
	}
	if !theme {
		t.Error(`index.html does not load the pre-paint theme script /theme-init.js`)
	}
	h, _ := New()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/theme-init.js", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "cerbix-theme") {
		t.Errorf("/theme-init.js = %d %.80q, want the theme script", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("/theme-init.js Content-Type = %q: under nosniff a script must be served as JavaScript", ct)
	}
}
