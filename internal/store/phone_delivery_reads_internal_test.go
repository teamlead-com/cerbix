package store

import (
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-041a §4.5, R13 — the two reads of phone_deliveries. Rows are seeded with direct SQL: the write
// path belongs to the reservation code, these reads must work on whatever it leaves.

func seedPhoneDelivery(t *testing.T, w phoneChannelWorld, channelID, incidentID, reason string, ago time.Duration) string {
	t.Helper()
	var inc any
	if incidentID != "" {
		inc = incidentID
	}
	var id string
	err := w.st.pool.QueryRow(w.ctx, `
		INSERT INTO phone_deliveries (project_id, channel_id, incident_id, number_hash, number_masked, kind,
		                              alert_kind, reason, notes, provider_status, provider_code, provider_ref,
		                              segments, attempts, created_at, updated_at)
		VALUES ($1, $2, $3, 'hash-secret', '+90•••••6869', 'sms', 'down', $4, ARRAY['link_omitted'],
		        201, 'ok', 'SM123', 2, 1, now() - $5::interval, now() - $5::interval)
		RETURNING id::text`, w.project, channelID, inc, reason, ago.String()).Scan(&id)
	if err != nil {
		t.Fatalf("seed phone delivery: %v", err)
	}
	return id
}

func TestListIncidentPhoneDeliveries(t *testing.T) {
	w := newPhoneChannelWorld(t)
	ch := w.numberChannel(t, domain.ChannelSMS, "+905551236869")
	inc1, err := w.st.CreateIncidentByPrincipal(w.ctx, domain.Incident{ProjectID: w.project, Title: "one",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMinor, Source: domain.SourceManual},
		domain.ImpactBasisManual, "opened", "alice", w.actor)
	if err != nil {
		t.Fatal(err)
	}
	inc2, err := w.st.CreateIncidentByPrincipal(w.ctx, domain.Incident{ProjectID: w.project, Title: "two",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMinor, Source: domain.SourceManual},
		domain.ImpactBasisManual, "opened", "alice", w.actor)
	if err != nil {
		t.Fatal(err)
	}
	old := seedPhoneDelivery(t, w, ch.ID, inc1.ID, "sent", 2*time.Hour)
	recent := seedPhoneDelivery(t, w, ch.ID, inc1.ID, "provider_rejected", time.Minute)
	seedPhoneDelivery(t, w, ch.ID, inc2.ID, "sent", time.Minute)

	got, err := w.st.ListIncidentPhoneDeliveries(w.ctx, inc1.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Oldest first, as the incident's timeline beside it (FR-041a review P3-2); the channel's last 50
	// stay newest first (TestListChannelPhoneDeliveries).
	if len(got) != 2 || got[0].ID != old || got[1].ID != recent {
		t.Fatalf("deliveries = %+v, want [%s %s] oldest first", got, old, recent)
	}
	d := got[0]
	if d.ChannelID != ch.ID || d.ChannelName != ch.Name || d.Kind != "sms" || d.AlertKind != "down" ||
		d.Reason != "sent" || d.Segments != 2 || d.Attempts != 1 {
		t.Fatalf("row = %+v", d)
	}
	if d.NumberMasked == nil || *d.NumberMasked != "+90•••••6869" {
		t.Fatalf("number_masked = %v", d.NumberMasked)
	}
	if d.ProviderStatus == nil || *d.ProviderStatus != 201 || d.ProviderCode == nil || *d.ProviderCode != "ok" ||
		d.ProviderRef == nil || *d.ProviderRef != "SM123" {
		t.Fatalf("provider facts = %v %v %v", d.ProviderStatus, d.ProviderCode, d.ProviderRef)
	}
	if len(d.Notes) != 1 || d.Notes[0] != "link_omitted" {
		t.Fatalf("notes = %v", d.Notes)
	}
	if d.IncidentID != "" || d.IncidentRef != "" {
		t.Fatalf("the incident's own list repeats the incident: %+v", d)
	}
	if none, err := w.st.ListIncidentPhoneDeliveries(w.ctx, "00000000-0000-0000-0000-000000000000"); err != nil || none == nil || len(none) != 0 {
		t.Fatalf("an incident without deliveries = %v, %v; want an empty non-nil list", none, err)
	}
}

func TestListChannelPhoneDeliveries(t *testing.T) {
	w := newPhoneChannelWorld(t)
	ch := w.numberChannel(t, domain.ChannelSMS, "+905551236869")
	other := w.numberChannel(t, domain.ChannelVoice, "+905551230147")
	inc, err := w.st.CreateIncidentByPrincipal(w.ctx, domain.Incident{ProjectID: w.project, Title: "one",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMinor, Source: domain.SourceManual},
		domain.ImpactBasisManual, "opened", "alice", w.actor)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := w.st.IncidentRef(w.ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	withInc := seedPhoneDelivery(t, w, ch.ID, inc.ID, "sent", time.Minute)
	noInc := seedPhoneDelivery(t, w, ch.ID, "", "not_deliverable", 2*time.Minute)
	seedPhoneDelivery(t, w, other.ID, inc.ID, "sent", time.Minute)
	for i := 0; i < 60; i++ {
		seedPhoneDelivery(t, w, ch.ID, "", "sent", time.Duration(10+i)*time.Minute)
	}

	got, err := w.st.ListChannelPhoneDeliveries(w.ctx, ch.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 {
		t.Fatalf("got %d rows, want the limit 50", len(got))
	}
	if got[0].ID != withInc || got[1].ID != noInc {
		t.Fatalf("order = %s, %s; want newest first", got[0].ID, got[1].ID)
	}
	if got[0].IncidentID != inc.ID || got[0].IncidentRef != ref || got[0].ChannelName != ch.Name {
		t.Fatalf("link = %q %q %q, want %s %s %s", got[0].IncidentID, got[0].IncidentRef, got[0].ChannelName, inc.ID, ref, ch.Name)
	}
	if got[1].IncidentID != "" || got[1].IncidentRef != "" {
		t.Fatalf("a delivery with no incident carries %q %q", got[1].IncidentID, got[1].IncidentRef)
	}
	for _, d := range got {
		if d.ChannelID != ch.ID {
			t.Fatalf("another channel's row: %+v", d)
		}
	}
	// A deleted incident leaves the row with no link (ON DELETE SET NULL).
	if _, err := w.st.pool.Exec(w.ctx, `DELETE FROM incidents WHERE id = $1`, inc.ID); err != nil {
		t.Fatal(err)
	}
	got, err = w.st.ListChannelPhoneDeliveries(w.ctx, ch.ID, 5)
	if err != nil || len(got) != 5 || got[0].ID != withInc || got[0].IncidentID != "" {
		t.Fatalf("after the incident is gone: %d rows %+v, %v", len(got), got, err)
	}
}
