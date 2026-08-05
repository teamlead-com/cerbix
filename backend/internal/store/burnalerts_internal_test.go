package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// TestEvaluateBurnAlerts covers the edge-triggered SLO burn-rate evaluation: an
// alert is enqueued once when the burn rate crosses the threshold, nothing is
// re-enqueued while it stays over (the burn_firing latch), and a recovery event is
// enqueued once when it drops back under.
func TestEvaluateBurnAlerts(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "h", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	beat := func(up bool, agoMin int, n int) {
		for i := 0; i < n; i++ {
			if _, err := st.pool.Exec(ctx,
				`INSERT INTO heartbeats (monitor_id, ts, up) VALUES ($1, $2, $3)`,
				mon.ID, time.Now().Add(-time.Duration(agoMin)*time.Minute), up); err != nil {
				t.Fatalf("seed heartbeat: %v", err)
			}
		}
	}

	// No burn target yet → evaluation is a no-op even with bad heartbeats.
	beat(false, 5, 3)
	beat(true, 5, 7)
	if fired, resolved, err := st.EvaluateBurnAlerts(ctx); err != nil || fired != 0 || resolved != 0 {
		t.Fatalf("no target → no alerts: fired=%d resolved=%d err=%v", fired, resolved, err)
	}

	// Enable burn alerting on a 99% objective (allowed bad = 1%, threshold 14.4×)
	// with one rule over (1h ∧ 10m) — the seeded beats sit at -5m, inside both.
	// 30% bad over the windows → burn rate 30 ≥ 14.4 → fires once.
	rule := []domain.BurnRule{{LongWindowSeconds: 3600, ShortWindowSeconds: 600, Threshold: 14.4, Severity: domain.BurnSeverityPage}}
	if _, err := st.UpsertMonitorSLATarget(ctx, mon.ID, "30d", 99, true, rule); err != nil {
		t.Fatalf("upsert burn target: %v", err)
	}
	fired, resolved, err := st.EvaluateBurnAlerts(ctx)
	if err != nil || fired != 1 || resolved != 0 {
		t.Fatalf("first eval: fired=%d resolved=%d err=%v (want 1/0)", fired, resolved, err)
	}
	if got := st.countOutbox(ctx, t, domain.TopicSLOBurnAlert, "pending"); got != 1 {
		t.Fatalf("burn alert outbox = %d, want 1", got)
	}
	assertFiring(t, st, ctx, mon.ID, true)

	// Re-evaluating while still burning enqueues nothing (edge-triggered latch).
	if fired, resolved, err := st.EvaluateBurnAlerts(ctx); err != nil || fired != 0 || resolved != 0 {
		t.Fatalf("idempotent eval: fired=%d resolved=%d err=%v (want 0/0)", fired, resolved, err)
	}
	if got := st.countOutbox(ctx, t, domain.TopicSLOBurnAlert, "pending"); got != 1 {
		t.Fatalf("burn alert outbox after idempotent eval = %d, want 1", got)
	}

	// Recovery: replace the window with all-up heartbeats → rate 0 < threshold →
	// one resolve event, latch cleared.
	if _, err := st.pool.Exec(ctx, `DELETE FROM heartbeats WHERE monitor_id = $1`, mon.ID); err != nil {
		t.Fatalf("clear heartbeats: %v", err)
	}
	beat(true, 5, 20)
	if fired, resolved, err := st.EvaluateBurnAlerts(ctx); err != nil || fired != 0 || resolved != 1 {
		t.Fatalf("recovery eval: fired=%d resolved=%d err=%v (want 0/1)", fired, resolved, err)
	}
	if got := st.countOutbox(ctx, t, domain.TopicSLOBurnAlert, "pending"); got != 2 {
		t.Fatalf("burn alert outbox after recovery = %d, want 2", got)
	}
	assertFiring(t, st, ctx, mon.ID, false)
}

