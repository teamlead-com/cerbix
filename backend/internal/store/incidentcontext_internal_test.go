package store

import (
	"strings"
	"testing"
	"time"

	"git.example.com/monitoring/cerbix/internal/domain"
)

func TestIncidentContextAndAppend(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mk := func(name, region string) domain.Monitor {
		m, err := st.CreateMonitor(ctx, domain.Monitor{
			ProjectID: proj.ID, Name: name, Type: domain.MonitorHTTP, Target: "https://x",
			IntervalSeconds: 60, TimeoutSeconds: 5, Region: region,
		})
		if err != nil {
			t.Fatalf("create monitor %s: %v", name, err)
		}
		return m
	}
	parent := mk("postgres-main", "geo1")
	childA := mk("api-svc", "geo1")
	childB := mk("worker-svc", "geo1")
	outside := mk("far-away", "geo1")

	now := time.Now().UTC()
	fail := func(m domain.Monitor, ts time.Time, msg string, code int) {
		if err := st.InsertHeartbeat(ctx, domain.Heartbeat{MonitorID: m.ID, Ts: ts, Up: false, Msg: msg, Code: code}); err != nil {
			t.Fatalf("insert hb: %v", err)
		}
	}
	// In-window failures: the incident's own monitor + two co-failures, all refused.
	fail(parent, now.Add(-time.Minute), "connect: connection refused", 0)
	fail(childA, now.Add(-30*time.Second), "connect: connection refused", 0)
	fail(childB, now.Add(time.Minute), "read: connection reset by peer", 0)
	// Outside the ±5m window: must not count.
	fail(outside, now.Add(-20*time.Minute), "context deadline exceeded", 0)

	inc := domain.Incident{ID: "unused", ProjectID: proj.ID, MonitorID: parent.ID, StartedAt: now}
	ictx, err := st.IncidentContext(ctx, inc)
	if err != nil {
		t.Fatalf("incident context: %v", err)
	}
	if ictx.CoFailureTotal != 2 {
		t.Fatalf("co-failures = %d (%v), want 2", ictx.CoFailureTotal, ictx.CoFailures)
	}
	if ictx.DominantClass != domain.ErrClassRefused {
		t.Fatalf("dominant class = %q, want refused", ictx.DominantClass)
	}
	if ictx.Region != "geo1" {
		t.Fatalf("region = %q, want geo1 (single-region)", ictx.Region)
	}

	// Mixed regions → no single-region hint.
	other := mk("elsewhere", "core")
	fail(other, now, "connect: connection refused", 0)
	if ictx2, _ := st.IncidentContext(ctx, inc); ictx2.Region != "" {
		t.Fatalf("mixed regions must clear the hint, got %q", ictx2.Region)
	}

	// Append is idempotent per incident.
	created, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: proj.ID, MonitorID: parent.ID, Title: "postgres-main is down",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor, Source: domain.SourceAuto,
	}, "auto-opened", "system")
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}
	body := ictx.Render()
	if added, err := st.AppendIncidentContext(ctx, created.ID, body); err != nil || !added {
		t.Fatalf("first append: added=%v err=%v", added, err)
	}
	if added, err := st.AppendIncidentContext(ctx, created.ID, body); err != nil || added {
		t.Fatalf("second append must be a no-op: added=%v err=%v", added, err)
	}
	ups, err := st.ListIncidentUpdates(ctx, created.ID)
	if err != nil {
		t.Fatalf("list updates: %v", err)
	}
	ctxUpdates := 0
	for _, u := range ups {
		if strings.HasPrefix(u.Body, domain.IncidentContextMarker) {
			ctxUpdates++
			if u.Author != "system" {
				t.Fatalf("context author = %q, want system", u.Author)
			}
		}
	}
	if ctxUpdates != 1 {
		t.Fatalf("context updates = %d, want exactly 1", ctxUpdates)
	}
}
