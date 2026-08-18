package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func TestListIncidents(t *testing.T) {
	h := newHandler(seededStore())
	rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/incidents", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list incidents = %d, want 200", rec.Code)
	}
	var incs []domain.Incident
	_ = json.Unmarshal(rec.Body.Bytes(), &incs)
	if len(incs) != 1 || incs[0].ID != "inc1" {
		t.Fatalf("p1 viewer should see inc1, got %+v", incs)
	}
}

func TestIncidentIsolation(t *testing.T) {
	h := newHandler(seededStore())
	// p1 viewer sees inc1 (own project) but not inc3 (other org) → 404 hidden.
	if rec := do(h, p1Viewer, http.MethodGet, "/api/v1/incidents/inc1", ""); rec.Code != http.StatusOK {
		t.Fatalf("p1 viewer get inc1 = %d, want 200", rec.Code)
	}
	if rec := do(h, p1Viewer, http.MethodGet, "/api/v1/incidents/inc3", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("p1 viewer get inc3 = %d, want 404", rec.Code)
	}
	if rec := do(h, outsider, http.MethodGet, "/api/v1/incidents/inc1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider get inc1 = %d, want 404", rec.Code)
	}
}

func TestCreateIncidentAuthz(t *testing.T) {
	h := newHandler(seededStore())
	// Outsider → 404 (project hidden).
	if rec := do(h, outsider, http.MethodPost, "/api/v1/projects/p1/incidents", `{"title":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider create incident = %d, want 404", rec.Code)
	}
	// Project viewer → 403 (insufficient role).
	if rec := do(h, p1Viewer, http.MethodPost, "/api/v1/projects/p1/incidents", `{"title":"x"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create incident = %d, want 403", rec.Code)
	}
	// Org admin (project-write) → 201.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/incidents", `{"title":"db slow","impact":"major","body":"opened"}`); rec.Code != http.StatusCreated {
		t.Fatalf("org admin create incident = %d, want 201", rec.Code)
	}
	// Missing title → 400.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/incidents", `{"impact":"major"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("create without title = %d, want 400", rec.Code)
	}
	// Unknown impact → 400.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/incidents", `{"title":"x","impact":"apocalyptic"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("create with bad impact = %d, want 400", rec.Code)
	}
}

