package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// A created monitor gets a slug derived from its display name, in the shape a bundle can
// name it by.
func TestCreatedMonitorGetsADerivedSlug(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "payments", "Payments")

	m, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "Checkout HTTP  /healthz", Type: domain.MonitorHTTP,
		Target: "https://checkout.example.com/healthz", IntervalSeconds: 30, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if m.Slug != "checkout-http-healthz" {
		t.Errorf("slug = %q, want checkout-http-healthz", m.Slug)
	}
	if !domain.ValidMonitorSlug(m.Slug) {
		t.Errorf("slug %q does not match %s", m.Slug, domain.MonitorSlugPattern())
	}
}

// Two monitors whose names normalize the same must not collide, and the disambiguation is
// deterministic rather than clock- or random-derived: the same input has to produce the same
// slug on every run and every replica, or a Git-tracked bundle stops being portable.
func TestSlugCollisionIsResolvedDeterministically(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "payments", "Payments")

	mk := func(name string) string {
		m, err := st.CreateMonitor(ctx, domain.Monitor{
			ProjectID: proj.ID, Name: name, Type: domain.MonitorHTTP,
			Target: "https://example.com/", IntervalSeconds: 30, Enabled: true,
		})
		if err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
		return m.Slug
	}
	first := mk("Checkout HTTP")
	second := mk("checkout-http")
	third := mk("CHECKOUT   http")

	if first != "checkout-http" {
		t.Errorf("first slug = %q", first)
	}
	if second != "checkout-http-1" || third != "checkout-http-2" {
		t.Errorf("collisions resolved to %q and %q, want checkout-http-1 and -2", second, third)
	}
}

// A caller may name the slug explicitly — that is how a bundle asserts the key it already
// uses — and a malformed one is refused rather than silently normalized.
func TestExplicitSlugIsHonouredAndValidated(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "payments", "Payments")

	m, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "Anything", Slug: "checkout-api", Type: domain.MonitorHTTP,
		Target: "https://example.com/", IntervalSeconds: 30, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if m.Slug != "checkout-api" {
		t.Errorf("slug = %q, want the one submitted", m.Slug)
	}

	_, err = st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "Bad", Slug: "Not A Slug", Type: domain.MonitorHTTP,
		Target: "https://example.com/", IntervalSeconds: 30, Enabled: true,
	})
	if err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("a malformed slug was accepted (err=%v)", err)
	}
}

// The slug is IMMUTABLE. A submitted change is refused by name rather than silently ignored:
// a caller that believes it renamed the reference key would write a bundle nothing resolves.
func TestSlugIsImmutable(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "payments", "Payments")

	m, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "checkout http", Type: domain.MonitorHTTP,
		Target: "https://example.com/", IntervalSeconds: 30, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Renaming the DISPLAY name leaves the slug alone.
	m.Name = "Checkout (primary)"
	renamed, err := st.UpdateMonitor(ctx, m)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renamed.Slug != m.Slug {
		t.Errorf("a display rename moved the slug %q -> %q", m.Slug, renamed.Slug)
	}

	// Submitting a different slug is an error, not a no-op.
	renamed.Slug = "something-else"
	if _, err := st.UpdateMonitor(ctx, renamed); !errors.Is(err, ErrMonitorSlugImmutable) {
		t.Fatalf("changing the slug returned %v, want ErrMonitorSlugImmutable", err)
	}
}

// Slugs are unique per PROJECT, not globally: two projects may each have a `checkout-http`,
// which is what makes the slug usable as a bundle key at all.
func TestSlugsAreUniquePerProject(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	a, _ := st.CreateProject(ctx, org.ID, "payments", "Payments")
	b, _ := st.CreateProject(ctx, org.ID, "search", "Search")

	for _, proj := range []string{a.ID, b.ID} {
		m, err := st.CreateMonitor(ctx, domain.Monitor{
			ProjectID: proj, Name: "checkout http", Type: domain.MonitorHTTP,
			Target: "https://example.com/", IntervalSeconds: 30, Enabled: true,
		})
		if err != nil {
			t.Fatalf("create in %s: %v", proj, err)
		}
		if m.Slug != "checkout-http" {
			t.Errorf("slug = %q in project %s, want the same key in both", m.Slug, proj)
		}
	}
}