// TestEvaluateRegionWorkerAlerts covers the region-worker alert lifecycle: a region
// with enabled monitors but no live worker fires once after the grace period, latches
// (no repeats while still missing), and emits one recovery when a worker returns.
func TestEvaluateRegionWorkerAlerts(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	if _, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "h", Type: domain.MonitorHTTP, Target: "https://x",
		Region: "geo1", IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true,
	}); err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	countRegion := func() int { return st.countOutbox(ctx, t, domain.TopicRegionWorkerAlert, "pending") }

	// First observation without a worker only starts the grace clock — no alert yet.
	if fired, resolved, err := st.EvaluateRegionWorkerAlerts(ctx, map[string]bool{"core": true}, 0); err != nil || fired != 0 || resolved != 0 {
		t.Fatalf("first observe: fired=%d resolved=%d err=%v (want 0/0)", fired, resolved, err)
	}
	if countRegion() != 0 {
		t.Fatalf("no alert expected on first observation, got %d", countRegion())
	}

	// A large grace suppresses the alert on the next tick (still within grace).
	if fired, _, err := st.EvaluateRegionWorkerAlerts(ctx, map[string]bool{"core": true}, 3600); err != nil || fired != 0 {
		t.Fatalf("within grace: fired=%d err=%v (want 0)", fired, err)
	}
	if countRegion() != 0 {
		t.Fatalf("grace should suppress, got %d alerts", countRegion())
	}

	// Grace elapsed (0s) → fires once, per affected project.
	if fired, resolved, err := st.EvaluateRegionWorkerAlerts(ctx, map[string]bool{"core": true}, 0); err != nil || fired != 1 || resolved != 0 {
		t.Fatalf("fire: fired=%d resolved=%d err=%v (want 1/0)", fired, resolved, err)
	}
	if countRegion() != 1 {
		t.Fatalf("region alert outbox = %d, want 1", countRegion())
	}

	// Still missing → latched, nothing new.
	if fired, resolved, err := st.EvaluateRegionWorkerAlerts(ctx, map[string]bool{"core": true}, 0); err != nil || fired != 0 || resolved != 0 {
		t.Fatalf("latched: fired=%d resolved=%d err=%v (want 0/0)", fired, resolved, err)
	}
	if countRegion() != 1 {
		t.Fatalf("region alert outbox after latch = %d, want 1", countRegion())
	}

	// Worker returns → one recovery, state cleared.
	if fired, resolved, err := st.EvaluateRegionWorkerAlerts(ctx, map[string]bool{"core": true, "geo1": true}, 0); err != nil || fired != 0 || resolved != 1 {
		t.Fatalf("recover: fired=%d resolved=%d err=%v (want 0/1)", fired, resolved, err)
	}
	if countRegion() != 2 {
		t.Fatalf("region alert outbox after recovery = %d, want 2", countRegion())
	}

	// Steady state with worker present → nothing.
	if fired, resolved, err := st.EvaluateRegionWorkerAlerts(ctx, map[string]bool{"core": true, "geo1": true}, 0); err != nil || fired != 0 || resolved != 0 {
		t.Fatalf("steady: fired=%d resolved=%d err=%v (want 0/0)", fired, resolved, err)
	}
}

// TestEnqueueDueSLAReports covers the weekly report watermark: an enabled project
// enqueues one report, a second run within the period enqueues nothing, and a
// disabled project is skipped entirely.
func TestEnqueueDueSLAReports(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	on, _ := st.CreateProject(ctx, org.ID, "api", "API")
	off, _ := st.CreateProject(ctx, org.ID, "web", "Web")

	// Disabled projects are never due.
	if n, err := st.EnqueueDueSLAReports(ctx); err != nil || n != 0 {
		t.Fatalf("no enabled project → 0 reports: n=%d err=%v", n, err)
	}

	if _, err := st.SetProjectSLAReport(ctx, on.ID, true); err != nil {
		t.Fatalf("enable report: %v", err)
	}
	// A freshly-enabled project (no watermark) is due immediately → one report.
	if n, err := st.EnqueueDueSLAReports(ctx); err != nil || n != 1 {
		t.Fatalf("first run: n=%d err=%v (want 1)", n, err)
	}
	if got := st.countOutbox(ctx, t, domain.TopicSLAReport, "pending"); got != 1 {
		t.Fatalf("sla report outbox = %d, want 1", got)
	}
	// The watermark now blocks a re-send within the 7-day period.
	if n, err := st.EnqueueDueSLAReports(ctx); err != nil || n != 0 {
		t.Fatalf("second run within period: n=%d err=%v (want 0)", n, err)
	}
	// The disabled project produced nothing.
	if got, _ := st.ProjectSLAReportEnabled(ctx, off.ID); got {
		t.Fatal("off project should report disabled")
	}
}

func assertFiring(t *testing.T, st *Store, ctx context.Context, monitorID string, want bool) {
	t.Helper()
	var firing bool
	if err := st.pool.QueryRow(ctx,
		`SELECT COALESCE((burn_rules->0->>'firing')::boolean, false)
		   FROM sla_targets WHERE monitor_id = $1 AND window_name = '30d'`, monitorID).Scan(&firing); err != nil {
		t.Fatalf("read rule firing: %v", err)
	}
	if firing != want {
		t.Fatalf("rule firing = %v, want %v", firing, want)
	}
}

