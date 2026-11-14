package store

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Second external review of iter-0210 (P2-1): the http prober now parses its target with
// domain.ParseHTTPTarget, which refuses a control byte anywhere. On main an http target probed with
// control bytes in exactly one place: after the first "#". url.Parse checks for control bytes only
// before the fragment, and the fragment is never sent. So "…/health#/dashboard\r\n" was UP, and
// without 00115 it would go DOWN. 00115 deletes the control bytes from the fragment of exactly those
// rows: the part before "#" is an http(s) URL with a host and no control byte, and the fragment's
// percent-escapes are valid. Every other row is left alone, because none of them ever probed.
func TestHTTPFragmentControlMigration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	// What the http prober on main did: http.NewRequest on the raw target, then Do.
	up := func(target string) bool {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
		if err != nil {
			return false
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}

	st, ctx := serviceSchemaStore(t)
	org, err := st.CreateOrganization(ctx, "httpfrag", "HTTP fragment")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "httpfrag", "HTTP fragment")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	ins := func(name, typ, target string) string {
		t.Helper()
		var id string
		if err := st.pool.QueryRow(ctx,
			`INSERT INTO monitors (project_id, name, slug, type, target, interval_seconds, timeout_seconds, config)
			 VALUES ($1, $2, $2, $3, $4, 60, 10, '{}'::jsonb) RETURNING id`,
			proj.ID, name, typ, target).Scan(&id); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
		return id
	}
	s := srv.URL
	cases := []struct {
		id, name, before, want string
		wasUp, migrated        bool
	}{
		{"", "CRLF after the fragment", s + "/health#/dashboard\r\n", s + "/health#/dashboard", true, true},
		{"", "tab inside the fragment", s + "/health#a\tb", s + "/health#ab", true, true},
		{"", "DEL in the fragment", s + "/health#x\x7f", s + "/health#x", true, true},
		{"", "only LF after #", s + "/health#\n", s + "/health#", true, true},
		{"", "upper-case scheme", strings.Replace(s, "http://", "HTTP://", 1) + "/health#x\r\n", strings.Replace(s, "http://", "HTTP://", 1) + "/health#x", true, true},
		// Never probed on main: left alone.
		{"", "LF without a fragment", s + "/health\n", s + "/health\n", false, false},
		{"", "control byte before #", s + "/he\nalth#x", s + "/he\nalth#x", false, false},
		{"", "invalid escape in the fragment", s + "/health#%zz\n", s + "/health#%zz\n", false, false},
		{"", "leading space", " " + s + "/health#x\n", " " + s + "/health#x\n", false, false},
	}
	for i := range cases {
		if got := up(cases[i].before); got != cases[i].wasUp {
			t.Fatalf("%s: UP on main = %v, the case says %v", cases[i].name, got, cases[i].wasUp)
		}
		cases[i].id = ins("frag-"+strconv.Itoa(i), "http", cases[i].before)
	}
	other := ins("promql-fragment", "promql", "http://prom.internal:9090#x\n")

	rerunMigrationUp(t, st, ctx, "00115_target_edge_whitespace.sql")
	rerunMigrationUp(t, st, ctx, "00115_target_edge_whitespace.sql") // idempotent

	read := func(id string) (string, int64) {
		t.Helper()
		var target string
		var rev int64
		if err := st.pool.QueryRow(ctx, `SELECT target, execution_revision FROM monitors WHERE id = $1`, id).Scan(&target, &rev); err != nil {
			t.Fatalf("read: %v", err)
		}
		return target, rev
	}
	for _, tc := range cases {
		target, rev := read(tc.id)
		if target != tc.want {
			t.Errorf("%s: target %q, want %q", tc.name, target, tc.want)
		}
		if rev != 1 {
			t.Errorf("%s: execution_revision %d, want 1 (no bump)", tc.name, rev)
		}
		if !tc.migrated {
			continue
		}
		if _, err := domain.ParseHTTPTarget(target); err != nil {
			t.Errorf("%s: %q does not parse after the migration: %v", tc.name, target, err)
		}
		if !up(target) {
			t.Errorf("%s: %q is not UP after the migration", tc.name, target)
		}
	}
	if target, _ := read(other); target != "http://prom.internal:9090#x\n" {
		t.Errorf("a promql row was changed: %q", target)
	}
}
