package api_test

import (
	"net/http"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-042 §4.2 — each creating door states the basis it used (I14).
func TestManualAndAlertmanagerCreatesStateTheirBasis(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)

	rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/incidents", `{"title":"db slow","impact":"major","body":"opened"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("manual create = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(fs.createBases) != 1 || fs.createBases[0] != domain.ImpactBasisManual {
		t.Fatalf("manual create bases = %v, want [manual]", fs.createBases)
	}

	fire := `{"alerts":[{"status":"firing","fingerprint":"fp-basis","labels":{"alertname":"HighLatency","severity":"critical"},"annotations":{"summary":"p99 high"}}]}`
	if rec := do(h, tokenEditor, http.MethodPost, "/api/v1/projects/p1/alerts/alertmanager", fire); rec.Code != http.StatusOK {
		t.Fatalf("alertmanager firing = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(fs.createBases) != 2 || fs.createBases[1] != domain.ImpactBasisAlertmanager {
		t.Fatalf("alertmanager create bases = %v, want [manual alertmanager]", fs.createBases)
	}
}