// TestBurnRulesMultiWindow covers the multi-window semantics (D-0098): a rule
// fires only when BOTH windows burn, rules latch independently, and an edit that
// keeps a rule's configuration preserves its firing latch while a config change
// resets it.
func TestBurnRulesMultiWindow(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "mw", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	beat := func(up bool, agoMin int, n int) {
		for i := 0; i < n; i++ {
			if _, err := st.pool.Exec(ctx,
				`INSERT INTO heartbeats (monitor_id, ts, up) VALUES ($1, $2, $3)`,
				mon.ID, time.Now().Add(-time.Duration(agoMin)*time.Minute).Add(time.Duration(i)*time.Second), up); err != nil {
				t.Fatalf("seed heartbeat: %v", err)
			}
		}
	}
	rules := []domain.BurnRule{
		{LongWindowSeconds: 3600, ShortWindowSeconds: 600, Threshold: 14.4, Severity: domain.BurnSeverityPage},
	}
	if _, err := st.UpsertMonitorSLATarget(ctx, mon.ID, "30d", 99, true, rules); err != nil {
		t.Fatalf("upsert target: %v", err)
	}

	// Old burn only: bad beats 30m ago (inside 1h, outside 10m) + fresh good beats
	// in the short window → long burns, short does not → NO alert (the AND).
	beat(false, 30, 5)
	beat(true, 30, 5)
	beat(true, 2, 10)
	if fired, resolved, err := st.EvaluateBurnAlerts(ctx); err != nil || fired != 0 || resolved != 0 {
		t.Fatalf("long-only burn must not fire: fired=%d resolved=%d err=%v", fired, resolved, err)
	}

	// Short window starts burning too → the rule fires once, payload carries the
	// severity and both windows.
	beat(false, 1, 6)
	if fired, _, err := st.EvaluateBurnAlerts(ctx); err != nil || fired != 1 {
		t.Fatalf("both-windows burn must fire once: fired=%d err=%v", fired, err)
	}
	var payload []byte
	if err := st.pool.QueryRow(ctx,
		`SELECT payload FROM outbox_events WHERE topic = $1 ORDER BY created_at DESC LIMIT 1`,
		domain.TopicSLOBurnAlert).Scan(&payload); err != nil {
		t.Fatalf("read alert payload: %v", err)
	}
	var alert domain.SLOBurnAlert
	if err := json.Unmarshal(payload, &alert); err != nil {
		t.Fatalf("decode alert: %v", err)
	}
	if alert.Severity != domain.BurnSeverityPage || alert.ShortWindowSeconds != 600 || alert.WindowSeconds != 3600 || !alert.Firing {
		t.Fatalf("alert attribution: %+v", alert)
	}

	// Re-upsert with the SAME rule config → the firing latch survives.
	tgt, err := st.UpsertMonitorSLATarget(ctx, mon.ID, "30d", 99, true, rules)
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if len(tgt.BurnRules) != 1 || !tgt.BurnRules[0].Firing {
		t.Fatalf("latch must survive an unchanged edit: %+v", tgt.BurnRules)
	}
	// Changing the threshold resets the latch (it's a different rule now).
	changed := []domain.BurnRule{{LongWindowSeconds: 3600, ShortWindowSeconds: 600, Threshold: 20, Severity: domain.BurnSeverityPage}}
	tgt, err = st.UpsertMonitorSLATarget(ctx, mon.ID, "30d", 99, true, changed)
	if err != nil {
		t.Fatalf("changed upsert: %v", err)
	}
	if len(tgt.BurnRules) != 1 || tgt.BurnRules[0].Firing {
		t.Fatalf("latch must reset on a config change: %+v", tgt.BurnRules)
	}

	// Enabling with nil rules on a fresh target seeds the SRE defaults.
	mon2, _ := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "mw2", Type: domain.MonitorHTTP, Target: "https://y",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true,
	})
	tgt2, err := st.UpsertMonitorSLATarget(ctx, mon2.ID, "30d", 99.9, true, nil)
	if err != nil {
		t.Fatalf("default upsert: %v", err)
	}
	def := domain.DefaultBurnRules()
	if len(tgt2.BurnRules) != len(def) || tgt2.BurnRules[0].Key() != def[0].Key() || tgt2.BurnRules[1].Key() != def[1].Key() {
		t.Fatalf("nil rules must seed SRE defaults: %+v", tgt2.BurnRules)
	}
}

