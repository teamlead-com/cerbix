package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// FR-030 invariants 1 and 2 at the API: 201 code points are refused with the field named; on update an
// omitted description is unchanged and "" clears it.
func TestMonitorDescriptionAtTheAPI(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)

	over := strings.Repeat("я", 201)
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"described","type":"tcp","target":"db:5432","description":"`+over+`"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "description") {
		t.Fatalf("201 code points: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"described","type":"tcp","target":"db:5432","description":"  What it is for.  "}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID          string `json:"id"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Description != "What it is for." {
		t.Fatalf("created description = %q, want trimmed", created.Description)
	}

	// Omitted on update: unchanged.
	rec = do(h, o1Admin, http.MethodPatch, "/api/v1/monitors/"+created.ID, `{"name":"renamed"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"description":"What it is for."`) {
		t.Fatalf("omitted on update: %d %s", rec.Code, rec.Body.String())
	}
	// "" on update: cleared.
	rec = do(h, o1Admin, http.MethodPatch, "/api/v1/monitors/"+created.ID, `{"description":""}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"description":""`) {
		t.Fatalf("cleared on update: %d %s", rec.Code, rec.Body.String())
	}
}
