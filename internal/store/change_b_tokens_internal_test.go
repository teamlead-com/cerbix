package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-025 D12 at the store's edges (invariant 17; iter-0165 task 2, Agent B): an EMPTY allow-list
// is stored as a list (`{}`, not NULL) and read back as a non-nil empty slice on every read
// path — so the middleware's nil-preserving copy hands authz a list that grants nothing; a list
// with duplicates is validated entry by entry and stored VERBATIM (the catalogue check validates
// names, it does not dedupe — documented, not a defect); an unknown entry among duplicates is
// still refused.
func TestApiTokenEmptyActionsListIsStoredAsAListAndDuplicatesAreKeptVerbatim(t *testing.T) {
	st, ctx := declStore(t)
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	none, err := st.CreateApiToken(ctx, domain.ApiToken{OrgID: org.ID, Name: "none", Role: domain.RoleOrgAdmin, Actions: []string{}}, HashToken("s-none"))
	if err != nil {
		t.Fatalf("create with an empty list: %v", err)
	}
	var isNull bool
	var card int
	if err := st.pool.QueryRow(ctx, `SELECT actions IS NULL, COALESCE(cardinality(actions), -1) FROM api_tokens WHERE id = $1`, none.ID).Scan(&isNull, &card); err != nil {
		t.Fatal(err)
	}
	if isNull || card != 0 {
		t.Fatalf("stored actions: null=%v cardinality=%d, want a non-null empty array", isNull, card)
	}
	reads := map[string]func() (domain.ApiToken, error){
		"create":  func() (domain.ApiToken, error) { return none, nil },
		"by hash": func() (domain.ApiToken, error) { return st.ApiTokenByHash(ctx, HashToken("s-none")) },
		"by id":   func() (domain.ApiToken, error) { return st.GetApiToken(ctx, none.ID) },
		"list": func() (domain.ApiToken, error) {
			all, err := st.ListApiTokensByOrg(ctx, org.ID)
			for _, tk := range all {
				if tk.ID == none.ID {
					return tk, err
				}
			}
			return domain.ApiToken{}, err
		},
	}
	for name, read := range reads {
		tk, err := read()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if tk.Actions == nil || len(tk.Actions) != 0 {
			t.Fatalf("%s: Actions = %#v, want a non-nil empty list (nil would mean the role decides)", name, tk.Actions)
		}
		// What the auth layer hands authz from this row grants nothing, org_admin or not.
		p := authz.Principal{Memberships: []domain.Membership{{OrgID: org.ID, Role: tk.Role}}, ViaToken: true, Actions: []authz.Action{}}
		if p.Can(authz.ActionOrgRead, org.ID, "") || p.Can(authz.ActionProjectRead, org.ID, "p1") {
			t.Fatalf("%s: an empty stored list widened to the role", name)
		}
	}

	dup, err := st.CreateApiToken(ctx, domain.ApiToken{OrgID: org.ID, Name: "dup", Role: domain.RoleEditor,
		Actions: []string{"change:record", "change:record", "gate:evaluate", "change:record"}}, HashToken("s-dup"))
	if err != nil {
		t.Fatalf("create with duplicates: %v", err)
	}
	if want := []string{"change:record", "change:record", "gate:evaluate", "change:record"}; !reflect.DeepEqual(dup.Actions, want) {
		t.Fatalf("duplicates stored as %v, want verbatim %v", dup.Actions, want)
	}
	_, err = st.CreateApiToken(ctx, domain.ApiToken{OrgID: org.ID, Name: "dup-bad", Role: domain.RoleEditor,
		Actions: []string{"change:record", "change:record", "change:read"}}, HashToken("s-dup-bad"))
	var ce *domain.ChangeError
	if !errors.As(err, &ce) || ce.Code != domain.ChangeErrActionUnknown || !strings.Contains(ce.Msg, "change:read") {
		t.Fatalf("unknown among duplicates: %v, want action_unknown naming change:read", err)
	}
	if _, err := st.ApiTokenByHash(ctx, HashToken("s-dup-bad")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused token exists: %v", err)
	}
}