// TestFindOpenIncidentByExternalKey covers external-key correlation used by the
// Alertmanager receiver: an open incident is found by (project, key), and once
// resolved it is no longer returned so a later "firing" opens a fresh one.
func TestFindOpenIncidentByExternalKey(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	if _, err := st.FindOpenIncidentByExternalKey(ctx, proj.ID, "fp-1"); err != ErrNotFound {
		t.Fatalf("no incident yet → ErrNotFound, got %v", err)
	}
	inc, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: proj.ID, Title: "latency", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceAPI, ExternalKey: "fp-1",
	}, "opened", "alertmanager")
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}
	got, err := st.FindOpenIncidentByExternalKey(ctx, proj.ID, "fp-1")
	if err != nil || got.ID != inc.ID || got.ExternalKey != "fp-1" {
		t.Fatalf("find open: %v %+v", err, got)
	}
	// Resolving it removes it from the open lookup.
	if _, err := st.AddIncidentUpdate(ctx, domain.IncidentUpdate{
		IncidentID: inc.ID, Status: domain.IncidentResolved, Body: "done", Author: "alertmanager",
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := st.FindOpenIncidentByExternalKey(ctx, proj.ID, "fp-1"); err != ErrNotFound {
		t.Fatalf("resolved incident should not be open, got %v", err)
	}
}

// TestOIDCSettingsRoundTrip covers the singleton OIDC override: ErrNotFound before
// any save, then upsert-and-read with the client secret surviving the encrypt/
// decrypt round-trip, and a second upsert replacing the row in place.
func TestOIDCSettingsRoundTrip(t *testing.T) {
	st, ctx := outboxTestStore(t)
	if _, err := st.GetOIDCSettings(ctx); err != ErrNotFound {
		t.Fatalf("no settings yet → ErrNotFound, got %v", err)
	}
	in := domain.OIDCSettings{
		Enabled: true, Issuer: "https://idp/x", ClientID: "cerbix", ClientSecret: "s3cr3t",
		RedirectURL: "https://c/auth/callback", Scopes: []string{"openid", "email"},
		ButtonLabel: "Continue with Keycloak", BootstrapAdmins: []string{"a@x"},
	}
	if err := st.UpsertOIDCSettings(ctx, in); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := st.GetOIDCSettings(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Enabled || got.Issuer != in.Issuer || got.ClientSecret != "s3cr3t" || len(got.Scopes) != 2 || got.ButtonLabel != in.ButtonLabel {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	// The stored secret must not be plaintext at rest (encryption disabled in test →
	// value stored as-is; assert it decrypts to the same regardless).
	in.ClientSecret = "rotated"
	in.Enabled = false
	if err := st.UpsertOIDCSettings(ctx, in); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got, _ = st.GetOIDCSettings(ctx)
	if got.Enabled || got.ClientSecret != "rotated" {
		t.Fatalf("in-place replace failed: %+v", got)
	}
}

// TestInstanceSettingsRoundTrip covers the singleton settings row: empty before any
// write, then per-group upsert + read for each group independently.
func TestInstanceSettingsRoundTrip(t *testing.T) {
	st, ctx := outboxTestStore(t)
	got, err := st.GetInstanceSettings(ctx)
	if err != nil {
		t.Fatalf("get empty: %v", err)
	}
	if got.Branding.Configured || got.AuthPolicy.Configured || got.Alerting.Configured || got.MonitorDefaults.Configured {
		t.Fatalf("no row yet → all unconfigured, got %+v", got)
	}

	if err := st.UpsertBranding(ctx, domain.Branding{Configured: true, ProductName: "Example Status", AccentColor: "#3b5bdb"}); err != nil {
		t.Fatalf("upsert branding: %v", err)
	}
	if err := st.UpsertAuthPolicy(ctx, domain.AuthPolicy{Configured: true, MinPasswordLen: 16, SessionTTLSeconds: 3600, RequireTOTP: domain.TOTPAdmins}); err != nil {
		t.Fatalf("upsert auth policy: %v", err)
	}
	got, err = st.GetInstanceSettings(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Branding.ProductName != "Example Status" || got.Branding.AccentColor != "#3b5bdb" {
		t.Fatalf("branding round-trip: %+v", got.Branding)
	}
	if got.AuthPolicy.MinPasswordLen != 16 || got.AuthPolicy.RequireTOTP != domain.TOTPAdmins {
		t.Fatalf("auth policy round-trip: %+v", got.AuthPolicy)
	}
	// Other groups remain unconfigured (per-group columns are independent).
	if got.Alerting.Configured || got.MonitorDefaults.Configured {
		t.Fatalf("untouched groups should stay unconfigured: %+v", got)
	}
}

// TestMailSettingsRoundTrip covers the mail group: persist + read with the SMTP
// password surviving the encrypt/decrypt round-trip (pass-through when disabled).
func TestMailSettingsRoundTrip(t *testing.T) {
	st, ctx := outboxTestStore(t)
	in := domain.MailSettings{
		Configured: true, Enabled: true, SMTPHost: "smtp.example", SMTPPort: 587,
		SMTPUsername: "user", SMTPPassword: "p@ss w0rd", From: "status@example", PublicBaseURL: "https://c",
	}
	if err := st.UpsertMail(ctx, in); err != nil {
		t.Fatalf("upsert mail: %v", err)
	}
	got, err := st.GetInstanceSettings(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Mail.Enabled || got.Mail.SMTPHost != "smtp.example" || got.Mail.SMTPPassword != "p@ss w0rd" || got.Mail.From != "status@example" {
		t.Fatalf("mail round-trip: %+v", got.Mail)
	}
}
