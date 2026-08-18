package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// A project-level SLO objective (iter-0155; func-audit-gaps-2 called it "a real feature, not a gap
// fix"). The owner's decision is REPORTING ONLY, and this file pins the three things that decision
// means on the wire — because a promise about what an operator's edit cannot cause has to be refused
// somewhere a reader can see, not implied by the absence of a code path.
func TestProjectObjectiveIsReportingOnly(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)

	// A burn field is REFUSED, not dropped. A silently ignored `burn_alert: true` is the worst
	// outcome available: the operator believes the project pages and nothing ever will.
	rec := do(h, o1Admin, http.MethodPut, "/api/v1/projects/p1/sla-target", `{"window":"30d","objective":99.9,"burn_alert":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("burn_alert at project scope = %d, want 400 — a field that looks configurable and is "+
			"ignored is worse than no field", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodPut, "/api/v1/projects/p1/sla-target", `{"window":"30d","objective":99.9,"burn_rules":[]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("burn_rules at project scope = %d, want 400", rec.Code)
	}

	// The objective itself goes through the ONE canonical rule (D-0165): the open interval (0,100).
	for _, bad := range []string{`{"objective":100}`, `{"objective":0}`, `{"objective":99.99995}`} {
		if rec := do(h, o1Admin, http.MethodPut, "/api/v1/projects/p1/sla-target", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("objective %s = %d, want 400 from the canonical rule", bad, rec.Code)
		}
	}

	// A good write is stored and read back canonical.
	rec = do(h, o1Admin, http.MethodPut, "/api/v1/projects/p1/sla-target", `{"window":"30d","objective":99.99994}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid objective = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var stored domain.SLATarget
	_ = json.Unmarshal(rec.Body.Bytes(), &stored)
	if stored.Objective != 99.9999 {
		t.Errorf("stored objective = %v, want the canonical 99.9999", stored.Objective)
	}
	if stored.BurnAlertEnabled || len(stored.BurnRules) != 0 {
		t.Errorf("a project target came back with burn alerting: %+v", stored)
	}

	rec = do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/sla-target?window=30d", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("read back = %d, want 200", rec.Code)
	}
}

// Authz and the honest absence: a viewer may read, only an editor may write, and a project with no
// objective says 404 rather than inventing one.
func TestProjectObjectiveAuthzAndAbsence(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)

	if rec := do(h, p1Viewer, http.MethodPut, "/api/v1/projects/p1/sla-target", `{"objective":99.9}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer write = %d, want 403", rec.Code)
	}
	if rec := do(h, outsider, http.MethodGet, "/api/v1/projects/p1/sla-target", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign org read = %d, want 404 (never 403: a non-member learns nothing)", rec.Code)
	}
	if rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/sla-target", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unset objective = %d, want 404 — absent is the answer; 99.9 would be a number nobody chose", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodGet, "/api/v1/projects/p1/sla-target?window=17d", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown window = %d, want 400", rec.Code)
	}
}

// The report STATES the objective when the project has one, and states nothing when it does not —
// never a zero budget, which reads as "you have spent it all".
func TestProjectReportStatesTheObjectiveOnlyWhenSet(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)

	rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/sla", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("report = %d, want 200", rec.Code)
	}
	var before struct {
		Windows []map[string]any `json:"windows"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &before)
	if len(before.Windows) == 0 {
		t.Fatal("report carries no windows")
	}
	for _, w := range before.Windows {
		if _, ok := w["objective"]; ok {
			t.Fatalf("window %v states an objective the project never set", w["window"])
		}
		if _, ok := w["error_budget"]; ok {
			t.Fatalf("window %v states a budget with no objective behind it", w["window"])
		}
	}

	if rec := do(h, o1Admin, http.MethodPut, "/api/v1/projects/p1/sla-target", `{"window":"30d","objective":99.9}`); rec.Code != http.StatusOK {
		t.Fatalf("set objective = %d", rec.Code)
	}
	rec = do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/sla", "")
	var after struct {
		Windows []map[string]any `json:"windows"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &after)
	var stated int
	for _, w := range after.Windows {
		if w["window"] == "30d" {
			if w["objective"] == nil {
				t.Error("the 30d window does not state the objective that was just set")
			}
			if w["error_budget"] == nil {
				t.Error("the 30d window states an objective with no budget derived from it")
			}
			// Burn fields must stay absent: the schema refuses paging at this scope, and
			// `burn_alert: false` would read as "not yet" rather than "cannot".
			if _, ok := w["burn_alert"]; ok {
				t.Error("the project window carries a burn_alert field — this scope cannot page")
			}
			stated++
		} else if w["objective"] != nil {
			t.Errorf("window %v borrowed the 30d objective", w["window"])
		}
	}
	if stated != 1 {
		t.Fatalf("the 30d window was not found in the report")
	}
}
