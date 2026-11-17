package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func TestPatchProjectRefusesInOrder(t *testing.T) {
	h := newHandler(seededStore())
	bad := `{"default_incident_impact":"severe"}`
	if rec := do(h, outsider, http.MethodPatch, "/api/v1/projects/p1", bad); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider = %d, want 404", rec.Code)
	}
	if rec := do(h, p1Viewer, http.MethodPatch, "/api/v1/projects/p1", bad); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer = %d, want 403", rec.Code)
	}
	rec := do(h, p1Admin, http.MethodPatch, "/api/v1/projects/p1", bad)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "default_incident_impact must be one of none, minor, major, critical") {
		t.Fatalf("invalid value = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, p1Admin, http.MethodPatch, "/api/v1/projects/p1", `{"name":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d, want 400", rec.Code)
	}
}

func TestAProjectAdminSetsTheDefault(t *testing.T) {
	h := newHandler(seededStore())
	rec := do(h, p1Admin, http.MethodPatch, "/api/v1/projects/p1", `{"default_incident_impact":"critical"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"default_incident_impact":"critical"`) {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body.String())
	}
}

func TestATokenCallerReachesTheStoreAsItself(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	rec := do(h, p1TokenAllowed, http.MethodPatch, "/api/v1/projects/p1", `{"default_incident_impact":"minor"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("token patch = %d %s", rec.Code, rec.Body.String())
	}
	if len(fs.projectDefaultActors) != 1 || !fs.projectDefaultActors[0].ViaToken || fs.projectDefaultActors[0].Label == "" {
		t.Fatalf("recorded actors = %+v", fs.projectDefaultActors)
	}
}

func TestAnEmptyOrNullPatchIsANoOp(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	for _, body := range []string{`{}`, `{"default_incident_impact":null}`} {
		if rec := do(h, p1Admin, http.MethodPatch, "/api/v1/projects/p1", body); rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", body, rec.Code)
		}
	}
	if len(fs.projectDefaultActors) != 0 {
		t.Fatalf("the store was called %d times", len(fs.projectDefaultActors))
	}
}

func TestProjectResponsesCarryTheDefault(t *testing.T) {
	h := newHandler(seededStore())
	const want = `"default_incident_impact":"` + string(domain.ImpactMajor) + `"`
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/organizations/o1/projects", ""},
		{http.MethodPost, "/api/v1/organizations/o1/projects", `{"slug":"fresh","name":"Fresh"}`},
		{http.MethodGet, "/api/v1/projects/p1", ""},
	} {
		rec := do(h, o1Admin, c.method, c.path, c.body)
		if rec.Code >= 300 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s %s = %d %s, want %s", c.method, c.path, rec.Code, rec.Body.String(), want)
		}
	}
}

func TestASameValuePatchAnswersWithTheProject(t *testing.T) {
	h := newHandler(seededStore())
	rec := do(h, p1Admin, http.MethodPatch, "/api/v1/projects/p1", `{"default_incident_impact":"major"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"default_incident_impact":"major"`) ||
		!strings.Contains(rec.Body.String(), `"id":"p1"`) {
		t.Fatalf("same-value patch = %d %s", rec.Code, rec.Body.String())
	}
}

// Spec §4.4: the default is an instance-owned preference, not part of what a file provider owns, so
// PATCH never consults file management. The fake refuses DeleteProject for this project with
// ErrManagedByFile (so it IS file-managed); the PATCH still answers 200 with the new value, not 409.
func TestPatchOnAFileManagedProjectAnswers200(t *testing.T) {
	fs := seededStore()
	fs.managedProjects = map[string]bool{"p1": true}
	h := newHandler(fs)
	if rec := do(h, o1Admin, http.MethodDelete, "/api/v1/projects/p1", ""); rec.Code != http.StatusConflict {
		t.Fatalf("precondition: delete = %d, want 409 (p1 must be file-managed)", rec.Code)
	}
	rec := do(h, p1Admin, http.MethodPatch, "/api/v1/projects/p1", `{"default_incident_impact":"minor"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"default_incident_impact":"minor"`) {
		t.Fatalf("patch on a file-managed project = %d %s, want 200 carrying minor", rec.Code, rec.Body.String())
	}
}

// Spec §4.4: a new project starts at major; create does not accept the field, and decodeJSON refuses
// unknown fields.
func TestCreateProjectRefusesTheDefaultField(t *testing.T) {
	rec := do(newHandler(seededStore()), o1Admin, http.MethodPost, "/api/v1/organizations/o1/projects",
		`{"slug":"fresh","name":"Fresh","default_incident_impact":"critical"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with default_incident_impact = %d %s, want 400", rec.Code, rec.Body.String())
	}
}