// The Go normalizer and the SQL backfill must agree, or a monitor adopted from an old row
// and one created today land on different shapes for the same name.
func TestNormalizationMatchesTheBackfillShape(t *testing.T) {
	cases := map[string]string{
		"Checkout HTTP":            "checkout-http",
		"  spaced  out  ":          "spaced-out",
		"UPPER_snake.case":         "upper-snake-case",
		"---leading and trailing-": "leading-and-trailing",
		"":                         "monitor",
		"123 numeric start":        "monitor-123-numeric-start",
	}
	for in, want := range cases {
		if got := domain.NormalizeMonitorSlug(in); got != want {
			t.Errorf("NormalizeMonitorSlug(%q) = %q, want %q", in, got, want)
		}
	}
	for in := range cases {
		if got := domain.NormalizeMonitorSlug(in); !domain.ValidMonitorSlug(got) {
			t.Errorf("NormalizeMonitorSlug(%q) produced %q, which is not a valid slug", in, got)
		}
	}
}

// The BACKFILL is the risky half of this migration: it touches rows that already exist in
// production. This test runs the REAL DO-block extracted from the migration file — not a
// paraphrase of it — against seeded rows, because a copy that drifts from the migration
// tests nothing that will actually run.
func TestBackfillPrefersTheProviderSourceUID(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "payments", "Payments")

	uiMon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "UI Created Monitor", Type: domain.MonitorHTTP,
		Target: "https://example.com/", IntervalSeconds: 30, Enabled: true,
	})
	if err != nil {
		t.Fatalf("ui monitor: %v", err)
	}
	fileMon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "Display Name Nobody Should Use", Type: domain.MonitorHTTP,
		Target: "https://example.com/2", IntervalSeconds: 30, Enabled: true,
	})
	if err != nil {
		t.Fatalf("file monitor: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO managed_monitors (monitor_id, provider_id, org_id, project_id, source_uid)
		 VALUES ($1,'payments-bundle',$2,$3,'checkout-api')`,
		fileMon.ID, org.ID, proj.ID); err != nil {
		t.Fatalf("ownership row: %v", err)
	}

	// Put the two rows back into the pre-migration state and re-run the real backfill.
	if _, err := st.pool.Exec(ctx, `ALTER TABLE monitors ALTER COLUMN slug DROP NOT NULL`); err != nil {
		t.Fatalf("relax not-null: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `ALTER TABLE monitors ALTER COLUMN slug SET NOT NULL`)
	})
	if _, err := st.pool.Exec(ctx, `UPDATE monitors SET slug = NULL WHERE id = ANY($1)`,
		[]string{uiMon.ID, fileMon.ID}); err != nil {
		t.Fatalf("null slugs: %v", err)
	}

	body, err := migrationBackfillBlock("00065_monitor_slug.sql")
	if err != nil {
		t.Fatalf("extract backfill: %v", err)
	}
	if _, err := st.pool.Exec(ctx, body); err != nil {
		t.Fatalf("run backfill: %v", err)
	}

	var uiSlug, fileSlug string
	if err := st.pool.QueryRow(ctx, `SELECT slug FROM monitors WHERE id=$1`, uiMon.ID).Scan(&uiSlug); err != nil {
		t.Fatalf("read ui slug: %v", err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT slug FROM monitors WHERE id=$1`, fileMon.ID).Scan(&fileSlug); err != nil {
		t.Fatalf("read file slug: %v", err)
	}
	if uiSlug != "ui-created-monitor" {
		t.Errorf("UI-owned slug = %q, want it derived from the display name", uiSlug)
	}
	if fileSlug != "checkout-api" {
		t.Fatalf("file-owned slug = %q, want the provider source uid: a UUID- or name-derived slug "+
			"would differ between installations and break bundle portability", fileSlug)
	}
}

// migrationBackfillBlock returns the DO block of a migration file, so a test can run exactly
// what ships rather than a restatement of it.
func migrationBackfillBlock(name string) (string, error) {
	raw, err := migrationsFS.ReadFile("migrations/" + name)
	if err != nil {
		return "", err
	}
	body := string(raw)
	start := strings.Index(body, "DO $$")
	if start < 0 {
		return "", errors.New("no DO block in " + name)
	}
	end := strings.Index(body[start:], "END $$;")
	if end < 0 {
		return "", errors.New("unterminated DO block in " + name)
	}
	return body[start : start+end+len("END $$;")], nil
}
