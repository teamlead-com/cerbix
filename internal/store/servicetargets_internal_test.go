package store

import (
	"errors"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-024 AC-0163-8 prerequisite: the service detail lists the service's SLO target inventory so
// the gate policy editor can offer exactly the windows a target exists for. These tests pin the
// three answers the read must keep apart: no targets (empty, NOT nil), targets (canonical window
// order, not write order, and only this service's), and not-in-project (ErrNotFound).

func TestServiceSLATargetInventoryIsEmptyNotNilWhenNoneDeclared(t *testing.T) {
	st, ctx := declStore(t)
	projectID, _, serviceID := seedService(t, st, ctx)

	got, err := st.ListServiceSLATargets(ctx, projectID, serviceID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got == nil {
		t.Fatal("a service with no targets must yield an EMPTY slice, not nil — the payload serializes it as [] never null")
	}
	if len(got) != 0 {
		t.Fatalf("list = %+v, want empty", got)
	}
}

func TestServiceSLATargetInventoryIsInCanonicalWindowOrderAndScopedToTheService(t *testing.T) {
	st, ctx := declStore(t)
	projectID, _, serviceID := seedService(t, st, ctx)
	other, err := st.CreateService(ctx, domain.Service{ProjectID: projectID, Slug: "search", Name: "Search"})
	if err != nil {
		t.Fatalf("other service: %v", err)
	}
	start := time.Now().UTC().Add(-time.Second)

	// Declared OUT of canonical order, with a re-upsert on one window so updated_at is exercised
	// as a fact of the LATEST write rather than of the first.
	for _, w := range []struct {
		name string
		obj  float64
	}{{"90d", 99.5}, {"24h", 99}, {"7d", 99.9}, {"7d", 99.95}} {
		if err := st.UpsertServiceSLATarget(ctx, projectID, serviceID, w.name, w.obj); err != nil {
			t.Fatalf("upsert %s: %v", w.name, err)
		}
	}
	// A neighbour's target must not leak into this service's inventory.
	if err := st.UpsertServiceSLATarget(ctx, projectID, other.ID, "30d", 99.99); err != nil {
		t.Fatalf("upsert neighbour: %v", err)
	}

	got, err := st.ListServiceSLATargets(ctx, projectID, serviceID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []struct {
		window string
		obj    float64
	}{{"24h", 99}, {"7d", 99.95}, {"90d", 99.5}}
	if len(got) != len(want) {
		t.Fatalf("list = %+v, want %d entries (24h, 7d, 90d — no 30d, that one is the neighbour's)", got, len(want))
	}
	for i, w := range want {
		if got[i].Window != w.window || got[i].Objective != w.obj {
			t.Errorf("entry %d = %s/%v, want %s/%v (canonical window order)", i, got[i].Window, got[i].Objective, w.window, w.obj)
		}
		if got[i].UpdatedAt.Before(start) {
			t.Errorf("entry %s updated_at = %v, want a timestamp from this test's writes", got[i].Window, got[i].UpdatedAt)
		}
	}

	// The neighbour sees only its own.
	theirs, err := st.ListServiceSLATargets(ctx, projectID, other.ID)
	if err != nil {
		t.Fatalf("list neighbour: %v", err)
	}
	if len(theirs) != 1 || theirs[0].Window != "30d" || theirs[0].Objective != 99.99 {
		t.Fatalf("neighbour list = %+v, want exactly its 30d/99.99", theirs)
	}
}

func TestServiceSLATargetInventoryOfAServiceOutsideTheProjectIsNotFound(t *testing.T) {
	st, ctx := declStore(t)
	projectID, _, serviceID := seedService(t, st, ctx)
	if err := st.UpsertServiceSLATarget(ctx, projectID, serviceID, "30d", 99.9); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	org, err := st.CreateOrganization(ctx, "globex", "Globex")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	foreign, err := st.CreateProject(ctx, org.ID, "billing", "Billing")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	// Through the wrong project the service does not exist — not "exists with no targets".
	if _, err := st.ListServiceSLATargets(ctx, foreign.ID, serviceID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign-project list = %v, want ErrNotFound", err)
	}
	if _, err := st.ListServiceSLATargets(ctx, projectID, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown-service list = %v, want ErrNotFound", err)
	}
}
