package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// The instance-level audit view (FR: global audit, iter-0155). Two properties, and the first is the
// one that matters: an org admin must never reach it. Instance-level entries are what a GLOBAL
// admin's own actions leave — `user.global_admin`, `user.delete`, provider and outbox operations —
// so a leak here hands one tenant's admin the history of the whole installation.
func TestGlobalAuditIsGlobalAdminOnly(t *testing.T) {
	fs := seededStore()
	fs.globalAudit = []domain.AuditEntry{
		{ID: "a1", Action: "user.global_admin", Target: "u2", CreatedAt: time.Now().UTC()},
		{ID: "a2", Action: "user.delete", Target: "u3", CreatedAt: time.Now().UTC().Add(-time.Minute)},
	}
	h := newHandler(fs)

	if rec := do(h, o1Admin, http.MethodGet, "/api/v1/admin/audit", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("org admin on the instance audit = %d, want 403 — an org admin must not read the "+
			"installation's own history", rec.Code)
	}
	if rec := do(h, o1Viewer, http.MethodGet, "/api/v1/admin/audit", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer on the instance audit = %d, want 403", rec.Code)
	}

	rec := do(h, globalAdmin, http.MethodGet, "/api/v1/admin/audit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("global admin = %d, want 200", rec.Code)
	}
	var listed []domain.AuditEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 2 || listed[0].Action != "user.global_admin" {
		t.Fatalf("listed = %+v, want the two instance entries newest first", listed)
	}
	// The entries carry no organization by construction; a non-empty org_id here would mean an
	// org-scoped row reached the instance listing.
	for _, e := range listed {
		if e.OrgID != "" {
			t.Errorf("entry %s carries org_id %q — the instance listing is org_id IS NULL only", e.ID, e.OrgID)
		}
	}
}

// The limit rides through to the store rather than being ignored, and a hostile value cannot ask for
// the whole table.
func TestGlobalAuditLimitIsBounded(t *testing.T) {
	fs := seededStore()
	for i := 0; i < 120; i++ {
		fs.globalAudit = append(fs.globalAudit, domain.AuditEntry{ID: string(rune('a' + i%26)), Action: "user.delete"})
	}
	h := newHandler(fs)

	var listed []domain.AuditEntry
	rec := do(h, globalAdmin, http.MethodGet, "/api/v1/admin/audit?limit=5", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if len(listed) != 5 {
		t.Fatalf("limit=5 returned %d entries", len(listed))
	}

	rec = do(h, globalAdmin, http.MethodGet, "/api/v1/admin/audit?limit=99999", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if len(listed) != 100 {
		t.Fatalf("limit=99999 returned %d entries, want the clamp at 100", len(listed))
	}
}