func TestIncidentUpdateLifecycle(t *testing.T) {
	h := newHandler(seededStore())
	// Viewer cannot post updates.
	if rec := do(h, p1Viewer, http.MethodPost, "/api/v1/incidents/inc1/updates", `{"status":"identified"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer add update = %d, want 403", rec.Code)
	}
	// Editor advances the incident.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/incidents/inc1/updates", `{"status":"identified","body":"root cause found"}`); rec.Code != http.StatusCreated {
		t.Fatalf("add update = %d, want 201", rec.Code)
	}
	// Unknown status → 400.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/incidents/inc1/updates", `{"status":"exploded"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad status update = %d, want 400", rec.Code)
	}
	// Resolve, then further updates are rejected (terminal).
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/incidents/inc1/updates", `{"status":"resolved","body":"fixed"}`); rec.Code != http.StatusCreated {
		t.Fatalf("resolve = %d, want 201", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/incidents/inc1/updates", `{"body":"more"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("update after resolve = %d, want 400", rec.Code)
	}
}

func TestListIncidentUpdates(t *testing.T) {
	h := newHandler(seededStore())
	rec := do(h, p1Viewer, http.MethodGet, "/api/v1/incidents/inc1/updates", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list updates = %d, want 200", rec.Code)
	}
	var ups []domain.IncidentUpdate
	_ = json.Unmarshal(rec.Body.Bytes(), &ups)
	if len(ups) != 1 {
		t.Fatalf("inc1 should have 1 update, got %d", len(ups))
	}
}

func TestPostmortemRequiresResolved(t *testing.T) {
	h := newHandler(seededStore())
	// inc1 is investigating → postmortem rejected.
	if rec := do(h, o1Admin, http.MethodPut, "/api/v1/incidents/inc1/postmortem", `{"body":"why"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("postmortem on open incident = %d, want 400", rec.Code)
	}
	// No postmortem yet → 404.
	if rec := do(h, o1Viewer, http.MethodGet, "/api/v1/incidents/inc1/postmortem", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get missing postmortem = %d, want 404", rec.Code)
	}
	// Resolve inc1, then publish a postmortem.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/incidents/inc1/updates", `{"status":"resolved"}`); rec.Code != http.StatusCreated {
		t.Fatalf("resolve = %d, want 201", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodPut, "/api/v1/incidents/inc1/postmortem", `{"body":"root cause + fix"}`); rec.Code != http.StatusOK {
		t.Fatalf("publish postmortem = %d, want 200", rec.Code)
	}
	if rec := do(h, o1Viewer, http.MethodGet, "/api/v1/incidents/inc1/postmortem", ""); rec.Code != http.StatusOK {
		t.Fatalf("get postmortem = %d, want 200", rec.Code)
	}
}

// FR-022 invariant 13: the incident detail names the members the SERVICE had at open
// time. Three answers must stay distinguishable and two of them are empty, which is the
// whole reason the field is a pointer:
//
//	no snapshot at all    → the key is ABSENT      (a monitor or project-level incident)
//	a snapshot of nothing → `"members": []`        (a service that genuinely had none)
//	a failed read         → `members_unavailable`  (never served as "no members")
//
// The store proves the durable half — the snapshot keeps naming a member deleted since
// (TestOpeningAServiceIncidentIsIdempotentAndSnapshotsItsMembers). This pins that the
// distinction survives the JSON, which is where a three-state answer usually dies.
func TestIncidentDetailNamesTheMembersTheServiceHadAtOpenTime(t *testing.T) {
	fs := seededStore()
	inc := fs.incidents["inc1"]
	inc.ServiceID = "svc1"
	fs.incidents["inc1"] = inc
	fs.memberSnapshots = map[string][]domain.IncidentMember{
		"inc1": {{MonitorID: "mon-gone", Name: "checkout-api", Roles: []string{"context", "sli"}}},
	}
	h := newHandler(fs)

	decode := func(t *testing.T) struct {
		Members            *[]domain.IncidentMember `json:"members"`
		MembersUnavailable bool                     `json:"members_unavailable"`
	} {
		t.Helper()
		rec := do(h, p1Viewer, http.MethodGet, "/api/v1/incidents/inc1", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("detail = %d: %s", rec.Code, rec.Body.String())
		}
		// A FRESH value every time: both fields would otherwise keep a previous answer
		// and an assertion could pass for the wrong reason.
		var out struct {
			Members            *[]domain.IncidentMember `json:"members"`
			MembersUnavailable bool                     `json:"members_unavailable"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	got := decode(t)
	if got.Members == nil || len(*got.Members) != 1 || (*got.Members)[0].Name != "checkout-api" {
		t.Fatalf("service incident members = %v, want the one snapshotted member — the snapshot is "+
			"written but nothing READS it, so a postmortem cannot name who was in the service", got.Members)
	}
	if len((*got.Members)[0].Roles) != 2 {
		t.Errorf("member roles = %v, want both roles the declaration gave it", (*got.Members)[0].Roles)
	}

	// A service that genuinely had no members: present, and empty.
	fs.memberSnapshots["inc1"] = []domain.IncidentMember{}
	if got := decode(t); got.Members == nil || len(*got.Members) != 0 {
		t.Fatalf("empty snapshot = %v, want a present-but-empty list: 'this service had no members' is a "+
			"fact, not a missing answer", got.Members)
	}

	// No snapshot at all — a monitor or project-level incident. The key must be ABSENT,
	// or a monitor incident would claim a service's empty membership.
	delete(fs.memberSnapshots, "inc1")
	if got := decode(t); got.Members != nil {
		t.Fatalf("an incident with NO snapshot reported members = %v, which makes 'no snapshot' and "+
			"'no members' the same answer", got.Members)
	}

	// A failed read is disclosed, on the same terms as the impacts ([288] P1-4).
	fs.memberSnapshotErr = errors.New("snapshot read exploded")
	if got := decode(t); got.Members != nil || !got.MembersUnavailable {
		t.Fatalf("degraded read = members %v / unavailable %v, want absent + true", got.Members, got.MembersUnavailable)
	}
}
