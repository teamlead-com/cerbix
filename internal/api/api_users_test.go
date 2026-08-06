package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func TestAdminUsersAdminOnly(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/admin/users", ""},
		{http.MethodPatch, "/api/v1/admin/users/u1", `{"is_global_admin":true}`},
		{http.MethodDelete, "/api/v1/admin/users/u1", ""},
	} {
		if rec := do(h, o1Admin, tc.method, tc.path, tc.body); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s as org-admin = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
}

func TestAdminUsersListAndSearch(t *testing.T) {
	fs := seededStore()
	fs.users["u1"] = domain.User{ID: "u1", Email: "ada@x.com", DisplayName: "Ada"}
	fs.users["u2"] = domain.User{ID: "u2", Email: "lone@x.com", DisplayName: "Lone Wolf"}
	fs.members["o1"] = append(fs.members["o1"], domain.Membership{ID: "m1", UserID: "u1", OrgID: "o1", Role: domain.RoleEditor})
	h := newHandler(fs)

	rec := do(h, globalAdmin, http.MethodGet, "/api/v1/admin/users", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200", rec.Code)
	}
	var users []domain.AdminUser
	_ = json.Unmarshal(rec.Body.Bytes(), &users)
	if len(users) != 2 {
		t.Fatalf("listed %d users, want 2", len(users))
	}
	if users[0].Email != "ada@x.com" || len(users[0].Memberships) != 1 {
		t.Fatalf("first user = %+v, want ada with 1 membership", users[0])
	}
	if users[1].Email != "lone@x.com" || len(users[1].Memberships) != 0 {
		t.Fatalf("second user = %+v, want org-less lone", users[1])
	}

	rec = do(h, globalAdmin, http.MethodGet, "/api/v1/admin/users?q=wolf", "")
	users = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &users)
	if len(users) != 1 || users[0].Email != "lone@x.com" {
		t.Fatalf("search = %+v, want just lone", users)
	}
}

func TestAdminUsersGlobalAdminToggle(t *testing.T) {
	fs := seededStore()
	// The acting principal must exist for the self-change guard and counters.
	fs.users["ga"] = domain.User{ID: "ga", Email: "root@x.com", IsGlobalAdmin: true}
	fs.users["u1"] = domain.User{ID: "u1", Email: "ada@x.com"}
	h := newHandler(fs)

	// Grant.
	rec := do(h, globalAdmin, http.MethodPatch, "/api/v1/admin/users/u1", `{"is_global_admin":true}`)
	if rec.Code != http.StatusOK || !fs.users["u1"].IsGlobalAdmin {
		t.Fatalf("grant = %d (admin=%v), want 200/true", rec.Code, fs.users["u1"].IsGlobalAdmin)
	}
	// Revoke while another admin remains.
	if rec := do(h, globalAdmin, http.MethodPatch, "/api/v1/admin/users/u1", `{"is_global_admin":false}`); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d, want 200", rec.Code)
	}
	// Changing your own flag is rejected.
	if rec := do(h, globalAdmin, http.MethodPatch, "/api/v1/admin/users/ga", `{"is_global_admin":false}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("self-change = %d, want 400", rec.Code)
	}
	// Demoting the last global admin (someone else's account) is rejected.
	fs.users["ga2"] = domain.User{ID: "ga2", Email: "other@x.com", IsGlobalAdmin: true}
	u := fs.users["ga"]
	u.IsGlobalAdmin = false
	fs.users["ga"] = u
	if rec := do(h, globalAdmin, http.MethodPatch, "/api/v1/admin/users/ga2", `{"is_global_admin":false}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("demote last = %d, want 400", rec.Code)
	}
	// Missing field / unknown user.
	if rec := do(h, globalAdmin, http.MethodPatch, "/api/v1/admin/users/u1", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing field = %d, want 400", rec.Code)
	}
	if rec := do(h, globalAdmin, http.MethodPatch, "/api/v1/admin/users/nope", `{"is_global_admin":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown user = %d, want 404", rec.Code)
	}
}

func TestAdminUsersDelete(t *testing.T) {
	fs := seededStore()
	fs.users["ga"] = domain.User{ID: "ga", Email: "root@x.com", IsGlobalAdmin: true}
	fs.users["u1"] = domain.User{ID: "u1", Email: "ada@x.com"}
	fs.members["o1"] = append(fs.members["o1"], domain.Membership{ID: "m1", UserID: "u1", OrgID: "o1", Role: domain.RoleEditor})
	h := newHandler(fs)

	// Self-deletion is rejected.
	if rec := do(h, globalAdmin, http.MethodDelete, "/api/v1/admin/users/ga", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("self-delete = %d, want 400", rec.Code)
	}
	// Deleting the last global admin is rejected.
	fs.users["ga2"] = domain.User{ID: "ga2", Email: "other@x.com", IsGlobalAdmin: true}
	u := fs.users["ga"]
	u.IsGlobalAdmin = false
	fs.users["ga"] = u
	if rec := do(h, globalAdmin, http.MethodDelete, "/api/v1/admin/users/ga2", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("delete last admin = %d, want 400", rec.Code)
	}
	// A regular user goes away along with the memberships.
	if rec := do(h, globalAdmin, http.MethodDelete, "/api/v1/admin/users/u1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", rec.Code)
	}
	if _, ok := fs.users["u1"]; ok {
		t.Fatal("user not deleted")
	}
	if len(fs.members["o1"]) != 0 {
		t.Fatalf("memberships not removed: %+v", fs.members["o1"])
	}
	if rec := do(h, globalAdmin, http.MethodDelete, "/api/v1/admin/users/u1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("double delete = %d, want 404", rec.Code)
	}
}
